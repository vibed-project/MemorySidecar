---
slug: /quickstart
sidebar_position: 3
title: Quickstart
---

# Quickstart

From nothing to a live sidecar handling KV, Episodic, Semantic, Artifact,
Lease, and Graph, over gRPC and HTTP, in a couple of minutes. Two paths:
**Docker Compose** (nothing to install) or a **local Go build**. If you would
rather grab a released binary or the published image, see
[Install](./install.md).

:::danger This page uses a publicly known keypair
The PASETO keypair below is a **development key whose private half is
committed to this repository**. It appears in `configs/example.yaml`,
`configs/compose.yaml`, `docker-compose.yml`, `README.md`, `examples/README.md`
and on this page.

Anyone who has read the repository can mint a valid token for **any tenant,
any namespace and any op** against a server that trusts it. Use it on your own
machine and nowhere else.

For anything past a laptop, generate your own:

```bash
mindctl token gen-keypair
```

Put the `public_key_hex` in your config and keep the `secret_key_hex` with
whatever issues your tokens. See [Security](./security.md).
:::

## Fastest: Docker Compose

Brings up mindD backed by Postgres (pgvector) and MinIO (S3), with helpers
to mint a token and drive the data plane. No Go or buf toolchain needed.

```bash
git clone https://github.com/vibed-project/mindD.git
cd mindD
docker compose up --build -d           # gRPC :7777, HTTP :8080, metrics :9090

# Mint a dev capability token (-T keeps the output clean).
export MINDD_TOKEN=$(docker compose run --rm -T token)

# Talk to it. No grpcurl needed, just mindctl:
docker compose run --rm -T mindctl kv put scratchpad hello world --token "$MINDD_TOKEN"
docker compose run --rm -T mindctl kv get scratchpad hello       --token "$MINDD_TOKEN"
docker compose run --rm -T mindctl ns ls                         --token "$MINDD_TOKEN"
```

`docker compose down -v` tears it all down.

The `token` helper signs with the dev private key set in `docker-compose.yml`
(the `x-paseto-secret` anchor); the server trusts its public half from
`configs/compose.yaml`. Local use only.

## Or: local build

### Prerequisites

- Go 1.26+
- [buf](https://buf.build/docs/installation), only if you want to regenerate
  the stubs; the checked-in `gen/` already has them.

```bash
brew install bufbuild/buf/buf
```

### Build and run

```bash
git clone https://github.com/vibed-project/mindD.git
cd mindD
make build                              # produces bin/mindd and bin/mindctl
./bin/mindd --config configs/example.yaml
```

`--config` is required; there is no default path in the binary. (The container
image supplies `/etc/mindd/config.yaml` through its `CMD`.)

You'll see structured JSON logs on stderr reporting each listener:

```json
{"msg":"listening","transport":"tcp","addr":"127.0.0.1:7777","security":"plaintext"}
{"msg":"listening","transport":"unix","addr":"/tmp/mindd.sock"}
{"msg":"mindd ready","version":"v0.1.0","commit":"..."}
```

### Mint a capability token

The dev keypair shipped in `configs/example.yaml` is for local use only. See
the warning at the top of this page.

```bash
export MINDD_PASETO_SECRET_HEX=38fb82e74985d41969ce39904d7cbe01dd31ea0b573dc8fc35c5689b8212ccc961a2d0067233cf8d6570c76f37573cbc31d33032ab256fe0c8032c0987d0fbf9

export MINDD_TOKEN=$(./bin/mindctl token issue \
  --tenant acme --agent agent-1 \
  --ns 'kv/*,episodic/events,semantic/notes,artifact/blobs,lease/locks,graph/knowledge' \
  --ops '*' --ttl 1h)
```

`--ops '*'` is a development shortcut. In real deployments, mint
fully-qualified ops (`kv.get`, `kv.put`), never bare verbs: a bare verb
matches across every block, so `--ops inspect` also grants `admin.inspect`,
which is not namespace scoped. See
[Capability tokens](./concepts/capabilities.md).

## Talk to it with `mindctl`

`mindctl` speaks part of the data plane too, using the token you just issued.
It reads `$MINDD_TOKEN` and `$MINDD_ADDR` (default `127.0.0.1:7777`), and takes
`--token`, `--addr` and `--tls` on every data-plane command:

```bash
mindctl kv put scratchpad hello world --ttl 5m
mindctl kv get scratchpad hello
mindctl semantic search notes "capital of France" --top-k 3
mindctl graph neighbors knowledge paris
mindctl episodic tail events --historical      # live stream; Ctrl-C to stop
mindctl ns ls                                  # namespaces + live item counts
```

That is the whole data-plane surface of the CLI in v0.1.0: `kv get`, `kv put`,
`semantic search`, `episodic tail`, `graph neighbors` and `ns ls`. Everything
else (artifact, lease, kv scan and delete, episodic append and range) goes
through the SDKs, raw gRPC, or the HTTP gateway.

### HTTP/JSON gateway

Unary RPCs on **kv, episodic, semantic, artifact and lease** are also reachable
over HTTP, same service, different transport:

```bash
curl -sS -X POST http://127.0.0.1:8080/mindd.kv.v1.KV/Put \
  -H "x-mindd-capability: Bearer $MINDD_TOKEN" \
  -H "Content-Type: application/json" \
  -d '{"namespace":"scratchpad","key":"hello","value":"d29ybGQ="}'
```

`graph` and `admin` are **not** registered on the gateway; use gRPC or an SDK
for those. See [HTTP / JSON gateway](./clients/http-gateway.md).

### Python

```python
import datetime as dt
from mindd import MindD

with MindD("127.0.0.1:7777", token=TOKEN) as m:
    m.kv.put("scratchpad", "hello", b"world", ttl=dt.timedelta(minutes=5))
    print(m.kv.get("scratchpad", "hello").value)  # b"world"
```

Install the SDK with `pip install ./sdk/python`, then take the whole tour:

```bash
python examples/agent_tour.py     # exercises every block as one agent turn
```

`agent_tour.py` reads `$MINDD_TOKEN` and `$MINDD_TARGET` (default
`127.0.0.1:7777`).

### TypeScript

```ts
import { MindD } from "@mindd/client";

const m = new MindD("127.0.0.1:7777", { token: process.env.MINDD_TOKEN! });
await m.kv.put("scratchpad", "hello", new TextEncoder().encode("world"));
const rec = await m.kv.get("scratchpad", "hello");
```

See [`examples/`](https://github.com/vibed-project/mindD/tree/main/examples),
[Python SDK](./clients/python.md) and [TypeScript SDK](./clients/typescript.md).

## What just happened

Every request you fired went through the same interceptor chain:

```
recovery -> observability -> auth -> policy -> service
```

`auth` decoded the PASETO token and attached the capability to the request
context. `policy` consulted the YAML rules in your config (`configs/example.yaml`
sets `default: allow` plus a deny, a rate limit and a cap rule). `service`
checked the capability again against the namespace and op, then dispatched to
the per-block driver.

## Next steps

- [Security](./security.md): the keypair warning, known limitations, and safe
  deployment.
- [Architecture](./concepts/architecture.md): what those stages do and why.
- [Capabilities](./concepts/capabilities.md): how tokens scope access.
- [Building blocks](./blocks/kv.md): per-service reference and full API shapes.
- [Helm](./deploy/helm.md): install in Kubernetes.
