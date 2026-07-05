---
title: Semantic
sidebar_position: 3
---

# Semantic

Embed-and-search over arbitrary records. Each namespace is bound to a
single embedder at config time, so the same record stored in different
namespaces can be embedded by different models.

## API

```proto
service Semantic {
  rpc Upsert(UpsertRequest) returns (UpsertResponse);
  rpc Search(SearchRequest) returns (SearchResponse);
  rpc Delete(DeleteRequest) returns (DeleteResponse);
  rpc Expire(ExpireRequest) returns (ExpireResponse);   // bounded, filter-scoped lifecycle op
}

message Record {
  string id = 1;          // empty → server assigns UUID
  string content = 2;     // text source; if non-empty and vector is empty, server embeds
  bytes  payload = 3;
  repeated float vector = 4;     // optional precomputed vector
  map<string, string> metadata = 5;
  google.protobuf.Timestamp created_at = 6;
  // Lifecycle & revisability (ADR-0003). All optional; unset = live/open-ended.
  google.protobuf.Timestamp valid_from = 7;   // becomes true at (default: now on write)
  google.protobuf.Timestamp valid_to   = 8;   // stops being true at (exclusive)
  google.protobuf.Timestamp deleted_at = 9;   // soft-delete tombstone
  repeated string supersedes = 10;            // ids this record revises → invalidated on write
  string source = 11;                         // opaque provenance handle
  uint64 version = 12;                         // server-set monotonic per-id counter (output)
  optional uint64 if_version = 13;             // CAS precondition on write (input-only)
}

message UpsertResponse {
  repeated string ids = 1;
  repeated uint64 versions = 2;   // new per-id version, aligned with ids
}

message SearchRequest {
  string namespace = 1;
  string query_text = 2;
  repeated float query_vector = 3;   // set query_text OR query_vector, not both
  uint32 top_k = 4;
  map<string, string> filter = 5;    // exact-match metadata filter
  bool include_payload = 6;
  bool include_vector = 7;
  google.protobuf.Timestamp as_of = 8;   // evaluate validity at this instant (default now)
  bool include_invalidated = 9;          // also return tombstoned/expired/future records
  bool ids_only = 10;                    // return only id + score (cheap seed set)
  repeated FieldPredicate predicates = 11;   // ranges / set membership, ANDed with filter
}

// EQ/NEQ/IN compare as strings; GT/GTE/LT/LTE compare numerically.
message FieldPredicate {
  string key = 1;
  PredicateOp op = 2;          // EQ | NEQ | GT | GTE | LT | LTE | IN
  repeated string values = 3;  // one value; IN takes one or more
}

message DeleteRequest {
  string namespace = 1;
  string id = 2;
  bool hard = 3;   // default false = soft delete (tombstone); true = physical delete
}

enum ExpireAction {
  EXPIRE_ACTION_UNSPECIFIED = 0;
  EXPIRE_ACTION_INVALIDATE  = 1;   // set valid_to = now()
  EXPIRE_ACTION_SOFT_DELETE = 2;   // set deleted_at = now()
  EXPIRE_ACTION_HARD_DELETE = 3;   // physical delete
}

message ExpireRequest {
  string namespace = 1;
  map<string, string> filter = 2;   // same shape as Search; empty matches all
  ExpireAction action = 3;
  uint32 max_rows = 4;              // required (> 0); caps the affected set
}

message ExpireResponse {
  uint64 affected = 1;
}
```

## Lifecycle & revisability

Per ADR-0003 (memory lifecycle primitives), the semantic block is **bitemporal** and
**revisable** — the substrate stores the timestamps and applies the read filter; the
agent decides what is valid or superseded (no inference is done server-side).

- **As-of-now by default.** `Search` returns only records that are live and valid *now*:
  `deleted_at IS NULL AND valid_from ≤ now < valid_to`. Invalidated, retracted, and
  not-yet-valid records are hidden — no client change required. This is what stops an
  agent recalling stale facts ("hallucinations of the past").
- **Point-in-time recall.** Pass `as_of` to evaluate validity at a past instant, or
  `include_invalidated=true` to bypass the filter entirely (audit / supersession chains).
- **Soft delete by default.** `Delete` sets `deleted_at` and retains the row (visible via
  `include_invalidated`); `hard=true` removes it physically. Re-`Upsert`ing an id
  resurrects it (clears the tombstone) unless you set `deleted_at` yourself.
- **Supersession.** Set `supersedes=[id,…]` on a new record and the server sets those
  records' `valid_to` to the new record's `valid_from` in the same transaction —
  localized, id-targeted, self-references ignored. `source` is an opaque provenance
  handle (e.g. an episodic cursor or artifact id).
- **Optimistic concurrency.** Each id carries a monotonic `version`; pass `if_version`
  to make a write conditional (`0` = must-not-exist). A mismatch fails
  `FailedPrecondition` and changes nothing. `UpsertResponse.versions` returns the new
  versions.
- **Bulk maintenance.** `Expire` applies `invalidate` / `soft_delete` / `hard_delete` to
  every record matching a metadata filter in one bounded server-side statement
  (`max_rows` required) — instead of a client-side read-all + per-id delete loop.

## Drivers

| Driver | Notes |
|---|---|
| `memory` | Brute-force cosine over an internal map. Normalises on write. Suitable for tests and small dev workloads. Honours the full lifecycle model above. |
| `postgres` | pgvector. One table **per namespace** (because `vector(N)` bakes the dimension into the column type), HNSW cosine index, `vector <=> $1` distance. `metadata @> $::jsonb` for filter; `predicates` compile to parameterized `metadata->>$k <op> $v` expressions (numeric casts guarded by a regex so malformed data is skipped, not fatal). Lifecycle columns (`valid_from`/`valid_to`/`deleted_at`/`supersedes`/`source`/`version`) are added idempotently in `ensureSchema`; the default live-search pre-filter is backed by a partial index over non-tombstoned rows. |

## Embedders

| Provider | Use case | Notes |
|---|---|---|
| `fake` | Tests, dev | Deterministic SHA-256-derived L2-normalised vectors. Reproducible, **not** semantically meaningful. |
| `ollama` | Local dev with real embeddings | Calls `POST /api/embed`. Zero API key. |
| `openai` | Production | Calls `POST /v1/embeddings`. API key from a config-named env var (never in YAML). Sends the `dimensions` request param for `text-embedding-3-*` truncation. |

All three implement the `Embedder` interface — adding Voyage, Cohere, or
a local llama.cpp endpoint is a one-file adapter.

## Configuration

```yaml
namespaces:
  - block: semantic
    name: notes
    backend: pg-main
    embedder:
      provider: openai
      model: text-embedding-3-small
      dimensions: 1536
      cache_size: 4096              # optional; embed-once cache, default 4096
      options:
        api_key_env: OPENAI_API_KEY
        timeout: 30s
```

The pgvector driver creates the table `semantic_notes` on startup with
`vector(1536)`.

Each namespace keeps a bounded **embedding cache** in front of its embedder:
identical content is embedded once and reused, so re-indexing the same text or
running a repeated `query_text` skips the provider round-trip. Tune it with
`embedder.cache_size` (omit/`0` = default 4096 entries; negative = disabled);
effectiveness shows up as `memsidecar.embedder.cache.{hits,misses}`.

## gRPC example

```bash
grpcurl -plaintext -H "x-memsidecar-capability: Bearer $TOKEN" \
  -d '{
    "namespace":"notes",
    "records":[
      {"id":"a","content":"apple pie recipe","metadata":{"topic":"food"}},
      {"id":"b","content":"how to debug a Go panic","metadata":{"topic":"code"}}
    ]
  }' \
  127.0.0.1:7777 memsidecar.semantic.v1.Semantic/Upsert

grpcurl -plaintext -H "x-memsidecar-capability: Bearer $TOKEN" \
  -d '{"namespace":"notes","query_text":"apple","top_k":2}' \
  127.0.0.1:7777 memsidecar.semantic.v1.Semantic/Search

# Revise a fact: "b" supersedes "a" — "a" is invalidated as of the new record.
grpcurl -plaintext -H "x-memsidecar-capability: Bearer $TOKEN" \
  -d '{"namespace":"notes","records":[
        {"id":"c","content":"how to debug a Go race","supersedes":["b"]}]}' \
  127.0.0.1:7777 memsidecar.semantic.v1.Semantic/Upsert

# Soft-delete every record tagged topic=food, in one bounded op.
grpcurl -plaintext -H "x-memsidecar-capability: Bearer $TOKEN" \
  -d '{"namespace":"notes","filter":{"topic":"food"},
       "action":"EXPIRE_ACTION_SOFT_DELETE","max_rows":100}' \
  127.0.0.1:7777 memsidecar.semantic.v1.Semantic/Expire
```

## Python example

```python
from memsidecar.semantic.v1 import semantic_pb2

resp = m.semantic.upsert("notes", [
    semantic_pb2.Record(id="a", content="apple pie recipe"),
])
print(resp.ids, resp.versions)   # ['a'] [1]

for hit in m.semantic.search("notes", query_text="apple", top_k=3):
    print(hit.record.id, hit.score)

# Revise "a" with optimistic concurrency, binding the new fact to the old one.
m.semantic.upsert("notes", [
    semantic_pb2.Record(id="b", content="apple crumble recipe",
                        supersedes=["a"], if_version=1),
])

# Soft delete, then recover it for audit.
m.semantic.delete("notes", "b")                     # tombstone (still stored)
m.semantic.search("notes", query_text="apple", include_invalidated=True)

# Bulk-invalidate by filter (one bounded server-side op).
n = m.semantic.expire("notes", filter={"topic": "food"},
                      action=semantic_pb2.EXPIRE_ACTION_INVALIDATE, max_rows=100)
```

## Op names

| Op | Method |
|---|---|
| `semantic.upsert` | `Semantic/Upsert` |
| `semantic.search` | `Semantic/Search` |
| `semantic.delete` | `Semantic/Delete` |
| `semantic.expire` | `Semantic/Expire` |

## Notes

- Either `query_text` **or** `query_vector` may be set, never both —
  ambiguous combinations are rejected with `InvalidArgument`.
- `Search` returns cosine similarity in `[-1, 1]`, higher is more similar.
  Memory driver's normalisation may overshoot 1.0 by ~1e-6 due to float32
  precision; pgvector clamps server-side.
- Records can carry a pre-computed `vector` instead of `content` — useful
  when you embed offline and only use memsidecar for storage + search.
- `filter` is exact-match (AND of equalities); `predicates` adds ranges and
  set membership. `EQ`/`NEQ`/`IN` compare metadata values as strings;
  `GT`/`GTE`/`LT`/`LTE` compare **numerically** — the predicate value must
  parse as a number (else `InvalidArgument`), and a record whose stored value
  isn't numeric is simply skipped, never erroring the query. A predicate only
  matches when the key is present. `filter` and all `predicates` AND together
  and run as a pre-filter before the ANN ordering.
- Lifecycle timestamps and `supersedes`/`source` are **stored, not interpreted**:
  the sidecar never decides what supersedes what or resolves entities — the agent
  supplies the values (consistent with the ADR non-goals).
- `ids_only=true` returns just each hit's `record.id` and `score`, skipping
  content/payload/vector/metadata (and the storage load / marshaling they cost);
  it overrides `include_payload`/`include_vector`. It's the **seed step** of
  agent-orchestrated hybrid recall: a semantic `Search` selects candidate ids,
  then the [Graph block](graph.md) expands them via `Neighbors`/`Traverse`. By
  convention a semantic record id and a graph node id denote the same entity, so
  the seed ids drop straight into a traversal — the sidecar does **no** traversal
  or orchestration itself (ADR-0002 §8).
