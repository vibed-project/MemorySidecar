"""LangGraph checkpointer backed by mindd.

``MindDSaver`` implements LangGraph's :class:`BaseCheckpointSaver` on top of
the sidecar's **kv** block, so a graph gets durable, multi-tenant thread state by
pointing at a running mindd — no bespoke persistence glue::

    from mindd import MindD
    from mindd.ext.langgraph import MindDSaver

    client = MindD("127.0.0.1:7777", token=TOKEN)
    saver = MindDSaver(client, namespace="checkpoints")
    graph = builder.compile(checkpointer=saver)
    graph.invoke({"messages": [...]}, {"configurable": {"thread_id": "t1"}})

Storage model mirrors LangGraph's reference ``InMemorySaver`` exactly, so the
runtime behaves identically. Everything lives under one kv namespace, keyed by
role:

* ``cp/<thread>/<ns>/<checkpoint_id>`` — the checkpoint (channel values popped
  out) plus its metadata and parent id, as one JSON envelope.
* ``bl/<thread>/<ns>/<channel>/<version>`` — one channel value per version.
  Checkpoints reference these by ``channel_versions``; unchanged channels are
  shared across checkpoints (the reason channel values are stored separately).
* ``wr/<thread>/<ns>/<checkpoint_id>/<task_id>/<idx>`` — a pending write.

Per-thread queries (get-latest, list) are served by a kv prefix scan; the
sidecar's episodic block is intentionally not used here because it has no
per-thread server-side filter (its cursor stream is per-namespace), which is the
access pattern a checkpointer needs.

Requires ``langgraph-checkpoint`` (``pip install mindd[langgraph]``).
"""
from __future__ import annotations

import asyncio
import base64
import json
from typing import Any, AsyncIterator, Iterator, Optional, Sequence
from urllib.parse import quote, unquote

import grpc
from langchain_core.runnables import RunnableConfig
from langgraph.checkpoint.base import (
    WRITES_IDX_MAP,
    BaseCheckpointSaver,
    ChannelVersions,
    Checkpoint,
    CheckpointMetadata,
    CheckpointTuple,
    get_checkpoint_id,
    get_checkpoint_metadata,
)
from langgraph.checkpoint.serde.base import SerializerProtocol

from ..client import MindD

__all__ = ["MindDSaver"]


def _q(s: Any) -> str:
    """Percent-encode one key component so separators in ids can't collide."""
    return quote(str(s), safe="")


class MindDSaver(BaseCheckpointSaver):
    """A LangGraph checkpointer whose storage is a mindd kv namespace.

    Args:
        client: A connected :class:`mindd.MindD`.
        namespace: The kv namespace to store checkpoints in. It must be declared
            on the server and the capability token must grant kv get/put/delete/
            scan on it.
        serde: Optional serializer; defaults to LangGraph's ``JsonPlusSerializer``.
    """

    def __init__(
        self,
        client: MindD,
        *,
        namespace: str = "checkpoints",
        serde: Optional[SerializerProtocol] = None,
    ) -> None:
        super().__init__(serde=serde)
        self.client = client
        self.namespace = namespace

    # -- key builders -------------------------------------------------------

    def _cp_key(self, thread: str, ns: str, cid: str) -> str:
        return f"cp/{_q(thread)}/{_q(ns)}/{_q(cid)}"

    def _cp_prefix(self, thread: str, ns: Optional[str]) -> str:
        if ns is None:
            return f"cp/{_q(thread)}/"
        return f"cp/{_q(thread)}/{_q(ns)}/"

    def _blob_key(self, thread: str, ns: str, channel: str, version: Any) -> str:
        return f"bl/{_q(thread)}/{_q(ns)}/{_q(channel)}/{_q(version)}"

    def _write_key(self, thread: str, ns: str, cid: str, task_id: str, idx: int) -> str:
        return f"wr/{_q(thread)}/{_q(ns)}/{_q(cid)}/{_q(task_id)}/{_q(idx)}"

    def _write_prefix(self, thread: str, ns: str, cid: str) -> str:
        return f"wr/{_q(thread)}/{_q(ns)}/{_q(cid)}/"

    # -- typed-blob envelopes ----------------------------------------------

    def _pack(self, obj: Any) -> dict[str, str]:
        """serde.dumps_typed -> a JSON-safe {type, b64(bytes)} envelope."""
        t, b = self.serde.dumps_typed(obj)
        return {"t": t, "b": base64.b64encode(b).decode("ascii")}

    def _unpack(self, env: dict[str, str]) -> Any:
        return self.serde.loads_typed((env["t"], base64.b64decode(env["b"])))

    # -- checkpoint record --------------------------------------------------

    def _put_bytes(self, key: str, value: bytes) -> None:
        self.client.kv.put(self.namespace, key, value)

    def _get_bytes(self, key: str) -> Optional[bytes]:
        resp = self.client.kv.get(self.namespace, key)
        return resp.value if resp.found else None

    # ----------------------------------------------------------------------
    # Sync API

    def put(
        self,
        config: RunnableConfig,
        checkpoint: Checkpoint,
        metadata: CheckpointMetadata,
        new_versions: ChannelVersions,
    ) -> RunnableConfig:
        cfg = config["configurable"]
        thread = cfg["thread_id"]
        ns = cfg.get("checkpoint_ns", "")

        c = checkpoint.copy()
        values: dict[str, Any] = c.pop("channel_values")  # type: ignore[misc]

        # One blob per channel version. Unchanged channels keep their prior blob
        # and are re-referenced by channel_versions on read.
        for channel, version in new_versions.items():
            env = self._pack(values[channel]) if channel in values else {"t": "empty", "b": ""}
            self._put_bytes(
                self._blob_key(thread, ns, channel, version),
                json.dumps(env).encode("utf-8"),
            )

        record = {
            "checkpoint": self._pack(c),
            "metadata": self._pack(get_checkpoint_metadata(config, metadata)),
            "parent": cfg.get("checkpoint_id"),
        }
        self._put_bytes(
            self._cp_key(thread, ns, checkpoint["id"]),
            json.dumps(record).encode("utf-8"),
        )
        return {
            "configurable": {
                "thread_id": thread,
                "checkpoint_ns": ns,
                "checkpoint_id": checkpoint["id"],
            }
        }

    def put_writes(
        self,
        config: RunnableConfig,
        writes: Sequence[tuple[str, Any]],
        task_id: str,
        task_path: str = "",
    ) -> None:
        cfg = config["configurable"]
        thread = cfg["thread_id"]
        ns = cfg.get("checkpoint_ns", "")
        cid = cfg["checkpoint_id"]
        for idx, (channel, value) in enumerate(writes):
            widx = WRITES_IDX_MAP.get(channel, idx)
            key = self._write_key(thread, ns, cid, task_id, widx)
            record = json.dumps(
                {
                    "task_id": task_id,
                    "channel": channel,
                    "value": self._pack(value),
                    "task_path": task_path,
                    "idx": widx,
                }
            ).encode("utf-8")
            if widx >= 0:
                # First-wins idempotency for regular writes: create only if the
                # (task_id, idx) slot is empty (atomic CAS via if_version=0). A
                # re-run that re-emits the same write must not clobber the first.
                try:
                    self.client.kv.put(self.namespace, key, record, if_version=0)
                except grpc.RpcError as e:
                    if e.code() == grpc.StatusCode.FAILED_PRECONDITION:
                        continue
                    raise
            else:
                # Special channels (__error__/__interrupt__/__resume__/…) are
                # last-wins: always overwrite.
                self.client.kv.put(self.namespace, key, record)

    def get_tuple(self, config: RunnableConfig) -> Optional[CheckpointTuple]:
        cfg = config["configurable"]
        thread = cfg["thread_id"]
        ns = cfg.get("checkpoint_ns", "")

        cid = get_checkpoint_id(config)
        if cid:
            raw = self._get_bytes(self._cp_key(thread, ns, cid))
            if raw is None:
                return None
            return self._to_tuple(thread, ns, cid, raw, config)

        # Latest: the max checkpoint_id wins (ids are monotonic, so lexical max
        # is newest — the same rule the reference saver uses).
        ids = self._scan_ids(self._cp_prefix(thread, ns))
        if not ids:
            return None
        cid = max(ids)
        raw = self._get_bytes(self._cp_key(thread, ns, cid))
        if raw is None:
            return None
        return self._to_tuple(thread, ns, cid, raw, None)

    def list(
        self,
        config: Optional[RunnableConfig],
        *,
        filter: Optional[dict[str, Any]] = None,
        before: Optional[RunnableConfig] = None,
        limit: Optional[int] = None,
    ) -> Iterator[CheckpointTuple]:
        if config is not None:
            thread = config["configurable"]["thread_id"]
            ns_filter = config["configurable"].get("checkpoint_ns")
            prefix = self._cp_prefix(thread, ns_filter)
        else:
            ns_filter = None
            prefix = "cp/"

        before_id = get_checkpoint_id(before) if before else None

        # (thread, ns, id, raw), newest first.
        entries: list[tuple[str, str, str, bytes]] = []
        for item in self.client.kv.scan(self.namespace, key_prefix=prefix, include_values=True):
            t, ns, cid = self._parse_cp_key(item.key)
            if ns_filter is not None and ns != ns_filter:
                continue
            entries.append((t, ns, cid, item.value))
        entries.sort(key=lambda e: e[2], reverse=True)

        remaining = limit
        for t, ns, cid, raw in entries:
            if before_id and cid >= before_id:
                continue
            record = json.loads(raw)
            metadata = self._unpack(record["metadata"])
            if filter and not all(metadata.get(k) == v for k, v in filter.items()):
                continue
            if remaining is not None:
                if remaining <= 0:
                    break
                remaining -= 1
            yield self._to_tuple(t, ns, cid, raw, None, record=record, metadata=metadata)

    def delete_thread(self, thread_id: str) -> None:
        for role in ("cp", "bl", "wr"):
            prefix = f"{role}/{_q(thread_id)}/"
            for item in self.client.kv.scan(self.namespace, key_prefix=prefix):
                self.client.kv.delete(self.namespace, item.key)

    # ----------------------------------------------------------------------
    # Async API — the kv client is blocking, so offload to a worker thread
    # rather than stalling the event loop.

    async def aput(
        self,
        config: RunnableConfig,
        checkpoint: Checkpoint,
        metadata: CheckpointMetadata,
        new_versions: ChannelVersions,
    ) -> RunnableConfig:
        return await asyncio.to_thread(self.put, config, checkpoint, metadata, new_versions)

    async def aput_writes(
        self,
        config: RunnableConfig,
        writes: Sequence[tuple[str, Any]],
        task_id: str,
        task_path: str = "",
    ) -> None:
        await asyncio.to_thread(self.put_writes, config, writes, task_id, task_path)

    async def aget_tuple(self, config: RunnableConfig) -> Optional[CheckpointTuple]:
        return await asyncio.to_thread(self.get_tuple, config)

    async def alist(
        self,
        config: Optional[RunnableConfig],
        *,
        filter: Optional[dict[str, Any]] = None,
        before: Optional[RunnableConfig] = None,
        limit: Optional[int] = None,
    ) -> AsyncIterator[CheckpointTuple]:
        items = await asyncio.to_thread(
            lambda: list(self.list(config, filter=filter, before=before, limit=limit))
        )
        for item in items:
            yield item

    async def adelete_thread(self, thread_id: str) -> None:
        await asyncio.to_thread(self.delete_thread, thread_id)

    # ----------------------------------------------------------------------
    # Internals

    def _scan_ids(self, prefix: str) -> list[str]:
        return [self._parse_cp_key(item.key)[2] for item in self.client.kv.scan(self.namespace, key_prefix=prefix)]

    @staticmethod
    def _parse_cp_key(key: str) -> tuple[str, str, str]:
        # cp/<thread>/<ns>/<checkpoint_id>, each component percent-encoded.
        _, thread, ns, cid = key.split("/", 3)
        return unquote(thread), unquote(ns), unquote(cid)

    def _load_blobs(self, thread: str, ns: str, versions: ChannelVersions) -> dict[str, Any]:
        result: dict[str, Any] = {}
        for channel, version in versions.items():
            raw = self._get_bytes(self._blob_key(thread, ns, channel, version))
            if raw is None:
                continue
            env = json.loads(raw)
            if env["t"] == "empty":
                continue
            result[channel] = self._unpack(env)
        return result

    def _load_writes(self, thread: str, ns: str, cid: str) -> list[tuple[str, str, Any]]:
        rows: list[tuple[str, int, str, Any]] = []
        for item in self.client.kv.scan(
            self.namespace, key_prefix=self._write_prefix(thread, ns, cid), include_values=True
        ):
            rec = json.loads(item.value)
            rows.append((rec["task_id"], rec["idx"], rec["channel"], self._unpack(rec["value"])))
        rows.sort(key=lambda r: (r[0], r[1]))
        return [(task_id, channel, value) for task_id, _idx, channel, value in rows]

    def _to_tuple(
        self,
        thread: str,
        ns: str,
        cid: str,
        raw: bytes,
        config: Optional[RunnableConfig],
        *,
        record: Optional[dict] = None,
        metadata: Any = None,
    ) -> CheckpointTuple:
        if record is None:
            record = json.loads(raw)
        checkpoint: Checkpoint = self._unpack(record["checkpoint"])
        if metadata is None:
            metadata = self._unpack(record["metadata"])
        parent = record.get("parent")
        return CheckpointTuple(
            config=config
            or {"configurable": {"thread_id": thread, "checkpoint_ns": ns, "checkpoint_id": cid}},
            checkpoint={
                **checkpoint,
                "channel_values": self._load_blobs(thread, ns, checkpoint["channel_versions"]),
            },
            metadata=metadata,
            parent_config=(
                {"configurable": {"thread_id": thread, "checkpoint_ns": ns, "checkpoint_id": parent}}
                if parent
                else None
            ),
            pending_writes=self._load_writes(thread, ns, cid),
        )
