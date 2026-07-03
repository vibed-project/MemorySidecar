# Implementation plan — paper-driven optimizations

> **Companion to** [`agent-memory-paper-audit.md`](agent-memory-paper-audit.md). That
> document is the *why* (grounded gap analysis against arXiv:2606.24775); this is the
> *how* (sequenced, ticketed delivery).
> **Ground rules:** every ticket adds a substrate primitive, respects the ADR-0001/0002
> non-goals, follows the "Adding a building block" / per-block shape in
> [`../architecture.md`](../architecture.md) and `AGENTS.md`, and lands behind its
> conformance suite. Nothing here moves ranking/relevance judgment or extraction
> server-side.

## 0. Conventions every ticket obeys

Load-bearing patterns from `AGENTS.md` — each ticket's checklist assumes these:

- **Proto first, then `make proto`.** `gen/` is checked in and never hand-edited. Add fields at new tags; never renumber. Prefer additive/`oneof` changes so wire compat holds.
- **Every block keeps the same shape.** `driver.go` (interface + `Record`/`*Options` + sentinel errors), `registry.go`, `service.go`, `drivers/<name>/`, `<block>test/conformance.go`. Mirror an existing block; don't invent structure.
- **Behavior is proven in the conformance suite,** not ad-hoc per-driver tests. New behavior ⇒ new `RunConformance` case exercised by both the memory and Postgres harnesses.
- **Auth gate stays in the service layer.** Any new RPC pulls `*auth.Capability` from context and checks `PermitsNamespace(block, ns)` + `PermitsOp(op)` before touching a driver. New ops ⇒ constant in `internal/auth/capability.go` + `methodToOp` mapping in `internal/interceptor/policy.go` (writes marked as writes).
- **gRPC status codes:** `InvalidArgument` / `PermissionDenied` / `NotFound` / `FailedPrecondition` (CAS) / `ResourceExhausted` (cost caps). No bare errors from handlers.
- **Postgres schema changes ship a numbered migration** (`internal/<block>/drivers/postgres/migrations/000N_*.up.sql` + `.down.sql`); new columns are nullable/defaulted so existing rows read as live.
- **CI mirror before "done":** `go vet ./...`, `go test -race -count=1 ./...`, `golangci-lint run`, `gofumpt -w` on changed files, `buf lint` + `buf format` on proto. Postgres/pgvector work also runs `make test-integration` (testcontainers, Docker up).
- **Hot-reload boundary preserved:** don't make driver registries hot-swappable; only auth/policy/log-level reload via `atomic.Pointer`.

## 1. Delivery sequence (the recommended order)

Cheap, scope-clean, high-impact first; the graph block last because it depends on the
cost caps and yields most once a production driver exists. IDs reference the audit §4.

```
Phase 1  Lifecycle foundation ......  U1 → U2 → U3 → U4        (module U / F3, F9)
Phase 2  Cost & observability ......  O1 → O2 → O3 → O4 → O5   (RQ5 / F5)   ‖ parallelisable with Phase 1
Phase 3  Efficiency & retrieval ....  Q1 → Q2 → Q3 → Q4 → Q5 → U5 → R2 → R3
Phase 4  Graph block (strategic) ...  R1 (ADR-0002)            (module R / F1, F4)
```

**Fast first sprint** (closes both most-emphasized findings at lowest effort/risk):
`U1 → O1 → U3 → Q1 → O4`. Rationale: U1 kills the F3 stale-fact class; O1 makes the F5
cost split visible with near-zero risk; U3 gives localized bulk maintenance on top of U1;
Q1 is the cheapest high-ROI win; O4 is the guardrail that also pre-wires the graph block.

Dependency notes: **U2/U3(invalidate action) require U1** (validity columns). **Q4 requires
O4** (candidate-budget cap). **R1 traversal caps require O4.** Everything else is
independent and can be parallelised across contributors.

---

## 2. Phase 1 — Lifecycle foundation (module U)

> **Status (2026-07-03):** U1, U2, U3, U4 implemented on the `semantic` block behind the
> `semantictest` conformance suite (validity window, soft-delete visibility, resurrection,
> supersession, bulk expire, versioning/CAS) and recorded in
> [ADR-0003](../decisions/adr-0003-memory-lifecycle.md). Verified on the in-memory driver
> (`go test -race ./...`) **and on real pgvector** — all 11 conformance cases pass via
> `make test-integration` (Podman). The integration run also fixed a pre-existing latent
> bug in the pgvector driver (nil `payload` inserted as SQL NULL vs. a `NOT NULL` column).
> Lifecycle theme (U1–U4) complete.

Closes F3 ("hallucinations of the past") and enables F9 (conservative, localized
consolidation). Pure storage + read-filter + bounded write. The agent supplies all
timestamps and decides what supersedes what; the sidecar never infers.

### U1 — Bitemporal validity + soft-delete on `semantic` · **L · start here**

- **Proto** (`proto/memsidecar/semantic/v1/semantic.proto`): add to `Record` `google.protobuf.Timestamp valid_from`, `valid_to`, `deleted_at` (new tags). Add to `SearchRequest` `google.protobuf.Timestamp as_of` and `bool include_invalidated`. Add `bool hard_delete` to `DeleteRequest`.
- **Driver** (`internal/semantic/driver.go`): mirror fields on `Record`; add `AsOf time.Time` + `IncludeInvalidated bool` to `SearchOptions`; add `Hard bool` to `DeleteOptions`.
- **Postgres** (`drivers/postgres/postgres.go` + new migration `0002_validity.up.sql`): nullable `valid_from timestamptz default now()`, `valid_to timestamptz`, `deleted_at timestamptz`; partial index `... where deleted_at is null and (valid_to is null or valid_to > now())`. Default `Search` `WHERE` appends `valid_from <= COALESCE($as_of, now()) AND (valid_to IS NULL OR valid_to > COALESCE($as_of, now())) AND deleted_at IS NULL`, unless `include_invalidated`. `Delete` sets `deleted_at=now()` unless `hard_delete`.
- **Memory** (`drivers/memory/memory.go`): mirror the same predicate in the search path and delete path.
- **Conformance** (`semantictest/conformance.go`): invalidated row is excluded by default; visible with `include_invalidated`; `as_of` returns the state at that time; soft vs. hard delete.
- **Acceptance:** existing rows read as live (columns default to live); no wire break; `make test-integration` green.

### U2 — `supersedes` / `source` pointers · **M · needs U1**

- **Proto:** `repeated string supersedes` + `string source` on `semantic.Record` and `episodic.Event` (and `AppendRequest`).
- **Postgres:** `supersedes text[]`, `source text`. On `Upsert`, in the **same transaction**, set `valid_to=now()` on the ids named in `supersedes` (localized, id-targeted — touches only named rows, never a scan; satisfies F5).
- **Conformance:** upserting B that supersedes A invalidates A atomically; A still reachable via `include_invalidated`; provenance `source` round-trips.
- **Scope guard:** the sidecar does **no** entity resolution — the agent names the ids.

### U3 — Bulk lifecycle op `Expire` · **M · invalidate action needs U1**

- **Proto:** `rpc Expire(ExpireRequest) returns (ExpireResponse)`. Request = `namespace`, the same `map<string,string> filter` shape `Search` uses, `Action action` (`INVALIDATE|SOFT_DELETE|HARD_DELETE`), `uint32 max_rows`. Response = `uint64 affected`.
- **Driver:** add `Expire(ctx, ns, ExpireOptions)` to the interface (mirror across both drivers).
- **Postgres:** one bounded statement reusing `metadata @> $1::jsonb` (invalidate ⇒ `UPDATE ... SET valid_to=now()`; hard ⇒ `DELETE`), `LIMIT max_rows`. `max_rows` **required** (reject `0` with `InvalidArgument`) so it stays localized.
- **Auth/policy:** `OpSemanticExpire` (`semantic.expire`) constant + `methodToOp` marked **write**.
- **Conformance:** filter selects the right subset; `max_rows` bounds it; `affected` count correct.
- **Why it matters:** replaces the agent-side global read-all + per-id delete loop (today `Delete` is single-id) with one index scan — the F5 localized-maintenance pattern.

### U4 — Semantic CAS parity · **M**

- Port KV's pattern verbatim: `version bigint` column on the semantic table (increment per `Upsert`), optional `if_version` per record, `SELECT ... FOR UPDATE` compare-then-write, `semantic.ErrVersionMismatch` → `FailedPrecondition`. Apply the row-lock only when `if_version` is set so unguarded batch `Upsert` keeps its single-statement throughput.
- **Conformance:** mirror `internal/kv/kvtest/conformance.go`'s `testVersioning`.

---

## 3. Phase 2 — Cost & observability (RQ5 / F5)

Makes the paper's central cost story visible. Mostly measures what already happens; can
run in parallel with Phase 1.

### O1 — `op.duration` histogram split write/index vs query · **M · lowest-risk high-value**

- In `internal/interceptor/observability.go`, create a `Float64Histogram` `memsidecar.op.duration` (seconds) from the injected `MeterProvider`; record in `annotate()` with attrs `{block, op, op_class=write|query, namespace, code}`.
- Source `op_class` from the existing `write` flag in `methodToOp` (`internal/interceptor/policy.go`) — lift the map to a shared lookup rather than duplicating it. Thread the meter through `ObservabilityUnary`/`Stream` constructors (`internal/server/server.go`).
- **Acceptance:** zero behavior change; cardinality bounded (blocks × ops × namespaces × codes); the write-vs-query latency split is queryable in Prometheus.

### O2 — Backend latency + result size + `sidecar_overhead` · **M**

- Service layer (results already in hand): `memsidecar.backend.duration` histogram wrapping the `Driver.<Op>` call (`{block, op, namespace}`); `memsidecar.result.size` for `Search` `len(hits)` / streamed counts for `Scan`/`Range`/`Tail`. Record `sidecar_overhead = op.duration − backend.duration` explicitly (the number RQ5 is about), and `hits[0].score` as a cheap evidence-completion proxy (F2).
- Keep to `hits[0]` + counts, not full arrays, to bound per-request cost.

### O3 — Namespace growth gauge + eviction counters · **L**

- Add `Stats()`/`Size()` to each `Driver` interface (mirror across all blocks — interface-shape invariant). `ObservableGauge memsidecar.namespace.items` (memory drivers know `len`; Postgres uses `reltuples` estimate — **not** `count(*)`). **Skip a bytes gauge** — `pg_total_relation_size` is a catalog call, not cheap.
- `Int64Counter memsidecar.eviction.total{block, namespace, cause}` incremented at KV lazy-expiry + `sweepLoop` DELETE points; `cause=consolidation` reserved for later.

### O4 — Policy cost caps · **L · unblocks Q4 and R1**

- Add cost fields to `policy.HookCtx` (`TopK, Limit, Depth, FanOut uint32`); populate in `internal/interceptor/policy.go` by extracting request fields (extend the existing per-request extraction).
- Add an optional `max` block to the policy `Rule` (`internal/policy/rules.go`); enforce in `RuleEngine.evaluate` (`rule_engine.go`) — exceed ⇒ reject (recommended default) with a distinct decision reason mapped to **`ResourceExhausted`** in the interceptor.
- First cap to wire: `Semantic.Search.top_k` (today normalized from `0`→default but with **no maximum**). This directly implements ADR-0002 §8's demand that graph depth/fan-out be hard-capped via the policy path.
- Keep `NoopEngine` passing; update both policy interceptors for the new `HookCtx` shape.

### O5 — OTLP metrics export + bench tie-in · **S + M**

- `internal/obs/metrics.go`: add an `exporter=otlp` branch (PeriodicReader, reusing the OTLP endpoint/headers config tracing already has); prometheus stays default; preserve the no-op path.
- Per-block `Benchmark*` funcs driven by the existing `Harness` emitting distinct write-path vs. query-path timings — reproduces the paper's construction-vs-query numbers per backend. Advisory, not a CI gate. Optional short doc mapping the paper's released testbed workloads onto the harness.

---

## 4. Phase 3 — Efficiency & retrieval substrate (module Q)

Substrate halves only (see audit §5 for what's deliberately left agent-side).

### Q1 — Embedding cache · **S · cheapest high-ROI**

- Bounded LRU keyed on `(namespace, embedder_model, sha256(content))` in front of `Embedder.Embed`, on the `Upsert` and `query_text` `Search` paths (`internal/semantic/service.go`). Size-capped, opt-outable per namespace. Emit hit/miss counters (ties into O-series).
- **Acceptance:** identical content within a namespace/model embeds once; provider call count drops measurably in a repeat-content test.

### Q2 — Time-range pre-filter on `Search` · **M**

- `created_after`/`created_before` `Timestamp` on `SearchRequest`; thread to `SearchOptions`; `AND created_at > $n / < $n` in the pre-`ORDER BY` `WHERE` slot (index-friendly). Supporting btree index on `created_at`. Memory driver mirrors. **No decay ranking** — the agent re-ranks by the `created_at` already returned.

### Q3 — Structured metadata predicates · **M**

- New `FieldPredicate{ string key; Op op (EQ|NEQ|GT|GTE|LT|LTE|IN); repeated string values }`; add `repeated FieldPredicate predicates` to `SearchRequest` via a `oneof`/parallel field so the existing exact-match `filter` map keeps working (treated as `EQ`). Compile to jsonb path expressions in Postgres (`(metadata->>'k')::numeric > $n`, `= ANY($n)`, …) in the same pre-`ORDER BY` slot; guard numeric casts (return `InvalidArgument`, not 500, on bad cast). Memory driver evaluates in `matchesFilter`.

### Q4 — Opt-in RRF hybrid · **L · needs O4**

- GIN `tsvector` index on `content`; `SearchMode mode {DENSE(default), SPARSE, HYBRID}` + `uint32 rerank_candidate_k` on `SearchRequest`, capped by O4. `HYBRID` = dense ANN + `websearch_to_tsquery`/`ts_rank`, each to `rerank_candidate_k`, fused with Reciprocal Rank Fusion (`k=60`), return `top_k`. Deterministic rank-merge — no cross-encoder, no relevance model. `DENSE` unchanged by default. Expose the text-search config as namespace config (default `simple`/`english`). Memory driver: simple term-overlap sparse lane.

### Q5 — Seed-then-expand ergonomics · **S**

- `bool ids_only` on `SearchRequest` (return `id`+`score`, skip content/payload/vector marshaling) for a cheap seed set. Document the "semantic `id` == graph node `id`" convention in the proto comments (ADR-0002 §8 leans "convention only"). No traversal in the sidecar.

### U5 — Access instrumentation + KV read-through TTL · **L** (Phase-3 slot)

- Opt-in `last_accessed`/`access_count` on KV + semantic (best-effort/sampled touch to avoid hot-path write amplification); optional per-namespace capacity policy evicting by heat `access_count * exp(-decay * age)`. KV `Get` slides expiry when a per-namespace flag is set (cache-tier residency). Gate all of it behind an explicit flag so pure-read workloads pay nothing.

### R2 / R3 — small representation fixes · **S each**

- **R2:** optional first-class `role`/`session_id` on `episodic.Event` (+ `after_time`/`before_time` on `RangeRequest`, with a supporting `(namespace, timestamp)` index) so F7/F1 grouping isn't convention-only.
- **R3:** implement the real `ivfflat` fallback on HNSW-create failure (else structured warning, not `_ = err`); optionally surface HNSW `m`/`ef_construction`/`ef_search` as namespace config. Hygiene, not an F5 headline.

---

## 5. Phase 4 — Graph block (module R, ADR-0002)

The strategic bet — the paper's strongest family for cross-session aggregation and
scattered-evidence assembly. Execute ADR-0002's own delivery plan; do it **after** O4
(traversal caps) exists. Incremental per the ADR:

1. `proto/memsidecar/graph/v1/graph.proto` (`UpsertNodes`/`UpsertEdges`, `GetNode`, `Neighbors` 1-hop, `Traverse` depth/fan-out-capped) + `make proto`. **Paper refinement:** put `valid_from`/`valid_to` as first-class **edge** fields (not just props) so the graph carries F3 revisability natively.
2. `internal/graph/` mirroring `internal/semantic/` (`driver.go`, `registry.go`, `service.go`, `drivers/memory/`, `graphtest/conformance.go`) + in-memory reference driver.
3. `OpGraph*` constants + `methodToOp` (upserts/deletes = writes); wire `buildGraphRegistry` in `cmd/memsidecar/main.go` + `server.go`; `config.go` validation for `block: graph`.
4. **Traversal caps via O4** — `MaxDepth`/`MaxNodes` hard-capped server-side, surfaced as `ResourceExhausted`, integrated with the policy `rate_limit`/`max` buckets (ADR-0002 §8).
5. One production driver + Python SDK client + `website/docs/blocks/graph.md`.

Deferred per ADR-0002 §8: backend-native query pass-through and any hybrid-recall
convenience — orchestration stays agent-side.

## 6. Effort roll-up

| Phase | Tickets | Rough size |
|---|---|---|
| 1 — Lifecycle | U1(L), U2(M), U3(M), U4(M) | ~1 block-unit |
| 2 — Cost/obs | O1(M), O2(M), O3(L), O4(L), O5(S+M) | ~1 block-unit |
| 3 — Efficiency/retrieval | Q1(S), Q2(M), Q3(M), Q4(L), Q5(S), U5(L), R2(S), R3(S) | ~1.5 block-units |
| 4 — Graph | R1 (ADR-0002 full) | XL (own epic) |

Everything in Phases 1–3 is additive and wire-compatible. Track each ticket to green CI
(`go test -race`, `golangci-lint`, `gofumpt`, `buf lint`/`format`, plus
`make test-integration` for the Postgres/pgvector items) and a conformance case before
calling it done.
