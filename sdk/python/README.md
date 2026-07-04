# memsidecar — Python client

Thin Python client for the [memsidecar](../../README.md) agent memory sidecar.
Wraps the gRPC stubs with idiomatic Python and injects the capability token on
every call.

## Install (editable)

```bash
cd sdk/python
python3 -m venv .venv
.venv/bin/pip install -e ".[dev]"
```

## Quickstart

Assumes the sidecar is running with `configs/example.yaml` and you've minted a
token:

```bash
export MEMSIDECAR_PASETO_SECRET_HEX=...   # from configs/example.yaml comment
export MEMSIDECAR_TOKEN=$(./bin/memctl token issue \
    --tenant acme --agent agent-1 \
    --ns 'kv/*,episodic/events,semantic/notes,artifact/blobs,lease/locks,graph/knowledge' \
    --ops '*' --ttl 1h)
```

Then:

```python
import datetime as dt
from memsidecar import MemSidecar
from memsidecar.semantic.v1 import semantic_pb2
from memsidecar.graph.v1 import graph_pb2

with MemSidecar("127.0.0.1:7777", token=MEMSIDECAR_TOKEN) as m:
    # KV
    m.kv.put("scratchpad", "hello", b"world", ttl=dt.timedelta(minutes=5))
    rec = m.kv.get("scratchpad", "hello")
    print(rec.value)  # b"world"

    # Episodic
    m.episodic.append("events", "tool_call", b"...")
    for ev in m.episodic.range("events", limit=10):
        print(ev.cursor, ev.type)

    # Semantic — bitemporal & revisable. Default search returns only records
    # live and valid *now*; pass as_of=... or include_invalidated=True for
    # point-in-time / audit reads. `supersedes` retires the ids it revises.
    m.semantic.upsert("notes", [
        semantic_pb2.Record(id="a", content="apple"),
    ])
    for hit in m.semantic.search("notes", query_text="apple", top_k=3):
        print(hit.record.id, hit.score)

    # Artifact
    ref = m.artifact.put("blobs", b"binary bytes", content_type="application/octet-stream")
    got = m.artifact.get("blobs", ref.id)

    # Lease
    handle = m.lease.acquire("locks", "deploy", ttl=dt.timedelta(minutes=1))
    m.lease.release(handle.holder_id, "locks", "deploy")

    # Graph — typed nodes/edges + bounded traversal (hard-capped server-side).
    # `from` is a Python keyword, so pass it via **{"from": ...}.
    m.graph.upsert_nodes("knowledge", [
        graph_pb2.Node(id="alice", labels=["Person"]),
        graph_pb2.Node(id="doc-1", labels=["Document"]),
    ])
    m.graph.upsert_edges("knowledge", [
        graph_pb2.Edge(id="e1", type="AUTHORED", **{"from": "alice"}, to="doc-1"),
    ])
    sub = m.graph.traverse("knowledge", "alice", depth=2, max_nodes=100)
    print([n.id for n in sub.nodes])
```

## Regenerating proto stubs

The protobuf + gRPC stubs under `src/memsidecar/{kv,episodic,...}/v1/` are
generated from `proto/` via `buf`. From the repo root:

```bash
make proto-python
```

Buf calls remote plugins (`buf.build/protocolbuffers/python` and
`buf.build/grpc/python`) — no local protoc-gen-* binaries required.

## Smoke test

With a running sidecar and `MEMSIDECAR_TOKEN` exported, run:

```bash
.venv/bin/pytest tests/
```
