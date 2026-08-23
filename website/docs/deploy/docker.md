---
title: Docker image
sidebar_position: 1
---

# Docker image

Multi-stage build to a distroless `nonroot` runtime, published to GHCR on every
tag.

## Pull the published image

```bash
docker pull ghcr.io/vibed-project/mindd:v0.1.0
docker pull ghcr.io/vibed-project/mindd:latest
```

Tag pushes (`v*`) run the full gate set (vet, unit, integration, lint) against
the tagged commit, then publish `linux/amd64` and `linux/arm64` under both the
semver tag and `:latest`. Images carry `org.opencontainers.image.*` labels
(source, revision, version, created, licenses, title, description).

## Build it yourself

```bash
make docker DOCKER_TAG=v0.1.0
# -> mindd:v0.1.0
```

Or directly:

```bash
docker build \
  --build-arg VERSION=v0.1.0 \
  --build-arg COMMIT=$(git rev-parse --short HEAD) \
  --build-arg BUILD_DATE=$(date -u +%Y-%m-%dT%H:%M:%SZ) \
  -t mindd:v0.1.0 .
```

The build stage is `golang:1.26-alpine` with `CGO_ENABLED=0` and `-trimpath`.
The runtime stage is `gcr.io/distroless/static:nonroot`, which runs as UID/GID
65532 with no shell.

## Layout in the image

| Path | Contents |
|---|---|
| `/usr/local/bin/mindd` | server binary |
| `/usr/local/bin/mindctl` | CLI (token issue, gen-keypair, MCP, data plane) |
| `/etc/mindd/config.yaml` | **expected to be mounted**; no default ships |
| `/tmp` | writable for the UDS socket and tmp files |

`ENTRYPOINT` is `/usr/local/bin/mindd`; `CMD` is
`["--config", "/etc/mindd/config.yaml"]`. The server has no built-in default
config path, so overriding `CMD` without a `--config` argument fails at startup
with `--config required`.

## Exposed ports

| Port | Purpose |
|---|---|
| 7777 | gRPC TCP |
| 8080 | HTTP / JSON gateway |
| 9090 | Prometheus `/metrics` |

The gateway and metrics listeners only bind when configured
(`server.http.addr`, `observability.metrics.exporter: prometheus`). The gRPC
TCP listener always binds: leaving `server.grpc.tcp` empty falls back to
`127.0.0.1:7777`, which inside a container is unreachable from outside it. Bind
`0.0.0.0` explicitly, as `configs/compose.yaml` does.

## Local run

```bash
docker run --rm \
  -p 7777:7777 -p 8080:8080 -p 9090:9090 \
  -v "$(pwd)/configs/compose.yaml:/etc/mindd/config.yaml:ro" \
  ghcr.io/vibed-project/mindd:v0.1.0
```

`configs/compose.yaml` binds `0.0.0.0` but expects Postgres and MinIO;
`configs/example.yaml` runs entirely in memory but binds `127.0.0.1`. For a
self-contained local stack, use `docker compose up` from the repo root instead,
which wires both together. See the [Quickstart](../quickstart.md).

:::warning Both example configs trust the public dev key
`configs/example.yaml` and `configs/compose.yaml` both set
`auth.paseto.public_key_hex` to this repository's development key, whose private
half is published with it. Mount your own config with your own key for anything
that is not a local experiment. See [Security](../security.md).
:::

## Multi-arch

The Dockerfile declares `TARGETOS` / `TARGETARCH` without defaults so BuildKit's
per-platform values are not shadowed, which means one `docker buildx`
invocation can target multiple architectures correctly:

```bash
docker buildx build --platform linux/amd64,linux/arm64 -t my-registry/mindd:v0.1.0 --push .
```

`make docker-multiarch` wraps the same thing (`DOCKER_PLATFORMS` defaults to
`linux/amd64,linux/arm64`).

## Build args

| Arg | Default | Purpose |
|---|---|---|
| `VERSION` | `dev` | embedded as `internal/version.Version` |
| `COMMIT` | `unknown` | embedded as `internal/version.Commit` |
| `BUILD_DATE` | `unknown` | embedded as `internal/version.BuildDate` |

These show up in `mindd --version`, in `mindctl ns ls`, in the Admin block's
`ServerInfo`, and in the OTel resource `service.version`.

## Security posture

- Distroless `nonroot`: no shell, no package manager, no userland.
- Static binaries with `CGO_ENABLED=0`.
- The Helm chart applies `readOnlyRootFilesystem: true` and
  `runAsNonRoot: true` on top, matching the restricted Pod Security Standard.
- The image does **not** make the sidecar safe to expose. gRPC reflection is
  unauthenticated and the HTTP gateway has no TLS. See
  [Security](../security.md).
