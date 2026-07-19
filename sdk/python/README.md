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

## LangGraph checkpointer

`memsidecar.ext.langgraph.MemSidecarSaver` implements LangGraph's
`BaseCheckpointSaver`, so a graph gets durable, multi-tenant thread state by
pointing at the sidecar — no bespoke persistence code. It stores checkpoints,
per-version channel blobs, and pending writes in a single **kv** namespace,
mirroring LangGraph's reference in-memory saver so the runtime behaves
identically (sync and async).

Install the extra (pulls in `langgraph-checkpoint`):

```bash
pip install "memsidecar[langgraph]"
```

The kv namespace must exist on the server and the token must grant kv
`get,put,delete,scan` on it:

```python
from memsidecar import MemSidecar
from memsidecar.ext.langgraph import MemSidecarSaver

client = MemSidecar("127.0.0.1:7777", token=MEMSIDECAR_TOKEN)
saver = MemSidecarSaver(client, namespace="checkpoints")

graph = builder.compile(checkpointer=saver)      # your StateGraph
cfg = {"configurable": {"thread_id": "user-42"}}
graph.invoke({"messages": [...]}, cfg)           # state persists on the sidecar
graph.invoke({"messages": [...]}, cfg)           # resumes the same thread
saver.delete_thread("user-42")                   # purge a thread
```

Per-thread reads (get-latest, `list`/history) use a kv prefix scan; the episodic
block isn't used here because it has no per-thread server-side filter, which is
the access pattern a checkpointer needs.

## CrewAI memory

`memsidecar.ext.crewai.MemSidecarStorage` implements CrewAI's `StorageBackend`
protocol (the pluggable storage under CrewAI 1.x's unified `Memory`), so a
crew's memories live on the sidecar instead of a local vector file.

Install the extra (pulls in `crewai`):

```bash
pip install "memsidecar[crewai]"
```

> **Install note.** The generated stubs require `protobuf>=7.35`, while
> CrewAI's `opentelemetry-proto` dependency still caps `protobuf<7`, so pip's
> resolver rejects the combined install. The two are compatible **at runtime**
> (CrewAI imports and runs fine on protobuf 7), so force it:
>
> ```bash
> pip install crewai
> pip install "protobuf>=7.35"        # runtime-compatible; overrides the cap
> pip install memsidecar --no-deps    # already have grpcio + protobuf
> ```

Both a `semantic/<namespace>` and a `kv/<namespace>` must exist on the server
(the token needs semantic + kv ops on them). CrewAI owns embedding — it fills
`MemoryRecord.embedding` before `save` and passes `search` a `query_embedding` —
so this backend stores those vectors as-is; the **semantic namespace's
`dimensions` must equal the CrewAI embedder's output dimension**.

```python
from crewai import Crew
from crewai.memory.unified_memory import Memory
from memsidecar import MemSidecar
from memsidecar.ext.crewai import MemSidecarStorage

client = MemSidecar("127.0.0.1:7777", token=MEMSIDECAR_TOKEN)
memory = Memory(storage=MemSidecarStorage(client, namespace="memories"))
crew = Crew(agents=[...], tasks=[...], memory=memory)
```

The **semantic** block holds the vector index (searched ids-only, then records
are rehydrated from kv); the **kv** block holds each record under `id/<id>` and
`rec<scope>/<id>`, which backs get-by-id plus the list/count/scope introspection
the memory runtime performs during encode and recall.

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
