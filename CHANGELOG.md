# Changelog

All notable changes to this project are documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/)
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).
mindD is pre-1.0: proto shapes under `proto/mindd/*/v1` may still change between
minor versions.

## [Unreleased]

### Fixed

- Neither SDK shipped its license text. Both declare Apache-2.0, whose §4(a)
  requires the license to travel with each distribution and §4(d) the NOTICE;
  the built wheel, sdist and npm tarball contained neither. Both packages now
  carry `LICENSE` and `NOTICE`.
- The Python SDK declared the `Typing :: Typed` classifier but shipped no
  PEP 561 marker, so every consumer's type checker silently ignored its
  annotations:

  ```
  error: Skipping analyzing "mindd.kv.v1": module is installed,
         but missing library stubs or py.typed marker  [import-untyped]
  ```

  `src/mindd/py.typed` is now included in the wheel.

### Security

- **Namespace-scoped policy rules now apply to streaming RPCs.** `PolicyStream`
  built its hook with an empty `Namespace`, because the namespace arrives in
  the stream's first message rather than in the interceptor's arguments. A rule
  such as `deny namespace: ["secret-*"]` — the shape used in
  `configs/example.yaml` — therefore never matched `KV/Scan`,
  `Episodic/Range`, `Episodic/Tail`, `Artifact/Put`, `Artifact/Get` or
  `Artifact/List`, and those calls fell through to the default effect. `cap`
  rules were inert on the same methods for the same reason.

  The decision is now deferred to the first `RecvMsg`, where the request is
  available and the hook is populated exactly as the unary path does. It is
  evaluated once per RPC, not once per message: a `rate_limit` rule consumes a
  bucket token per evaluation, so a per-message check would charge a streaming
  call many times over. A handler that reports success without ever receiving
  a message now fails closed rather than reading as an allow.

  Note this only affected the *policy engine*. The capability token's own
  namespace pattern was, and is, enforced on every RPC inside each service.

## [0.1.0] - 2026-08-23

First tagged release. All six memory blocks work over their documented
backends, with a gRPC API, an HTTP/JSON mirror, PASETO/JWT capability tokens,
a policy engine, OpenTelemetry tracing and Prometheus metrics.

### Blocks and drivers

| Block | Purpose | Drivers |
|---|---|---|
| `kv` | Typed, TTL'd key-value | memory, postgres |
| `episodic` | Append-only event log with Range + Tail | memory, postgres |
| `semantic` | Embed-and-search; bitemporal and revisable | memory, postgres (pgvector) |
| `artifact` | Blob storage with metadata | memory, fs, s3/minio |
| `lease` | Distributed locks with TTL | memory, postgres |
| `graph` | Typed nodes/edges with bounded traversal | memory, postgres |

### Security

- **Artifact ids are validated on every path.** The `fs` driver applied its id
  regex only in `Put` and `List`, so `Stat`, `Open`, `Delete` and `PatchMeta`
  built filesystem paths straight from the caller's id: a capability scoped to
  one namespace could read and delete files anywhere the process could reach.
  Validation now lives in `internal/artifact/validate.go` and is applied in the
  `fs` driver's `paths()`, in the `s3` driver's key construction, and at the
  RPC boundary. `.` and `..` are rejected explicitly — they match the id
  character class and are exactly what `filepath.Join` treats as traversal.
- **The Helm chart no longer ships a default signing key.** It previously
  defaulted `config.auth.paseto.public_key_hex` to this repository's
  development key, whose private half is published alongside it, together with
  `policy.default: allow` — so a default `helm install` accepted tokens anyone
  who had read the repo could mint, for any tenant. The chart now fails at
  template time if the key is unset, and also if it is set to that known
  keypair.
- **Tenant isolation works on the `fs` artifact driver.** The qualified
  `<tenant>\x1f<namespace>` form was rejected by the old namespace regex, so
  `Put` and `List` failed for every tenant-isolated deployment while the
  unvalidated read paths traversed freely.

### Fixed

- `kv` on postgres lost version increments under concurrent create.
  `SELECT ... FOR UPDATE` takes no lock when the row does not exist, so
  concurrent first writers all computed version 1 and the `ON CONFLICT` branch
  wrote that stale value instead of incrementing. Measured 4979 and 4991 of
  5000 across two runs of the block's own conformance case, which had never
  been executed by CI.
- `internal/version` falls back to `debug.ReadBuildInfo()`, so a binary from
  `go install ...@v0.1.0` reports the tagged version instead of `dev`.
- The Dockerfile gave BuildKit's `TARGETOS`/`TARGETARCH` defaults, which
  shadows the per-platform values it injects — every platform cross-compiled to
  amd64, so an arm64-labelled image contained x86-64 binaries.
- Python SDK dependency floors were below what the committed stubs require.
  `protobuf>=7.35` raised `VersionError` (gencode is 7.36) and `grpcio>=1.60`
  raised `TypeError` on stub construction (`_registered_method` landed in
  1.63). Both verified against real installs and raised.
- TypeScript SDK required `@bufbuild/protobuf ^2.2.0`, but the generated code
  imports `@bufbuild/protobuf/codegenv2`, whose exports entry first appears in
  2.5.0.

### Added

- **Tag-triggered server releases** (`.github/workflows/release.yml`). Pushing
  `v*` re-runs vet, unit, integration and lint against the tagged commit, then
  publishes a semver-tagged multi-arch image to GHCR alongside `:latest` and
  attaches cross-platform `mindd`/`mindctl` binaries plus `checksums.txt` to a
  GitHub Release. Previously a tag fired no workflow at all.
- **CI runs the integration suite.** Every production driver — postgres for
  kv/episodic/semantic/graph/lease and s3 for artifact — sits behind the
  `integration` build tag, so before this the only storage code CI had ever
  executed was the in-memory drivers.
- Container images carry `org.opencontainers.image.*` labels.
- CI builds and tests both SDKs. The Python job installs at the declared
  dependency floors and constructs a gRPC stub, which is what catches a stale
  floor or a regenerated-but-incompatible proto stub. (The SDK's own pytest
  suite is skipped without a live server, so it currently proves only that the
  suite imports.)
- This changelog, and `SECURITY.md`.

### Known limitations

- **The policy engine does not see the namespace on streaming RPCs.**
  `KV/Scan`, `Episodic/Range`, `Episodic/Tail`, `Artifact/Put`, `Artifact/Get`
  and `Artifact/List` reach the policy hook with an empty namespace, so a rule
  like `deny namespace: ["secret-*"]` does not match them and they fall through
  to the default effect. Namespace-scoped deny rules are only reliable on unary
  RPCs in this release. *(Fixed after v0.1.0 — see Unreleased.)*
- **Capability ops match on the bare verb.** A token minted with `--ops inspect`
  for the `lease` block also satisfies `admin.inspect`, which is not namespace
  scoped and exposes cross-namespace introspection. Mint tokens with
  fully-qualified ops (`kv.get`, not `get`) until this is tightened.
- **`tenant_isolation` defaults to false**, meaning every tenant shares one
  physical store and only the token's namespace pattern separates them.
- **The access log and traces carry no principal.** The observability
  interceptor runs above auth, so no request is attributed to a tenant, agent
  or token id.
- `artifact` on s3 does not persist `sha256` or `size` (the metadata patch hook
  is unimplemented for that driver), so `Stat`/`List` report an empty digest.
- Leases derive expiry from the application clock and compare it against the
  database clock; clock skew between the two can expire or extend a lease.
- Expired leases are never reclaimed from storage.
- The gRPC reflection service is registered unconditionally and is exempt from
  auth, so an exposed sidecar reveals its schema to unauthenticated callers.

[Unreleased]: https://github.com/vibed-project/mindD/compare/v0.1.0...HEAD
[0.1.0]: https://github.com/vibed-project/mindD/releases/tag/v0.1.0
