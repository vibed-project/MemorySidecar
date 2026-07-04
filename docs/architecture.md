# Architecture

memsidecar is a co-located process exposing a small, framework-agnostic API
over pluggable backends for agent memory. The target architecture is described
in [ADR-0001](decisions/adr-0001-memory-sidecar.md).

This document describes the **walking skeleton**, which now covers all six
building blocks (`kv`, `episodic`, `semantic`, `artifact`, `lease`, `graph`)
over pluggable backends. Every block follows the same internal shape, so the
seams described here are the same for all of them.

## Request flow

```
┌──────────┐ gRPC ┌───────────────────────────────────────────────────────────┐
│  Agent   │─────▶│ recovery → observability → auth → policy → block service  │
│ (client) │      │            (slog+OTel)    (token  (NoopEngine or          │
└──────────┘      │                            check) YAML RuleEngine)         │
                  │                                              │              │
                  │                                              ▼              │
                  │                              ┌──────────────────────────┐  │
                  │                              │ <block>.Registry → Driver │  │
                  │                              │ (memory │ postgres │ …)   │  │
                  │                              └──────────────────────────┘  │
                  └───────────────────────────────────────────────────────────┘
```

The interceptor chain order is intentional:

1. **Recovery** wraps everything so panics become `Internal` errors.
2. **Observability** records latency + a span on the wire boundary, plus a
   memory-aware `memsidecar.op.duration` (split by write/query op-class) and
   per-block backend-latency / result-shape metrics.
3. **Auth** verifies the bearer token (`x-memsidecar-capability` metadata) and
   attaches a `*auth.Capability` to the request context.
4. **Policy** evaluates the configured `policy.Engine`: `NoopEngine` (default,
   allow-all) when no `policy` section is present, otherwise the YAML-driven
   `RuleEngine` (allow / deny / rate_limit / cap).
5. **Service** dispatches to the building-block implementation.

## Package layout

| Package | Role |
|---|---|
| `proto/`, `gen/` | gRPC service contracts (checked-in generated code) |
| `internal/auth` | `TokenVerifier` interface, PASETO and JWT impls, `Capability` scoping |
| `internal/config` | YAML + env loader (koanf) with validation |
| `internal/interceptor` | gRPC interceptors |
| `internal/policy` | Policy engine: `NoopEngine` + YAML-driven `RuleEngine` (allow/deny/rate-limit/cap) |
| `internal/kv` | KV service + driver interface + registry |
| `internal/kv/drivers/memory` | In-memory KV driver (lazy expiry + sweeper) |
| `internal/kv/drivers/postgres` | Postgres KV driver via pgx/v5 + embedded migrations |
| `internal/episodic` | Episodic service + driver interface + registry |
| `internal/episodic/drivers/memory` | In-memory episodic driver with Tail fan-out |
| `internal/episodic/drivers/postgres` | Postgres episodic driver (polling Tail) |
| `internal/semantic` | Semantic service + driver interface + registry; bitemporal lifecycle (validity, soft-delete, supersedes/source, as-of reads, bulk `Expire`, `if_version` CAS) |
| `internal/semantic/embedder` | `Embedder` interface + deterministic `Fake` impl |
| `internal/semantic/embedder/ollama` | Adapter for Ollama `/api/embed` (local, zero API key) |
| `internal/semantic/embedder/openai` | Adapter for OpenAI-compatible `/v1/embeddings` |
| `internal/semantic/drivers/memory` | In-memory brute-force cosine driver |
| `internal/semantic/drivers/postgres` | pgvector driver (table-per-namespace, HNSW index) |
| `internal/artifact` | Artifact service + driver interface + registry |
| `internal/artifact/drivers/memory` | In-memory blob driver |
| `internal/artifact/drivers/fs` | Local-filesystem driver with shard layout + atomic writes |
| `internal/artifact/drivers/s3` | S3/MinIO driver via minio-go |
| `internal/lease` | Lease service + driver interface + registry |
| `internal/lease/drivers/memory` | In-memory lease driver (poll-on-wait, cond on Release) |
| `internal/lease/drivers/postgres` | Postgres lease driver (INSERT…ON CONFLICT…WHERE expired) |
| `internal/graph` | Graph service + driver interface + registry; bounded `Neighbors`/`Traverse` with server-side depth/fan-out/node/edge hard caps |
| `internal/graph/drivers/memory` | In-memory graph driver (adjacency maps, BFS traversal) |
| `internal/obs` | OTel tracing (stdout / OTLP) + Prometheus metrics + slog bootstrap |
| `internal/server` | Listener lifecycle, interceptor wiring, gRPC server |
| `cmd/memsidecar` | Server binary (composition root) |
| `cmd/memctl` | Admin CLI — currently only `token issue` and `token gen-keypair` |

## Adding a new building block

All six blocks exist today; the most recent, `graph`, is a fully worked
example of this recipe (see [ADR-0002 §11](decisions/adr-0002-graph-building-block.md)).
To add the next one — call it `<block>`:

1. Add `proto/memsidecar/<block>/v1/<block>.proto`, run `make proto`.
2. Mirror `internal/kv/` under `internal/<block>/`: `driver.go`, `registry.go`,
   `service.go`, `drivers/<name>/...`, `<block>test/conformance.go`.
3. Add the new ops to `internal/auth/capability.go` (`Op<Block>*` constants).
4. Wire the service in `internal/server/server.go` and the registry
   (`build<Block>Registry`) in `cmd/memsidecar/main.go`.
5. Map the gRPC methods to ops in `internal/interceptor/policy.go`
   (`methodToOp`), marking writes so the op-class metric split is correct.
6. Add a `block: <block>` clause to `internal/config/config.go` validation.

## Adding a new backend

Pick the building block, implement its `Driver` interface, place it under
`internal/<block>/drivers/<name>/`, and add a case to the switch in
`cmd/memsidecar/main.go:buildKVRegistry` (or its episodic equivalent).

## Transport security

The gRPC TCP listener supports TLS and mTLS via `server.grpc.tls`:

| Setting | Behaviour |
|---|---|
| `cert_file` + `key_file` | TLS-only. Server presents the cert; client validates it. |
| `cert_file` + `key_file` + `client_ca_file` + `require_client_cert: true` | mTLS. Client must present a cert chained to `client_ca_file`. |
| `cert_file` + `key_file` + `client_ca_file` (require false) | "If a client presents a cert, validate it; otherwise allow." |

UDS connections are always plaintext — filesystem permissions are the authz
boundary, and TLS on a unix socket is just overhead.

`NextProtos: ["h2"]` is set on the server `tls.Config` to satisfy grpc-go's
ALPN requirement (grpc/grpc-go#434).

Capability tokens are checked after the TLS handshake, so mTLS authenticates
the *peer* while the token scopes *what they can do*.

## Key rotation

Both PASETO and JWT (RS256) verifiers accept a list of trusted public keys:

```yaml
auth:
  verifier: paseto
  paseto:
    public_key_hexes:
      - "<new>"
      - "<old>"
```

Tokens minted by any of the listed keys verify. The standard rotation flow:

1. Add the new key alongside the old one. `SIGHUP` to reload — both work.
2. Switch the issuer to mint with the new private key.
3. Wait for the longest token TTL.
4. Remove the old key. `SIGHUP` again.

Combined with the hot-reload slice, this rotation never drops in-flight
RPCs and never invalidates a not-yet-expired token in production.

## Hot reload

`SIGHUP` re-reads the YAML config and atomically swaps:

- the **auth verifier** (e.g. rotate PASETO public key without restart)
- the **policy engine** (live deny/rate-limit changes)
- the **log level**

Everything else still requires a full restart:

- listener addresses (gRPC TCP/UDS, HTTP gateway, Prometheus)
- tracing & metrics exporters
- driver registries (adding a backend, swapping a Postgres DSN, etc.)

The swap is implemented via `atomic.Pointer` holders (`policy.Holder`,
`auth.VerifierHolder`) and an `slog.LevelVar` — interceptors see the new
configuration on the next request after the SIGHUP fires.

## What's out of scope today

Since the earliest slice, mTLS, the Helm chart, the container image, real
embedders (Ollama/OpenAI), the S3 driver, the YAML policy engine, the semantic
lifecycle primitives (ADR-0003), and the `graph` block (ADR-0002) have all
landed. Highlights still pending:

- A production `graph` backend driver (only the in-memory reference driver
  ships today — ADR-0002 §6).
- Bidi-streaming RPCs; additional language SDKs beyond Python (e.g. TypeScript).
- Multi-tenant DB hardening.
- Hot-reload of backends/listeners.
- Real release/versioning tooling (proto shapes may still change).
