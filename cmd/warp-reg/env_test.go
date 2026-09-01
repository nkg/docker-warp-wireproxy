package main

import (
	"encoding/base64"
	"testing"
)

func TestListenerBindWinsOverPort(t *testing.T) {
	t.Setenv("SOCKS5_BIND", "127.0.0.1:1234")
	t.Setenv("SOCKS5_PORT", "9999")

	l, err := listener("SOCKS5", "0.0.0.0:1080")
	if err != nil {
		t.Fatal(err)
	}
	if l.Bind != "127.0.0.1:1234" {
		t.Errorf("Bind = %q, want _BIND to win over _PORT", l.Bind)
	}
}

func TestListenerPortBuildsBind(t *testing.T) {
	t.Setenv("HTTP_PORT", "8888")

	l, err := listener("HTTP", "0.0.0.0:8080")
	if err != nil {
		t.Fatal(err)
	}
	if l.Bind != "0.0.0.0:8888" {
		t.Errorf("Bind = %q, want 0.0.0.0:8888", l.Bind)
	}
}

func TestListenerHostOverride(t *testing.T) {
	t.Setenv("HTTP_PORT", "8888")
	t.Setenv("HTTP_HOST", "127.0.0.1")

	l, err := listener("HTTP", "0.0.0.0:8080")
	if err != nil {
		t.Fatal(err)
	}
	if l.Bind != "127.0.0.1:8888" {
		t.Errorf("Bind = %q, want 127.0.0.1:8888", l.Bind)
	}
}

// An explicitly empty _BIND disables the front-end; that is how you run a
// SOCKS5-only or HTTP-only container.
func TestListenerEmptyBindDisables(t *testing.T) {
	t.Setenv("HTTP_BIND", "")

	l, err := listener("HTTP", "0.0.0.0:8080")
	if err != nil {
		t.Fatal(err)
	}
	if l.Enabled() {
		t.Errorf("Bind = %q, want disabled", l.Bind)
	}
}

func TestListenerDefaultWhenUnset(t *testing.T) {
	l, err := listener("SOCKS5", "0.0.0.0:1080")
	if err != nil {
		t.Fatal(err)
	}
	if l.Bind != "0.0.0.0:1080" {
		t.Errorf("Bind = %q, want the fallback", l.Bind)
	}
}

// Half a credential pair would leave the proxy open while looking configured.
func TestListenerRejectsHalfCredentials(t *testing.T) {
	t.Setenv("SOCKS5_USER", "admin")

	if _, err := listener("SOCKS5", "0.0.0.0:1080"); err == nil {
		t.Error("want an error when _USER is set without _PASSWD")
	}
}

// Spaces break wireproxy's ini parsing of the credential value.
func TestListenerRejectsSpacesInCredentials(t *testing.T) {
	t.Setenv("SOCKS5_USER", "admin")
	t.Setenv("SOCKS5_PASSWD", "hunter 2")

	if _, err := listener("SOCKS5", "0.0.0.0:1080"); err == nil {
		t.Error("want an error for a password containing a space")
	}
}

func TestListenerRejectsBadPort(t *testing.T) {
	t.Setenv("SOCKS5_PORT", "70000")

	if _, err := listener("SOCKS5", "0.0.0.0:1080"); err == nil {
		t.Error("want an error for an out-of-range port")
	}
}

func TestLoadSettingsDefaults(t *testing.T) {
	s, err := LoadSettings()
	if err != nil {
		t.Fatal(err)
	}
	if s.MTU != defaultMTU {
		t.Errorf("MTU = %d, want %d", s.MTU, defaultMTU)
	}
	if s.API != defaultAPI {
		t.Errorf("API = %q", s.API)
	}
	if !s.Socks5.Enabled() || !s.HTTP.Enabled() {
		t.Error("both proxies should be enabled by default")
	}
	if len(s.DNS) != 4 {
		t.Errorf("DNS = %v, want the dual-stack Cloudflare default", s.DNS)
	}
}

func TestLoadSettingsRejectsNoListeners(t *testing.T) {
	t.Setenv("SOCKS5_BIND", "")
	t.Setenv("HTTP_BIND", "")

	if _, err := LoadSettings(); err == nil {
		t.Error("want an error when the container would listen on nothing")
	}
}

// EXTRA_CONFIG can supply listeners of its own (tunnels, SNI), so it makes
// disabling both built-in proxies legitimate.
func TestLoadSettingsAllowsNoListenersWithExtraConfig(t *testing.T) {
	t.Setenv("SOCKS5_BIND", "")
	t.Setenv("HTTP_BIND", "")
	t.Setenv("EXTRA_CONFIG", "[SNI]\nBindAddress = 0.0.0.0:443")

	if _, err := LoadSettings(); err != nil {
		t.Errorf("EXTRA_CONFIG should permit disabling both proxies: %v", err)
	}
}

func TestLoadSettingsRejectsBadMTU(t *testing.T) {
	t.Setenv("WARP_MTU", "9000")

	if _, err := LoadSettings(); err == nil {
		t.Error("want an error for an out-of-range MTU")
	}
}

func TestLoadSettingsRejectsBadBindAddress(t *testing.T) {
	t.Setenv("SOCKS5_BIND", "not-an-address")

	if _, err := LoadSettings(); err == nil {
		t.Error("want an error for a bind address with no port")
	}
}

func TestLoadSettingsRejectsMetricsWithoutInfo(t *testing.T) {
	t.Setenv("METRICS_BIND", "0.0.0.0:9095")
	t.Setenv("INFO_BIND", "")

	if _, err := LoadSettings(); err == nil {
		t.Error("want an error when the exporter has nothing to scrape")
	}
}

func TestEnvList(t *testing.T) {
	fallback := []string{"a"}

	if got := envList("NOT_SET_ANYWHERE", fallback); len(got) != 1 || got[0] != "a" {
		t.Errorf("unset should yield the fallback, got %v", got)
	}

	t.Setenv("SOME_LIST", " x , y ,, z ")
	got := envList("SOME_LIST", fallback)
	if len(got) != 3 || got[0] != "x" || got[2] != "z" {
		t.Errorf("envList = %v, want trimmed [x y z]", got)
	}

	t.Setenv("SOME_LIST", "")
	if got := envList("SOME_LIST", fallback); len(got) != 0 {
		t.Errorf("explicitly empty should yield an empty list, got %v", got)
	}
}

func TestEnvBool(t *testing.T) {
	for _, v := range []string{"1", "true", "TRUE", "yes", "on"} {
		t.Setenv("B", v)
		if !envBool("B", false) {
			t.Errorf("envBool(%q) = false, want true", v)
		}
	}
	for _, v := range []string{"0", "false", "no", "off"} {
		t.Setenv("B", v)
		if envBool("B", true) {
			t.Errorf("envBool(%q) = true, want false", v)
		}
	}
}

func TestGenerateKeypairIsClamped(t *testing.T) {
	priv, pub, err := generateKeypair()
	if err != nil {
		t.Fatal(err)
	}
	if priv == "" || pub == "" || priv == pub {
		t.Fatalf("bad keypair: priv=%q pub=%q", priv, pub)
	}

	raw := decodeBase64(t, priv)
	if len(raw) != 32 {
		t.Fatalf("private key is %d bytes, want 32", len(raw))
	}
	// Standard X25519 clamping, so the persisted bytes are the bytes in use.
	if raw[0]&7 != 0 {
		t.Errorf("low 3 bits of byte 0 not cleared: %08b", raw[0])
	}
	if raw[31]&128 != 0 {
		t.Errorf("high bit of byte 31 not cleared: %08b", raw[31])
	}
	if raw[31]&64 == 0 {
		t.Errorf("bit 6 of byte 31 not set: %08b", raw[31])
	}
}

func TestRegistrationValid(t *testing.T) {
	var nilReg *Registration
	if nilReg.Valid() {
		t.Error("nil registration should be invalid")
	}
	if (&Registration{PrivateKey: "a"}).Valid() {
		t.Error("partial registration should be invalid")
	}
	full := &Registration{PrivateKey: "a", PeerKey: "b", AddressV4: "c", AddressV6: "d"}
	if !full.Valid() {
		t.Error("complete registration should be valid")
	}
}

func decodeBase64(t *testing.T, s string) []byte {
	t.Helper()
	raw, err := base64.StdEncoding.DecodeString(s)
	if err != nil {
		t.Fatalf("decoding %q: %v", s, err)
	}
	return raw
}
