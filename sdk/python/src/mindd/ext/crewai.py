"""CrewAI memory storage backend backed by mindd.

``MindDStorage`` implements CrewAI's ``StorageBackend`` protocol (the
pluggable storage under CrewAI 1.x's unified ``Memory``), so a crew's memories
live on a running mindd instead of a local vector file::

    from crewai import Crew
    from crewai.memory.unified_memory import Memory
    from mindd import MindD
    from mindd.ext.crewai import MindDStorage

    client = MindD("127.0.0.1:7777", token=TOKEN)
    memory = Memory(storage=MindDStorage(client, namespace="memories"))
    crew = Crew(agents=[...], tasks=[...], memory=memory)

    # …or register it process-wide:
    from crewai.memory.storage.factory import set_memory_storage_factory
    set_memory_storage_factory(lambda spec: MindDStorage(client))

CrewAI owns embedding: it populates ``MemoryRecord.embedding`` before ``save``
and hands ``search`` a ``query_embedding``. This backend therefore stores those
vectors as-is and searches by vector — the mindd **semantic** namespace's
``dimensions`` must equal the CrewAI embedder's output dimension, and its
server-side embedder is never invoked on the write path.

Two blocks are used, under one shared namespace name:

* **semantic** — the vector index. ``save`` upserts (id, content, vector);
  ``search`` runs an ids-only ANN query, then records are rehydrated from kv.
* **kv** — the record store and scope index. Each record is written under
  ``id/<id>`` (direct lookup) and ``rec<scope>/<id>`` (prefix scan), which backs
  get_record, list/count, and the scope/category introspection the memory
  runtime calls during encode and recall.

Requires ``crewai`` (``pip install mindd[crewai]``).
"""
from __future__ import annotations

import asyncio
import json
from datetime import datetime
from typing import Any, Iterable, Iterator, Optional
from urllib.parse import quote

import grpc
from crewai.memory.storage.backend import StorageBackend
from crewai.memory.types import MemoryRecord, ScopeInfo

from ..client import MindD
from ..semantic.v1 import semantic_pb2

__all__ = ["MindDStorage"]


def _q(s: Any) -> str:
    return quote(str(s), safe="")


class MindDStorage:
    """A CrewAI ``StorageBackend`` whose storage is a mindd instance.

    Args:
        client: A connected :class:`mindd.MindD`.
        namespace: Name shared by the semantic and kv namespaces to use. Both
            ``semantic/<namespace>`` and ``kv/<namespace>`` must be declared on
            the server, and the token must grant the relevant ops on them.
        over_fetch: When a search has scope/category/metadata filters, fetch
            this multiple of ``limit`` candidates before filtering client-side
            (mirrors the reference LanceDB backend).
    """

    def __init__(
        self,
        client: MindD,
        *,
        namespace: str = "memories",
        semantic_namespace: Optional[str] = None,
        kv_namespace: Optional[str] = None,
        over_fetch: int = 3,
    ) -> None:
        self.client = client
        self.semantic_ns = semantic_namespace or namespace
        self.kv_ns = kv_namespace or namespace
        self._over = max(1, over_fetch)

    # -- key builders -------------------------------------------------------

    @staticmethod
    def _id_key(rid: str) -> str:
        return f"id/{_q(rid)}"

    @staticmethod
    def _scope_key(scope: str, rid: str) -> str:
        # Scope slashes are kept literal so a scope prefix is a kv key prefix.
        return f"rec{scope}/{_q(rid)}"

    @staticmethod
    def _scan_prefix(scope_prefix: Optional[str]) -> str:
        if not scope_prefix or not scope_prefix.strip("/"):
            return "id/"  # every record has exactly one id/ key -> "all"
        return f"rec{scope_prefix.rstrip('/')}"

    # -- (de)serialization --------------------------------------------------

    @staticmethod
    def _dump(record: MemoryRecord) -> bytes:
        # Store the whole record, embedding included, so read-path records
        # round-trip losslessly — a read-modify-update must not lose the vector.
        # MemoryRecord.embedding is Field(exclude=True), so add it back by hand.
        data = record.model_dump(mode="json")
        data["embedding"] = record.embedding
        return json.dumps(data).encode("utf-8")

    @staticmethod
    def _load(raw: bytes) -> MemoryRecord:
        return MemoryRecord.model_validate(json.loads(raw))

    @staticmethod
    def _scope_matches(scope: str, scope_prefix: Optional[str]) -> bool:
        prefix = (scope_prefix or "").rstrip("/")
        if not prefix.strip("/"):
            return True
        return scope.startswith(prefix)

    # -- internals ----------------------------------------------------------

    def _scan_records(self, scope_prefix: Optional[str]) -> Iterator[MemoryRecord]:
        prefix = self._scan_prefix(scope_prefix)
        for item in self.client.kv.scan(self.kv_ns, key_prefix=prefix, include_values=True):
            yield self._load(item.value)

    def _drop_stale_scope_key(self, record: MemoryRecord) -> None:
        # save/update are upserts by id; if a record's scope changed since it was
        # last written, its old rec<scope>/<id> key must be removed or it becomes
        # an orphan (ghost in counts/lists, and a delete-by-old-scope would erase
        # the relocated record).
        old = self.get_record(record.id)
        if old is not None and old.scope != record.scope:
            self.client.kv.delete(self.kv_ns, self._scope_key(old.scope, record.id))

    def _save_one(self, record: MemoryRecord) -> None:
        self._drop_stale_scope_key(record)
        self.client.semantic.upsert(
            self.semantic_ns,
            [semantic_pb2.Record(id=record.id, content=record.content, vector=(record.embedding or []))],
        )
        raw = self._dump(record)
        self.client.kv.put(self.kv_ns, self._id_key(record.id), raw)
        self.client.kv.put(self.kv_ns, self._scope_key(record.scope, record.id), raw)

    def _delete_one(self, record: MemoryRecord) -> None:
        try:
            self.client.semantic.delete(self.semantic_ns, record.id, hard=True)
        except grpc.RpcError:
            pass  # a missing vector must not block index cleanup
        self.client.kv.delete(self.kv_ns, self._id_key(record.id))
        self.client.kv.delete(self.kv_ns, self._scope_key(record.scope, record.id))

    # -- StorageBackend: writes --------------------------------------------

    def save(self, records: list[MemoryRecord]) -> None:
        if not records:
            return
        for r in records:
            self._drop_stale_scope_key(r)
        self.client.semantic.upsert(
            self.semantic_ns,
            [semantic_pb2.Record(id=r.id, content=r.content, vector=(r.embedding or [])) for r in records],
        )
        for r in records:
            raw = self._dump(r)
            self.client.kv.put(self.kv_ns, self._id_key(r.id), raw)
            self.client.kv.put(self.kv_ns, self._scope_key(r.scope, r.id), raw)

    def update(self, record: MemoryRecord) -> None:
        self._save_one(record)

    def delete(
        self,
        scope_prefix: Optional[str] = None,
        categories: Optional[list[str]] = None,
        record_ids: Optional[list[str]] = None,
        older_than: Optional[datetime] = None,
        metadata_filter: Optional[dict[str, Any]] = None,
    ) -> int:
        if record_ids is not None:
            matched = [r for r in (self.get_record(i) for i in record_ids) if r is not None]
        else:
            matched = list(self._scan_records(scope_prefix))
            if categories:
                matched = [r for r in matched if any(c in r.categories for c in categories)]
            if metadata_filter:
                matched = [r for r in matched if all(r.metadata.get(k) == v for k, v in metadata_filter.items())]
            if older_than is not None:
                matched = [r for r in matched if r.created_at < older_than]
        for r in matched:
            self._delete_one(r)
        return len(matched)

    def reset(self, scope_prefix: Optional[str] = None) -> None:
        self.delete(scope_prefix=scope_prefix)

    # -- StorageBackend: reads ---------------------------------------------

    def search(
        self,
        query_embedding: list[float],
        scope_prefix: Optional[str] = None,
        categories: Optional[list[str]] = None,
        metadata_filter: Optional[dict[str, Any]] = None,
        limit: int = 10,
        min_score: float = 0.0,
    ) -> list[tuple[MemoryRecord, float]]:
        if not query_embedding or limit <= 0:
            return []
        filtered = bool(scope_prefix and scope_prefix.strip("/")) or bool(categories) or bool(metadata_filter)
        top_k = limit * self._over if filtered else limit
        hits = self.client.semantic.search(
            self.semantic_ns, query_vector=query_embedding, top_k=top_k, ids_only=True
        )
        out: list[tuple[MemoryRecord, float]] = []
        for hit in hits:
            record = self.get_record(hit.record.id)
            if record is None:
                continue
            if not self._scope_matches(record.scope, scope_prefix):
                continue
            if categories and not any(c in record.categories for c in categories):
                continue
            if metadata_filter and not all(record.metadata.get(k) == v for k, v in metadata_filter.items()):
                continue
            # Gate on the raw cosine ([-1, 1]) so min_score=0.0 excludes
            # anti-correlated hits; only the returned score is clamped to [0, 1].
            raw = float(hit.score)
            if raw >= min_score:
                out.append((record, max(0.0, min(1.0, raw))))
                if len(out) >= limit:
                    break
        return out[:limit]

    def get_record(self, record_id: str) -> Optional[MemoryRecord]:
        resp = self.client.kv.get(self.kv_ns, self._id_key(record_id))
        return self._load(resp.value) if resp.found else None

    def list_records(
        self, scope_prefix: Optional[str] = None, limit: int = 200, offset: int = 0
    ) -> list[MemoryRecord]:
        records = sorted(self._scan_records(scope_prefix), key=lambda r: r.created_at, reverse=True)
        return records[offset : offset + limit]

    def count(self, scope_prefix: Optional[str] = None) -> int:
        prefix = self._scan_prefix(scope_prefix)
        return sum(1 for _ in self.client.kv.scan(self.kv_ns, key_prefix=prefix))

    def list_categories(self, scope_prefix: Optional[str] = None) -> dict[str, int]:
        counts: dict[str, int] = {}
        for record in self._scan_records(scope_prefix):
            for category in record.categories:
                counts[category] = counts.get(category, 0) + 1
        return counts

    def list_scopes(self, parent: str = "/") -> list[str]:
        parent_norm = "/" + parent.strip("/") if parent.strip("/") else "/"
        children: set[str] = set()
        for record in self._scan_records(None if parent_norm == "/" else parent_norm):
            scope = record.scope
            if parent_norm == "/":
                rest = scope.strip("/")
                base = ""
            else:
                if scope != parent_norm and not scope.startswith(parent_norm + "/"):
                    continue
                rest = scope[len(parent_norm):].strip("/")
                base = parent_norm.rstrip("/")
            if not rest:
                continue
            children.add(f"{base}/{rest.split('/', 1)[0]}")
        return sorted(children)

    def get_scope_info(self, scope: str) -> ScopeInfo:
        records = list(self._scan_records(scope))
        categories = sorted({c for r in records for c in r.categories})
        dates = [r.created_at for r in records]
        return ScopeInfo(
            path=scope,
            record_count=len(records),
            categories=categories,
            oldest_record=min(dates) if dates else None,
            newest_record=max(dates) if dates else None,
            child_scopes=self.list_scopes(scope),
        )

    # -- StorageBackend: async (offload the blocking client to a thread) ----

    async def asave(self, records: list[MemoryRecord]) -> None:
        await asyncio.to_thread(self.save, records)

    async def asearch(
        self,
        query_embedding: list[float],
        scope_prefix: Optional[str] = None,
        categories: Optional[list[str]] = None,
        metadata_filter: Optional[dict[str, Any]] = None,
        limit: int = 10,
        min_score: float = 0.0,
    ) -> list[tuple[MemoryRecord, float]]:
        return await asyncio.to_thread(
            self.search, query_embedding, scope_prefix, categories, metadata_filter, limit, min_score
        )

    async def adelete(
        self,
        scope_prefix: Optional[str] = None,
        categories: Optional[list[str]] = None,
        record_ids: Optional[list[str]] = None,
        older_than: Optional[datetime] = None,
        metadata_filter: Optional[dict[str, Any]] = None,
    ) -> int:
        return await asyncio.to_thread(
            self.delete, scope_prefix, categories, record_ids, older_than, metadata_filter
        )
