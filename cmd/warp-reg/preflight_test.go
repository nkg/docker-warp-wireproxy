package main

import (
	"net"
	"strings"
	"testing"
)

func freePort(t *testing.T) (addr string, release func()) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	return ln.Addr().String(), func() { ln.Close() }
}

func TestPreflightPortsPassesWhenFree(t *testing.T) {
	addr, release := freePort(t)
	release() // now free, but a port we know nothing else grabbed

	s := &Settings{Socks5: Listener{Bind: addr}}
	if err := PreflightPorts(s); err != nil {
		t.Errorf("expected a free port to pass: %v", err)
	}
}

// The case that motivated this: instances sharing a network namespace share
// one port space, so a second instance collides on INFO_BIND even when its
// proxy ports differ. Previously that surfaced as a bare panic from inside
// wireproxy, after registration had already happened.
func TestPreflightPortsDetectsOccupiedInfoBind(t *testing.T) {
	addr, release := freePort(t)
	defer release() // keep it held, simulating the first instance

	socks, releaseSocks := freePort(t)
	releaseSocks()

	s := &Settings{
		Socks5:   Listener{Bind: socks},
		InfoBind: addr,
	}
	err := PreflightPorts(s)
	if err == nil {
		t.Fatal("expected an error for an occupied INFO_BIND")
	}

	msg := err.Error()
	for _, want := range []string{"INFO_BIND", addr, "network_mode", "port space"} {
		if !strings.Contains(msg, want) {
			t.Errorf("error should mention %q, got:\n%s", want, msg)
		}
	}
}

// Two of our own listeners on one address can never work; say so directly
// rather than reporting the second as "already in use" by the first.
func TestPreflightPortsRejectsSelfCollision(t *testing.T) {
	addr, release := freePort(t)
	release()

	s := &Settings{
		Socks5: Listener{Bind: addr},
		HTTP:   Listener{Bind: addr},
	}
	err := PreflightPorts(s)
	if err == nil {
		t.Fatal("expected an error when two listeners share an address")
	}
	if !strings.Contains(err.Error(), "every listener needs its own port") {
		t.Errorf("expected a self-collision message, got: %v", err)
	}
}

func TestPreflightPortsIgnoresDisabledListeners(t *testing.T) {
	socks, release := freePort(t)
	release()

	// Empty binds are disabled listeners and must not be probed.
	s := &Settings{
		Socks5:      Listener{Bind: socks},
		HTTP:        Listener{},
		InfoBind:    "",
		MetricsBind: "",
	}
	if err := PreflightPorts(s); err != nil {
		t.Errorf("disabled listeners should be skipped: %v", err)
	}
}

func TestListenersNamesEveryBinding(t *testing.T) {
	s := &Settings{
		Socks5:      Listener{Bind: "0.0.0.0:1080"},
		HTTP:        Listener{Bind: "0.0.0.0:8080"},
		InfoBind:    "127.0.0.1:9080",
		MetricsBind: "0.0.0.0:9095",
	}
	got := listeners(s)
	if len(got) != 4 {
		t.Fatalf("got %d listeners, want 4: %+v", len(got), got)
	}
	// Every entry must name the variable an operator would change.
	for _, l := range got {
		if l.Env == "" {
			t.Errorf("listener %s has no env var attributed", l.Addr)
		}
	}
}
