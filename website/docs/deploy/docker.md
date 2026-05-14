---
title: Docker image
sidebar_position: 1
---

# Docker image

Multi-stage build → distroless `nonroot`.

## Build

```bash
make docker DOCKER_TAG=v0.1.0
# → memsidecar:v0.1.0
```

Or directly:

```bash
docker build \
  --build-arg VERSION=v0.1.0 \
  --build-arg COMMIT=$(git rev-parse --short HEAD) \
  --build-arg BUILD_DATE=$(date -u +%Y-%m-%dT%H:%M:%SZ) \
  -t memsidecar:v0.1.0 .
```

The build stage is `golang:1.26-alpine` with `CGO_ENABLED=0` and
`-trimpath`. The runtime stage is `gcr.io/distroless/static:nonroot`,
which runs as UID/GID 65532 with no shell.

## Layout in the image

| Path | Contents |
|---|---|
| `/usr/local/bin/memsidecar` | server binary |
| `/usr/local/bin/memctl` | admin CLI |
| `/etc/memsidecar/config.yaml` | **expected to be mounted** (no default ships) |
| `/tmp` | writable for the UDS socket and tmp files |

`ENTRYPOINT` is `/usr/local/bin/memsidecar`; `CMD` is
`["--config", "/etc/memsidecar/config.yaml"]`.

## Exposed ports

| Port | Purpose |
|---|---|
| 7777 | gRPC TCP |
| 8080 | HTTP / JSON gateway |
| 9090 | Prometheus `/metrics` |

## Local run

```bash
docker run --rm \
  -p 7777:7777 -p 8080:8080 -p 9090:9090 \
  -v $(pwd)/configs:/etc/memsidecar:ro \
  memsidecar:v0.1.0
```

`-v $(pwd)/configs:/etc/memsidecar:ro` mounts the repo's example config.
Override with your own config path.

## Multi-arch

The Dockerfile doesn't pin a `--platform`, so a single `docker buildx`
invocation can target multiple architectures:

```bash
docker buildx build --platform linux/amd64,linux/arm64 -t my-registry/memsidecar:v0.1.0 --push .
```

## Build args

| Arg | Default | Purpose |
|---|---|---|
| `VERSION` | `dev` | embedded as `internal/version.Version` |
| `COMMIT` | `unknown` | embedded as `internal/version.Commit` |
| `BUILD_DATE` | `unknown` | embedded as `internal/version.BuildDate` |

These show up in `memsidecar --version` and in the OTel resource
`service.version`.

## Security posture

- Distroless `nonroot`: no shell, no package manager, no userland.
- Static binaries with `CGO_ENABLED=0`.
- The Helm chart applies `readOnlyRootFilesystem: true` and
  `runAsNonRoot: true` on top, matching the restricted Pod Security
  Standard.
