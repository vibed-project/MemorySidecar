---
title: Raw gRPC
sidebar_position: 3
---

# Raw gRPC

If the Python SDK doesn't fit your language or you want to skip the
wrapper, talk to mindD directly with the standard gRPC tooling.

## Reflection

Server reflection is registered alongside the six services and the
gRPC health protocol, so any gRPC client can discover the API without
the .proto files:

```bash
$ grpcurl -plaintext 127.0.0.1:7777 list
grpc.health.v1.Health
grpc.reflection.v1.ServerReflection
grpc.reflection.v1alpha.ServerReflection
mindd.admin.v1.Admin
mindd.artifact.v1.Artifact
mindd.episodic.v1.Episodic
mindd.graph.v1.Graph
mindd.kv.v1.KV
mindd.lease.v1.Lease
mindd.semantic.v1.Semantic
```

## Authentication header

Attach the capability token in the gRPC metadata key
`x-mindd-capability`:

```bash
grpcurl -plaintext \
  -H "x-mindd-capability: Bearer $TOKEN" \
  -d '...' 127.0.0.1:7777 mindd.kv.v1.KV/Get
```

In code:

| Language | Snippet |
|---|---|
| Go | `metadata.AppendToOutgoingContext(ctx, "x-mindd-capability", "Bearer "+tok)` |
| Python (raw) | `stub.Get(req, metadata=[("x-mindd-capability", f"Bearer {tok}")])` |
| TypeScript | `client.get(req, new grpc.Metadata({"x-mindd-capability": "Bearer "+tok}))` |
| Node `@grpc/grpc-js` | same as TypeScript |

The Python SDK installs a client-side interceptor so you don't have to
thread metadata through every call — see [Python](./python.md).

## Unix domain socket

The default config opens `/tmp/mindd.sock` alongside the TCP
listener. UDS is plaintext (filesystem perms are the boundary) and
strictly faster for same-pod traffic.

```bash
grpcurl -plaintext \
  -H "x-mindd-capability: Bearer $TOKEN" \
  -d '{"namespace":"scratchpad","key":"hello"}' \
  unix:///tmp/mindd.sock mindd.kv.v1.KV/Get
```

> **`grpcurl -unix` is broken on recent grpc-go** — use the `unix:///`
> URI form as above. The `-unix=true` flag triggers a "missing port in
> address" error.

## TLS

Once `server.grpc.tls` is configured:

```bash
grpcurl -cacert /path/to/server.crt -authority localhost \
  -H "x-mindd-capability: Bearer $TOKEN" \
  -d '...' 127.0.0.1:7777 mindd.kv.v1.KV/Get
```

For mTLS, also pass `-cert client.crt -key client.key`.

## Health check

```bash
grpcurl -plaintext 127.0.0.1:7777 grpc.health.v1.Health/Check
{"status":"SERVING"}
```

The chart's Kubernetes probes use the gRPC health protocol on this port.

## Generated stubs

The Go stubs ship in `gen/mindd/{kv,episodic,semantic,artifact,lease,graph}/v1/`.
The Python stubs ship under `sdk/python/src/mindd/`. Other languages:
regenerate from `proto/` with your favourite protoc plugin or a buf
remote plugin (`buf.build/protocolbuffers/<lang>` + `buf.build/grpc/<lang>`).
