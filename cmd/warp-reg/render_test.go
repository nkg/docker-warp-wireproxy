package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func baseSettings() *Settings {
	return &Settings{
		Endpoint:           "engage.cloudflareclient.com:2408",
		MTU:                1280,
		Keepalive:          25,
		ConfPath:           "/etc/wireproxy/wireproxy.conf",
		DNS:                []string{"1.1.1.1", "2606:4700:4700::1111"},
		CheckAlive:         []string{"1.1.1.1"},
		CheckAliveInterval: 5,
		Socks5:             Listener{Bind: "0.0.0.0:1080"},
		HTTP:               Listener{Bind: "0.0.0.0:8080"},
	}
}

func testRegistration() *Registration {
	return &Registration{
		PrivateKey: "aPrivateKey=",
		PeerKey:    "aPeerKey=",
		AddressV4:  "172.16.0.2",
		AddressV6:  "2606:4700:110:8a1b::1",
	}
}

// Multi-valued keys must be emitted comma-separated on one line. wireproxy
// reads them with key.String(), which returns only the first occurrence, so
// repeated keys silently drop the IPv6 address and the ::/0 route.
func TestRenderConfigUsesCommaSeparatedMultiValues(t *testing.T) {
	got := RenderConfig(baseSettings(), testRegistration())

	for _, want := range []string{
		"Address = 172.16.0.2/32, 2606:4700:110:8a1b::1/128\n",
		"AllowedIPs = 0.0.0.0/0, ::/0\n",
		"DNS = 1.1.1.1, 2606:4700:4700::1111\n",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("config missing %q\ngot:\n%s", want, got)
		}
	}

	for _, key := range []string{"Address", "AllowedIPs", "DNS"} {
		if n := countKeyLines(got, key); n != 1 {
			t.Errorf("key %q appears on %d lines, want exactly 1 "+
				"(repeated keys are silently truncated by wireproxy)", key, n)
		}
	}
}

func countKeyLines(config, key string) int {
	n := 0
	for _, line := range strings.Split(config, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), key+" =") {
			n++
		}
	}
	return n
}

func TestRenderConfigOmitsDisabledListeners(t *testing.T) {
	s := baseSettings()
	s.HTTP = Listener{}
	got := RenderConfig(s, testRegistration())

	if strings.Contains(got, "[http]") {
		t.Errorf("disabled HTTP listener still emitted:\n%s", got)
	}
	if !strings.Contains(got, "[Socks5]") {
		t.Errorf("enabled SOCKS5 listener missing:\n%s", got)
	}
}

func TestRenderConfigEmitsCredentialsOnlyWhenSet(t *testing.T) {
	s := baseSettings()
	got := RenderConfig(s, testRegistration())
	if strings.Contains(got, "Username") {
		t.Errorf("credentials emitted when none configured:\n%s", got)
	}

	s.Socks5 = Listener{Bind: "0.0.0.0:1080", Username: "admin", Password: "hunter2"}
	got = RenderConfig(s, testRegistration())
	if !strings.Contains(got, "Username = admin\nPassword = hunter2\n") {
		t.Errorf("credentials missing:\n%s", got)
	}
}

// WGConfig mode must not emit an [Interface]/[Peer] pair, or wireproxy sees a
// duplicate device definition.
func TestRenderConfigWGConfigModeSkipsInterface(t *testing.T) {
	s := baseSettings()
	s.WGConfig = "/etc/wireproxy/wg.conf"
	got := RenderConfig(s, &Registration{})

	if strings.Contains(got, "[Interface]") || strings.Contains(got, "[Peer]") {
		t.Errorf("WGConfig mode emitted an inline device:\n%s", got)
	}
	if !strings.Contains(got, "WGConfig = /etc/wireproxy/wg.conf\n") {
		t.Errorf("WGConfig directive missing:\n%s", got)
	}
	if !strings.Contains(got, "[Socks5]") {
		t.Errorf("proxy sections should still be emitted in WGConfig mode:\n%s", got)
	}
}

func TestRenderConfigAppendsExtraConfig(t *testing.T) {
	s := baseSettings()
	s.ExtraConfig = "[TCPClientTunnel]\nBindAddress = 127.0.0.1:25565\nTarget = example.com:25565"
	got := RenderConfig(s, testRegistration())

	if !strings.Contains(got, "[TCPClientTunnel]") {
		t.Errorf("EXTRA_CONFIG not appended:\n%s", got)
	}
	if !strings.HasSuffix(got, "\n") {
		t.Error("config should end with a newline")
	}
}

func TestRenderConfigOmitsEmptyDNSAndCheckAlive(t *testing.T) {
	s := baseSettings()
	s.DNS = nil
	s.CheckAlive = nil
	got := RenderConfig(s, testRegistration())

	if strings.Contains(got, "DNS =") {
		t.Errorf("empty DNS should be omitted entirely:\n%s", got)
	}
	// CheckAliveInterval is only valid alongside CheckAlive; wireproxy errors
	// out if it appears on its own.
	if strings.Contains(got, "CheckAlive") {
		t.Errorf("empty CheckAlive should omit both keys:\n%s", got)
	}
}

// A config we generated must be recognised as ours on the next boot, so that
// it gets regenerated and environment changes take effect. A config supplied
// by the operator must never be claimed.
func TestIsGeneratedConfig(t *testing.T) {
	dir := t.TempDir()

	ours := filepath.Join(dir, "generated.conf")
	if err := WriteConfig(ours, RenderConfig(baseSettings(), testRegistration())); err != nil {
		t.Fatal(err)
	}
	if !IsGeneratedConfig(ours) {
		t.Error("a config written by RenderConfig should be recognised as generated")
	}

	theirs := filepath.Join(dir, "operator.conf")
	if err := os.WriteFile(theirs, []byte("[Interface]\nPrivateKey = abc\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if IsGeneratedConfig(theirs) {
		t.Error("an operator-supplied config must never be treated as generated")
	}

	// A file shorter than the marker must not panic or false-positive.
	short := filepath.Join(dir, "short.conf")
	if err := os.WriteFile(short, []byte("#\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if IsGeneratedConfig(short) {
		t.Error("a truncated file must not be treated as generated")
	}

	if IsGeneratedConfig(filepath.Join(dir, "does-not-exist.conf")) {
		t.Error("a missing file must not be treated as generated")
	}
}
