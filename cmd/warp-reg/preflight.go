package main

import (
	"fmt"
	"net"
	"strings"
)

// listenSpec is one address warp-reg is about to hand to wireproxy, paired
// with the environment variable an operator would change to move it.
type listenSpec struct {
	Env  string
	Addr string
}

// listeners returns every TCP address this configuration will bind.
func listeners(s *Settings) []listenSpec {
	var out []listenSpec
	add := func(env, addr string) {
		if addr != "" {
			out = append(out, listenSpec{Env: env, Addr: addr})
		}
	}
	add("SOCKS5_BIND / SOCKS5_PORT", s.Socks5.Bind)
	add("HTTP_BIND / HTTP_PORT", s.HTTP.Bind)
	add("INFO_BIND", s.InfoBind)
	add("METRICS_BIND", s.MetricsBind)
	return out
}

// PreflightPorts checks that every address this instance will listen on is
// actually free, and fails with an explanation naming the variable to change.
//
// Without this, a clash surfaces as a bare Go panic from inside wireproxy --
// "panic: listen tcp 127.0.0.1:9080: bind: address already in use" plus a
// stack trace -- after registration has already happened, with nothing to say
// which setting caused it or why.
//
// The case that makes this worth doing is a shared network namespace
// (`network_mode: "service:..."` or `"container:..."`): every container
// attached to another container's network shares one port space, so a second
// instance collides on the DEFAULTS even when its proxy ports have been
// changed, because INFO_BIND is still 127.0.0.1:9080.
//
// This is advisory. There is an unavoidable race between releasing the probe
// socket and wireproxy binding it, so a clash appearing in that window still
// surfaces the old way; the check exists to catch the overwhelmingly common
// case where the port was already taken before we started.
func PreflightPorts(s *Settings) error {
	seen := make(map[string]string, 4)

	for _, l := range listeners(s) {
		// Two of our own listeners on one address can never work, and would
		// otherwise be reported as the confusing "already in use" below.
		if prev, dup := seen[l.Addr]; dup {
			return fmt.Errorf("%s and %s are both set to %s; every listener needs its own port",
				prev, l.Env, l.Addr)
		}
		seen[l.Addr] = l.Env

		ln, err := net.Listen("tcp", l.Addr)
		if err != nil {
			return portInUseError(l, err)
		}
		if err := ln.Close(); err != nil {
			return fmt.Errorf("releasing probe socket on %s: %w", l.Addr, err)
		}
	}
	return nil
}

func portInUseError(l listenSpec, cause error) error {
	var b strings.Builder
	fmt.Fprintf(&b, "cannot bind %s (%s): %v\n", l.Addr, l.Env, cause)
	b.WriteString("\n")
	b.WriteString("Something else in this network namespace is already listening there.\n")
	b.WriteString("\n")
	b.WriteString("If you are running several instances that share a network namespace\n")
	b.WriteString("(network_mode: \"service:...\" or \"container:...\"), they all share a\n")
	b.WriteString("single port space, so each instance needs its own value for every\n")
	b.WriteString("listener -- SOCKS5_PORT, HTTP_PORT, INFO_BIND and METRICS_BIND -- not just\n")
	b.WriteString("the proxy ports. INFO_BIND defaults to 127.0.0.1:9080 and is the one most\n")
	b.WriteString("often missed.\n")
	b.WriteString("\n")
	b.WriteString("Set INFO_BIND=\"\" to turn the health endpoint off entirely, at the cost of\n")
	b.WriteString("the container healthcheck and the Prometheus exporter.")
	return fmt.Errorf("%s", b.String())
}
