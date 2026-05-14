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
}

message Record {
  string id = 1;          // empty → server assigns UUID
  string content = 2;     // text source; if non-empty and vector is empty, server embeds
  bytes  payload = 3;
  repeated float vector = 4;     // optional precomputed vector
  map<string, string> metadata = 5;
}

message SearchRequest {
  string namespace = 1;
  string query_text = 2;
  repeated float query_vector = 3;   // set query_text OR query_vector, not both
  uint32 top_k = 4;
  map<string, string> filter = 5;    // exact-match metadata filter
  bool include_payload = 6;
  bool include_vector = 7;
}
```

## Drivers

| Driver | Notes |
|---|---|
| `memory` | Brute-force cosine over an internal map. Normalises on write. Suitable for tests and small dev workloads. |
| `postgres` | pgvector. One table **per namespace** (because `vector(N)` bakes the dimension into the column type), HNSW cosine index, `vector <=> $1` distance. `metadata @> $::jsonb` for filter. |

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
      options:
        api_key_env: OPENAI_API_KEY
        timeout: 30s
```

The pgvector driver creates the table `semantic_notes` on startup with
`vector(1536)`.

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
```

## Python example

```python
from memsidecar.semantic.v1 import semantic_pb2

m.semantic.upsert("notes", [
    semantic_pb2.Record(id="a", content="apple pie recipe"),
])
for hit in m.semantic.search("notes", query_text="apple", top_k=3):
    print(hit.record.id, hit.score)
```

## Op names

| Op | Method |
|---|---|
| `semantic.upsert` | `Semantic/Upsert` |
| `semantic.search` | `Semantic/Search` |
| `semantic.delete` | `Semantic/Delete` |

## Notes

- Either `query_text` **or** `query_vector` may be set, never both —
  ambiguous combinations are rejected with `InvalidArgument`.
- `Search` returns cosine similarity in `[-1, 1]`, higher is more similar.
  Memory driver's normalisation may overshoot 1.0 by ~1e-6 due to float32
  precision; pgvector clamps server-side.
- Records can carry a pre-computed `vector` instead of `content` — useful
  when you embed offline and only use memsidecar for storage + search.
