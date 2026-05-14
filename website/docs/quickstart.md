---
slug: /quickstart
sidebar_position: 2
title: Quickstart
---

# Quickstart

Five minutes from clone to a live sidecar handling KV, Episodic, Semantic,
Artifact, and Lease over gRPC and HTTP.

## Prerequisites

- Go 1.26+
- [buf](https://buf.build/docs/installation), [grpcurl](https://github.com/fullstorydev/grpcurl)
- Optional: Docker for the integration test suite

```bash
brew install bufbuild/buf/buf grpcurl
go install google.golang.org/protobuf/cmd/protoc-gen-go@latest
go install google.golang.org/grpc/cmd/protoc-gen-go-grpc@latest
```

## Build

```bash
git clone https://github.com/m-koerbaecher/memsidecar.git
cd memsidecar
make proto     # regenerate protobuf stubs (idempotent)
make build     # produces bin/memsidecar and bin/memctl
```

## Run

The shipped example config has a dev keypair baked in and starts the gRPC
listener (TCP+UDS), the HTTP/JSON gateway, and the Prometheus `/metrics`
endpoint.

```bash
./bin/memsidecar --config configs/example.yaml
```

You'll see structured JSON logs reporting each listener:

```json
{"msg":"listening","transport":"http","purpose":"metrics","addr":":9090"}
{"msg":"listening","transport":"tcp","addr":"127.0.0.1:7777","security":"plaintext"}
{"msg":"listening","transport":"unix","addr":"/tmp/memsidecar.sock"}
{"msg":"listening","transport":"http","purpose":"gateway","addr":"127.0.0.1:8080"}
{"msg":"memsidecar ready"}
```

## Mint a capability token

The dev keypair shipped in `configs/example.yaml` is for local use only.

```bash
export MEMSIDECAR_PASETO_SECRET_HEX=38fb82e74985d41969ce39904d7cbe01dd31ea0b573dc8fc35c5689b8212ccc961a2d0067233cf8d6570c76f37573cbc31d33032ab256fe0c8032c0987d0fbf9

TOKEN=$(./bin/memctl token issue \
  --tenant acme --agent agent-1 \
  --ns 'kv/*,episodic/events,semantic/notes,artifact/blobs,lease/locks' \
  --ops '*' --ttl 1h)
```

For real deployments, generate a fresh keypair with
`./bin/memctl token gen-keypair` and put the **public** half in
`configs/example.yaml`.

## Talk to it

### KV (gRPC)

```bash
grpcurl -plaintext -H "x-memsidecar-capability: Bearer $TOKEN" \
  -d '{"namespace":"scratchpad","key":"hello","value":"d29ybGQ="}' \
  127.0.0.1:7777 memsidecar.kv.v1.KV/Put

grpcurl -plaintext -H "x-memsidecar-capability: Bearer $TOKEN" \
  -d '{"namespace":"scratchpad","key":"hello"}' \
  127.0.0.1:7777 memsidecar.kv.v1.KV/Get
```

### KV (HTTP/JSON gateway)

Same service, different transport:

```bash
curl -sS -X POST http://127.0.0.1:8080/memsidecar.kv.v1.KV/Put \
  -H "x-memsidecar-capability: Bearer $TOKEN" \
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

Install the SDK with `cd sdk/python && pip install -e .[dev]`.

## What just happened

Every request you fired went through the same five-stage interceptor chain:

```
recovery → observability → auth → policy → service
```

`auth` decoded the PASETO token and attached the capability to the request
context. `policy` consulted the YAML rules in `configs/example.yaml` (default
allow). `service` dispatched to the per-block driver — in this case the
in-memory KV driver shipped in the example config.

## Next steps

- [Architecture](./concepts/architecture.md) — what those stages do and why.
- [Capabilities](./concepts/capabilities.md) — how tokens scope access.
- [Building blocks](./blocks/kv.md) — per-service reference and full API
  shapes.
- [Helm](./deploy/helm.md) — install in Kubernetes.
