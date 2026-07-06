---
title: Use cases
sidebar_position: 3
---

# Use cases

The [block reference](../blocks/kv.md) tells you *what each block does*. This
page is the other half: *what you'd actually build with it, and why the
sidecar shape makes it easier than rolling your own.*

Every recipe below maps a concrete agent problem onto shipped features. They
compose — a real deployment usually runs several at once against the same
sidecar, sharing one auth model, one policy surface, and one observability
story.

---

## Cache expensive tool calls with a hot tier

**Problem.** Agents re-invoke the same expensive tools — web fetches, LLM
sub-calls, database lookups — with identical arguments. Naïve caching either
grows without bound or evicts the entries you're about to reuse.

**Build it with** the [KV block](../blocks/kv.md). Key a namespace on the
tool-call signature, `Put` the result with a `ttl`, and `Get` before every
call. Turn on the **cache-tier access policy** to make it behave like a real
cache rather than a plain TTL map:

```yaml
namespaces:
  - block: kv
    name: tool-cache
    backend: mem-default
    access:
      track: true                  # count hits per key
      slide_ttl_seconds: 300       # read-through: a Get pushes expiry out to now+5m
      capacity: 10000              # cap the namespace
      heat_half_life_seconds: 3600 # evict the *coldest* keys first, not the oldest
```

Hot keys stay resident because every read slides their TTL; cold keys age out
and, over capacity, are evicted by heat (`access_count · 2^(−age/half_life)`)
rather than blindly by age. `memsidecar.namespace.items` and
`memsidecar.eviction.total{cause="capacity"}` let you watch the tier work.
See [KV → cache-tier access policy](../blocks/kv.md#cache-tier-access-policy-in-memory-only).

---

## Keep a knowledge base that stays correct as facts change

**Problem.** A long-running agent learns things that later turn out to be
wrong or superseded. A plain vector store just accumulates contradictory
duplicates — and can never answer *"what did we believe last Tuesday?"* for
an audit.

**Build it with** the [Semantic block](../blocks/semantic.md)'s
bitemporal lifecycle. When you write a correction, name what it
replaces; the prior records are invalidated in the same call:

```jsonc
// Upsert — "v2 replaces v1", as a revision, not a second copy
{
  "records": [{
    "content": "The API rate limit is 1000 req/min.",
    "supersedes": ["fact-rate-limit-v1"],
    "source": "docs-crawl-2026-07"
  }]
}
```

- `supersedes` invalidates the old record atomically — search stops returning it.
- `valid_from` / `valid_to` model *when a fact was true*, independent of when
  you wrote it; an **as-of** search reconstructs the knowledge state at any past
  instant.
- `deleted_at` soft-deletes (tombstone, auditable) instead of dropping rows.
- `Expire` retires a whole slice by filter — one bounded call to forget a
  source, a tenant, or everything before a cutoff.
- `if_version` gives optimistic concurrency so two agents can't clobber each
  other's correction.

This is the difference between a memory that *grows* and a memory that stays
*true*. See [Semantic → lifecycle](../blocks/semantic.md).

---

## Retrieve reliably across paraphrase *and* exact tokens

**Problem.** Dense vector search is great at paraphrase but routinely misses
the tokens that matter most to agents — error codes, SKUs, function names,
UUIDs. Keyword search nails those but misses "the login thing is broken" ≈
"authentication failure".

**Build it with** the Semantic block's **hybrid search** (Q4). One request
runs a dense lane and a sparse (full-text) lane and fuses them with Reciprocal
Rank Fusion:

```jsonc
{
  "namespace": "notes",
  "query": "connection reset ECONNRESET during checkout",
  "mode": "SEARCH_MODE_HYBRID",
  "rerank_candidate_k": 50,        // per-lane candidate depth before fusion
  "top_k": 10,
  "predicates": [{ "field": "service", "op": "EQ", "value": "checkout" }],
  "created_after": "2026-06-01T00:00:00Z"
}
```

The exact token `ECONNRESET` is found by the sparse lane even when its
embedding is unremarkable; the paraphrased intent is found by the dense lane;
RRF merges them. **Metadata predicates** and a **time window** pre-filter the
candidate set, and `ids_only` returns just the ids for a cheap first stage that
feeds a rerank or a graph expansion. Configure the sparse lane's language with
`text_search:` on the namespace. See [Semantic → search modes](../blocks/semantic.md).

---

## Reconstruct conversations and follow agents live

**Problem.** You need to replay exactly what an agent saw, group events by
conversation, and — for a supervisor or a live dashboard — watch new events
as they land.

**Build it with** the [Episodic block](../blocks/episodic.md). `Append` events
with first-class `role` and `session_id`, so grouping isn't a metadata
convention you have to remember to honour:

- `Range` replays history with monotonic cursors *and* an event-timestamp
  window (`after_time` / `before_time`), forward or `reverse`.
- `Tail` streams the live edge, or replays from a cursor and then transitions
  to live — the backbone of a "watch this agent" view. Slow readers are
  detached rather than allowed to block writers.

Pair it with the Semantic block to turn raw episodes into recallable memory:
log everything to episodic, embed the parts worth remembering into semantic.

---

## Model relationships that change over time

**Problem.** "What is *like* this?" is a vector question. "What is *connected*
to this, and how?" is a graph question — and the connections themselves have a
lifetime (who owned this service in Q1, who reported to whom before the
re-org).

**Build it with** the [Graph block](../blocks/graph.md). Write typed nodes and
edges — sharing ids with semantic records so the two views line up — and let
the sidecar serve **bounded** `Neighbors` / `Traverse`. Edges are
**bitemporal** (#18): give them `valid_from` / `valid_to` and ask an
`as_of` question:

```jsonc
// "Who did alice report to on 2026-03-01?" — traversal as of a past date
{
  "start": "person:alice",
  "edge_types": ["REPORTS_TO"],
  "max_depth": 3,
  "as_of": "2026-03-01T00:00:00Z"
}
```

Traversal cost is hard-capped server-side (depth × fan-out), so an agent can't
walk the whole graph by accident — and a [policy cap](../concepts/policy.md) can
lower the ceiling per tenant. See [Graph → bitemporal edges](../blocks/graph.md).

---

## Coordinate multiple agents on shared state

**Problem.** Two cooperating agents want to mutate the same resource, and if
one crashes mid-operation the lock must not be held forever.

**Build it with** the [Lease block](../blocks/lease.md). `Acquire` with a
required `ttl` and an optional `wait_for`; `Renew` on a heartbeat while you
work; `Release` when done. If the holder dies, the TTL expires the lease
automatically — no stuck locks, no manual reaper.

---

## Store what agents generate

**Problem.** Agents produce artifacts — images, audio, rendered reports,
large structured outputs — that are too big to sit in a record and that you
want to fetch back later by id.

**Build it with** the [Artifact block](../blocks/artifact.md). `Put` is
client-streaming (upload in chunks), `Get` is server-streaming (download in
chunks), and `Stat` returns metadata without the body. The same API fronts a
local filesystem in dev and S3/MinIO in production — the agent code doesn't
change.

---

## Govern cost for autonomous agents

**Problem.** An autonomous or adversarial agent issues a `top_k: 100000`
search or a maximum-depth traversal and melts your backend — and you have no
way to see whether your spend is going to writes (indexing) or queries.

**Build it with** the [policy engine](../concepts/policy.md) and
[observability](../ops/observability.md), both of which live in the sidecar
precisely because it sits on the request path.

- **Bound magnitude per request** with an `effect: cap` rule — `top_k`,
  scan `limit`, graph `depth` / `fan_out`, hybrid `rerank_candidate_k`. Over
  the cap surfaces as `ResourceExhausted` so clients back off.
- **Rate-limit** abusive callers with token-bucket rules, keyed per
  tenant/agent/namespace/op.
- **See where the cost goes**: `memsidecar_op_duration_seconds` splits
  **write/index time from query time** — the distinction the memory
  literature centres on — and `backend.duration` isolates the engine's share
  from the sidecar's overhead. The embedding cache's hit/miss rates show
  exactly how many provider calls you're avoiding.

This is memory-layer cost governance you'd otherwise have to bolt onto every
agent by hand.

---

## Swap backends without touching agent code

Cutting across all of the above: an agent only ever speaks the sidecar's gRPC
API. Which engine actually stores the data — in-memory for tests and local
dev, Postgres / pgvector / S3 in production — is a
[config](../config/reference.md) decision, not a code change. The same is true
of auth, policy, and telemetry: they're declared once at the edge, not
re-implemented in every framework and every agent.

That's the whole thesis. The recipes above are what you get to build *on top
of* not having to solve memory plumbing again.
