# Audit — mindD against "Are We Ready For An Agent-Native Memory System?"

> **Type:** Analysis / design input (not an ADR — no decision is made here)
> **Source paper:** Zhou et al., *Are We Ready For An Agent-Native Memory System?*, arXiv:2606.24775v1
> **Scope:** the five shipped blocks (`kv`, `episodic`, `semantic`, `artifact`, `lease`) plus the proposed `graph` block (ADR-0002), auth/policy, and observability, as of `main`.
> **File/line references** point at `main` at time of writing; treat them as anchors, not guarantees.

## 1. The paper's lens

The paper studies agent memory as a **data-management system** and decomposes any such
system into a tuple `M_sys = ⟨R, S, Q, U⟩`:

| Module | Meaning | Taxonomy the paper uses |
|---|---|---|
| **R** — representation & storage | logical format + physical engine | token-level sequence (text / vector); graph & tree topology (temporal KG, hierarchy); heterogeneous composite. Physical: transient in-context register; specialized single-engine (vector / graph / relational / object); heterogeneous multi-engine. |
| **S** — extraction | raw traces → memory primitives | raw concatenation; schema-free semantic extraction; schema-constrained structured extraction. |
| **Q** — retrieval & routing | find relevant subset | native attention; semantic dense KNN; topological subgraph traversal; autonomous agentic routing (function-call / query-expansion); multi-stage hybrid execution (sequential filter→rank; parallel ensemble + RRF/MMR/cross-encoder). |
| **U** — maintenance | lifecycle after write | conflict resolution & versioning (timestamp multi-versioning, logical invalidation via `valid_from`/`valid_to`, provenance, dedup); capacity management (constraint hard eviction — FIFO/token/TTL; score-based priority eviction — heat = frequency × decay); semantic consolidation (inline compaction; tool-driven CRUD); continuous parametric optimization. |

### The nine findings, condensed

- **F1 Workload-aligned:** no architecture dominates; match structure to the workload bottleneck. Temporal/graph → cross-session aggregation & event-order; coarse-to-fine filtering → exact grounding in long coherent dialogue; trace-preserving → stateful execution / operation order.
- **F2 Evidence-centric:** retrieval is an *evidence-completion* problem, not top-1 ranking. Early localization vs. evidence assembly are separate targets. Explicit structure (links/hierarchy) wins for scattered/temporally-distant evidence; flat similarity only wins short-range. Recall degrades sharply with temporal distance.
- **F3 Temporal update fidelity:** build *revisability* into the representation — later facts must bind to the same entity/event, not append as undifferentiated text. Missing lifecycle management ⇒ stale facts = **"hallucinations of the past."**
- **F4 Horizon-structured:** as horizon grows, the challenge is choosing the right abstraction, not storing more. Relation-aware indexing for scattered facts; coarse-to-fine summarization to locate the right session; multi-view filtering against distractors.
- **F5 Operational scaling rule:** efficiency is governed by **maintenance scope**, not by whether structure is used. Localized update/search = best cost-utility; global reorganization (graph-wide consolidation, multi-store sync, whole-memory rewrite) = orders-of-magnitude higher index-construction time & query latency without proportional accuracy. **Measure construction time vs. query time separately.**
- **F6 Representation granularity:** preserving recoverable evidence > compactness. Raw text best for exact detail; light compression keeps reasoning but hurts exact match; deeper hierarchy improves access but can't restore removed content.
- **F7 Late filtering:** extraction should preserve context at write time — coarser segmentation, limited rewriting, keep both user and assistant turns.
- **F8 Retrieval strategy:** moderate hybrid dense+sparse fusion beats sparse-leaning; light query planning helps; extra reflection adds cost without gain.
- **F9 Maintenance design:** conservative consolidation is the best default; delayed flushing fragments evidence; overly coarse summarization obscures cues.

### How mindD sits inside the lens

mindD is intentionally the **substrate** half of `⟨R, S, Q, U⟩`. It owns the physical
side of `R`, the lifecycle side of `U`, and the primitive side of `Q`; it deliberately
leaves `S` (extraction) and the *orchestration* of `Q` (hybrid recall assembly, prompt
construction) to the agent — consistent with the ADR-0001/0002 non-goals ("not an agent
framework, not a vector/graph DB, not an inference cache, not a context-window compiler").

The consequence that drives this whole audit: **the paper's two most-emphasized failure
modes — F3 (stale facts) and F5 (cost = maintenance scope, measured as construction vs.
query) — fall squarely on the two layers mindD owns and is currently thinnest in:
`U` and observability.**

## 2. Where mindD already aligns

Stated first, because the paper rewards these and they should not be disturbed:

- **Trace-preserving `episodic`** (`Append`/`Range`/`Tail`, monotonic `cursor`) is exactly the "trace-preserving memory" that tops the paper's stateful/DB-Bench workload (F1). Operation order is preserved.
- **Fronts engines, doesn't reimplement them** — in-memory, Postgres, pgvector, fs, S3/MinIO. This is the paper's "heterogeneous multi-engine" physical layer done as a substrate (R-physical), and it keeps the ADR non-goals intact.
- **KV already has CAS + TTL** — a monotonic `Version` with `IfVersion` compare-and-swap and `ExpiresAt` with lazy read-gating + a background sweeper. That is the paper's "constraint-based hard eviction" (U-capacity) plus real optimistic concurrency.
- **Capability + policy + OTel** give the "one auth / one policy / one observability story" the paper implicitly argues fragmented per-framework memory lacks.
- **ADR-0002 (`graph` block, Proposed)** already targets the paper's strongest-performing representation family (temporal KG / relationship-aware recall). The instinct is correct and validated by the data; it is a sequencing question, not a direction question.
- **Conservative by default** — no aggressive server-side consolidation exists, which the paper (F9) says is the right default.

## 3. Per-module current state (grounded)

### R — representation & storage — **partial**

- `semantic` Record is a flat token-vector row: `id, content, payload, vector, metadata (map<string,string>), created_at` (`proto/.../semantic.proto:19-31`; `internal/semantic/driver.go:13-20`). No relationships, hierarchy, or lifecycle fields.
- Physical storage is specialized single-engine per driver: brute-force cosine in memory, or **pgvector with an HNSW index** (`vector_cosine_ops`) in Postgres (`internal/semantic/drivers/postgres/postgres.go` ~`243-264`). *Correction to a common assumption:* the index is HNSW, not ivfflat — the `ivfflat` fallback in the code is a dead no-op (`_ = err`), so on pgvector < 0.5.0 the store silently degrades to a sequential scan (near-dead code path in 2026, but real).
- `episodic` Event carries a server `timestamp` but `Range`/`Tail` window by **cursor only** — no timestamp predicate, no `session_id`/correlation field, no server-side filter by `type`/actor (`proto/.../episodic.proto`). Append-only with **no retention/compaction** ⇒ unbounded growth.
- No graph/tree topology anywhere (`internal/` has exactly the five blocks; ADR-0002 not yet built).
- The only embedder present is a deterministic SHA-256 `Fake` (`internal/semantic/embedder/embedder.go`); real provider adapters are referenced as sibling packages.

**Gaps vs. paper:** no graph/temporal-KG or hierarchy (R type 2 — the paper's strongest family, F1/F4); flat metadata only; time stored but not a queryable representation dimension (F4); no lifecycle fields on the record to support revisability (F3).

### S — extraction — **out of scope (by design, correctly)**

Extraction is agent-side; `Upsert` accepts either pre-embedded vectors or content-to-embed. This matches the ADR non-goals and the paper's framing (S is usually LLM/agent-side). The only adjacent, in-scope nudge: F7 ("keep both user and assistant turns") would be better served if `episodic` had an optional first-class `role`/`session_id` field rather than relying on convention in the `metadata` map.

### Q — retrieval & routing — **gap**

- Pure dense cosine KNN. `Search` accepts `query_text` (server embeds) **or** `query_vector`, `top_k` (default 10), an **exact-match** metadata `filter` (jsonb `@>` containment, applied before `ORDER BY`/`LIMIT`), and include flags (`proto/.../semantic.proto:44-66`; `internal/semantic/drivers/postgres/postgres.go` ~`163-171`).
- **No** sparse/BM25 lane, **no** fusion (RRF/MMR), **no** reranking, **no** time-range or recency scoping (though `created_at` is stored and returned), **no** structured predicates (ranges / `!=` / `OR` / `IN`), **no** server cap on `top_k`.
- `Delete` is single-id only (`proto/.../semantic.proto:68-71`).

**Gaps vs. paper:** exact-token evidence (ids, codes, names) is unrecoverable when embeddings blur it (F8); recall can't be scoped or narrowed as temporal distance grows (F2/F4); multi-view distractor filtering is limited to whole-string equality (F4).

### U — maintenance & lifecycle — **biggest gap**

- `semantic.Upsert` is a **destructive** last-writer-wins `ON CONFLICT (id) DO UPDATE` overwrite with no history (`internal/semantic/drivers/postgres/postgres.go` ~`117-125`); `Delete` is **physical**. There is no `valid_from`/`valid_to`, no `supersedes`/provenance, no soft-delete/tombstone, no as-of read, no version column on semantic.
- Eviction across the substrate is **TTL-only constraint eviction** (KV `ExpiresAt` + lazy gate + `sweepLoop`/`sweepExpired`). There is **no** score/heat/decay-based priority eviction and **no** access instrumentation (`Get`/`Search` update no counter).
- CAS exists only in KV, not semantic.

**Gaps vs. paper:** this is F3 verbatim — without lifecycle governance the semantic store returns stale facts ("hallucinations of the past"); revisability is not expressible in the representation; and there is no localized bulk-maintenance primitive (F5/F9), forcing agents into global read-all + per-id delete loops.

### Cost & observability (RQ5 / F5) — **gap**

- Metrics come only from the third-party otelgrpc stats handler, labeled by gRPC method + status (`internal/server/server.go`). Write-path and query-path methods are collapsed into one undifferentiated per-method histogram — the paper's central construction-vs-query split is impossible.
- The interceptor already **measures** handler latency and discards it (`internal/interceptor/observability.go`); `methodToOp` already carries a `write` flag (`internal/interceptor/policy.go`) that could source an op-class label for free.
- No driver-level backend timing (so "we front engines" overhead is unmeasurable), no result-set-size metric, no per-namespace growth/cardinality, no eviction/consolidation counters.
- `RuleEngine` supports `allow`/`deny`/`rate_limit` but `rate_limit` throttles request **frequency** only — it cannot bound the **magnitude** of a single request (no `max top_k`, no traversal depth/fan-out cap). ADR-0002 §8 explicitly asks for the latter.
- Metrics export is prometheus-only (`internal/obs/metrics.go`), while tracing already supports OTLP — so cost metrics and traces can't currently land in the same backend.
- No `Embedding` cache anywhere in `internal/semantic` (verified: the only `sha256` use is the `Fake` embedder). Every `Upsert` with content and every `query_text` search re-calls the provider (`internal/semantic/service.go:81`).

## 4. Recommendations

Grouped by module, ranked within group by impact. Every item is a **substrate primitive**
(columns, filters, counters, bounded ops) — the agent supplies the judgment; the sidecar
supplies storage, filtering, and measurement. Effort: S/M/L/XL. The three items an
adversarial pass removed for crossing the scope line are listed in §5.

### U — lifecycle (highest impact: F3, F9, F5)

| ID | Recommendation | Findings | Effort | Impact |
|---|---|---|---|---|
| U1 | **Bitemporal validity + soft-delete** on semantic: `valid_from`/`valid_to`/`deleted_at`; `Search` defaults to "as-of now" and never returns invalidated/retracted rows. Add optional `as_of` timestamp and `include_invalidated` (F3 audit: "what was the stale answer based on"). | F3, U(a), F9 | L | high |
| U2 | **`supersedes` / `source` pointers** (first-class) on Record and Event, binding a later fact to what it revises. With U1, a superseding upsert sets `valid_to=now()` on named ids in one transaction — localized, not global (F5). | F3, U(a) | M | high |
| U3 | **Bulk lifecycle op** — `Expire(namespace, filter, action∈{invalidate,soft_delete,hard_delete}, max_rows)` reusing the existing `metadata @>` predicate. Replaces the global read-all + per-id `Delete` loop with one bounded server-side statement. | F5, F9, U(b/c) | M | high |
| U4 | **Semantic CAS parity** — port KV's `IfVersion` optimistic-concurrency pattern to `Upsert` so revisions under concurrency are deterministic. | U(a), F3 | M | medium |
| U5 | **Access instrumentation → heat eviction** — optional `last_accessed`/`access_count` (opt-in per namespace) enabling score-based priority eviction beyond TTL, plus KV read-through TTL refresh (slide expiry on `Get`) for genuine cache-tier residency. | U(b) | L | medium |

### Cost & observability (RQ5: F5)

| ID | Recommendation | Findings | Effort | Impact |
|---|---|---|---|---|
| O1 | **`op.duration` histogram** split by op-class (write/index vs. query), block, namespace — the RQ5 construction-vs-query split. Source op-class from the existing `write` flag; reuse discarded interceptor latency. | F5 | M | high |
| O2 | **Backend-latency + result-size** at the service layer; record `sidecar_overhead = op − backend` as a first-class value; record `len(hits)` as an evidence-completion proxy (F2). | F5, F2 | M | high |
| O3 | **Per-namespace item-count / growth gauge + eviction counters** (`cause=ttl|sweep|consolidation`). Ship the count gauge (`reltuples`); *skip a bytes gauge* — not cheaply obtainable in Postgres. | F5, U(b) | L | high |
| O4 | **Policy cost caps** — `max_top_k`, scan/range `limit`, traversal `depth`/`fan_out` on policy rules, returning `ResourceExhausted`. ADR-0002 §8-mandated; pre-wires the graph block; enforces F5 discipline. | F5 | L | high |
| O5 | **OTLP metrics export** so cost metrics land with traces; **conformance/bench tie-in** emitting distinct write-path vs. query-path timings per driver (reproduces the paper's construction-vs-query numbers). | F5 | S+M | medium |

### Q — retrieval substrate (F2, F4, F8) — substrate halves only

| ID | Recommendation | Findings | Effort | Impact |
|---|---|---|---|---|
| Q1 | **Embedding cache** keyed on `(namespace, model, sha256(content))` — bounded LRU. Verified absent; highest cost-ROI item. | F5 | S | high |
| Q2 | **Time-range pre-filter** on `Search` (`created_after`/`created_before`) over the `created_at` already stored — indexable, inert. *Keep the pre-filter; leave recency-weighted ranking to the agent.* | F2, F4 | M | high |
| Q3 | **Structured metadata predicates** — extend exact-match filter to `{eq,neq,gt,gte,lt,lte,in}` via a backward-compatible `oneof`, compiled to jsonb/SQL. Enables multi-view filtering (F4). | F4 | M | medium |
| Q4 | **Opt-in RRF hybrid** (dense + `tsvector`/GIN sparse lane, deterministic Reciprocal Rank Fusion, `DENSE` stays default), candidate budget policy-capped (O4). The one defensible server-side fusion — a deterministic rank-merge, not a relevance model. | F8, F2 | L | medium |
| Q5 | **Seed-then-expand ergonomics** — `ids_only` projection + document the "semantic id == graph node id" convention in the proto; orchestration stays agent-side. | F2, F4 | S | medium |

### R — representation (F1, F4) — strategic

| ID | Recommendation | Findings | Effort | Impact |
|---|---|---|---|---|
| R1 | **Ship the `graph` block (ADR-0002)** — the paper's strongest family for cross-session aggregation & scattered-evidence assembly. Refinement: make `valid_from`/`valid_to` first-class **edge** fields so the graph carries F3 revisability natively (what makes Zep/Cognee win the knowledge-update slices). Sequence **after** O4 (traversal caps) and the cheap U/observability wins. | F1, F4, R type 2 | XL | high (ceiling) |
| R2 | **Optional `role`/`session_id` on `episodic` Event** so F7 ("keep both user + assistant turns", cross-session grouping) is first-class, not convention. | F7, F1 | S | low |
| R3 | **Fix the dead pgvector `ivfflat` fallback** (hygiene, not an F5 win) — attempt real `ivfflat` on HNSW failure, else log rather than swallow; optionally expose HNSW `m`/`ef_*` tuning. | F5 (hygiene) | S | low |

## 5. Explicitly rejected (scope discipline)

These would move ranking/relevance judgment or orchestration server-side, violating the
ADR "not a context-window compiler / not an agent framework" non-goals. Kept agent-side:

- **Recency-decay scoring knob** (`similarity × exp(-age/half_life)`) — deciding how much to discount stale evidence is retrieval-orchestration judgment (F2/F8 are agent-side tuning findings). *Keep the time-range pre-filter (Q2); drop the decay ranking.*
- **Server-side MMR diversity re-rank** — the λ relevance/diversity trade-off is a subjective relevance model; pgvector has no native MMR, so the sidecar would be *implementing a retrieval engine*. Cheap for the agent to do once it holds candidate ids+vectors.
- **`candidate_k` widening as agent-fuel** — redundant with `top_k` and invites the large-scan cost F5 warns against; fold any bounded budget into Q4's policy-capped candidate set, keep the `ids_only` projection and the id-sharing *convention* (Q5).

Also intentionally **not** added: server-side extraction/summarization (module S), hybrid-recall
orchestration/prompt assembly, entity resolution / relationship inference (an explicit
ADR-0002 non-goal), and continuous parametric optimization (offline model training).

## 6. One-line verdict

mindD is directionally right and already strong on trace preservation, engine-fronting,
and the auth/policy/observability story. Against the paper it is thin in exactly two owned
layers — **`U` (lifecycle/revisability, F3)** and **cost observability (F5)** — plus a
retrieval substrate that stops at dense KNN. Closing `U` + observability is cheap, scope-clean,
and closes the paper's two most-emphasized gaps; the `graph` block is the correct strategic
bet, sequenced behind the cost caps it depends on. See
[`optimization-plan.md`](optimization-plan.md) for the phased delivery.
