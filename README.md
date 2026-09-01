# warp-wireproxy

[![build](https://github.com/nkg/docker-warp-wireproxy/actions/workflows/build.yml/badge.svg)](https://github.com/nkg/docker-warp-wireproxy/actions/workflows/build.yml)
[![watch-upstream](https://github.com/nkg/docker-warp-wireproxy/actions/workflows/watch-upstream.yml/badge.svg)](https://github.com/nkg/docker-warp-wireproxy/actions/workflows/watch-upstream.yml)
[![License: MIT](https://img.shields.io/badge/License-MIT-blue.svg)](LICENSE)

> Cloudflare WARP as a SOCKS5 / HTTP proxy, in an ~17 MB container.
> Userspace WireGuard — no `NET_ADMIN`, no `SYS_MODULE`, no `/dev/net/tun`,
> no `/lib/modules`, no `privileged`, no `sysctls`, and no root.

Built on [wireproxy](https://github.com/whyvl/wireproxy), which speaks
WireGuard entirely in userspace and exposes it as a proxy. That is the whole
trick: there is no network interface to create, so there is nothing to grant
the container.

```bash
docker run -d --name warp -p 1080:1080 ghcr.io/nkg/warp-wireproxy

curl -x socks5h://127.0.0.1:1080 https://www.cloudflare.com/cdn-cgi/trace
# ...
# warp=on
```

Also on Docker Hub as `earcandy/warp-wireproxy`.

## Contents

- [Why](#why)
- [Quick start](#quick-start)
- [Many instances](#many-instances)
- [Shared network namespaces](#shared-network-namespaces)
- [Apple `container`](#apple-container-macos-26)
- [Configuration](#configuration)
- [Health and metrics](#health-and-metrics)
- [Bring your own config](#bring-your-own-config)
- [Architectures](#architectures)
- [How it works](#how-it-works)
- [Building](#building)
- [Automation](#automation)
- [Credits](#credits)

## Why

Most WARP containers either ship Cloudflare's official `warp-cli` (which needs
a Debian/Ubuntu base, `dbus`, a daemon, and `sudo`) or route the whole
container network through a kernel WireGuard interface (which needs
`NET_ADMIN`, `SYS_MODULE`, and a matching host kernel module).

This one does neither.

|                       | warp-wireproxy | `warp-cli` images | kernel-WireGuard images |
| --------------------- | -------------- | ----------------- | ----------------------- |
| Image size            | ~17 MB         | ~500 MB           | ~30 MB                  |
| Root required         | no             | yes (`sudo`)      | yes                     |
| Extra capabilities    | none           | none              | `NET_ADMIN`, `SYS_MODULE` |
| Host kernel module    | no             | no                | yes                     |
| Shell / curl / jq in image | shell only | yes               | usually                 |
| Architectures         | 8 (10 shell-less) | amd64, arm64   | varies                  |

## Quick start

### Docker

```bash
docker run -d --name warp \
    --restart unless-stopped \
    -p 1080:1080 \
    -p 8080:8080 \
    -v warp-state:/var/lib/warp-wireproxy \
    ghcr.io/nkg/warp-wireproxy
```

### Compose

See [`docker-compose.yml`](docker-compose.yml) for a hardened example
(`cap_drop: ALL`, `read_only`, `no-new-privileges`).

```bash
docker compose up -d
docker compose exec warp /usr/bin/warp-reg health && echo healthy
```

### Many instances

Each instance registers its own WARP device, with its own WireGuard keys and
tunnel address. The shipped compose file scales as-is:

```bash
docker compose up -d --scale warp=10
docker compose ps                      # shows the host port for each replica
```

Ports come from a host-side **range**, so replicas never collide:

```yaml
ports:
  - "${SOCKS5_PORTS:-1080-1089}:1080"
  - "${HTTP_PORTS:-8080-8089}:8080"
```

Widen the range (via a `.env` file or by editing) to scale past ten. The
container listens on 1080/8080 *internally* and each replica has its own
network namespace, so the internal ports never collide — only the host side
has to vary. **This stops being true if you attach instances to a shared
network namespace**; see
[Shared network namespaces](#shared-network-namespaces).

Two things will quietly collapse your instances into one identity:

- **A named volume.** `warp-state:/var/lib/warp-wireproxy` is *shared* by
  every replica, so they all load the same WireGuard keys, present to
  Cloudflare as a single device, and race to write the same
  `registration.json`. The compose file uses an **anonymous** volume
  (`- /var/lib/warp-wireproxy`) so each replica gets its own, persisted
  across restarts of that replica. Use named volumes only if you declare one
  *per instance*.
- **`container_name`.** Container names must be unique, so setting one caps
  you at a single replica. It is deliberately unset.

If you need a specific host port pinned to a specific instance rather than
whatever the range hands out, declare the services explicitly — there is a
worked example in the comments at the bottom of `docker-compose.yml`.

Registering many devices at once is fine: `warp-reg` retries with exponential
backoff if Cloudflare rate-limits, rather than crash-looping.

**On egress IPs.** Instances usually leave from different Cloudflare
addresses, but that is a property of Cloudflare's anycast routing, not of the
tunnel identity: the egress address is chosen per session, so it is not
guaranteed unique per instance and can change on reconnect. Measured on a
two-instance run, separate registrations gave both different tunnel addresses
and different egress IPs — but two instances *sharing* one registration also
showed different egress IPs while using a single WireGuard identity. Do not
use "the IPs differ" as evidence that your instances are configured
correctly; check the tunnel address instead:

```bash
docker compose exec --index 1 warp /bin/sh -c 'grep ^Address /var/lib/warp-wireproxy/wireproxy.conf'
docker compose exec --index 2 warp /bin/sh -c 'grep ^Address /var/lib/warp-wireproxy/wireproxy.conf'
```

Different `Address` lines mean genuinely separate devices. Identical ones mean
the instances are sharing state — see the volume note above.

`EXPOSE 1080 8080` in the Dockerfile is metadata only. It does not bind,
publish or reserve anything, so it never constrains the port variables. It
feeds only `docker run -P`.

### Shared network namespaces

By default each container gets its own network namespace, so replicas can all
listen on the same internal ports. If you instead attach containers to an
existing namespace — `network_mode: "service:<name>"` or
`network_mode: "container:<id>"`, the pattern used for sidecars — they share
**one port space**, and the rules change:

**Every listener needs its own port.** Not just the proxies: `SOCKS5_PORT`,
`HTTP_PORT`, `INFO_BIND` *and* `METRICS_BIND` must all differ between
instances in the same namespace.

`INFO_BIND` is the one that catches people. It defaults to `127.0.0.1:9080`
for every instance, so a second instance collides *even when you have
correctly given it a different SOCKS5 port*. warp-reg probes all of its
listeners before registering and fails with a message naming the variable,
rather than letting wireproxy panic with a stack trace after a WARP device has
already been enrolled:

```
cannot bind 127.0.0.1:9080 (INFO_BIND): bind: address already in use
```

Set `INFO_BIND=""` to switch the health endpoint off entirely if you would
rather not allocate a port per instance — at the cost of the container
healthcheck and the Prometheus exporter.

**Ports are published on the owning service.** A container joining another's
namespace may not declare `ports:` of its own; Compose rejects it. Publish
each instance's port on whichever service owns the namespace.

**`sysctls` cannot be set at all** on a container sharing a namespace — Docker
rejects the combination. This image needs none, so that is only a concern when
migrating a config from an image that did.

A worked layout for two instances in one namespace:

| Instance | `SOCKS5_PORT` | `INFO_BIND` |
| --- | --- | --- |
| 1 | `1080` | `127.0.0.1:9080` |
| 2 | `1081` | `127.0.0.1:9081` |

### Apple `container` (macOS 26+)

The image runs unmodified on Apple's [`container`](https://github.com/apple/container),
and that is what it is developed and tested against:

```bash
container run -d --name warp \
    -p 1080:1080 -p 8080:8080 \
    -v warp-state:/var/lib/warp-wireproxy \
    ghcr.io/nkg/warp-wireproxy

curl -x socks5h://127.0.0.1:1080 https://www.cloudflare.com/cdn-cgi/trace
```

Four differences from Docker are worth knowing:

**No healthcheck support.** `container` ignores the image's `HEALTHCHECK`, so
there is no `State.Health` to inspect. Run the same probe by hand — it is the
identical command the `HEALTHCHECK` invokes:

```bash
container exec warp /usr/bin/warp-reg health && echo healthy
```

**No compose.** Use `container run` with the flags above; the
`docker-compose.yml` hardening options (`cap_drop`, `read_only`) have no
equivalent and are simply not needed — the container is already non-root with
no capabilities, and each one gets its own VM.

**Each container gets a routable IP**, so you can skip `-p` entirely and talk
to it directly:

```bash
container ls           # note the ADDR column, e.g. 192.168.64.18
curl -x socks5h://192.168.64.18:1080 https://www.cloudflare.com/cdn-cgi/trace
```

**The builder runs in its own VM** and sometimes comes up without a working
resolver, which shows as `apk ... temporary error (try again later)` during
the build. Recreate it:

```bash
container builder delete
container builder start --cpus 4 --memory 8G --dns 192.168.64.1
```

Note `--memory` needs a unit suffix — a bare `8192` is parsed as zero and
rejected.

### Verify

```bash
curl -x socks5h://127.0.0.1:1080 https://www.cloudflare.com/cdn-cgi/trace   # SOCKS5
curl -x http://127.0.0.1:8080    https://www.cloudflare.com/cdn-cgi/trace   # HTTP
```

`warp=on` means traffic is exiting through Cloudflare. Use `socks5h://` rather
than `socks5://` so DNS is resolved through the tunnel too, not on your host.

## Configuration

Everything is environment variables. Every one has a working default; running
the image with no configuration at all gives you a working dual-stack proxy.

### Listeners

| Variable | Description | Default |
| --- | --- | --- |
| `SOCKS5_BIND` | SOCKS5 listen address. Empty string disables it. | `0.0.0.0:1080` |
| `SOCKS5_PORT` | Port only; ignored if `SOCKS5_BIND` is set. | — |
| `SOCKS5_HOST` | Host for `SOCKS5_PORT`. | `0.0.0.0` |
| `SOCKS5_USER` / `SOCKS5_PASSWD` | SOCKS5 auth. Must be set together. No spaces. | — |
| `HTTP_BIND` | HTTP proxy listen address. Empty string disables it. | `0.0.0.0:8080` |
| `HTTP_PORT` / `HTTP_HOST` | As above. | — |
| `HTTP_USER` / `HTTP_PASSWD` | HTTP proxy auth. Must be set together. No spaces. | — |

Setting only one half of a credential pair is rejected at startup rather than
silently leaving the proxy open.

### Tunnel

| Variable | Description | Default |
| --- | --- | --- |
| `WARP_MTU` | Tunnel MTU. | `1280` |
| `DNS_SERVERS` | Resolvers used inside the tunnel, comma-separated. | Cloudflare v4 + v6 |
| `WARP_ENDPOINT` | WireGuard peer endpoint. | `engage.cloudflareclient.com:2408` |
| `WARP_KEEPALIVE` | `PersistentKeepalive`, seconds. | `25` |
| `WARP_API` | Registration API. | `…cloudflareclient.com/v0a2025/reg` |
| `WARP_STATE_DIR` | Where the registration is cached. Empty disables caching. | `/var/lib/warp-wireproxy` |
| `WARP_REREGISTER` | Force a new device on every start. | `false` |
| `WARP_REGISTER_RETRIES` | Registration attempts before giving up. | `5` |
| `WARP_REGISTER_TIMEOUT` | Per-attempt HTTP timeout, seconds. | `30` |

### Health, metrics and config

| Variable | Description | Default |
| --- | --- | --- |
| `INFO_BIND` | wireproxy's `/metrics` + `/readyz` endpoint. Empty disables it. | `127.0.0.1:9080` |
| `METRICS_BIND` | Prometheus exporter address. Unset = exporter off. | — |
| `CHECK_ALIVE` | Addresses pinged through the tunnel to drive `/readyz`. | `1.1.1.1`, `2606:4700:4700::1111` |
| `CHECK_ALIVE_INTERVAL` | Ping interval, seconds. | `5` |
| `WIREPROXY_CONF` | Config file path. A file you supply here is used as-is; one warp-reg generated is regenerated. | `/etc/wireproxy/wireproxy.conf` |
| `WG_CONFIG` | Use an existing WireGuard config instead of registering. | — |
| `EXTRA_CONFIG` | Raw text appended to the generated config. | — |
| `WIREPROXY_SILENT` | Pass `-s` to wireproxy. | `false` |

### Persistence

Mount a volume at `/var/lib/warp-wireproxy` and the WARP registration is
cached there. Restarts then reuse the same device and keys instead of
enrolling a new one with Cloudflare each time, which avoids both API
rate-limiting and a trail of orphaned device registrations.

Persistence is best-effort: if the directory is unwritable (a read-only mount,
a volume owned by another uid) the container logs a warning and registers
afresh rather than refusing to start.

## Health and metrics

### Healthcheck

The image's `HEALTHCHECK` runs `warp-reg health`, which queries wireproxy's
`/readyz`. That endpoint is backed by ICMP echoes that actually traverse the
tunnel to Cloudflare and back — so it reports on the tunnel, not merely on
whether a socket is listening.

```bash
docker inspect -f '{{.State.Health.Status}}' warp
```

### Prometheus

Set `METRICS_BIND` and the entrypoint also serves a Prometheus exporter. It is
built into the same binary, so it adds nothing to the image.

```bash
docker run -d -p 1080:1080 -p 9095:9095 \
    -e METRICS_BIND=0.0.0.0:9095 \
    ghcr.io/nkg/warp-wireproxy

curl -s http://127.0.0.1:9095/metrics
```

wireproxy's own `/metrics` speaks the WireGuard UAPI dialect (`tx_bytes=…`,
hex keys), which Prometheus cannot scrape. The exporter translates it:

| Metric | Type | Meaning |
| --- | --- | --- |
| `warp_wireproxy_up` | gauge | wireproxy's info endpoint responded |
| `warp_wireproxy_ready` | gauge | every `CHECK_ALIVE` target is answering |
| `warp_wireproxy_peers` | gauge | configured WireGuard peers |
| `warp_wireproxy_peer_receive_bytes_total` | counter | bytes received from the peer |
| `warp_wireproxy_peer_transmit_bytes_total` | counter | bytes sent to the peer |
| `warp_wireproxy_peer_last_handshake_timestamp_seconds` | gauge | last completed handshake |
| `warp_wireproxy_peer_handshake_age_seconds` | gauge | seconds since handshake, `-1` if never |
| `warp_wireproxy_peer_persistent_keepalive_seconds` | gauge | configured keepalive |
| `warp_wireproxy_check_alive_last_pong_timestamp_seconds` | gauge | last pong per target |
| `warp_wireproxy_device_listen_port` | gauge | local UDP source port |
| `warp_wireproxy_device_errno` | gauge | device errno, `0` is healthy |
| `warp_wireproxy_scrape_duration_seconds` | gauge | time spent scraping |
| `warp_wireproxy_build_info` | gauge | version label |

Peer keys are re-encoded from hex to the base64 form `wg show` prints, so they
match your config by eye.

An alert worth having:

```yaml
- alert: WarpTunnelStale
  expr: warp_wireproxy_ready == 0 or warp_wireproxy_peer_handshake_age_seconds > 300
  for: 5m
```

`/healthz` on the same port is a plain 200/503 liveness probe for
orchestrators that want one separate from the scrape.

### Debugging

`warp-reg` is also a small CLI:

```bash
docker exec warp /usr/bin/warp-reg config    # show the generated config
docker exec warp /usr/bin/warp-reg metrics   # one Prometheus scrape
docker exec warp /usr/bin/warp-reg health    # exit 0 if the tunnel is up
docker exec warp /usr/bin/warp-reg version
docker exec -it warp /bin/sh                 # busybox, unless WITH_SHELL=false
```

## Bring your own config

The image is not WARP-only.

**A complete wireproxy config** — a file you put at `WIREPROXY_CONF` is used
verbatim and registration is skipped:

```bash
docker run -d -p 1080:1080 \
    -v ./my.conf:/etc/wireproxy/wireproxy.conf:ro \
    ghcr.io/nkg/warp-wireproxy
```

**An existing WireGuard config** — `WG_CONFIG` points at a `wg-quick` file;
the proxy sections still come from the environment:

```bash
docker run -d -p 1080:1080 \
    -e WG_CONFIG=/wg/peer.conf \
    -v ./peer.conf:/wg/peer.conf:ro \
    ghcr.io/nkg/warp-wireproxy
```

**Extra sections** — `EXTRA_CONFIG` is appended verbatim, which is how you
reach wireproxy features that have no environment variable of their own:

```bash
docker run -d -p 25565:25565 \
    -e EXTRA_CONFIG=$'[TCPClientTunnel]\nBindAddress = 0.0.0.0:25565\nTarget = play.example.net:25565' \
    ghcr.io/nkg/warp-wireproxy
```

warp-reg tells the two apart by the `# Generated by warp-reg` header it
writes. A config it generated is regenerated on every start, so changes to
environment variables actually take effect; a config without that header is
never touched. Without this distinction the file written on first boot would
be mistaken for yours on every later boot, and no environment change would
ever apply.

See wireproxy's [README](https://github.com/whyvl/wireproxy#sample-config-file)
for `[TCPClientTunnel]`, `[TCPServerTunnel]`, `[UDPProxyTunnel]`, `[SNI]`,
`TunnelDomains` and friends.

## Architectures

Published by default:

`linux/amd64` · `linux/386` · `linux/arm/v6` · `linux/arm/v7` · `linux/arm64`
· `linux/ppc64le` · `linux/riscv64` · `linux/s390x`

That is a superset of every linux binary wireproxy itself releases — upstream
ships no `ppc64le`, and its single `arm` asset is `GOARM=6`, so `arm/v7` hosts
would otherwise run the v6 build. Building from source fixes both.

`linux/loong64` and `linux/mips64le` also compile, but no `alpine` image is
published for them, so the bundled busybox cannot be built. Build with
`--build-arg WITH_SHELL=false` to target them (and to save ~1 MB, at the cost
of `docker exec … /bin/sh`).

## How it works

```
                       ┌─────────────────────────────────────────┐
  your app             │ container (scratch, non-root, no caps)  │
      │                │                                         │
      │ SOCKS5 :1080   │   ┌──────────┐      ┌────────────────┐  │
      ├───────────────►│──►│          │      │                │  │
      │ HTTP   :8080   │   │ wireproxy├─────►│ userspace      │  │
      ├───────────────►│──►│          │ UDP  │ WireGuard      │──┼──► Cloudflare
      │                │   └────┬─────┘      └────────────────┘  │    WARP
      │ Prom   :9095   │        │ /metrics /readyz               │
      └───────────────►│──► warp-reg (exporter + healthcheck)    │
                       └─────────────────────────────────────────┘
```

On start, `warp-reg`:

1. Reuses the cached registration, or generates a clamped X25519 keypair and
   `POST`s the public key to Cloudflare's registration API, retrying with
   exponential backoff.
2. Renders `wireproxy.conf` from the environment.
3. Hands off to wireproxy. Without `METRICS_BIND` it `exec`s, so wireproxy
   becomes PID 1 and receives signals directly with no supervisor in the way;
   with it, `warp-reg` stays as a parent to serve the exporter and forwards
   `SIGTERM`.

### One thing worth knowing

wireproxy loads its ini file with `AllowShadows`, but reads multi-valued keys
via `key.String()` — which returns only the **first** occurrence — and then
splits that on commas itself. So this:

```ini
Address = 172.16.0.2/32
Address = 2606:4700:110::1/128     # silently ignored
AllowedIPs = 0.0.0.0/0
AllowedIPs = ::/0                  # silently ignored
```

quietly gives you an IPv4-only tunnel with no IPv6 route, while looking
correct. The generated config always writes them on one line:

```ini
Address = 172.16.0.2/32, 2606:4700:110::1/128
AllowedIPs = 0.0.0.0/0, ::/0
```

There is a regression test pinning this.

## Building

```bash
git clone https://github.com/nkg/docker-warp-wireproxy
cd docker-warp-wireproxy

go test ./...          # unit tests, no container runtime needed
scripts/test.sh        # build the image and verify it reaches WARP
```

`scripts/test.sh` auto-detects Docker, Apple `container`, or Podman; override
with `RUNTIME=container`. It runs 18 checks including a live `warp=on` probe,
egress-IP masking, the Prometheus endpoint, and restart behaviour.

Building by hand:

```bash
docker build -t warp-wireproxy .
container build --platform linux/arm64 -t warp-wireproxy .   # Apple container
```

Build arguments:

| Arg | Description | Default |
| --- | --- | --- |
| `WIREPROXY_REF` | wireproxy git tag to build. | see [`.wireproxy-version`](.wireproxy-version) |
| `WIREPROXY_REPO` | wireproxy fork to build from. | `whyvl/wireproxy` |
| `WITH_SHELL` | Bundle busybox at `/bin/sh`. | `true` |
| `VERSION` | Stamped into `warp-reg version`. | `dev` |

Multi-arch:

```bash
docker buildx build --platform linux/amd64,linux/arm64 -t warp-wireproxy .
```

Both Go stages cross-compile from `$BUILDPLATFORM`, so multi-arch builds do
not run under QEMU.

### Where the size goes

Measured inside a running arm64 container with `du`:

| Component | Size |
| --- | --- |
| `wireproxy` | 9.2 MB |
| `warp-reg` | 5.9 MB |
| busybox (static, optional) | 1.1 MB |
| CA bundle, `passwd`, `nsswitch` | 0.2 MB |
| **Total** | **16.5 MB** |

amd64 binaries are slightly larger, so expect ~17.5 MB there.

Both binaries are Go, and most of `warp-reg` is `net/http` plus `crypto/tls`
— the cost of doing the WARP registration and the Prometheus exporter without
shipping `curl`, `jq` and `openssl`. Dropping the exporter would save roughly
2 MB; dropping busybox saves another 1 MB and adds two architectures.

## Automation

Two workflows, in [`.github/workflows`](.github/workflows).

### `build.yml`

Runs on push, PR, manual dispatch, and weekly. Five jobs:

| Job | What it does |
| --- | --- |
| `resolve` | Decides which wireproxy version this run builds |
| `test` | `gofmt`, `go vet`, `go test -race`, and a version-pin drift check |
| `cross-compile` | Builds both binaries for all 10 architectures in parallel |
| `verify` | Builds an amd64 image, runs it, and requires a real `warp=on` |
| `publish` | Multi-arch build and push to GHCR (and Docker Hub, if configured) |

`verify` gates `publish`, so nothing reaches the registry until a container
has actually been started and proven to route traffic through WARP. Pull
requests never publish.

The weekly cron rebuilds the *current* pin, which is how the image picks up
base image and CA bundle updates without anyone pushing a commit.

### `watch-upstream.yml`

Polls [`whyvl/wireproxy`](https://github.com/whyvl/wireproxy) every 6 hours
and publishes a new image when a release appears.

```
check ──► new release? ──no──► done
              │
             yes
              ▼
          build.yml (test → cross-compile → verify → publish)
              │
        ┌─────┴─────┐
     success      failure
        │             │
        ▼             ▼
   commit the    open/update a
   version bump  tracking issue
```

Three details worth knowing:

**It publishes before it commits.** The version bump lands in the repo only
after the new upstream release has been built, verified against live WARP, and
pushed. A broken upstream release therefore leaves the pin untouched, files an
issue, and gets retried on the next tick — rather than pinning the repo to
something that does not build.

**It calls the build workflow directly** rather than relying on its own commit
to trigger it. Pushes made with `GITHUB_TOKEN` deliberately do not trigger
further workflows, so a commit-and-hope design would silently never build. The
version is passed as a `workflow_call` input rather than read from the
checkout, because at that point the bump is not committed yet.

**`.wireproxy-version` is the single source of truth.** The `test` job fails
if the Dockerfile's `ARG WIREPROXY_REF` defaults drift away from it.

Manual control:

```bash
gh workflow run watch-upstream.yml                    # check now
gh workflow run watch-upstream.yml -f ref=v1.1.2      # build a specific tag
gh workflow run watch-upstream.yml -f force=true      # rebuild the current pin
```

Images are tagged `latest`, `wireproxy-<upstream tag>`, and `sha-<short sha>`,
so you can pin to a specific upstream wireproxy version if you want to.

### Publishing

The image goes to **GHCR** out of the box and to **Docker Hub** as well once
you configure it. Nothing in the workflow names a Docker Hub account, so
enabling it is purely a matter of adding two secrets — no code change.

**GHCR** works with no setup; `GITHUB_TOKEN` covers the push. One gotcha:
a newly created package under a personal account is **private by default**,
even in a public repo. After the first successful publish:

> github.com/users/nkg/packages/container/warp-wireproxy/settings
> → Change visibility → Public

Do that once. While it is private, `docker pull ghcr.io/nkg/warp-wireproxy`
fails with `denied` for everyone else, which reads like a broken image rather
than a permissions setting. In the same place, "Connect repository" links the
package to the repo so the Packages sidebar and the README show up.

**Docker Hub** is opt-in. Nothing in the workflow names an account — it is
read from a secret — so enabling it needs no code change:

1. **Create the repository.** hub.docker.com → Repositories → Create
   repository. Name it `warp-wireproxy`, visibility **Public**. Leave the
   description blank; the workflow fills it in from this README.

2. **Create an access token.** hub.docker.com → your avatar → Account
   settings → **Personal access tokens** → Generate new token.

   | Field | Value |
   | --- | --- |
   | Description | `github-actions warp-wireproxy` |
   | Expiration | your call — a year is reasonable |
   | Access permissions | **Read, Write, Delete** (see below) |

   Copy the token now; it is shown exactly once.

   **On the scope.** The two things this workflow does need different tiers:

   | Operation | Scope |
   | --- | --- |
   | Push images (`docker login` + push) | `Read & Write` |
   | Sync this README to the description | `Read, Write, Delete` |

   The description sync `PATCH`es the repository object, which counts as repo
   administration rather than an image push — hence the wider tier. If you
   would rather keep the token narrow, use `Read & Write`: the publish
   succeeds and only the description step is skipped, because it is marked
   `continue-on-error`. You can then paste the description into Docker Hub by
   hand once, and it will simply stop being updated automatically.

3. **Add two GitHub secrets.** In the repo → Settings → Secrets and variables
   → Actions → New repository secret, twice:

   | Secret | Value |
   | --- | --- |
   | `DOCKERHUB_USERNAME` | `earcandy` |
   | `DOCKERHUB_TOKEN` | the token from step 2 |

That is all. The next publish pushes to both registries and syncs this README
to the Docker Hub description, and the image becomes:

```bash
docker pull earcandy/warp-wireproxy
```

Until the secrets exist the workflow logs `Docker Hub not configured;
publishing to GHCR only` and carries on — it never fails for a missing Docker
Hub. Setting only one of the two logs a warning rather than half-configuring.

Rotating the token later means replacing `DOCKERHUB_TOKEN` only; the workflow
does not need touching.

**Actions permissions** — the upstream watcher commits the version bump, so
Settings → Actions → General → Workflow permissions must be
**Read and write permissions**.

## Credits

- [wireproxy](https://github.com/whyvl/wireproxy) — the userspace WireGuard
  proxy this is built on.
- [Mon-ius/Docker-Warp-Socks](https://github.com/Mon-ius/Docker-Warp-Socks) —
  the self-contained WARP registration approach.
- [ErcinDedeoglu/cloudflare-warp](https://github.com/ErcinDedeoglu/cloudflare-warp)
  — healthcheck, persistence and documentation patterns.
- [Cloudflare WARP](https://developers.cloudflare.com/warp-client/).

## Notice

Not affiliated with, endorsed by, or connected to Cloudflare. WARP's
[terms of service](https://www.cloudflare.com/application/terms/) apply to
traffic you send through it.

## License

[MIT](LICENSE). wireproxy itself is ISC-licensed and is compiled into the
published image.
