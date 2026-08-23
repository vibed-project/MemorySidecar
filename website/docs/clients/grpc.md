---
title: Raw gRPC
sidebar_position: 4
---

# Raw gRPC

If neither SDK fits your language, or you want to skip the wrapper, talk to
mindD directly with the standard gRPC tooling.

## Reflection

Server reflection is registered alongside the seven services and the gRPC
health protocol, so any gRPC client can discover the API without the `.proto`
files:

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

(Which of the block services appear depends on which blocks your config
declares namespaces for. `KV` is always registered; the rest are conditional.)

:::warning Reflection is unauthenticated
Reflection is registered unconditionally and is exempt from the auth
interceptor, so anything that can open a TCP connection gets the full schema
without a token. There is no config switch to disable it in this release.

Bind the listener to loopback, a UDS, or a private network. See
[Security](../security.md).
:::

## Authentication header

Attach the capability token in the gRPC metadata key `x-mindd-capability`:

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
| TypeScript (raw Connect) | `capabilityInterceptor(token)` from `@mindd/client` |
| Node `@grpc/grpc-js` | `new grpc.Metadata({"x-mindd-capability": "Bearer "+tok})` |

Both SDKs install a client-side interceptor so you don't have to thread
metadata through every call. See [Python](./python.md) and
[TypeScript](./typescript.md).

## Unix domain socket

`configs/example.yaml` opens `/tmp/mindd.sock` alongside the TCP listener. UDS
is plaintext (filesystem permissions are the boundary; the socket is chmod
`0660` after bind) and strictly faster for same-pod traffic.

```bash
grpcurl -plaintext \
  -H "x-mindd-capability: Bearer $TOKEN" \
  -d '{"namespace":"scratchpad","key":"hello"}' \
  unix:///tmp/mindd.sock mindd.kv.v1.KV/Get
```

> **`grpcurl -unix` is broken on recent grpc-go.** Use the `unix:///` URI form
> as above; the `-unix=true` flag triggers a "missing port in address" error.

Note that the TCP listener still binds even when you configure a UDS: an empty
`server.grpc.tcp` defaults to `127.0.0.1:7777`. See the
[config reference](../config/reference.md#server).

## TLS

Once `server.grpc.tls` is configured with both `cert_file` and `key_file`:

```bash
grpcurl -cacert /path/to/server.crt -authority localhost \
  -H "x-mindd-capability: Bearer $TOKEN" \
  -d '...' 127.0.0.1:7777 mindd.kv.v1.KV/Get
```

For mTLS, also pass `-cert client.crt -key client.key`. The minimum version is
TLS 1.3.

Enabling TLS on the gRPC listener breaks the HTTP gateway, which dials it back
over loopback with insecure credentials. Run one or the other, or terminate
gateway traffic at an ingress. See
[HTTP / JSON gateway](./http-gateway.md).

## Health check

```bash
grpcurl -plaintext 127.0.0.1:7777 grpc.health.v1.Health/Check
{"status":"SERVING"}
```

Per-service statuses are also set (`mindd.kv.v1.KV`, and one per registered
block). The chart's Kubernetes probes use the gRPC health protocol on this
port, with readiness aimed at `mindd.kv.v1.KV`.

## Generated stubs

- Go: `gen/mindd/{kv,episodic,semantic,artifact,lease,graph,admin}/v1/`
- Python: `sdk/python/src/mindd/`
- TypeScript: `sdk/typescript/src/gen/` (protobuf-es + Connect)

Other languages: regenerate from `proto/` with your favourite protoc plugin or
a buf remote plugin (`buf.build/protocolbuffers/<lang>` plus
`buf.build/grpc/<lang>`).
