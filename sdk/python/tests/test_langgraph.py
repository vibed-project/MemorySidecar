"""Integration tests for the LangGraph checkpointer (memsidecar.ext.langgraph).

Needs a running memsidecar with a kv namespace for checkpoints and a token that
grants kv get/put/delete/scan on it. Set MEMSIDECAR_TOKEN (and optionally
MEMSIDECAR_ADDR, MEMSIDECAR_CHECKPOINT_NS); the tests skip otherwise.
"""
import os
import uuid

import pytest

pytestmark = pytest.mark.skipif(
    not os.getenv("MEMSIDECAR_TOKEN"),
    reason="needs a running memsidecar (set MEMSIDECAR_TOKEN)",
)


@pytest.fixture
def saver():
    from memsidecar import MemSidecar
    from memsidecar.ext.langgraph import MemSidecarSaver

    addr = os.getenv("MEMSIDECAR_ADDR", "127.0.0.1:7777")
    ns = os.getenv("MEMSIDECAR_CHECKPOINT_NS", "checkpoints")
    client = MemSidecar(addr, token=os.environ["MEMSIDECAR_TOKEN"])
    try:
        yield MemSidecarSaver(client, namespace=ns)
    finally:
        client.close()


def _cfg(thread, cid=None):
    c = {"thread_id": thread, "checkpoint_ns": ""}
    if cid:
        c["checkpoint_id"] = cid
    return {"configurable": c}


def test_direct_roundtrip_writes_and_list(saver):
    thread = "direct-" + uuid.uuid4().hex

    # cp1: one channel.
    cp1 = {
        "v": 1, "id": "0000000001", "ts": "2026-01-01T00:00:00Z",
        "channel_values": {"messages": ["a"]},
        "channel_versions": {"messages": 1},
        "versions_seen": {}, "updated_channels": ["messages"],
    }
    nc1 = saver.put(_cfg(thread), cp1, {"source": "input", "step": -1, "parents": {}}, {"messages": 1})

    # cp2 builds on cp1: bumps messages, adds count. Only new_versions get blobs;
    # get_tuple must reassemble channel_values from the versioned blobs.
    cp2 = {
        "v": 1, "id": "0000000002", "ts": "2026-01-01T00:00:01Z",
        "channel_values": {"messages": ["a", "b"], "count": 5},
        "channel_versions": {"messages": 2, "count": 1},
        "versions_seen": {}, "updated_channels": ["messages", "count"],
    }
    nc2 = saver.put(nc1, cp2, {"source": "loop", "step": 0, "parents": {}}, {"messages": 2, "count": 1})

    got = saver.get_tuple(_cfg(thread, "0000000002"))
    assert got is not None
    assert got.checkpoint["channel_values"] == {"messages": ["a", "b"], "count": 5}
    assert got.parent_config["configurable"]["checkpoint_id"] == "0000000001"

    # No checkpoint_id -> latest (max id).
    latest = saver.get_tuple(_cfg(thread))
    assert latest.config["configurable"]["checkpoint_id"] == "0000000002"

    # Pending writes, including a special (negative-index) channel.
    saver.put_writes(nc2, [("messages", "c"), ("__error__", "boom")], task_id="t1")
    pw = saver.get_tuple(_cfg(thread, "0000000002")).pending_writes
    assert ("t1", "messages", "c") in pw
    assert ("t1", "__error__", "boom") in pw

    # Regular writes are first-wins: re-emitting the same (task_id, idx) is a
    # no-op, so a nondeterministic re-run cannot clobber the recorded value.
    saver.put_writes(nc2, [("messages", "c-new")], task_id="t1")
    pw = saver.get_tuple(_cfg(thread, "0000000002")).pending_writes
    assert ("t1", "messages", "c") in pw
    assert ("t1", "messages", "c-new") not in pw
    # Special (negative-index) channels are last-wins: they overwrite.
    saver.put_writes(nc2, [("__error__", "boom2")], task_id="t1")
    pw = saver.get_tuple(_cfg(thread, "0000000002")).pending_writes
    assert ("t1", "__error__", "boom2") in pw
    assert ("t1", "__error__", "boom") not in pw

    def ids(**kw):
        return [t.config["configurable"]["checkpoint_id"] for t in saver.list(_cfg(thread), **kw)]

    assert ids() == ["0000000002", "0000000001"]          # newest first
    assert ids(limit=1) == ["0000000002"]                 # limit
    assert ids(before=_cfg(thread, "0000000002")) == ["0000000001"]  # before
    assert ids(filter={"source": "loop"}) == ["0000000002"]          # metadata filter

    saver.delete_thread(thread)
    assert saver.get_tuple(_cfg(thread)) is None
    assert ids() == []


def test_real_graph_persists_across_invocations(saver):
    pytest.importorskip("langgraph.graph")
    import operator
    from typing import Annotated, TypedDict

    from langgraph.graph import END, START, StateGraph

    class State(TypedDict):
        count: Annotated[int, operator.add]

    def bump(_state: State):
        return {"count": 1}

    builder = StateGraph(State)
    builder.add_node("bump", bump)
    builder.add_edge(START, "bump")
    builder.add_edge("bump", END)
    app = builder.compile(checkpointer=saver)

    thread = "graph-" + uuid.uuid4().hex
    cfg = {"configurable": {"thread_id": thread}}

    r1 = app.invoke({"count": 0}, cfg)
    r2 = app.invoke({"count": 0}, cfg)  # resumes from the persisted checkpoint

    assert r1["count"] >= 1
    assert r2["count"] > r1["count"], "state did not persist across invocations"

    # get_state / get_state_history route through get_tuple / list.
    assert app.get_state(cfg).values["count"] == r2["count"]
    history = list(app.get_state_history(cfg))
    assert len(history) >= 2

    saver.delete_thread(thread)
    assert app.get_state(cfg).values == {}
