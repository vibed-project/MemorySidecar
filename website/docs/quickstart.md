---
slug: /quickstart
sidebar_position: 2
title: Quickstart
---

# Quickstart

From nothing to a live sidecar handling KV, Episodic, Semantic, Artifact,
Lease, and Graph — over gRPC and HTTP — in a couple of minutes. Two paths:
**Docker Compose** (nothing to install) or a **local Go build**.

## Fastest: Docker Compose

Brings up memsidecar backed by Postgres (pgvector) and MinIO (S3), with helpers
to mint a token and drive the data plane. No Go/buf toolchain needed.

```bash
git clone https://github.com/m-koerbaecher/memsidecar.git
cd memsidecar
docker compose up --build -d           # gRPC :7777, HTTP :8080, metrics :9090

# Mint a dev capability token (‑T keeps the output clean).
export MEMSIDECAR_TOKEN=$(docker compose run --rm -T token)

# Talk to it — no grpcurl, just memctl:
docker compose run --rm -T memctl kv put scratchpad hello world --token "$MEMSIDECAR_TOKEN"
docker compose run --rm -T memctl kv get scratchpad hello       --token "$MEMSIDECAR_TOKEN"
docker compose run --rm -T memctl ns ls                         --token "$MEMSIDECAR_TOKEN"
```

`docker compose down -v` tears it all down. The `token` service uses the public
dev keypair baked into `configs/compose.yaml` — **local use only**.

## Or: local build

### Prerequisites

- Go 1.26+
- [buf](https://buf.build/docs/installation) (only to regenerate stubs; the
  checked-in `gen/` already has them)

```bash
brew install bufbuild/buf/buf
```

### Build & run

```bash
git clone https://github.com/m-koerbaecher/memsidecar.git
cd memsidecar
make build                              # produces bin/memsidecar and bin/memctl
./bin/memsidecar --config configs/example.yaml
```

You'll see structured JSON logs reporting each listener:

```json
{"msg":"listening","transport":"tcp","addr":"127.0.0.1:7777","security":"plaintext"}
{"msg":"listening","transport":"http","purpose":"gateway","addr":"127.0.0.1:8080"}
{"msg":"memsidecar ready"}
```

### Mint a capability token

The dev keypair shipped in `configs/example.yaml` is for local use only.

```bash
export MEMSIDECAR_PASETO_SECRET_HEX=38fb82e74985d41969ce39904d7cbe01dd31ea0b573dc8fc35c5689b8212ccc961a2d0067233cf8d6570c76f37573cbc31d33032ab256fe0c8032c0987d0fbf9

export MEMSIDECAR_TOKEN=$(./bin/memctl token issue \
  --tenant acme --agent agent-1 \
  --ns 'kv/*,episodic/events,semantic/notes,artifact/blobs,lease/locks,graph/knowledge' \
  --ops '*' --ttl 1h)
```

For real deployments, generate a fresh keypair with
`./bin/memctl token gen-keypair` and put the **public** half in your config.

## Talk to it with `memctl`

`memctl` speaks the data plane too, using the token you just issued — it reads
`$MEMSIDECAR_TOKEN` and `$MEMSIDECAR_ADDR` (default `127.0.0.1:7777`) by default:

```bash
memctl kv put scratchpad hello world --ttl 5m
memctl kv get scratchpad hello
memctl semantic search notes "capital of France" --top-k 3
memctl graph neighbors knowledge paris
memctl episodic tail events --historical      # live stream; Ctrl-C to stop
memctl ns ls                                  # namespaces + live item counts
```

### HTTP/JSON gateway

Every unary RPC is also reachable over HTTP — same service, different transport:

```bash
curl -sS -X POST http://127.0.0.1:8080/memsidecar.kv.v1.KV/Put \
  -H "x-memsidecar-capability: Bearer $MEMSIDECAR_TOKEN" \
  -H "Content-Type: application/json" \
  -d '{"namespace":"scratchpad","key":"hello","value":"d29ybGQ="}'
```

### Python

```python
import datetime as dt
from memsidecar import MemSidecar

with MemSidecar("127.0.0.1:7777", token=TOKEN) as m:
    m.kv.put("scratchpad", "hello", b"world", ttl=dt.timedelta(minutes=5))
    print(m.kv.get("scratchpad", "hello").value)  # b"world"
```

Install the SDK with `pip install ./sdk/python`, then take the whole tour:

```bash
python examples/agent_tour.py     # exercises every block as one agent turn
```

See [`examples/`](https://github.com/m-koerbaecher/memsidecar/tree/main/examples).

## What just happened

Every request you fired went through the same five-stage interceptor chain:

```
recovery → observability → auth → policy → service
```

`auth` decoded the PASETO token and attached the capability to the request
context. `policy` consulted the YAML rules in your config (default allow).
`service` dispatched to the per-block driver.

## Next steps

- [Architecture](./concepts/architecture.md) — what those stages do and why.
- [Capabilities](./concepts/capabilities.md) — how tokens scope access.
- [Building blocks](./blocks/kv.md) — per-service reference and full API shapes.
- [Helm](./deploy/helm.md) — install in Kubernetes.
