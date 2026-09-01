package main

import (
	"context"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"
)

// wireproxy's --info endpoint speaks two dialects:
//
//	/metrics  the WireGuard UAPI "get" dialog -- flat key=value lines, hex
//	          keys, decimal counters, with each `public_key=` line starting a
//	          new peer block. Not Prometheus exposition format.
//	/readyz   JSON {"<CheckAlive target>": <unix seconds of last pong>},
//	          served with 503 when any target is stale.
//
// This file turns both into real Prometheus metrics. It lives in the same
// binary as the entrypoint, so exporting costs the image nothing.

// Peer is one WireGuard peer parsed out of the UAPI dialog.
type Peer struct {
	PublicKey     string
	Endpoint      string
	LastHandshake float64
	TxBytes       float64
	RxBytes       float64
	Keepalive     float64
	AllowedIPs    []string
}

// DeviceStats is the parsed form of the whole /metrics response.
type DeviceStats struct {
	ListenPort float64
	Errno      float64
	Peers      []Peer
}

// parseUAPI parses the WireGuard UAPI get dialog. Unknown keys are ignored so
// that a future wireguard-go adding fields does not break the exporter.
func parseUAPI(body string) DeviceStats {
	var d DeviceStats
	var cur *Peer

	flush := func() {
		if cur != nil {
			d.Peers = append(d.Peers, *cur)
			cur = nil
		}
	}

	for _, line := range strings.Split(body, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		key, value, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}

		switch key {
		case "public_key":
			// A new public_key line ends the previous peer block.
			flush()
			cur = &Peer{PublicKey: hexToBase64(value)}
		case "listen_port":
			d.ListenPort = toFloat(value)
		case "errno":
			d.Errno = toFloat(value)
		case "endpoint":
			if cur != nil {
				cur.Endpoint = value
			}
		case "last_handshake_time_sec":
			if cur != nil {
				cur.LastHandshake = toFloat(value)
			}
		case "tx_bytes":
			if cur != nil {
				cur.TxBytes = toFloat(value)
			}
		case "rx_bytes":
			if cur != nil {
				cur.RxBytes = toFloat(value)
			}
		case "persistent_keepalive_interval":
			if cur != nil {
				cur.Keepalive = toFloat(value)
			}
		case "allowed_ip":
			if cur != nil {
				cur.AllowedIPs = append(cur.AllowedIPs, value)
			}
		}
	}
	flush()
	return d
}

// hexToBase64 converts a UAPI hex key into the base64 form that `wg show` and
// every WireGuard config file use, so operators can match a peer by eye.
func hexToBase64(h string) string {
	raw, err := hex.DecodeString(h)
	if err != nil {
		return h
	}
	return base64.StdEncoding.EncodeToString(raw)
}

func toFloat(s string) float64 {
	f, err := strconv.ParseFloat(strings.TrimSpace(s), 64)
	if err != nil {
		return 0
	}
	return f
}

// Exporter scrapes a wireproxy --info endpoint on demand.
type Exporter struct {
	InfoAddr string
	Client   *http.Client
	Version  string
}

// NewExporter builds an exporter against the given wireproxy info address.
func NewExporter(infoAddr, version string) *Exporter {
	return &Exporter{
		InfoAddr: infoAddr,
		Client:   &http.Client{Timeout: 5 * time.Second},
		Version:  version,
	}
}

func (e *Exporter) get(ctx context.Context, path string) (string, int, error) {
	url := "http://" + e.InfoAddr + path
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return "", 0, err
	}
	resp, err := e.Client.Do(req)
	if err != nil {
		return "", 0, err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return "", resp.StatusCode, err
	}
	return string(raw), resp.StatusCode, nil
}

// Gather renders the full Prometheus exposition response.
func (e *Exporter) Gather(ctx context.Context) string {
	start := time.Now()
	var b strings.Builder

	metricsBody, _, metricsErr := e.get(ctx, "/metrics")
	readyBody, readyStatus, readyErr := e.get(ctx, "/readyz")

	up := 0
	if metricsErr == nil {
		up = 1
	}

	writeMetric(&b, "warp_wireproxy_up",
		"Whether wireproxy's info endpoint was scraped successfully.",
		"gauge", nil, float64(up))

	writeMetric(&b, "warp_wireproxy_build_info",
		"Build information for this warp-reg exporter.",
		"gauge", map[string]string{"version": e.Version}, 1)

	if metricsErr != nil {
		// Still emit `up 0` and the scrape duration so an alert can fire on
		// the exporter being reachable while wireproxy is not.
		writeMetric(&b, "warp_wireproxy_scrape_duration_seconds",
			"Time spent scraping wireproxy's info endpoint.",
			"gauge", nil, time.Since(start).Seconds())
		return b.String()
	}

	d := parseUAPI(metricsBody)

	writeMetric(&b, "warp_wireproxy_device_listen_port",
		"UDP source port the userspace WireGuard device is bound to.",
		"gauge", nil, d.ListenPort)

	writeMetric(&b, "warp_wireproxy_device_errno",
		"errno reported by the userspace WireGuard device; 0 means healthy.",
		"gauge", nil, d.Errno)

	writeMetric(&b, "warp_wireproxy_peers",
		"Number of configured WireGuard peers.",
		"gauge", nil, float64(len(d.Peers)))

	if len(d.Peers) > 0 {
		writeHeader(&b, "warp_wireproxy_peer_last_handshake_timestamp_seconds",
			"Unix time of the most recent completed handshake with the peer; 0 means never.",
			"gauge")
		for _, p := range d.Peers {
			writeSample(&b, "warp_wireproxy_peer_last_handshake_timestamp_seconds", peerLabels(p), p.LastHandshake)
		}

		writeHeader(&b, "warp_wireproxy_peer_transmit_bytes_total",
			"Total bytes sent to the peer through the tunnel.", "counter")
		for _, p := range d.Peers {
			writeSample(&b, "warp_wireproxy_peer_transmit_bytes_total", peerLabels(p), p.TxBytes)
		}

		writeHeader(&b, "warp_wireproxy_peer_receive_bytes_total",
			"Total bytes received from the peer through the tunnel.", "counter")
		for _, p := range d.Peers {
			writeSample(&b, "warp_wireproxy_peer_receive_bytes_total", peerLabels(p), p.RxBytes)
		}

		writeHeader(&b, "warp_wireproxy_peer_persistent_keepalive_seconds",
			"Configured persistent keepalive interval for the peer; 0 means disabled.", "gauge")
		for _, p := range d.Peers {
			writeSample(&b, "warp_wireproxy_peer_persistent_keepalive_seconds", peerLabels(p), p.Keepalive)
		}

		// Seconds-since-handshake is what you actually alert on, and computing
		// it here avoids every dashboard having to special-case the never-yet
		// -handshaked value of 0.
		now := float64(time.Now().Unix())
		writeHeader(&b, "warp_wireproxy_peer_handshake_age_seconds",
			"Seconds since the last completed handshake; -1 when no handshake has happened yet.", "gauge")
		for _, p := range d.Peers {
			age := -1.0
			if p.LastHandshake > 0 {
				age = now - p.LastHandshake
			}
			writeSample(&b, "warp_wireproxy_peer_handshake_age_seconds", peerLabels(p), age)
		}
	}

	// /readyz drives the readiness gauge and the per-target pong timestamps.
	ready := 0
	if readyErr == nil && readyStatus == http.StatusOK {
		ready = 1
	}
	writeMetric(&b, "warp_wireproxy_ready",
		"Whether wireproxy /readyz reports every CheckAlive target as responding.",
		"gauge", nil, float64(ready))

	if readyErr == nil {
		var pongs map[string]int64
		if err := json.Unmarshal([]byte(readyBody), &pongs); err == nil && len(pongs) > 0 {
			targets := make([]string, 0, len(pongs))
			for t := range pongs {
				targets = append(targets, t)
			}
			// Sorted so the exposition output is stable between scrapes.
			sort.Strings(targets)

			writeHeader(&b, "warp_wireproxy_check_alive_last_pong_timestamp_seconds",
				"Unix time of the last ICMP echo reply received from a CheckAlive target through the tunnel.",
				"gauge")
			for _, t := range targets {
				writeSample(&b, "warp_wireproxy_check_alive_last_pong_timestamp_seconds",
					map[string]string{"target": t}, float64(pongs[t]))
			}
		}
	}

	writeMetric(&b, "warp_wireproxy_scrape_duration_seconds",
		"Time spent scraping wireproxy's info endpoint.",
		"gauge", nil, time.Since(start).Seconds())

	return b.String()
}

func peerLabels(p Peer) map[string]string {
	return map[string]string{
		"public_key": p.PublicKey,
		"endpoint":   p.Endpoint,
	}
}

func writeHeader(b *strings.Builder, name, help, typ string) {
	fmt.Fprintf(b, "# HELP %s %s\n# TYPE %s %s\n", name, help, name, typ)
}

func writeMetric(b *strings.Builder, name, help, typ string, labels map[string]string, value float64) {
	writeHeader(b, name, help, typ)
	writeSample(b, name, labels, value)
}

func writeSample(b *strings.Builder, name string, labels map[string]string, value float64) {
	b.WriteString(name)
	if len(labels) > 0 {
		keys := make([]string, 0, len(labels))
		for k := range labels {
			keys = append(keys, k)
		}
		sort.Strings(keys)

		b.WriteString("{")
		for i, k := range keys {
			if i > 0 {
				b.WriteString(",")
			}
			fmt.Fprintf(b, "%s=%q", k, escapeLabel(labels[k]))
		}
		b.WriteString("}")
	}
	fmt.Fprintf(b, " %s\n", formatFloat(value))
}

// escapeLabel applies Prometheus label-value escaping. %q would also escape
// non-ASCII into \u sequences, which the exposition format does not accept.
func escapeLabel(v string) string {
	r := strings.NewReplacer(`\`, `\\`, `"`, `\"`, "\n", `\n`)
	return r.Replace(v)
}

func formatFloat(f float64) string {
	if f == float64(int64(f)) {
		return strconv.FormatInt(int64(f), 10)
	}
	return strconv.FormatFloat(f, 'g', -1, 64)
}

// ServeMetrics runs the exporter HTTP server until the context is cancelled.
func ServeMetrics(ctx context.Context, addr string, e *Exporter) error {
	mux := http.NewServeMux()

	mux.HandleFunc("/metrics", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
		io.WriteString(w, e.Gather(r.Context()))
	})

	// A plain liveness probe for orchestrators that want one separate from the
	// metrics scrape.
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		_, status, err := e.get(r.Context(), "/readyz")
		if err != nil || status != http.StatusOK {
			w.WriteHeader(http.StatusServiceUnavailable)
			io.WriteString(w, "unhealthy\n")
			return
		}
		io.WriteString(w, "ok\n")
	})

	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		io.WriteString(w, `<html><head><title>warp-wireproxy</title></head><body>
<h1>warp-wireproxy</h1>
<ul><li><a href="/metrics">/metrics</a></li><li><a href="/healthz">/healthz</a></li></ul>
</body></html>`)
	})

	srv := &http.Server{
		Addr:              addr,
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
	}

	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdownCtx)
	}()

	logf("prometheus exporter listening on %s (/metrics, /healthz)", addr)
	if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		return err
	}
	return nil
}
