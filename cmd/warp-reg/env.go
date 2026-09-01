package main

import (
	"fmt"
	"net"
	"os"
	"strconv"
	"strings"
	"time"
)

// Settings is the fully resolved runtime configuration, assembled from the
// environment. Every field has a working default so that `docker run <image>`
// with no options at all produces a usable WARP proxy.
type Settings struct {
	// WARP registration
	API        string
	Endpoint   string
	Referrer   string
	MTU        int
	Keepalive  int
	StateDir   string
	Reregister bool
	Timeout    time.Duration
	Retries    int

	// Tunnel
	DNS                []string
	CheckAlive         []string
	CheckAliveInterval int

	// Proxies
	Socks5      Listener
	HTTP        Listener
	InfoBind    string
	MetricsBind string

	// Config file plumbing
	ConfPath    string
	WGConfig    string
	ExtraConfig string
	Silent      bool
}

// Listener is one proxy front-end (SOCKS5 or HTTP). An empty Bind disables it.
type Listener struct {
	Bind     string
	Username string
	Password string
}

// Enabled reports whether this proxy front-end should be written to the config.
func (l Listener) Enabled() bool { return l.Bind != "" }

const (
	defaultAPI      = "https://api.cloudflareclient.com/v0a2025/reg"
	defaultEndpoint = "engage.cloudflareclient.com:2408"
	defaultStateDir = "/var/lib/warp-wireproxy"
	defaultConf     = "/etc/wireproxy/wireproxy.conf"
	defaultInfo     = "127.0.0.1:9080"

	// 1280 is the IPv6 minimum MTU. WARP's own clients negotiate lower than the
	// WireGuard default of 1420, and 1280 is the value that survives every path
	// we have seen without PMTU blackholing.
	defaultMTU = 1280
)

// defaultDNS is Cloudflare's own resolver, dual-stack. Sent to the userspace
// netstack, so it never touches the host resolver.
var defaultDNS = []string{"1.1.1.1", "1.0.0.1", "2606:4700:4700::1111", "2606:4700:4700::1001"}

// defaultCheckAlive is pinged through the tunnel to drive /readyz. One v4 and
// one v6 target so a half-broken tunnel still shows up as unhealthy.
var defaultCheckAlive = []string{"1.1.1.1", "2606:4700:4700::1111"}

// LoadSettings resolves configuration from the environment, validating as it
// goes. It returns an error rather than falling back to a silently-wrong value.
func LoadSettings() (*Settings, error) {
	s := &Settings{
		API:                env("WARP_API", defaultAPI),
		Endpoint:           env("WARP_ENDPOINT", defaultEndpoint),
		Referrer:           os.Getenv("WARP_REFERRER"),
		StateDir:           env("WARP_STATE_DIR", defaultStateDir),
		Reregister:         envBool("WARP_REREGISTER", false),
		ConfPath:           env("WIREPROXY_CONF", defaultConf),
		WGConfig:           os.Getenv("WG_CONFIG"),
		ExtraConfig:        os.Getenv("EXTRA_CONFIG"),
		Silent:             envBool("WIREPROXY_SILENT", false),
		DNS:                envList("DNS_SERVERS", defaultDNS),
		CheckAlive:         envList("CHECK_ALIVE", defaultCheckAlive),
		CheckAliveInterval: 0, // set below
		InfoBind:           envAllowEmpty("INFO_BIND", defaultInfo),
		MetricsBind:        os.Getenv("METRICS_BIND"),
	}

	var err error
	if s.MTU, err = envInt("WARP_MTU", defaultMTU); err != nil {
		return nil, err
	}
	if s.MTU < 576 || s.MTU > 1500 {
		return nil, fmt.Errorf("WARP_MTU must be between 576 and 1500, got %d", s.MTU)
	}
	if s.Keepalive, err = envInt("WARP_KEEPALIVE", 25); err != nil {
		return nil, err
	}
	if s.CheckAliveInterval, err = envInt("CHECK_ALIVE_INTERVAL", 5); err != nil {
		return nil, err
	}
	if s.CheckAliveInterval < 1 {
		return nil, fmt.Errorf("CHECK_ALIVE_INTERVAL must be >= 1, got %d", s.CheckAliveInterval)
	}
	if s.Retries, err = envInt("WARP_REGISTER_RETRIES", 5); err != nil {
		return nil, err
	}
	secs, err := envInt("WARP_REGISTER_TIMEOUT", 30)
	if err != nil {
		return nil, err
	}
	s.Timeout = time.Duration(secs) * time.Second

	if s.Socks5, err = listener("SOCKS5", "0.0.0.0:1080"); err != nil {
		return nil, err
	}
	if s.HTTP, err = listener("HTTP", "0.0.0.0:8080"); err != nil {
		return nil, err
	}
	if !s.Socks5.Enabled() && !s.HTTP.Enabled() && s.ExtraConfig == "" {
		return nil, fmt.Errorf("both SOCKS5_BIND and HTTP_BIND are empty and EXTRA_CONFIG is unset: " +
			"the container would listen on nothing")
	}

	for _, spec := range []struct{ name, addr string }{
		{"SOCKS5_BIND", s.Socks5.Bind},
		{"HTTP_BIND", s.HTTP.Bind},
		{"INFO_BIND", s.InfoBind},
		{"METRICS_BIND", s.MetricsBind},
	} {
		if spec.addr == "" {
			continue
		}
		if _, _, err := net.SplitHostPort(spec.addr); err != nil {
			return nil, fmt.Errorf("%s is not a host:port address: %q", spec.name, spec.addr)
		}
	}

	// The Prometheus exporter scrapes wireproxy's own info endpoint, so that
	// endpoint has to exist for the exporter to have anything to report.
	if s.MetricsBind != "" && s.InfoBind == "" {
		return nil, fmt.Errorf("METRICS_BIND is set but INFO_BIND is empty: " +
			"the exporter has no wireproxy endpoint to scrape")
	}

	return s, nil
}

// listener builds a Listener from the <prefix>_BIND / <prefix>_PORT /
// <prefix>_USER / <prefix>_PASSWD family. _BIND wins over _PORT; setting
// either to the empty string explicitly disables the front-end.
func listener(prefix, fallback string) (Listener, error) {
	l := Listener{
		Username: os.Getenv(prefix + "_USER"),
		Password: os.Getenv(prefix + "_PASSWD"),
	}

	bind, bindSet := os.LookupEnv(prefix + "_BIND")
	port, portSet := os.LookupEnv(prefix + "_PORT")

	switch {
	case bindSet:
		l.Bind = strings.TrimSpace(bind)
	case portSet:
		p := strings.TrimSpace(port)
		if p == "" {
			l.Bind = ""
			break
		}
		n, err := strconv.Atoi(p)
		if err != nil || n < 1 || n > 65535 {
			return l, fmt.Errorf("%s_PORT must be a port number 1-65535, got %q", prefix, port)
		}
		l.Bind = net.JoinHostPort(env(prefix+"_HOST", "0.0.0.0"), p)
	default:
		l.Bind = fallback
	}

	// wireproxy treats Username/Password as a pair; half a pair is a
	// configuration mistake that would silently leave the proxy open.
	if (l.Username == "") != (l.Password == "") {
		return l, fmt.Errorf("%s_USER and %s_PASSWD must be set together (got user=%q, password set=%t)",
			prefix, prefix, l.Username, l.Password != "")
	}
	// Space-separated values break wireproxy's ini parsing of the credential.
	if strings.ContainsAny(l.Username, " \t") || strings.ContainsAny(l.Password, " \t") {
		return l, fmt.Errorf("%s_USER / %s_PASSWD must not contain spaces or tabs", prefix, prefix)
	}

	return l, nil
}

func env(key, fallback string) string {
	if v, ok := os.LookupEnv(key); ok && strings.TrimSpace(v) != "" {
		return strings.TrimSpace(v)
	}
	return fallback
}

// envAllowEmpty is env() for variables where the empty string is a meaningful
// value rather than "unset". Setting INFO_BIND="" disables the endpoint; env()
// would silently restore the default instead.
func envAllowEmpty(key, fallback string) string {
	if v, ok := os.LookupEnv(key); ok {
		return strings.TrimSpace(v)
	}
	return fallback
}

func envInt(key string, fallback int) (int, error) {
	v, ok := os.LookupEnv(key)
	if !ok || strings.TrimSpace(v) == "" {
		return fallback, nil
	}
	n, err := strconv.Atoi(strings.TrimSpace(v))
	if err != nil {
		return 0, fmt.Errorf("%s must be an integer, got %q", key, v)
	}
	return n, nil
}

func envBool(key string, fallback bool) bool {
	v, ok := os.LookupEnv(key)
	if !ok {
		return fallback
	}
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "1", "true", "yes", "on":
		return true
	case "0", "false", "no", "off", "":
		return false
	}
	return fallback
}

// envList splits a comma-separated environment variable. An explicitly empty
// value yields an empty list, which callers use to omit the key entirely.
func envList(key string, fallback []string) []string {
	v, ok := os.LookupEnv(key)
	if !ok {
		return fallback
	}
	var out []string
	for _, part := range strings.Split(v, ",") {
		if p := strings.TrimSpace(part); p != "" {
			out = append(out, p)
		}
	}
	return out
}
