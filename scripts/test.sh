#!/usr/bin/env bash
#
# Local build-and-verify for warp-wireproxy. Builds the image for the host
# architecture, starts it, and checks that traffic really exits through WARP.
#
# Works with Docker, Apple's `container` (macOS 26+), and Podman. Override the
# choice with RUNTIME=<docker|container|podman>.
#
# Usage: scripts/test.sh

set -euo pipefail
cd "$(dirname "$0")/.."

IMAGE=warp-wireproxy:test
NAME=warp-wireproxy-test
SOCKS_PORT=${SOCKS_PORT:-11080}
HTTP_PORT=${HTTP_PORT:-18080}
PROM_PORT=${PROM_PORT:-19095}

pass=0
fail=0

ok()   { printf '  \033[32mPASS\033[0m %s\n' "$1"; pass=$((pass + 1)); }
bad()  { printf '  \033[31mFAIL\033[0m %s\n' "$1"; fail=$((fail + 1)); }
step() { printf '\n\033[1m==> %s\033[0m\n' "$1"; }

# ---------------------------------------------------------------------------
# Runtime detection
# ---------------------------------------------------------------------------
detect_runtime() {
    if [ -n "${RUNTIME:-}" ]; then echo "$RUNTIME"; return; fi
    # Prefer a Docker daemon that is actually up; the CLI existing is not
    # enough, and a broken Docker Desktop leaves the binary behind.
    if command -v docker >/dev/null 2>&1 && docker version >/dev/null 2>&1; then
        echo docker; return
    fi
    if command -v container >/dev/null 2>&1; then echo container; return; fi
    if command -v podman >/dev/null 2>&1; then echo podman; return; fi
    echo "no container runtime found (tried docker, container, podman)" >&2
    exit 1
}

RT=$(detect_runtime)

cleanup() {
    $RT rm -f "$NAME" >/dev/null 2>&1 || true
}
trap cleanup EXIT

printf '\033[1mwarp-wireproxy local test\033[0m\n'
printf '  runtime: %s\n' "$RT"

# ---------------------------------------------------------------------------
step "Go tests"
if go vet ./... && go test ./...; then ok "go vet + go test"; else bad "go vet + go test"; fi

if [ -n "$(gofmt -l ./cmd)" ]; then
    bad "gofmt (run: gofmt -w ./cmd)"
else
    ok "gofmt"
fi

# .wireproxy-version is the source of truth for which wireproxy gets built.
WIREPROXY_REF=$(tr -d '[:space:]' < .wireproxy-version)
pin_ok=true
while read -r line; do
    [ "${line#*=}" = "$WIREPROXY_REF" ] || pin_ok=false
done < <(grep -o 'ARG WIREPROXY_REF=.*' Dockerfile)
if $pin_ok; then ok "version pins agree ($WIREPROXY_REF)"; else bad "Dockerfile pin != .wireproxy-version"; fi

# ---------------------------------------------------------------------------
step "Building image"
printf '  wireproxy: %s\n' "$WIREPROXY_REF"
build_args=(--build-arg VERSION=test --build-arg "WIREPROXY_REF=$WIREPROXY_REF")
if [ "$RT" = "container" ]; then
    # Apple's builder runs in its own VM. If this fails to reach the network,
    # recreate it with a working resolver:
    #   container builder delete
    #   container builder start --cpus 4 --memory 8G --dns 192.168.64.1
    $RT build --platform "linux/$(uname -m | sed 's/x86_64/amd64/;s/aarch64/arm64/')" \
        "${build_args[@]}" -t "$IMAGE" .
else
    $RT build "${build_args[@]}" -t "$IMAGE" .
fi
ok "image built"

# ---------------------------------------------------------------------------
step "Starting container"
cleanup
$RT run -d --name "$NAME" \
    -p "${SOCKS_PORT}:1080" \
    -p "${HTTP_PORT}:8080" \
    -p "${PROM_PORT}:9095" \
    -e METRICS_BIND=0.0.0.0:9095 \
    "$IMAGE" >/dev/null
ok "container started"

# ---------------------------------------------------------------------------
step "Waiting for the tunnel"
# `warp-reg health` is exactly what the image's HEALTHCHECK runs, and unlike
# `inspect .State.Health` it works on runtimes with no healthcheck support --
# Apple's `container` has none.
healthy=false
for i in $(seq 1 30); do
    if $RT exec "$NAME" /usr/bin/warp-reg health >/dev/null 2>&1; then
        healthy=true; printf '  attempt %2d: healthy\n' "$i"; break
    fi
    printf '  attempt %2d: not ready\n' "$i"
    sleep 5
done
if $healthy; then ok "warp-reg health reports the tunnel up"; else bad "tunnel never came up"; fi

# ---------------------------------------------------------------------------
step "Proxy routing"
for spec in "socks5h://127.0.0.1:${SOCKS_PORT} SOCKS5" "http://127.0.0.1:${HTTP_PORT} HTTP"; do
    set -- $spec
    trace=$(curl -fsS --max-time 30 -x "$1" https://www.cloudflare.com/cdn-cgi/trace 2>/dev/null || true)
    if grep -qE '^warp=(on|plus)$' <<<"$trace"; then
        ok "$2 exits through WARP ($(grep '^ip=' <<<"$trace"))"
    else
        bad "$2 did not report warp=on"
    fi
done

# The whole point is that the proxy's exit IP is not yours.
real_ip=$(curl -fsS --max-time 15 https://api.ipify.org 2>/dev/null || echo "")
warp_ip=$(curl -fsS --max-time 30 -x "socks5h://127.0.0.1:${SOCKS_PORT}" https://api.ipify.org 2>/dev/null || echo "")
if [ -n "$real_ip" ] && [ -n "$warp_ip" ] && [ "$real_ip" != "$warp_ip" ]; then
    ok "egress IP is masked ($real_ip -> $warp_ip)"
elif [ -z "$real_ip" ] || [ -z "$warp_ip" ]; then
    bad "could not compare egress IPs"
else
    bad "egress IP unchanged ($real_ip)"
fi

# ---------------------------------------------------------------------------
step "Prometheus exporter"
metrics=$(curl -fsS --max-time 10 "http://127.0.0.1:${PROM_PORT}/metrics" 2>/dev/null || true)
for probe in "warp_wireproxy_up 1" "warp_wireproxy_ready 1" "warp_wireproxy_peer_receive_bytes_total{"; do
    if grep -qF "$probe" <<<"$metrics"; then ok "exposes $probe"; else bad "missing $probe"; fi
done

# ---------------------------------------------------------------------------
step "Restart behaviour"
before=$($RT exec "$NAME" /bin/sh -c 'md5sum /var/lib/warp-wireproxy/registration.json' 2>/dev/null | awk '{print $1}')
$RT stop "$NAME" >/dev/null 2>&1 || true
$RT start "$NAME" >/dev/null 2>&1 || true
sleep 15
after=$($RT exec "$NAME" /bin/sh -c 'md5sum /var/lib/warp-wireproxy/registration.json' 2>/dev/null | awk '{print $1}')

if [ -n "$before" ] && [ "$before" = "$after" ]; then
    ok "registration reused across restart (no new WARP device)"
else
    bad "registration was not reused"
fi

logs=$($RT logs "$NAME" 2>&1 || true)
if grep -qa "reusing cached WARP registration" <<<"$logs"; then
    ok "logged the cache hit"
else
    bad "expected a cache-reuse log line"
fi
# A config we generated must be regenerated, or environment changes made
# between restarts would silently never apply.
if grep -qa "regenerating" <<<"$logs"; then
    ok "regenerated its own config (env changes take effect)"
else
    bad "did not regenerate its own config"
fi

# ---------------------------------------------------------------------------
step "Image contents"
if $RT exec "$NAME" /bin/sh -c 'id -u' >/dev/null 2>&1; then
    uid=$($RT exec "$NAME" /bin/sh -c 'id -u' 2>/dev/null | tr -d '[:space:]')
    if [ "$uid" = "65532" ]; then ok "runs as non-root (uid $uid)"; else bad "unexpected uid: $uid"; fi

    applets=$($RT exec "$NAME" /bin/sh -c 'ls /bin | wc -l' 2>/dev/null | tr -d '[:space:]')
    if [ "${applets:-0}" -gt 50 ]; then
        ok "busybox applets installed ($applets)"
    else
        bad "shell has almost no applets ($applets) -- symlinks missing"
    fi

    got=$($RT exec "$NAME" /usr/bin/wireproxy --version 2>&1 | head -1)
    if grep -qF "$WIREPROXY_REF" <<<"$got"; then
        ok "image contains $got"
    else
        bad "image contains '$got', wanted $WIREPROXY_REF"
    fi

    printf '  size breakdown:\n'
    $RT exec "$NAME" /bin/sh -c \
        'du -sh /usr/bin/wireproxy /usr/bin/warp-reg /bin/busybox /etc/ssl/certs 2>/dev/null; du -sh / 2>/dev/null' \
        | sed 's/^/    /'
else
    printf '  (no shell in image; built with WITH_SHELL=false)\n'
fi

# ---------------------------------------------------------------------------
printf '\n\033[1mSummary:\033[0m %d passed, %d failed\n' "$pass" "$fail"
if [ "$fail" -gt 0 ]; then
    printf '\nContainer logs:\n'
    $RT logs "$NAME" 2>&1 | tail -40
    exit 1
fi
