package main

import (
	"strings"
	"testing"
)

// Captured verbatim from a live wireproxy --info /metrics response.
const sampleUAPI = `private_key=REDACTED
listen_port=61913
public_key=6e65ce0be175171
preshared_key=REDACTED
protocol_version=1
endpoint=162.159.192.1:2408
last_handshake_time_sec=1788284082
last_handshake_time_nsec=979171000
tx_bytes=5764
rx_bytes=11138
persistent_keepalive_interval=25
allowed_ip=0.0.0.0/0
allowed_ip=::/0
`

func TestParseUAPI(t *testing.T) {
	d := parseUAPI(sampleUAPI)

	if d.ListenPort != 61913 {
		t.Errorf("ListenPort = %v, want 61913", d.ListenPort)
	}
	if len(d.Peers) != 1 {
		t.Fatalf("got %d peers, want 1", len(d.Peers))
	}

	p := d.Peers[0]
	if p.Endpoint != "162.159.192.1:2408" {
		t.Errorf("Endpoint = %q", p.Endpoint)
	}
	if p.LastHandshake != 1788284082 {
		t.Errorf("LastHandshake = %v", p.LastHandshake)
	}
	if p.TxBytes != 5764 || p.RxBytes != 11138 {
		t.Errorf("tx/rx = %v/%v, want 5764/11138", p.TxBytes, p.RxBytes)
	}
	if p.Keepalive != 25 {
		t.Errorf("Keepalive = %v", p.Keepalive)
	}
	if len(p.AllowedIPs) != 2 {
		t.Errorf("AllowedIPs = %v, want both v4 and v6 routes", p.AllowedIPs)
	}
}

// Each public_key line starts a new peer block; fields must not bleed across.
func TestParseUAPISeparatesPeers(t *testing.T) {
	const twoPeers = `listen_port=51820
public_key=aa
tx_bytes=1
rx_bytes=2
public_key=bb
tx_bytes=30
rx_bytes=40
`
	d := parseUAPI(twoPeers)
	if len(d.Peers) != 2 {
		t.Fatalf("got %d peers, want 2", len(d.Peers))
	}
	if d.Peers[0].TxBytes != 1 || d.Peers[1].TxBytes != 30 {
		t.Errorf("peer fields bled across blocks: %v", d.Peers)
	}
}

// Device-level keys appear before any public_key and must not be attributed
// to a peer, and unknown keys must be ignored rather than panicking.
func TestParseUAPIIgnoresUnknownKeysAndJunk(t *testing.T) {
	const messy = `listen_port=1
some_future_key=whatever
a line with no equals sign

public_key=aa
errno=0
`
	d := parseUAPI(messy)
	if d.ListenPort != 1 {
		t.Errorf("ListenPort = %v, want 1", d.ListenPort)
	}
	if len(d.Peers) != 1 {
		t.Errorf("got %d peers, want 1", len(d.Peers))
	}
}

// UAPI reports keys as hex; operators read them as base64 everywhere else.
func TestHexToBase64(t *testing.T) {
	hex := strings.Repeat("00", 32)
	got := hexToBase64(hex)
	want := "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA="
	if got != want {
		t.Errorf("hexToBase64 = %q, want %q", got, want)
	}

	// Non-hex input is passed through rather than dropped.
	if got := hexToBase64("not-hex"); got != "not-hex" {
		t.Errorf("hexToBase64 mangled non-hex input: %q", got)
	}
}

func TestFormatFloat(t *testing.T) {
	cases := map[float64]string{
		0:          "0",
		25:         "25",
		-1:         "-1",
		1788284082: "1788284082",
	}
	for in, want := range cases {
		if got := formatFloat(in); got != want {
			t.Errorf("formatFloat(%v) = %q, want %q", in, got, want)
		}
	}
	// Fractional values must not be rendered in scientific notation that
	// Prometheus would reject.
	if got := formatFloat(0.0104); strings.ContainsAny(got, "eE") {
		t.Errorf("formatFloat(0.0104) = %q, unexpected exponent form", got)
	}
}

func TestEscapeLabel(t *testing.T) {
	got := escapeLabel(`a"b\c` + "\n")
	want := `a\"b\\c\n`
	if got != want {
		t.Errorf("escapeLabel = %q, want %q", got, want)
	}
}

// Label sets must be emitted in a stable (sorted) order so that scrapes are
// byte-identical when nothing changed.
func TestWriteSampleSortsLabels(t *testing.T) {
	var b strings.Builder
	writeSample(&b, "m", map[string]string{"z": "1", "a": "2"}, 3)
	if got := b.String(); got != `m{a="2",z="1"} 3`+"\n" {
		t.Errorf("writeSample = %q", got)
	}
}

func TestLoopbackize(t *testing.T) {
	cases := map[string]string{
		"0.0.0.0:9080":   "127.0.0.1:9080",
		"127.0.0.1:9080": "127.0.0.1:9080",
		"[::]:9080":      "[::1]:9080",
		"1.2.3.4:9080":   "1.2.3.4:9080",
		"garbage":        "garbage",
	}
	for in, want := range cases {
		if got := loopbackize(in); got != want {
			t.Errorf("loopbackize(%q) = %q, want %q", in, got, want)
		}
	}
}
