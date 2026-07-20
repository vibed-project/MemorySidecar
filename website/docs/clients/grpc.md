---
title: Raw gRPC
sidebar_position: 3
---

# Raw gRPC

If the Python SDK doesn't fit your language or you want to skip the
wrapper, talk to memsidecar directly with the standard gRPC tooling.

## Reflection

Server reflection is registered alongside the six services and the
gRPC health protocol, so any gRPC client can discover the API without
the .proto files:

```bash
$ grpcurl -plaintext 127.0.0.1:7777 list
grpc.health.v1.Health
grpc.reflection.v1.ServerReflection
grpc.reflection.v1alpha.ServerReflection
memsidecar.admin.v1.Admin
memsidecar.artifact.v1.Artifact
memsidecar.episodic.v1.Episodic
memsidecar.graph.v1.Graph
memsidecar.kv.v1.KV
memsidecar.lease.v1.Lease
memsidecar.semantic.v1.Semantic
```

## Authentication header

Attach the capability token in the gRPC metadata key
`x-memsidecar-capability`:

```bash
grpcurl -plaintext \
  -H "x-memsidecar-capability: Bearer $TOKEN" \
  -d '...' 127.0.0.1:7777 memsidecar.kv.v1.KV/Get
```

In code:

| Language | Snippet |
|---|---|
| Go | `metadata.AppendToOutgoingContext(ctx, "x-memsidecar-capability", "Bearer "+tok)` |
| Python (raw) | `stub.Get(req, metadata=[("x-memsidecar-capability", f"Bearer {tok}")])` |
| TypeScript | `client.get(req, new grpc.Metadata({"x-memsidecar-capability": "Bearer "+tok}))` |
| Node `@grpc/grpc-js` | same as TypeScript |

The Python SDK installs a client-side interceptor so you don't have to
thread metadata through every call — see [Python](./python.md).

## Unix domain socket

The default config opens `/tmp/memsidecar.sock` alongside the TCP
listener. UDS is plaintext (filesystem perms are the boundary) and
strictly faster for same-pod traffic.

```bash
grpcurl -plaintext \
  -H "x-memsidecar-capability: Bearer $TOKEN" \
  -d '{"namespace":"scratchpad","key":"hello"}' \
  unix:///tmp/memsidecar.sock memsidecar.kv.v1.KV/Get
```

> **`grpcurl -unix` is broken on recent grpc-go** — use the `unix:///`
> URI form as above. The `-unix=true` flag triggers a "missing port in
> address" error.

## TLS

Once `server.grpc.tls` is configured:

```bash
grpcurl -cacert /path/to/server.crt -authority localhost \
  -H "x-memsidecar-capability: Bearer $TOKEN" \
  -d '...' 127.0.0.1:7777 memsidecar.kv.v1.KV/Get
```

For mTLS, also pass `-cert client.crt -key client.key`.

## Health check

```bash
grpcurl -plaintext 127.0.0.1:7777 grpc.health.v1.Health/Check
{"status":"SERVING"}
```

The chart's Kubernetes probes use the gRPC health protocol on this port.

## Generated stubs

The Go stubs ship in `gen/memsidecar/{kv,episodic,semantic,artifact,lease,graph}/v1/`.
The Python stubs ship under `sdk/python/src/memsidecar/`. Other languages:
regenerate from `proto/` with your favourite protoc plugin or a buf
remote plugin (`buf.build/protocolbuffers/<lang>` + `buf.build/grpc/<lang>`).
