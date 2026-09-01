package main

import (
	"bytes"
	"context"
	"crypto/ecdh"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"time"
)

// Registration is everything we need to rebuild a wireproxy config, and the
// only thing we persist. It is written with 0600 because PrivateKey is a
// WireGuard secret.
type Registration struct {
	PrivateKey string `json:"private_key"`
	PublicKey  string `json:"public_key"`
	PeerKey    string `json:"peer_public_key"`
	AddressV4  string `json:"address_v4"`
	AddressV6  string `json:"address_v6"`
	ClientID   string `json:"client_id"`
	DeviceID   string `json:"device_id"`
	Token      string `json:"token"`
	Registered string `json:"registered_at"`
}

// Valid reports whether a persisted registration has everything the config
// renderer needs. Anything less and we re-register rather than emit a
// half-populated tunnel.
func (r *Registration) Valid() bool {
	return r != nil && r.PrivateKey != "" && r.PeerKey != "" && r.AddressV4 != "" && r.AddressV6 != ""
}

// warpRegResponse mirrors only the fields of Cloudflare's registration
// response that we consume.
type warpRegResponse struct {
	ID     string `json:"id"`
	Token  string `json:"token"`
	Config struct {
		ClientID string `json:"client_id"`
		Peers    []struct {
			PublicKey string `json:"public_key"`
		} `json:"peers"`
		Interface struct {
			Addresses struct {
				V4 string `json:"v4"`
				V6 string `json:"v6"`
			} `json:"addresses"`
		} `json:"interface"`
	} `json:"config"`
}

// generateKeypair produces a clamped X25519 private key and its public key,
// both base64-encoded the way WireGuard and Cloudflare expect.
//
// The key is clamped here rather than left to the consumer: wireguard-go and
// crypto/ecdh both clamp internally during scalar multiplication, so an
// unclamped key would still work, but persisting the canonical clamped form
// means the bytes on disk are exactly the bytes in use.
func generateKeypair() (priv string, pub string, err error) {
	var k [32]byte
	if _, err := rand.Read(k[:]); err != nil {
		return "", "", fmt.Errorf("reading random bytes: %w", err)
	}
	k[0] &= 248
	k[31] &= 127
	k[31] |= 64

	key, err := ecdh.X25519().NewPrivateKey(k[:])
	if err != nil {
		return "", "", fmt.Errorf("deriving X25519 key: %w", err)
	}
	return base64.StdEncoding.EncodeToString(key.Bytes()),
		base64.StdEncoding.EncodeToString(key.PublicKey().Bytes()),
		nil
}

// Register enrols a fresh device with Cloudflare WARP and returns the
// resulting tunnel parameters. It retries with exponential backoff, because a
// container restarting into a rate-limited or briefly unreachable API should
// wait rather than crash-loop.
func Register(ctx context.Context, s *Settings) (*Registration, error) {
	priv, pub, err := generateKeypair()
	if err != nil {
		return nil, err
	}

	body, err := json.Marshal(map[string]string{
		// Cloudflare wants a millisecond-precision timestamp with an explicit
		// offset; it only checks that the field parses and is recent.
		"tos":      time.Now().UTC().Format("2006-01-02T15:04:05.000-07:00"),
		"key":      pub,
		"referrer": s.Referrer,
	})
	if err != nil {
		return nil, fmt.Errorf("encoding registration payload: %w", err)
	}

	client := &http.Client{Timeout: s.Timeout}

	var lastErr error
	for attempt := 1; attempt <= s.Retries; attempt++ {
		reg, err := postRegistration(ctx, client, s.API, body)
		if err == nil {
			reg.PrivateKey = priv
			reg.PublicKey = pub
			reg.Registered = time.Now().UTC().Format(time.RFC3339)
			return reg, nil
		}
		lastErr = err

		if attempt == s.Retries || ctx.Err() != nil {
			break
		}
		// 2s, 4s, 8s, ... capped at 30s.
		backoff := time.Duration(1<<attempt) * time.Second
		if backoff > 30*time.Second {
			backoff = 30 * time.Second
		}
		logf("registration attempt %d/%d failed: %v (retrying in %s)", attempt, s.Retries, err, backoff)
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(backoff):
		}
	}

	return nil, fmt.Errorf("registering with WARP after %d attempts: %w", s.Retries, lastErr)
}

func postRegistration(ctx context.Context, client *http.Client, api string, body []byte) (*Registration, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, api, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("building request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "okhttp/3.12.1")

	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	// Cap the read: a malformed or hostile endpoint should not be able to make
	// us allocate without bound.
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, fmt.Errorf("reading response: %w", err)
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return nil, fmt.Errorf("WARP API returned %s: %s", resp.Status, truncate(string(raw), 200))
	}

	var parsed warpRegResponse
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return nil, fmt.Errorf("decoding response: %w", err)
	}
	if len(parsed.Config.Peers) == 0 {
		return nil, fmt.Errorf("WARP API response contained no peers")
	}

	reg := &Registration{
		PeerKey:   parsed.Config.Peers[0].PublicKey,
		AddressV4: parsed.Config.Interface.Addresses.V4,
		AddressV6: parsed.Config.Interface.Addresses.V6,
		ClientID:  parsed.Config.ClientID,
		DeviceID:  parsed.ID,
		Token:     parsed.Token,
	}
	if reg.PeerKey == "" || reg.AddressV4 == "" || reg.AddressV6 == "" {
		return nil, fmt.Errorf("WARP API response was missing peer key or interface addresses")
	}
	return reg, nil
}

// EnsureRegistration returns a usable registration, reusing the one cached in
// the state directory when possible.
//
// Persistence is best-effort by design: an unwritable or missing state
// directory (a read-only bind mount, a volume owned by another uid) degrades
// to registering afresh on each boot rather than failing the container.
func EnsureRegistration(ctx context.Context, s *Settings) (*Registration, error) {
	path := statePath(s)

	if path != "" && !s.Reregister {
		if reg, err := loadRegistration(path); err != nil {
			logf("ignoring cached registration at %s: %v", path, err)
		} else if reg.Valid() {
			logf("reusing cached WARP registration from %s (registered %s)", path, reg.Registered)
			return reg, nil
		}
	}

	logf("registering a new device with Cloudflare WARP at %s", s.API)
	reg, err := Register(ctx, s)
	if err != nil {
		return nil, err
	}
	logf("registered: v4=%s v6=%s", reg.AddressV4, reg.AddressV6)

	if path != "" {
		if err := saveRegistration(path, reg); err != nil {
			logf("warning: could not persist registration to %s: %v", path, err)
			logf("warning: a new device will be registered on every restart; "+
				"mount a writable volume at %s to avoid this", s.StateDir)
		} else {
			logf("cached registration at %s", path)
		}
	}
	return reg, nil
}

func statePath(s *Settings) string {
	if s.StateDir == "" {
		return ""
	}
	return filepath.Join(s.StateDir, "registration.json")
}

func loadRegistration(path string) (*Registration, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var reg Registration
	if err := json.Unmarshal(raw, &reg); err != nil {
		return nil, fmt.Errorf("malformed cache: %w", err)
	}
	if !reg.Valid() {
		return nil, fmt.Errorf("cache is missing required fields")
	}
	return &reg, nil
}

func saveRegistration(path string, reg *Registration) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	raw, err := json.MarshalIndent(reg, "", "  ")
	if err != nil {
		return err
	}

	// Write-then-rename so a crash mid-write cannot leave a truncated cache
	// that we would later refuse to parse.
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, append(raw, '\n'), 0o600); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		os.Remove(tmp)
		return err
	}
	return nil
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}
