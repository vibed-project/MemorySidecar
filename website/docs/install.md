---
title: Install
sidebar_position: 2
---

# Install

v0.1.0 is tagged. There are four ways to get `mindd` (the server) and
`mindctl` (the CLI), in rough order of how quickly they get you running.

:::warning Before you deploy anything
Every example in this documentation uses a development PASETO keypair whose
private half is published in this repository. Generate your own with
`mindctl token gen-keypair` before you run mindD anywhere that matters. See
[Security](./security.md).
:::

## Container image

Multi-arch (`linux/amd64`, `linux/arm64`), published to GHCR on every tag:

```bash
docker pull ghcr.io/vibed-project/mindd:v0.1.0
# or the moving tag
docker pull ghcr.io/vibed-project/mindd:latest
```

The image is distroless `nonroot` (UID/GID 65532, no shell, no package
manager). It ships both binaries, expects a config mounted at
`/etc/mindd/config.yaml`, and exposes 7777 (gRPC), 8080 (HTTP gateway) and
9090 (metrics):

```bash
docker run --rm \
  -p 7777:7777 -p 8080:8080 -p 9090:9090 \
  -v "$(pwd)/configs/example.yaml:/etc/mindd/config.yaml:ro" \
  ghcr.io/vibed-project/mindd:v0.1.0
```

See [Docker image](./deploy/docker.md) for the full layout and build args.

## Release binaries

Each tag attaches cross-platform archives plus `checksums.txt` to the
[GitHub Release](https://github.com/vibed-project/mindD/releases). Every
archive contains **both** `mindd` and `mindctl`, because `mindctl` mints the
tokens the `mindd` it ships with will accept, so the two should never be
version-skewed. Platforms: linux, darwin and windows on amd64 and arm64.

```bash
VERSION=0.1.0
OS=$(uname -s | tr '[:upper:]' '[:lower:]')     # linux | darwin
ARCH=$(uname -m | sed 's/x86_64/amd64/;s/aarch64/arm64/')

curl -sSLO "https://github.com/vibed-project/mindD/releases/download/v${VERSION}/mindd_${VERSION}_${OS}_${ARCH}.tar.gz"
curl -sSLO "https://github.com/vibed-project/mindD/releases/download/v${VERSION}/checksums.txt"
sha256sum -c checksums.txt --ignore-missing

tar xzf "mindd_${VERSION}_${OS}_${ARCH}.tar.gz"
sudo install -m 0755 mindd mindctl /usr/local/bin/
```

The archives also carry `LICENSE`, `NOTICE`, `README.md`, `CHANGELOG.md` and
`configs/example.yaml`.

## `go install`

Needs Go 1.26 or newer. This builds from source at the tag, so there is no
checksum step and no container runtime involved:

```bash
go install github.com/vibed-project/mindD/cmd/mindd@v0.1.0
go install github.com/vibed-project/mindD/cmd/mindctl@v0.1.0
```

Note the capital `D` in the module path. Binaries land in
`$(go env GOPATH)/bin`.

`go install` applies no linker flags, so the version is recovered from the
module's build info instead: `mindd --version` reports `v0.1.0` rather than
`dev`.

## Build from source

```bash
git clone https://github.com/vibed-project/mindD.git
cd mindD
make build            # produces bin/mindd and bin/mindctl
```

`make build` stamps the version, commit and build date from git. You only need
[buf](https://buf.build/docs/installation) if you intend to regenerate the
protobuf stubs; the generated code is checked in under `gen/`.

## Helm

```bash
helm install mindd deploy/helm/mindd \
  --set config.auth.paseto.public_key_hex=<your-public-key-hex>
```

`image.repository` already defaults to `ghcr.io/vibed-project/mindd`, and
`image.tag` defaults to the chart's `appVersion`. The chart has **no** default
signing key and refuses to render without one. See [Helm](./deploy/helm.md).

## Client SDKs

| Language | Package | Source |
|---|---|---|
| Python | `mindd` | [`sdk/python`](https://github.com/vibed-project/mindD/tree/main/sdk/python) |
| TypeScript | `@mindd/client` | [`sdk/typescript`](https://github.com/vibed-project/mindD/tree/main/sdk/typescript) |

The SDKs version and release independently of the server, from their own tag
prefixes (`python-sdk-v*`, `typescript-sdk-v*`). See
[Python SDK](./clients/python.md) and
[TypeScript SDK](./clients/typescript.md).

## Verify the install

```bash
mindd --version      # mindd v0.1.0 (<commit>, <date>)
mindctl version      # mindctl v0.1.0 (<commit>, <date>)
```

`mindd` takes a single required flag, `--config`. There is no default config
path in the binary; the container image supplies one through its `CMD`.

## Compatibility

mindD is pre-1.0. Proto shapes under `proto/mindd/*/v1` may still change
between minor versions. `CHANGELOG.md` records what moved, including a
**Known limitations** section for each release; the ones that affect how you
should deploy are reproduced on the [Security](./security.md) page.
