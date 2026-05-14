---
title: HTTP / JSON gateway
sidebar_position: 2
---

# HTTP / JSON gateway

`grpc-gateway` mirrors the gRPC services over HTTP/JSON so any HTTP client
(curl, the browser, language stacks without a gRPC story) can talk to
memsidecar. It runs on its own listener configured by
`server.http.addr`.

## URL scheme

No per-RPC annotations are used. Every method is reachable at:

```
POST /<package>.<service>/<method>
```

with a JSON body matching the request proto. Matches gRPC's URL scheme
exactly.

## Capability headers

The gateway forwards two incoming HTTP headers to the inner gRPC call as
metadata:

- `x-memsidecar-capability` — the bearer token (required).
- `Authorization` — forwarded but currently unused; reserved for future
  use.

Missing or invalid tokens come back as HTTP 401 with the gRPC `code: 16`
in the body:

```json
{"code":16,"message":"missing x-memsidecar-capability header"}
```

## Examples

### KV

```bash
curl -sS -X POST http://127.0.0.1:8080/memsidecar.kv.v1.KV/Put \
  -H "x-memsidecar-capability: Bearer $TOKEN" \
  -H "Content-Type: application/json" \
  -d '{"namespace":"scratchpad","key":"hello","value":"d29ybGQ="}'

curl -sS -X POST http://127.0.0.1:8080/memsidecar.kv.v1.KV/Get \
  -H "x-memsidecar-capability: Bearer $TOKEN" \
  -d '{"namespace":"scratchpad","key":"hello"}'
```

### Episodic server-stream

`Range` and `Tail` are server-streaming. The gateway emits **newline-
delimited JSON envelopes**, one per event:

```bash
curl -sS -N -X POST http://127.0.0.1:8080/memsidecar.episodic.v1.Episodic/Range \
  -H "x-memsidecar-capability: Bearer $TOKEN" \
  -d '{"namespace":"events"}'
```

```
{"result":{"id":"...","cursor":"1","type":"tool_call",...}}
{"result":{"id":"...","cursor":"2",...}}
```

### Semantic

```bash
curl -sS -X POST http://127.0.0.1:8080/memsidecar.semantic.v1.Semantic/Search \
  -H "x-memsidecar-capability: Bearer $TOKEN" \
  -d '{"namespace":"notes","queryText":"apple","topK":2}'
```

Note: JSON-over-HTTP follows protobuf's standard camelCase mapping
(`top_k` → `topK`).

## What doesn't work

- **Client-streaming RPCs.** `Artifact.Put` is client-streaming and
  there's no clean HTTP mapping without annotations or WebSockets. Use
  the gRPC transport for uploads (or wrap it server-side into a
  POST-with-body endpoint as a future slice).
- **Bi-directional streaming.** Same reason; no client-stream RPCs ship
  today anyway.

## Configuration

```yaml
server:
  http:
    addr: "0.0.0.0:8080"   # empty disables the gateway
```

The gateway dials the local gRPC listener under the hood (TCP or UDS).
Don't enable the gateway without enabling at least one gRPC listener.
