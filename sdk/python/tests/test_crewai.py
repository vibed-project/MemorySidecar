"""Integration tests for the CrewAI storage backend (mindd.ext.crewai).

Exercises the StorageBackend contract directly (supplying embeddings + query
vectors, so no LLM is needed). Needs a running mindd with a semantic and a
kv namespace of the same name, whose semantic embedder dimension is 8, and a
token granting semantic + kv ops on them. Set MINDD_TOKEN (and optionally
MINDD_ADDR, MINDD_MEM_NS); the tests skip otherwise.
"""
import asyncio
import os
import uuid
from datetime import datetime, timezone

import pytest

pytestmark = pytest.mark.skipif(
    not os.getenv("MINDD_TOKEN"),
    reason="needs a running mindd (set MINDD_TOKEN)",
)

DIM = 8


def _vec(i: float, at: int = 0) -> list[float]:
    v = [0.0] * DIM
    v[at] = i
    return v


@pytest.fixture
def storage():
    from mindd import MindD
    from mindd.ext.crewai import MindDStorage

    addr = os.getenv("MINDD_ADDR", "127.0.0.1:7777")
    ns = os.getenv("MINDD_MEM_NS", "memories")
    client = MindD(addr, token=os.environ["MINDD_TOKEN"])
    try:
        yield MindDStorage(client, namespace=ns)
    finally:
        client.close()


def _rec(content, embedding, scope="/", categories=None, metadata=None, rid=None):
    from crewai.memory.types import MemoryRecord

    now = datetime.now(timezone.utc)
    kw = dict(
        content=content, embedding=embedding, scope=scope,
        categories=categories or [], metadata=metadata or {},
        created_at=now, last_accessed=now,
    )
    if rid:
        kw["id"] = rid
    return MemoryRecord(**kw)


def test_protocol_conformance(storage):
    from crewai.memory.storage.backend import StorageBackend

    assert isinstance(storage, StorageBackend)


def test_backend_crud_and_search(storage):
    root = "/t" + uuid.uuid4().hex[:10]
    a = _rec("apple", _vec(1.0, 0), scope=root + "/a", categories=["fruit"], metadata={"k": "1"})
    b = _rec("carrot", _vec(1.0, 1), scope=root + "/b", categories=["veg"], metadata={"k": "2"})
    c = _rec("cherry", _vec(1.0, 2), scope=root + "/a", categories=["fruit", "red"], metadata={"k": "1"})
    storage.save([a, b, c])

    # Counts + scope introspection.
    assert storage.count(root) == 3
    assert storage.count(root + "/a") == 2
    assert storage.count(root + "/b") == 1
    assert storage.list_scopes(root) == sorted([root + "/a", root + "/b"])
    assert storage.list_categories(root) == {"fruit": 2, "veg": 1, "red": 1}
    info = storage.get_scope_info(root)
    assert info.record_count == 3
    assert set(info.categories) == {"fruit", "veg", "red"}
    assert sorted(info.child_scopes) == sorted([root + "/a", root + "/b"])

    # get_record + list_records.
    assert storage.get_record(a.id).content == "apple"
    assert storage.get_record("nope-" + uuid.uuid4().hex) is None
    assert len(storage.list_records(root)) == 3
    assert len(storage.list_records(root, limit=2)) == 2

    # Vector search: a query near a's vector ranks a first, score in [0,1].
    hits = storage.search([0.9, 0.1] + [0.0] * (DIM - 2), limit=3)
    assert hits, "expected search hits"
    top, score = hits[0]
    assert top.id == a.id
    assert 0.0 <= score <= 1.0

    # Filters: scope, category, metadata each narrow the result set.
    scoped = storage.search(_vec(1.0, 0), scope_prefix=root + "/b", limit=5)
    assert all(r.scope.startswith(root + "/b") for r, _ in scoped)
    assert a.id not in {r.id for r, _ in scoped}
    cat = storage.search(_vec(1.0, 1), categories=["veg"], limit=5)
    assert {r.id for r, _ in cat} == {b.id}
    md = storage.search(_vec(1.0, 1), metadata_filter={"k": "2"}, limit=5)
    assert {r.id for r, _ in md} == {b.id}
    # min_score drops the near-orthogonal matches.
    strong = storage.search([0.95, 0.05] + [0.0] * (DIM - 2), limit=5, min_score=0.9)
    assert {r.id for r, _ in strong} == {a.id}

    # update: move a to a new scope; the old scope key must not linger.
    a_moved = _rec("apple", _vec(1.0, 0), scope=root + "/c", categories=["fruit"], metadata={"k": "1"}, rid=a.id)
    storage.update(a_moved)
    assert storage.count(root + "/a") == 1  # only c remains
    assert storage.count(root + "/c") == 1
    assert storage.get_record(a.id).scope == root + "/c"

    # delete by ids, then by scope.
    assert storage.delete(record_ids=[c.id]) == 1
    assert storage.get_record(c.id) is None
    assert storage.count(root) == 2
    assert storage.delete(scope_prefix=root + "/b") == 1
    assert storage.count(root) == 1

    # reset wipes the subtree.
    storage.reset(root)
    assert storage.count(root) == 0


def test_write_invariants(storage):
    root = "/w" + uuid.uuid4().hex[:10]

    # Re-saving an id at a new scope must drop the old scope key, not orphan it.
    x = _rec("x", _vec(1.0, 0), scope=root + "/a", rid="X-" + uuid.uuid4().hex)
    storage.save([x])
    storage.save([_rec("x", _vec(1.0, 0), scope=root + "/b", rid=x.id)])
    assert storage.count(root + "/a") == 0
    assert storage.count(root + "/b") == 1
    assert storage.count(root) == 1
    # Deleting the OLD scope must not destroy the relocated record.
    assert storage.delete(scope_prefix=root + "/a") == 0
    assert storage.get_record(x.id) is not None
    assert storage.get_record(x.id).scope == root + "/b"

    # A read-modify-update keeps the vector: the record round-trips with its
    # embedding, and update() must not wipe the searchable vector.
    got = storage.get_record(x.id)
    assert got.embedding is not None
    got.importance = 0.9
    storage.update(got)
    assert x.id in {r.id for r, _ in storage.search(_vec(1.0, 0), scope_prefix=root, limit=5)}
    assert storage.get_record(x.id).importance == 0.9

    storage.reset(root)
    assert storage.count(root) == 0


def test_min_score_excludes_anticorrelated(storage):
    root = "/m" + uuid.uuid4().hex[:10]
    a = _rec("aligned", _vec(1.0, 0), scope=root)
    b = _rec("opposite", [-1.0] + [0.0] * (DIM - 1), scope=root)
    storage.save([a, b])
    # min_score=0.0 must drop the anti-correlated record (cosine -1), not clamp
    # it to 0.0 and keep it.
    ids = {r.id for r, _ in storage.search(_vec(1.0, 0), scope_prefix=root, limit=10, min_score=0.0)}
    assert a.id in ids
    assert b.id not in ids
    storage.reset(root)


def test_async_roundtrip(storage):
    root = "/a" + uuid.uuid4().hex[:10]

    async def run():
        r = _rec("async-note", _vec(1.0, 3), scope=root)
        await storage.asave([r])
        res = await storage.asearch(_vec(1.0, 3), scope_prefix=root, limit=5)
        assert any(rr.id == r.id for rr, _ in res)
        assert await storage.adelete(scope_prefix=root) >= 1

    asyncio.run(run())
    assert storage.count(root) == 0
