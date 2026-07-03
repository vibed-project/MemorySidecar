# ADR-0003 — Memory lifecycle primitives (bitemporal validity & soft-delete)

> **Status:** Proposed  ·  **Decision drivers:** Revisability as a first-class substrate
> concern; framework-agnostic protocol; the agent owns judgment, the sidecar owns storage
> · **Date:** 2026-07-03
>
> Extends [ADR-0001](adr-0001-memory-sidecar.md). It refines the `semantic` (and later
> `episodic`/`kv`) blocks; it does not revisit the cross-cutting decisions established
> there. Motivated by the audit in
> [`../analysis/agent-memory-paper-audit.md`](../analysis/agent-memory-paper-audit.md).

## 1. Context and problem statement

The `semantic` block today is *destructive*: `Upsert` is a last-writer-wins
`ON CONFLICT (id) DO UPDATE` overwrite with no history, and `Delete` physically removes a
row. There is no way to express that a fact *was* true and later stopped being true, nor
to retract a fact while keeping it auditable.

The agent-memory study (arXiv:2606.24775) identifies the absence of exactly this lifecycle
governance as the single most damaging gap: without it, a memory system keeps returning
superseded facts — the paper's **"hallucinations of the past" (Finding 3)**. Its systems
that handle knowledge updates best (Zep, Cognee, Mem0g) all bind a later fact to what it
revises and *logically invalidate* the old one via validity timestamps rather than deleting
it. The same study shows (Finding 5) that this maintenance must stay **localized** — a
bounded, id- or filter-targeted operation, never a global rewrite.

memsidecar is the natural owner of these primitives: it sits on the write and read path,
already scopes by namespace and capability, and already carries a `created_at` timestamp.
What it lacks are the *columns* and the *default read filter* that make revisability
expressible. This ADR adds them.

## 2. Decision

Add **bitemporal validity** and **soft-delete** to the `semantic` record, with a default
"as-of now" read filter, and give `Delete` an explicit hard-delete escape hatch.

Three new optional fields on the record:

| Field | Meaning | Default on write |
|---|---|---|
| `valid_from` | wall-clock time the fact becomes true (application/valid time) | `now()` if unset |
| `valid_to` | exclusive upper bound — the fact stops being true at this instant | unset = open-ended (still valid) |
| `deleted_at` | soft-delete tombstone; the row is retracted but retained | unset = live |

Read semantics (`Search`):

- **Default** (as-of *now*): a record is returned only when
  `deleted_at IS NULL AND valid_from <= now() AND (valid_to IS NULL OR valid_to > now())`.
  Invalidated, retracted, and not-yet-valid records are invisible. This is the change that
  closes Finding 3 for every existing caller *without any client change*.
- **`as_of <t>`**: evaluate the same predicate at time `t` instead of now — point-in-time
  recall ("what did we believe on date X").
- **`include_invalidated`**: bypass the lifecycle predicate entirely and return tombstoned
  / expired / future rows too — for audit and supersession-chain inspection ("what stale
  fact produced this answer"). Metadata filtering still applies.

Write semantics:

- `Upsert` sets `valid_from = provided-or-now()`, `valid_to = provided-or-null`,
  `deleted_at = provided-or-null`. Because `deleted_at` defaults to null, **re-upserting an
  id resurrects it** (clears a prior tombstone) unless the caller explicitly retracts.
- `Delete` gains a `hard` flag. **Soft (default):** set `deleted_at = now()` on a live row;
  the row is retained and reachable via `include_invalidated`. **Hard:** physical `DELETE`,
  as today.

Existing rows and callers are unaffected: the new columns default to a live state
(`valid_from = created_at`/`now()`, `valid_to`/`deleted_at` null), so every current row
reads as valid and every current `Search`/`Delete` call behaves as before.

## 3. Scope — what this is and is not

**Is:** inert storage columns plus a default read filter, both owned by the substrate. The
agent supplies `valid_from`/`valid_to` and decides when to retract; the sidecar stores the
values and applies a `WHERE` clause. No LLM is invoked; no engine is reimplemented.

**Is not:**

- **Not automatic supersession or conflict inference.** The sidecar never computes that
  record B invalidates record A. Binding a later fact to what it revises (a `supersedes`
  pointer that sets `valid_to` on the named ids in one localized transaction) is the
  **follow-on U2**; even there the agent names the ids. Consistent with the ADR-0002
  non-goal of no entity resolution / relationship inference.
- **Not consolidation or summarization.** Merging redundant facts is agent-side (module S).
- **Not global reorganization.** Every operation here is id-targeted or, in the follow-on
  bulk-invalidate (**U3**), filter-scoped with a required `max_rows` bound — localized per
  Finding 5.

## 4. Consequences

- **Positive:** the substrate can now represent revisable, auditable memory; the default
  read stops returning stale facts; point-in-time recall becomes possible; the design
  generalizes to `episodic`/`kv` and lays the ground for U2 (supersedes/provenance) and U3
  (bulk `Expire`).
- **Cost:** three nullable columns + a partial index on the `semantic` table; a widened
  `WHERE` clause on the hot search path (kept index-friendly — the lifecycle predicate is a
  pre-filter applied before ANN ordering, backed by a partial index on the live set).
- **Wire compatibility:** additive proto fields at new tags; no renumbering; the
  `semantic.Driver` interface's `Delete` signature gains a `DeleteOptions` parameter
  (mirrors the `kv` block's shape). Generated code is regenerated via `make proto`.
- **Migration:** the `semantic` Postgres driver manages its schema inline in `ensureSchema`
  (no numbered migration files for this block); the new columns are added idempotently with
  `ALTER TABLE ... ADD COLUMN IF NOT EXISTS`, so existing tables upgrade in place.

## 5. Alternatives considered

| Option | Why rejected |
|---|---|
| Keep destructive overwrite; let agents version by convention (e.g. `metadata["valid_to"]`) | Not queryable as a default read filter; every agent re-implements invalidation client-side and still over-fetches stale rows — the exact coupling ADR-0001 removes. |
| Physical delete + a separate append-only audit log | Loses point-in-time recall and the supersession chain; splits one fact's history across two blocks. |
| Full system-time bitemporality (transaction-time *and* valid-time) | Heavier schema and query surface than the workload needs now. Valid-time + a soft-delete tombstone covers Finding 3; revisit if transaction-time audit is required. |
| Store validity only in the `graph` block (ADR-0002) | Graph is post-v0.2 and not every deployment runs it; revisability is needed on the `semantic` substrate that ships today. |

## 6. Delivery

This ADR corresponds to ticket **U1** in
[`../analysis/optimization-plan.md`](../analysis/optimization-plan.md). Follow-ons under the
same lifecycle theme: **U2** (`supersedes`/`source` provenance pointers), **U3** (bulk
`Expire` by filter), **U4** (semantic CAS parity with `kv`). Each lands behind the
`semantictest` conformance suite and is exercised by both the in-memory and pgvector
harnesses.
