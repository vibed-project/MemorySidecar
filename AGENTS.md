# AGENTS.md

Guidance for AI agents working in this repository. Keep changes consistent with
the patterns below; they are load-bearing.

## What this is

**mindD** is a self-hosted, OSS, framework-agnostic **memory sidecar for
agentic systems**. It runs as a co-process to one or more agents and exposes a
small gRPC API (with an HTTP/JSON mirror) over **pluggable backends** for the
six kinds of memory agent stacks keep reinventing:

| Block | Purpose | Drivers implemented |
|---|---|---|
| `kv` | Typed, TTL'd key-value (tool-result cache, scratchpads) | memory, postgres |
| `episodic` | Append-only event/message/tool-call log with Range + Tail | memory, postgres |
| `semantic` | Embed-and-search over records; bitemporal & revisable (validity, soft-delete, supersedes/source, as-of reads, bulk Expire, `if_version` CAS) | memory (brute-force cosine), postgres (pgvector) |
| `artifact` | Blob storage with metadata | memory, fs, s3/minio |
| `lease` | Distributed locks with TTL | memory, postgres |
| `graph` | Typed nodes/edges with bounded, hard-capped traversal (Neighbors/Traverse) | memory |

Agents talk to the sidecar; the sidecar talks to the substrate. The design
follows Dapr's building-block model, narrowed to memory/state. Status is
**feature-complete against the roadmap, pre-release**: all six blocks work, but
nothing is released and proto shapes may still change.

Out of scope by design: this is **not** an agent framework, an inference
cache, a context-window compiler, or a vector DB. Don't add those.

## Layout

```
proto/         protobuf service definitions (buf)
gen/           generated Go gRPC code — CHECKED IN, never hand-edit
cmd/
  mindd/  the server (composition root: main.go wires everything)
  mindctl/      admin CLI: `token issue`, `token gen-keypair`
internal/
  auth/        TokenVerifier (PASETO v4.public default + JWT), Capability scoping
  config/      koanf YAML + env loader with validation
  interceptor/ gRPC chain: recovery → observability → auth → policy
  policy/      rule engine (allow/deny/rate_limit/cap) + atomic.Pointer Holder
  obs/         OTel tracing + Prometheus metrics + slog bootstrap
  server/      listener lifecycle, TLS/mTLS, HTTP gateway
  <block>/     per block: driver.go (interface), registry.go, service.go,
               drivers/<name>/, <block>test/conformance.go
sdk/python/    Python client (idiomatic per-block clients)
deploy/helm/   Helm chart
website/        Docusaurus docs site
configs/        example YAML config
```

## Build, test, lint

Use the Makefile. Key targets:

```bash
make build              # builds bin/mindd and bin/mindctl
make test               # go test ./...  (unit; no Docker needed)
make test-integration   # go test -tags=integration ./...  (needs Docker)
make lint               # golangci-lint run
make run-dev            # run server with configs/example.yaml
make proto              # regenerate Go stubs from proto/ (needs buf + plugins)
make proto-python       # regenerate Python stubs (buf remote plugins)
```

What CI enforces (mirror it locally before claiming done):
- `go vet ./...`
- `go test -race -count=1 ./...`
- `golangci-lint run` (enabled: errcheck, govet, ineffassign, staticcheck,
  unused, gosec). **No formatter is CI-enforced** — `gofumpt` is a
  dev-workflow convention only (run `gofumpt -w` on changed files); it was
  deliberately dropped from `.golangci.yml` because golangci's bundled build
  drifts from the standalone tool and made CI red on version skew, not real
  issues.
- `buf lint` **and** `buf format --diff --exit-code` for proto changes

Integration tests are gated behind the `//go:build integration` tag and use
testcontainers (Postgres, MinIO). They do **not** run in the default `make
test` or in CI's unit job — run `make test-integration` locally with Docker up
when you touch a postgres/s3 driver.

## Conventions that matter

- **Generated code (`gen/`) is checked in and never hand-edited.** Change
  `proto/*.proto`, then `make proto`. Proto lint waivers live in `buf.yaml`
  (e.g. service is `KV` not `KVService`, by project convention).
- **Every block follows the same shape.** `driver.go` defines a `Driver`
  interface (must be safe for concurrent use) plus `Record`/`*Options` structs
  and sentinel errors (`ErrNotFound`, …). `registry.go` maps namespace → driver
  (`Bind` / `BindShared`). `service.go` implements the generated gRPC server and
  gates every call through the capability (see below). Mirror an existing block
  rather than inventing a new structure.
- **Drivers prove themselves via the conformance suite.** Each block ships
  `internal/<block>/<block>test/conformance.go` with a `Harness` interface and
  `RunConformance(t, h)`. A new backend gets coverage by implementing `Harness`
  and calling `RunConformance` — add behavior there, not as ad-hoc per-driver
  tests.
- **Auth is enforced in the service layer, not just the interceptor.** Services
  pull the `*auth.Capability` from context and check
  `PermitsNamespace(block, ns)` and `PermitsOp(op)` before touching a driver.
  Keep that gate when adding RPCs.
- **gRPC errors use `codes`/`status`** — `InvalidArgument` for bad input,
  `PermissionDenied` for scope failures, `NotFound` for missing
  namespace/record. Don't return bare errors from RPC handlers.
- **Config is koanf-based YAML + env.** Secrets come from env via `*_env`
  option keys (e.g. `dsn_env`, `api_key_env`, `MINDD_PASETO_SECRET_HEX`) —
  never put secrets in YAML or commit them. The keypair in
  `configs/example.yaml` is dev-only.
- **Encryption at rest is a Driver decorator**, not a driver or service change.
  `internal/<block>/encrypted.go` wraps the block's `Driver` to seal payload
  bytes; everything the backend queries on stays plaintext. A decorator must
  forward the block's optional capability interfaces (`namespaceSizer.Size`,
  artifact's `metaPatcher`) or the type assertions in `registry.go` silently
  stop matching. Supported on `kv` and `episodic`; `config.encryptableBlocks`
  gates the rest.
- **Hot reload (SIGHUP)** swaps only the auth verifier, policy engine, and log
  level — via `atomic.Pointer` holders. Listeners, exporters, and driver
  registries require a full restart. Preserve this boundary; don't make a
  registry hot-swappable without revisiting `reloadConfig`.
- **Logging is `slog` with structured fields**; no `fmt.Println` in server
  paths (CLI/`main` error exit is fine).

## Adding a building block

Mirror `internal/kv/`, then wire it in:

1. Add `proto/mindd/<block>/v1/<block>.proto`, run `make proto`.
2. Create `internal/<block>/`: `driver.go`, `registry.go`, `service.go`,
   `drivers/<name>/`, `<block>test/conformance.go`.
3. Add op constants to `internal/auth/capability.go` (`Op<Block>*`).
4. Map each gRPC method → op in `internal/interceptor/policy.go` (`methodToOp`).
5. Wire the service in `internal/server/server.go` and a
   `build<Block>Registry` in `cmd/mindd/main.go`.
6. Extend `internal/config/config.go` validation for `block: <block>`.

## Adding a backend driver

Implement the block's `Driver` interface under
`internal/<block>/drivers/<name>/`, pass the conformance suite, and add a
`case` to the driver switch in the relevant `build*Registry` in
`cmd/mindd/main.go`. Postgres/S3 drivers read connection details from
`config.BackendConfig.Options` via the `stringOpt`/`intOpt`/`durationOpt`
helpers in `main.go`.

## Docs

User-facing docs are the Docusaurus site under `website/` (`make docs-dev` →
http://localhost:3000). Architecture notes are in `docs/architecture.md`. When
a change alters behavior an operator would see (config keys, ports, security
posture), update the relevant `website/docs/` page too.
