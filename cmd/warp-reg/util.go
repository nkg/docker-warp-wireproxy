package main

import "net"

// loopbackize rewrites a wildcard bind address into one that can actually be
// dialled from inside the container. A service listening on 0.0.0.0:9080 is
// reachable at 127.0.0.1:9080, but connecting to 0.0.0.0 itself is not
// portable, and "::" needs the v6 loopback rather than the v4 one.
func loopbackize(addr string) string {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return addr
	}
	switch host {
	case "", "0.0.0.0":
		host = "127.0.0.1"
	case "::", "[::]":
		host = "::1"
	}
	return net.JoinHostPort(host, port)
}
