"""End-to-end smoke test against a running memsidecar.

Requires the server to be running locally at 127.0.0.1:7777 with the dev
config (configs/example.yaml). Driven by the SDK README quickstart.
"""
from __future__ import annotations

import datetime as dt
import os
import time
import uuid

import pytest

from memsidecar import MemSidecar


TARGET = os.environ.get("MEMSIDECAR_TARGET", "127.0.0.1:7777")
TOKEN = os.environ.get("MEMSIDECAR_TOKEN")


pytestmark = pytest.mark.skipif(not TOKEN, reason="MEMSIDECAR_TOKEN not set")


@pytest.fixture
def client():
    with MemSidecar(TARGET, token=TOKEN) as m:
        yield m


def test_kv_roundtrip(client):
    key = f"smoke-{uuid.uuid4()}"
    client.kv.put("scratchpad", key, b"hello", content_type="text/plain")
    rec = client.kv.get("scratchpad", key)
    assert rec.found
    assert rec.value == b"hello"
    assert client.kv.delete("scratchpad", key).existed


def test_episodic_append_range(client):
    n = client.episodic.append("events", "smoke", b"x")
    assert n.cursor > 0
    events = list(client.episodic.range("events", after_cursor=n.cursor - 1, limit=1))
    assert events and events[0].cursor == n.cursor


def test_semantic_upsert_search(client):
    from memsidecar.semantic.v1 import semantic_pb2
    rid = f"smoke-{uuid.uuid4()}"
    client.semantic.upsert("notes", [
        semantic_pb2.Record(id=rid, content="banana split"),
    ])
    hits = client.semantic.search("notes", query_text="banana split", top_k=1)
    assert hits and hits[0].record.id == rid
    client.semantic.delete("notes", rid)


def test_artifact_roundtrip(client):
    rid = f"smoke-{uuid.uuid4()}"
    payload = os.urandom(2048)
    ref = client.artifact.put("blobs", payload, id=rid, content_type="application/octet-stream")
    assert ref.size == len(payload)
    got = client.artifact.get("blobs", rid)
    assert got == payload
    assert client.artifact.delete("blobs", rid)


def test_lease_acquire_release(client):
    key = f"smoke-{uuid.uuid4()}"
    handle = client.lease.acquire("locks", key, ttl=dt.timedelta(seconds=30))
    assert handle.holder_id
    info = client.lease.inspect("locks", key)
    assert info.held and info.handle.holder_id == handle.holder_id
    assert client.lease.release(handle.holder_id, "locks", key)
