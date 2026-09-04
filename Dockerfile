# syntax=docker/dockerfile:1

# warp-wireproxy -- Cloudflare WARP as a SOCKS5/HTTP proxy, in ~17 MB.
#
# Every Go stage cross-compiles from $BUILDPLATFORM, so no QEMU emulation is
# involved: Go targets the requested architecture directly, which turns a
# ten-platform matrix from hours into minutes.

# WITH_SHELL=true bundles a ~1 MB static busybox at /bin/sh so the container
# can be debugged with `docker exec`. It is the only component that must exist
# for the target architecture, and so the only thing limiting the platform
# matrix -- see the note above the shell stages.
ARG WITH_SHELL=true

# ---------------------------------------------------------------------------
# Stage 1: wireproxy, built from source.
#
# Building rather than unpacking a release tarball buys two things: upstream
# publishes no ppc64le or loong64 asset (both compile cleanly), and its single
# `arm` asset is GOARM=6, so arm/v7 hosts would otherwise run the v6 binary.
# ---------------------------------------------------------------------------
FROM --platform=$BUILDPLATFORM golang:1.26-alpine AS wireproxy-build

ARG WIREPROXY_REPO=https://github.com/whyvl/wireproxy.git
ARG WIREPROXY_REF=v1.1.3
ARG TARGETARCH
ARG TARGETVARIANT

RUN apk add --no-cache git

WORKDIR /src
RUN git clone --depth 1 --branch "${WIREPROXY_REF}" "${WIREPROXY_REPO}" .

RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    set -eux; \
    case "${TARGETVARIANT}" in \
        v5) export GOARM=5 ;; \
        v6) export GOARM=6 ;; \
        v7) export GOARM=7 ;; \
    esac; \
    CGO_ENABLED=0 GOOS=linux GOARCH="${TARGETARCH}" \
        go build -trimpath -ldflags "-s -w -X main.version=${WIREPROXY_REF}" \
        -o /out/wireproxy ./cmd/wireproxy

# ---------------------------------------------------------------------------
# Stage 2: warp-reg -- this project's entrypoint, healthcheck and exporter.
# Standard library only, so there is nothing to download and no go.sum.
# ---------------------------------------------------------------------------
FROM --platform=$BUILDPLATFORM golang:1.26-alpine AS warp-reg-build

ARG VERSION=dev
ARG TARGETARCH
ARG TARGETVARIANT

WORKDIR /src
COPY go.mod ./
COPY cmd ./cmd

RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    set -eux; \
    case "${TARGETVARIANT}" in \
        v5) export GOARM=5 ;; \
        v6) export GOARM=6 ;; \
        v7) export GOARM=7 ;; \
    esac; \
    CGO_ENABLED=0 GOOS=linux GOARCH="${TARGETARCH}" \
        go build -trimpath -ldflags "-s -w -X main.version=${VERSION}" \
        -o /out/warp-reg ./cmd/warp-reg

# ---------------------------------------------------------------------------
# Stage 3: the architecture-independent parts of the rootfs.
#
# CA certificates and the passwd/group/nsswitch files are plain text, so this
# stage runs on $BUILDPLATFORM and works for every target -- including
# loong64 and mips64le, for which no alpine image is published.
# ---------------------------------------------------------------------------
FROM --platform=$BUILDPLATFORM alpine:3.22 AS rootfs

RUN apk add --no-cache ca-certificates

# Assembles /rootfs, which the final stage copies in wholesale:
#   - passwd/group so the numeric uid resolves and `USER warp` means something
#   - resolv.conf and nsswitch.conf because Go's pure resolver reads both;
#     Docker bind-mounts over resolv.conf at run time, but it must exist first
#   - the two directories the runtime writes to, owned by the runtime uid
RUN set -eux; \
    mkdir -p /rootfs/etc/ssl/certs /rootfs/etc/wireproxy \
             /rootfs/var/lib/warp-wireproxy /rootfs/tmp; \
    cp /etc/ssl/certs/ca-certificates.crt /rootfs/etc/ssl/certs/; \
    echo 'warp:x:65532:65532:warp:/nonexistent:/sbin/nologin' > /rootfs/etc/passwd; \
    echo 'warp:x:65532:' > /rootfs/etc/group; \
    : > /rootfs/etc/resolv.conf; \
    echo 'hosts: files dns' > /rootfs/etc/nsswitch.conf; \
    chown -R 65532:65532 /rootfs/etc/wireproxy /rootfs/var/lib/warp-wireproxy; \
    chmod 1777 /rootfs/tmp

# ---------------------------------------------------------------------------
# Stage 4: the optional shell.
#
# busybox-static comes from alpine, which publishes 386, amd64, arm/v6,
# arm/v7, arm64, ppc64le, riscv64 and s390x -- a superset of every linux
# binary wireproxy releases. Build with --build-arg WITH_SHELL=false to drop
# the shell, save ~1 MB, and additionally target loong64 and mips64le.
# ---------------------------------------------------------------------------
FROM alpine:3.22 AS shell-true
# Every applet gets its own symlink. Linking only `sh` yields a shell in which
# `ls`, `id` and `cat` are all "not found", which is not a debuggable
# container. The links are RELATIVE (ls -> busybox, not /extra/bin/busybox) so
# they still resolve once this directory is copied to /bin in the final image;
# `busybox --install -s` would bake in the build-time path and break.
# ~400 symlinks cost a few KB.
RUN set -eux; \
    apk add --no-cache busybox-static; \
    mkdir -p /extra/bin; \
    cp /bin/busybox.static /extra/bin/busybox; \
    for applet in $(/extra/bin/busybox --list); do \
        ln -sf busybox "/extra/bin/${applet}"; \
    done; \
    test -L /extra/bin/sh && test -L /extra/bin/ls

FROM --platform=$BUILDPLATFORM alpine:3.22 AS shell-false
RUN mkdir -p /extra/bin

# Resolves to shell-true or shell-false depending on the WITH_SHELL build arg.
FROM shell-${WITH_SHELL} AS shell

# ---------------------------------------------------------------------------
# Stage 5: the final image.
# ---------------------------------------------------------------------------
FROM scratch

ARG VERSION=dev
ARG WIREPROXY_REF=v1.1.3
ARG COMMIT_SHA=unknown

LABEL org.opencontainers.image.title="warp-wireproxy" \
      org.opencontainers.image.description="Cloudflare WARP as a SOCKS5/HTTP proxy over userspace WireGuard. No NET_ADMIN, no kernel module, ~17MB." \
      org.opencontainers.image.licenses="MIT" \
      org.opencontainers.image.version="${VERSION}" \
      org.opencontainers.image.revision="${COMMIT_SHA}" \
      org.opencontainers.image.base.name="scratch" \
      wireproxy.version="${WIREPROXY_REF}"

COPY --from=rootfs /rootfs/ /
COPY --from=shell /extra/ /
COPY --from=wireproxy-build /out/wireproxy /usr/bin/wireproxy
COPY --from=warp-reg-build /out/warp-reg /usr/bin/warp-reg

# wireproxy is entirely userspace: no capabilities, no /dev/net/tun, no kernel
# WireGuard module, and therefore no reason to run as root.
USER 65532:65532

# SOCKS5_BIND, HTTP_BIND and INFO_BIND are deliberately NOT set here.
#
# warp-reg already falls back to exactly these values in code
# (listener("SOCKS5", "0.0.0.0:1080") and friends), so setting them again
# here changed nothing about the defaults -- but it did break SOCKS5_PORT and
# HTTP_PORT completely. `_BIND` wins over `_PORT`, and os.LookupEnv cannot
# tell an image default from something the operator set, so `_PORT` was read
# and then always overridden.
#
# That mattered most in the case the README recommends `_PORT` for: several
# instances sharing one network namespace, where each needs its own port.
# Every instance asked for 1080, exactly one got it, and the rest crash-looped
# on "address already in use" -- while warp-reg's own error message advised
# setting SOCKS5_PORT.
ENV WARP_STATE_DIR=/var/lib/warp-wireproxy \
    WIREPROXY_CONF=/etc/wireproxy/wireproxy.conf

# Metadata only: EXPOSE documents the default listeners. It does not bind,
# publish or reserve anything, so it never constrains the port environment
# variables. The one thing it feeds is `docker run -P`, which publishes
# exactly these ports; if you move the listeners and rely on -P, publish
# explicitly with -p instead.
#
# Whether two instances can share these ports depends on the network mode:
#
#   default (bridge)   each container has its own network namespace, so any
#                      number of instances may listen on 1080 internally and
#                      be published on different host ports.
#
#   network_mode:      the container joins another container's namespace and
#   "service:..."      shares ONE port space with everything else in it. Every
#   or "container:..." instance then needs its own SOCKS5_PORT, HTTP_PORT,
#                      INFO_BIND and METRICS_BIND. INFO_BIND is the one people
#                      miss, because its default is the same for every
#                      instance; warp-reg checks for this at startup and says
#                      so rather than letting wireproxy panic.
#
# The info endpoint stays on loopback and the exporter is opt-in, so neither
# is listed here.
EXPOSE 1080 8080

VOLUME ["/var/lib/warp-wireproxy"]

# Backed by ICMP echoes that actually traverse the tunnel to Cloudflare and
# back, rather than merely checking that a socket is listening.
HEALTHCHECK --interval=30s --timeout=5s --start-period=20s --retries=3 \
    CMD ["/usr/bin/warp-reg", "health"]

ENTRYPOINT ["/usr/bin/warp-reg"]
CMD ["run"]
