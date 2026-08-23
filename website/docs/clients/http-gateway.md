---
title: HTTP / JSON gateway
sidebar_position: 3
---

# HTTP / JSON gateway

`grpc-gateway` mirrors most of the gRPC services over HTTP/JSON so any HTTP
client (curl, the browser, language stacks without a gRPC story) can talk to
mindD. It runs on its own listener configured by `server.http.addr`.

## Which services are mirrored

| Service | On the gateway? |
|---|---|
| `mindd.kv.v1.KV` | yes |
| `mindd.episodic.v1.Episodic` | yes |
| `mindd.semantic.v1.Semantic` | yes |
| `mindd.artifact.v1.Artifact` | yes (except `Put`, which is client-streaming) |
| `mindd.lease.v1.Lease` | yes |
| `mindd.graph.v1.Graph` | **no** |
| `mindd.admin.v1.Admin` | **no** |

`graph` and `admin` are registered on the gRPC server but not on the gateway
mux, so their methods return 404 over HTTP. Use gRPC or an SDK for those.

## URL scheme

No per-RPC annotations are used. Every mirrored method is reachable at:

```
POST /<package>.<service>/<method>
```

with a JSON body matching the request proto. This matches gRPC's own URL
scheme exactly.

## Capability headers

The gateway forwards two incoming HTTP headers to the inner gRPC call as
metadata:

- `x-mindd-capability`, the bearer token (required).
- `Authorization`, forwarded but currently unused; reserved for future use.

Missing or invalid tokens come back as HTTP 401 with the gRPC `code: 16` in the
body:

```json
{"code":16,"message":"missing x-mindd-capability header"}
```

## Examples

### KV

```bash
curl -sS -X POST http://127.0.0.1:8080/mindd.kv.v1.KV/Put \
  -H "x-mindd-capability: Bearer $TOKEN" \
  -H "Content-Type: application/json" \
  -d '{"namespace":"scratchpad","key":"hello","value":"d29ybGQ="}'

curl -sS -X POST http://127.0.0.1:8080/mindd.kv.v1.KV/Get \
  -H "x-mindd-capability: Bearer $TOKEN" \
  -d '{"namespace":"scratchpad","key":"hello"}'
```

### Episodic server-stream

`Range` and `Tail` are server-streaming. The gateway emits newline-delimited
JSON envelopes, one per event:

```bash
curl -sS -N -X POST http://127.0.0.1:8080/mindd.episodic.v1.Episodic/Range \
  -H "x-mindd-capability: Bearer $TOKEN" \
  -d '{"namespace":"events"}'
```

```
{"result":{"id":"...","cursor":"1","type":"tool_call",...}}
{"result":{"id":"...","cursor":"2",...}}
```

### Semantic

```bash
curl -sS -X POST http://127.0.0.1:8080/mindd.semantic.v1.Semantic/Search \
  -H "x-mindd-capability: Bearer $TOKEN" \
  -d '{"namespace":"notes","queryText":"apple","topK":2}'
```

JSON-over-HTTP follows protobuf's standard camelCase mapping (`top_k` becomes
`topK`).

## What doesn't work

- **The `graph` and `admin` services.** Not registered on the gateway.
- **Client-streaming RPCs.** `Artifact/Put` is client-streaming and there's no
  clean HTTP mapping without annotations or WebSockets. Use the gRPC transport
  for uploads.
- **Bi-directional streaming.** Same reason; no bidi RPCs ship today anyway.

## Configuration

```yaml
server:
  http:
    addr: "0.0.0.0:8080"   # empty disables the gateway
```

The gateway dials the local gRPC listener under the hood, preferring
`server.grpc.tcp` and falling back to `server.grpc.uds` (a filesystem path is
rewritten to a `unix://` target). Setting `server.http.addr` with no gRPC
listener configured is a startup error.

:::warning The gateway has no TLS
The internal dial to the gRPC listener uses insecure credentials, so enabling
`server.grpc.tls` while relying on the loopback gateway will break the
gateway's own dial. The gateway listener itself also has no TLS option in this
release. Terminate TLS at an ingress and keep the gateway on loopback or a
private network. See [Security](../security.md) and
[TLS and mTLS](../ops/tls.md).
:::
