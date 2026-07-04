---
title: Python SDK
sidebar_position: 1
---

# Python SDK

A thin client wrapping the gRPC stubs. Lives in `sdk/python/` and
installs as `memsidecar`.

## Install

```bash
cd sdk/python
python3 -m venv .venv
.venv/bin/pip install -e ".[dev]"
```

Requires Python 3.10+. Runtime deps are just `grpcio` + `protobuf`.

## Hello world

```python
import datetime as dt
from memsidecar import MemSidecar

with MemSidecar("127.0.0.1:7777", token=MEMSIDECAR_TOKEN) as m:
    m.kv.put("scratchpad", "hello", b"world", ttl=dt.timedelta(minutes=5))
    rec = m.kv.get("scratchpad", "hello")
    print(rec.value)  # b"world"
```

`MemSidecar` opens a gRPC channel wrapped with a `CapabilityInterceptor`
that attaches `x-memsidecar-capability: Bearer <token>` to every outgoing
call (unary, server-stream, client-stream, bidi). The token is shared
across all six per-block sub-clients (`m.kv`, `m.episodic`, `m.semantic`,
`m.artifact`, `m.lease`, `m.graph`).

## Per-block surface

### KV

```python
m.kv.put(ns, key, value, *, ttl=None, content_type="", metadata=None, if_version=None)
m.kv.get(ns, key)
m.kv.delete(ns, key, *, if_version=None)
for item in m.kv.scan(ns, key_prefix="", limit=0, include_values=False):
    ...
```

### Episodic

```python
ev = m.episodic.append(ns, type="tool_call", payload=b"...", metadata={...})
for ev in m.episodic.range(ns, after_cursor=0, limit=100):
    ...
for ev in m.episodic.tail(ns, include_historical=True, after_cursor=0):
    ...  # blocks; iterator yields events as they arrive
```

### Semantic

```python
from memsidecar.semantic.v1 import semantic_pb2

resp = m.semantic.upsert(ns, [
    semantic_pb2.Record(id="a", content="apple"),
])
resp.ids, resp.versions          # assigned ids + new per-id versions (aligned)
for hit in m.semantic.search(ns, query_text="apple", top_k=3, filter={"topic":"food"}):
    print(hit.record.id, hit.score)
m.semantic.delete(ns, "a")       # soft delete (tombstone) by default
```

You can also pass `query_vector=[...]` for a pre-embedded query, or
provide records with their own pre-computed `vector` to skip the
embedder entirely.

**Lifecycle (bitemporal & revisable).** Records carry validity and
provenance fields set directly on the `Record`, and `search`/`delete`/`expire`
expose the read and retraction surface. See [Semantic](../blocks/semantic.md).

```python
from google.protobuf.timestamp_pb2 import Timestamp

# Revise a fact: write the new one and bind it to what it supersedes.
# `supersedes` sets valid_to on the named ids in one localized transaction.
m.semantic.upsert(ns, [
    semantic_pb2.Record(id="b", content="apples are red",
                        supersedes=["a"], source="tool:web"),
])

# Point-in-time recall — what we believed as of a past instant.
past = Timestamp(); past.FromJsonString("2026-01-01T00:00:00Z")
m.semantic.search(ns, query_text="apple", as_of=past.ToDatetime())

# Audit view — also return tombstoned / expired / not-yet-valid rows.
m.semantic.search(ns, query_text="apple", include_invalidated=True)

# Hard delete (physical) instead of a tombstone.
m.semantic.delete(ns, "b", hard=True)

# Bounded bulk lifecycle action — max_rows is required.
n = m.semantic.expire(
    ns, filter={"topic": "food"},
    action=semantic_pb2.EXPIRE_ACTION_INVALIDATE, max_rows=500)
```

Optimistic concurrency mirrors `kv`: set `if_version` on a `Record` to
make the upsert a compare-and-swap (stale version → `FailedPrecondition`);
the response's `versions` carry the new value.

### Artifact

```python
ref = m.artifact.put(ns, payload_bytes, id="optional", content_type="image/png")
got = m.artifact.get(ns, ref.id)           # bytes
meta = m.artifact.stat(ns, ref.id)
m.artifact.delete(ns, ref.id)
```

`put` chunks at 64 KiB internally; `get` reassembles. For large blobs
where you don't want everything in memory, use the raw stubs directly
(`m.artifact._stub`).

### Lease

```python
handle = m.lease.acquire(ns, "deploy", ttl=dt.timedelta(seconds=60))
try:
    do_work()
finally:
    m.lease.release(handle.holder_id, ns, "deploy")
```

`acquire(wait_for=...)` blocks for up to that duration on a held key.
`renew(holder_id, ns, key, ttl=...)` extends; `inspect(ns, key)` peeks.

### Graph

```python
from memsidecar.graph.v1 import graph_pb2

m.graph.upsert_nodes(ns, [
    graph_pb2.Node(id="alice", labels=["Person"], props={"team": "core"}),
    graph_pb2.Node(id="doc-1", labels=["Document"]),
])
m.graph.upsert_edges(ns, [
    graph_pb2.Edge(id="e1", type="AUTHORED", **{"from": "alice"}, to="doc-1"),
])

node = m.graph.get_node(ns, "alice")

# 1-hop neighbors, filtered by edge type / direction / neighbor label.
nb = m.graph.neighbors(ns, "alice", edge_types=["AUTHORED"],
                       direction=graph_pb2.DIRECTION_OUT, limit=50)
nb.nodes, nb.edges

# Bounded multi-hop expansion — depth/max_nodes of 0 use server defaults;
# both are hard-capped server-side and reject with ResourceExhausted.
sub = m.graph.traverse(ns, "alice", depth=2, max_nodes=100,
                       direction=graph_pb2.DIRECTION_BOTH)

m.graph.delete_node(ns, "doc-1", cascade=True)   # cascade removes incident edges
m.graph.delete_edge(ns, "e1")
```

`from` is a Python keyword, so the generated field keeps its proto name:
construct with `Edge(**{"from": "alice"})` and read it with
`getattr(edge, "from")` (there is no `from_` alias). Node and edge ids are
caller-supplied, so they can be shared with `semantic` record ids to compose
hybrid recall (semantic search → graph expand) in the agent. See
[Graph](../blocks/graph.md).

## TLS

```python
import grpc
creds = grpc.ssl_channel_credentials(
    root_certificates=open("server.crt","rb").read())
m = MemSidecar("memsidecar.example.com:7777",
               token=TOK,
               secure=True,
               channel_credentials=creds)
```

For mTLS, build the credentials with `private_key=` / `certificate_chain=`
in `grpc.ssl_channel_credentials`.

## Regenerating stubs

The protobuf + gRPC stubs ship in the package. To regenerate after a
proto change:

```bash
make proto-python
```

Buf uses **remote** plugins (`buf.build/protocolbuffers/python` +
`buf.build/grpc/python`), so no local `protoc-gen-*` install is needed.

## Smoke test

With a running sidecar and `MEMSIDECAR_TOKEN` exported:

```bash
cd sdk/python
.venv/bin/pytest tests/
```

Covers KV, Episodic, Semantic, Artifact, Lease round-trips.
