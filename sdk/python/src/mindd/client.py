"""High-level Python client for mindd.

Usage::

    from mindd.client import MindD

    with MindD("127.0.0.1:7777", token=os.environ["MINDD_TOKEN"]) as m:
        m.kv.put("scratchpad", "hello", b"world")
        rec = m.kv.get("scratchpad", "hello")
        print(rec.value)

The block clients (`m.kv`, `m.episodic`, `m.semantic`, `m.artifact`, `m.lease`,
`m.graph`) plus `m.admin` (cross-namespace introspection) wrap the raw generated
stubs with idiomatic Python types and inject the capability token on every call.
"""
from __future__ import annotations

import datetime as _dt
from typing import Iterable, Iterator, List, Mapping, Optional

import grpc
from google.protobuf import duration_pb2 as _duration_pb2
from google.protobuf import timestamp_pb2 as _timestamp_pb2

from ._auth import CapabilityInterceptor
from .kv.v1 import kv_pb2, kv_pb2_grpc
from .episodic.v1 import episodic_pb2, episodic_pb2_grpc
from .semantic.v1 import semantic_pb2, semantic_pb2_grpc
from .artifact.v1 import artifact_pb2, artifact_pb2_grpc
from .lease.v1 import lease_pb2, lease_pb2_grpc
from .graph.v1 import graph_pb2, graph_pb2_grpc
from .admin.v1 import admin_pb2, admin_pb2_grpc


# ---------------------------------------------------------------------------
# Helpers


def _to_duration(d) -> Optional[_duration_pb2.Duration]:
    if d is None:
        return None
    if isinstance(d, _duration_pb2.Duration):
        return d
    if isinstance(d, _dt.timedelta):
        out = _duration_pb2.Duration()
        out.FromTimedelta(d)
        return out
    if isinstance(d, (int, float)):
        out = _duration_pb2.Duration()
        out.FromSeconds(int(d))
        return out
    raise TypeError(f"unsupported duration type {type(d).__name__}")


def _to_timestamp(t) -> Optional[_timestamp_pb2.Timestamp]:
    if t is None:
        return None
    if isinstance(t, _timestamp_pb2.Timestamp):
        return t
    if isinstance(t, _dt.datetime):
        out = _timestamp_pb2.Timestamp()
        out.FromDatetime(t)
        return out
    raise TypeError(f"unsupported timestamp type {type(t).__name__}")


# ---------------------------------------------------------------------------
# Block clients


class _KV:
    def __init__(self, channel: grpc.Channel):
        self._stub = kv_pb2_grpc.KVStub(channel)

    def put(
        self,
        namespace: str,
        key: str,
        value: bytes,
        *,
        ttl=None,
        content_type: str = "",
        metadata: Optional[Mapping[str, str]] = None,
        if_version: Optional[int] = None,
    ) -> kv_pb2.PutResponse:
        req = kv_pb2.PutRequest(
            namespace=namespace, key=key, value=value, content_type=content_type,
            metadata=dict(metadata or {}),
        )
        ttlpb = _to_duration(ttl)
        if ttlpb is not None:
            req.ttl.CopyFrom(ttlpb)
        if if_version is not None:
            req.if_version = if_version
        return self._stub.Put(req)

    def get(self, namespace: str, key: str) -> kv_pb2.GetResponse:
        return self._stub.Get(kv_pb2.GetRequest(namespace=namespace, key=key))

    def multi_get(self, namespace: str, keys: Iterable[str]) -> List[kv_pb2.KVItem]:
        """Fetch many keys in one round-trip. Missing or expired keys are
        omitted and repeats are deduplicated; results are ordered by key.
        """
        return list(self._stub.MultiGet(kv_pb2.MultiGetRequest(
            namespace=namespace, keys=list(keys),
        )).items)

    def delete(
        self, namespace: str, key: str, *, if_version: Optional[int] = None,
    ) -> kv_pb2.DeleteResponse:
        req = kv_pb2.DeleteRequest(namespace=namespace, key=key)
        if if_version is not None:
            req.if_version = if_version
        return self._stub.Delete(req)

    def scan(
        self, namespace: str, *, key_prefix: str = "", limit: int = 0,
        include_values: bool = False, start_after: str = "",
    ) -> Iterator[kv_pb2.KVItem]:
        """Stream keys in ascending (byte) order. ``start_after`` is an
        exclusive resume cursor — pass the last key from the previous page to
        continue; ``limit`` caps the page.
        """
        req = kv_pb2.ScanRequest(
            namespace=namespace, key_prefix=key_prefix, limit=limit,
            include_values=include_values, start_after=start_after,
        )
        return iter(self._stub.Scan(req))


class _Episodic:
    def __init__(self, channel: grpc.Channel):
        self._stub = episodic_pb2_grpc.EpisodicStub(channel)

    def append(
        self, namespace: str, type: str, payload: bytes = b"",
        *, metadata: Optional[Mapping[str, str]] = None,
        role: str = "", session_id: str = "",
        dedup_key: str = "", supersedes: Optional[Iterable[str]] = None,
        source: str = "",
    ) -> episodic_pb2.Event:
        """Append an event. ``dedup_key`` makes the write idempotent under retry
        — the first append for a (namespace, dedup_key) writes, later ones
        return the stored event unchanged. ``supersedes`` tombstones the named
        earlier events (revisability); ``source`` is an opaque provenance handle.
        """
        resp = self._stub.Append(episodic_pb2.AppendRequest(
            namespace=namespace, type=type, payload=payload,
            metadata=dict(metadata or {}), role=role, session_id=session_id,
            dedup_key=dedup_key, supersedes=list(supersedes or []), source=source,
        ))
        return resp.event

    def range(
        self, namespace: str, *,
        after_cursor: int = 0, before_cursor: int = 0,
        limit: int = 0, reverse: bool = False,
        after_time: Optional[_dt.datetime] = None,
        before_time: Optional[_dt.datetime] = None,
        include_deleted: bool = False,
        session_id: str = "", role: str = "", type: str = "",
    ) -> Iterator[episodic_pb2.Event]:
        """Replay events. ``after_time``/``before_time`` add an exclusive
        event-timestamp window, combined (AND) with the cursor bounds.

        ``session_id``/``role``/``type`` are equality predicates (empty = no
        filter) ANDed with the window — e.g. reconstruct one conversation with
        ``session_id="s1"``. ``include_deleted=True`` also returns tombstoned
        (superseded or Expire'd) events.
        """
        req = episodic_pb2.RangeRequest(
            namespace=namespace, after_cursor=after_cursor,
            before_cursor=before_cursor, limit=limit, reverse=reverse,
            include_deleted=include_deleted,
            session_id=session_id, role=role, type=type,
        )
        at = _to_timestamp(after_time)
        if at is not None:
            req.after_time.CopyFrom(at)
        bt = _to_timestamp(before_time)
        if bt is not None:
            req.before_time.CopyFrom(bt)
        return iter(self._stub.Range(req))

    def tail(
        self, namespace: str, *,
        after_cursor: int = 0, include_historical: bool = False,
    ) -> Iterator[episodic_pb2.Event]:
        req = episodic_pb2.TailRequest(
            namespace=namespace, after_cursor=after_cursor,
            include_historical=include_historical,
        )
        return iter(self._stub.Tail(req))

    def expire(
        self, namespace: str, *,
        action: "episodic_pb2.ExpireAction.ValueType", max_rows: int,
        before_cursor: int = 0, before_time: Optional[_dt.datetime] = None,
    ) -> int:
        """Tombstone or remove events in a bounded retention window, in one
        server-side operation. The window is a cursor/time upper bound — at
        least one of ``before_cursor`` / ``before_time`` is required. ``action``
        is an ``episodic_pb2.ExpireAction`` value (``EXPIRE_ACTION_SOFT_DELETE``
        / ``_HARD_DELETE``); ``max_rows`` caps the affected set (oldest-first)
        and is required. Returns the number of events affected.
        """
        req = episodic_pb2.ExpireRequest(
            namespace=namespace, before_cursor=before_cursor,
            action=action, max_rows=max_rows,
        )
        bt = _to_timestamp(before_time)
        if bt is not None:
            req.before_time.CopyFrom(bt)
        return self._stub.Expire(req).affected


class _Semantic:
    def __init__(self, channel: grpc.Channel):
        self._stub = semantic_pb2_grpc.SemanticStub(channel)

    def upsert(
        self, namespace: str, records: Iterable[semantic_pb2.Record],
    ) -> semantic_pb2.UpsertResponse:
        """Upsert records. The response carries the assigned ``ids`` and the new
        per-id ``versions`` (aligned with ids). Lifecycle fields
        (``valid_from``/``valid_to``/``deleted_at``/``supersedes``/``source``/
        ``if_version``) are set directly on each ``semantic_pb2.Record``.
        """
        return self._stub.Upsert(semantic_pb2.UpsertRequest(
            namespace=namespace, records=list(records),
        ))

    def search(
        self, namespace: str, *,
        query_text: str = "", query_vector: Optional[Iterable[float]] = None,
        top_k: int = 10, filter: Optional[Mapping[str, str]] = None,
        predicates: Optional[Iterable[semantic_pb2.FieldPredicate]] = None,
        created_after: Optional[_dt.datetime] = None,
        created_before: Optional[_dt.datetime] = None,
        mode: "semantic_pb2.SearchMode.ValueType" = semantic_pb2.SEARCH_MODE_UNSPECIFIED,
        rerank_candidate_k: int = 0,
        include_payload: bool = False, include_vector: bool = False,
        as_of: Optional[_dt.datetime] = None, include_invalidated: bool = False,
        ids_only: bool = False,
    ) -> List[semantic_pb2.Hit]:
        """Search a namespace. By default only records live and valid *now* are
        returned; pass ``as_of`` for point-in-time recall or
        ``include_invalidated=True`` to also see tombstoned/expired records.

        ``filter`` is exact-match; ``predicates`` adds ranges and set membership
        (``semantic_pb2.FieldPredicate`` with ``PREDICATE_OP_GT``/``_IN``/…),
        ANDed with ``filter``.

        ``ids_only=True`` returns just each hit's ``record.id`` and ``score``
        (content/payload/vector/metadata are skipped) — a cheap seed set whose
        ids you can expand through the graph block's ``neighbors``/``traverse``.

        ``mode`` selects the retrieval lanes: ``SEARCH_MODE_DENSE`` (default,
        vector-only), ``SEARCH_MODE_SPARSE`` (lexical), or ``SEARCH_MODE_HYBRID``
        (RRF fusion of both). SPARSE and HYBRID require ``query_text``;
        ``rerank_candidate_k`` bounds each lane's candidate depth before fusion.
        """
        req = semantic_pb2.SearchRequest(
            namespace=namespace, query_text=query_text, top_k=top_k,
            filter=dict(filter or {}), predicates=list(predicates or []),
            mode=mode, rerank_candidate_k=rerank_candidate_k,
            include_payload=include_payload, include_vector=include_vector,
            include_invalidated=include_invalidated, ids_only=ids_only,
        )
        if query_vector is not None:
            req.query_vector.extend(query_vector)
        ts = _to_timestamp(as_of)
        if ts is not None:
            req.as_of.CopyFrom(ts)
        ca = _to_timestamp(created_after)
        if ca is not None:
            req.created_after.CopyFrom(ca)
        cb = _to_timestamp(created_before)
        if cb is not None:
            req.created_before.CopyFrom(cb)
        return list(self._stub.Search(req).hits)

    def delete(self, namespace: str, id: str, *, hard: bool = False) -> bool:
        """Delete a record. Default is a soft delete (tombstone, still visible
        via ``include_invalidated``); pass ``hard=True`` to remove it entirely.
        """
        return self._stub.Delete(semantic_pb2.DeleteRequest(
            namespace=namespace, id=id, hard=hard,
        )).existed

    def expire(
        self, namespace: str, *,
        action: "semantic_pb2.ExpireAction.ValueType", max_rows: int,
        filter: Optional[Mapping[str, str]] = None,
    ) -> int:
        """Apply a bounded lifecycle action to every record matching ``filter``
        (empty matches all), in one server-side operation. ``action`` is a
        ``semantic_pb2.ExpireAction`` value (``EXPIRE_ACTION_INVALIDATE`` /
        ``_SOFT_DELETE`` / ``_HARD_DELETE``); ``max_rows`` caps the affected set
        and is required. Returns the number of records affected.
        """
        return self._stub.Expire(semantic_pb2.ExpireRequest(
            namespace=namespace, filter=dict(filter or {}),
            action=action, max_rows=max_rows,
        )).affected


class _Artifact:
    """Artifact uses gRPC streaming for Put/Get; these helpers buffer in memory."""

    CHUNK_SIZE = 64 * 1024

    def __init__(self, channel: grpc.Channel):
        self._stub = artifact_pb2_grpc.ArtifactStub(channel)

    def put(
        self, namespace: str, data: bytes, *, id: str = "",
        content_type: str = "", metadata: Optional[Mapping[str, str]] = None,
    ) -> artifact_pb2.ArtifactRef:
        def gen() -> Iterator[artifact_pb2.PutRequest]:
            yield artifact_pb2.PutRequest(init=artifact_pb2.PutInit(
                namespace=namespace, id=id, content_type=content_type,
                metadata=dict(metadata or {}),
            ))
            for off in range(0, len(data), self.CHUNK_SIZE):
                yield artifact_pb2.PutRequest(chunk=artifact_pb2.PutChunk(
                    data=data[off:off + self.CHUNK_SIZE],
                ))
        return self._stub.Put(gen()).ref

    def get(self, namespace: str, id: str, *, offset: int = 0, length: int = 0) -> bytes:
        req = artifact_pb2.GetRequest(
            namespace=namespace, id=id, offset=offset, length=length,
        )
        buf = bytearray()
        for msg in self._stub.Get(req):
            if msg.WhichOneof("body") == "chunk":
                buf.extend(msg.chunk.data)
        return bytes(buf)

    def stat(self, namespace: str, id: str) -> artifact_pb2.StatResponse:
        return self._stub.Stat(artifact_pb2.StatRequest(namespace=namespace, id=id))

    def delete(self, namespace: str, id: str) -> bool:
        return self._stub.Delete(artifact_pb2.DeleteRequest(namespace=namespace, id=id)).existed

    def list(
        self, namespace: str, *,
        filter: Optional[Mapping[str, str]] = None,
        start_after: str = "", limit: int = 0,
    ) -> Iterator[artifact_pb2.ArtifactMeta]:
        """Enumerate a namespace's artifacts (metadata only, no bytes) in
        ascending id order. ``filter`` is exact-match on metadata; ``start_after``
        is an exclusive resume cursor (pass the last id from the previous page);
        ``limit`` caps the page.
        """
        req = artifact_pb2.ListRequest(
            namespace=namespace, filter=dict(filter or {}),
            start_after=start_after, limit=limit,
        )
        return iter(self._stub.List(req))


class _Lease:
    def __init__(self, channel: grpc.Channel):
        self._stub = lease_pb2_grpc.LeaseStub(channel)

    def acquire(
        self, namespace: str, key: str, *, ttl, wait_for=None,
        metadata: Optional[Mapping[str, str]] = None,
    ) -> lease_pb2.LeaseHandle:
        req = lease_pb2.AcquireRequest(
            namespace=namespace, key=key, metadata=dict(metadata or {}),
        )
        ttlpb = _to_duration(ttl)
        if ttlpb is None:
            raise ValueError("ttl is required")
        req.ttl.CopyFrom(ttlpb)
        wfpb = _to_duration(wait_for)
        if wfpb is not None:
            req.wait_for.CopyFrom(wfpb)
        return self._stub.Acquire(req).handle

    def renew(self, holder_id: str, namespace: str, key: str, *, ttl) -> lease_pb2.LeaseHandle:
        req = lease_pb2.RenewRequest(holder_id=holder_id, namespace=namespace, key=key)
        ttlpb = _to_duration(ttl)
        if ttlpb is None:
            raise ValueError("ttl is required")
        req.ttl.CopyFrom(ttlpb)
        return self._stub.Renew(req).handle

    def release(self, holder_id: str, namespace: str, key: str) -> bool:
        return self._stub.Release(lease_pb2.ReleaseRequest(
            holder_id=holder_id, namespace=namespace, key=key,
        )).existed

    def inspect(self, namespace: str, key: str) -> lease_pb2.InspectResponse:
        return self._stub.Inspect(lease_pb2.InspectRequest(namespace=namespace, key=key))

    def list(self, namespace: str) -> List[lease_pb2.LeaseHandle]:
        """Return every currently-held (unexpired) lease in the namespace,
        ordered by key — for deadlock/orphan discovery and cleanup.
        """
        return list(self._stub.List(lease_pb2.ListRequest(namespace=namespace)).leases)


class _Graph:
    """Relationship-aware recall: node/edge upserts and bounded traversal.

    Traversal is always bounded server-side; ``depth``/``max_nodes``/``limit``
    of 0 use the server defaults. Node/edge ids are caller-supplied and can be
    shared with semantic record ids for agent-orchestrated hybrid recall.
    """

    def __init__(self, channel: grpc.Channel):
        self._stub = graph_pb2_grpc.GraphStub(channel)

    def upsert_nodes(self, namespace: str, nodes: Iterable[graph_pb2.Node]) -> List[str]:
        return list(self._stub.UpsertNodes(graph_pb2.UpsertNodesRequest(
            namespace=namespace, nodes=list(nodes),
        )).ids)

    def upsert_edges(self, namespace: str, edges: Iterable[graph_pb2.Edge]) -> List[str]:
        return list(self._stub.UpsertEdges(graph_pb2.UpsertEdgesRequest(
            namespace=namespace, edges=list(edges),
        )).ids)

    def get_node(self, namespace: str, id: str) -> graph_pb2.Node:
        return self._stub.GetNode(graph_pb2.GetNodeRequest(namespace=namespace, id=id))

    def neighbors(
        self, namespace: str, node_id: str, *,
        edge_types: Optional[Iterable[str]] = None,
        direction: "graph_pb2.Direction.ValueType" = graph_pb2.DIRECTION_UNSPECIFIED,
        node_labels: Optional[Iterable[str]] = None, limit: int = 0,
        as_of: Optional[_dt.datetime] = None,
    ) -> graph_pb2.NeighborsResponse:
        req = graph_pb2.NeighborsRequest(
            namespace=namespace, node_id=node_id,
            edge_types=list(edge_types or []), direction=direction,
            node_labels=list(node_labels or []), limit=limit,
        )
        ts = _to_timestamp(as_of)
        if ts is not None:
            req.as_of.CopyFrom(ts)
        return self._stub.Neighbors(req)

    def traverse(
        self, namespace: str, start_id: str, *,
        edge_types: Optional[Iterable[str]] = None,
        direction: "graph_pb2.Direction.ValueType" = graph_pb2.DIRECTION_UNSPECIFIED,
        depth: int = 0, max_nodes: int = 0,
        as_of: Optional[_dt.datetime] = None,
    ) -> graph_pb2.Subgraph:
        req = graph_pb2.TraverseRequest(
            namespace=namespace, start_id=start_id,
            edge_types=list(edge_types or []), direction=direction,
            depth=depth, max_nodes=max_nodes,
        )
        ts = _to_timestamp(as_of)
        if ts is not None:
            req.as_of.CopyFrom(ts)
        return self._stub.Traverse(req)

    def delete_node(self, namespace: str, id: str, *, cascade: bool = False) -> bool:
        return self._stub.DeleteNode(graph_pb2.DeleteNodeRequest(
            namespace=namespace, id=id, cascade=cascade,
        )).existed

    def delete_edge(self, namespace: str, id: str) -> bool:
        return self._stub.DeleteEdge(graph_pb2.DeleteEdgeRequest(namespace=namespace, id=id)).existed


class _Admin:
    """Cross-namespace server introspection. Requires the ``admin.inspect`` op."""

    def __init__(self, channel: grpc.Channel):
        self._stub = admin_pb2_grpc.AdminStub(channel)

    def list_namespaces(self) -> admin_pb2.ListNamespacesResponse:
        """List every configured namespace with its block, backend, driver, live
        item count (when the driver reports one cheaply — see ``has_count``), and
        embedder (semantic only), plus the server version/commit.
        """
        return self._stub.ListNamespaces(admin_pb2.ListNamespacesRequest())


# ---------------------------------------------------------------------------
# Top-level client


class MindD:
    """Connection + per-block clients with a single capability token."""

    def __init__(
        self, target: str, *, token: str,
        secure: bool = False,
        channel_credentials: Optional[grpc.ChannelCredentials] = None,
    ):
        if secure:
            credentials = channel_credentials or grpc.ssl_channel_credentials()
            base = grpc.secure_channel(target, credentials)
        else:
            base = grpc.insecure_channel(target)
        self._channel = grpc.intercept_channel(base, CapabilityInterceptor(token))
        self.kv = _KV(self._channel)
        self.episodic = _Episodic(self._channel)
        self.semantic = _Semantic(self._channel)
        self.artifact = _Artifact(self._channel)
        self.lease = _Lease(self._channel)
        self.graph = _Graph(self._channel)
        self.admin = _Admin(self._channel)

    def close(self) -> None:
        self._channel.close()

    def __enter__(self) -> "MindD":
        return self

    def __exit__(self, exc_type, exc, tb) -> None:
        self.close()
