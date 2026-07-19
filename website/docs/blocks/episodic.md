---
title: Episodic
sidebar_position: 2
---

# Episodic

Append-only log of agent events: tool calls, messages, observations,
anything timestamped that you'll want to replay. Cursors are monotonic
within a namespace; readers can both replay history (`Range`) and follow
live (`Tail`).

## API

```proto
service Episodic {
  rpc Append(AppendRequest) returns (AppendResponse);
  rpc Range (RangeRequest)  returns (stream Event);
  rpc Tail  (TailRequest)   returns (stream Event);
  rpc Expire(ExpireRequest) returns (ExpireResponse);
}
```

- Server assigns each event a UUID `id` and a monotonic `cursor` per
  namespace (the first event has `cursor=1`). Cursors never repeat, even after
  events are removed by `Expire`.
- Events carry first-class `role` (speaker/actor — `user` / `assistant` /
  `tool` / …) and `session_id` grouping keys, so conversation grouping and
  cross-session assembly aren't a metadata convention.
- `Range` supports `after_cursor` / `before_cursor` / `limit` / `reverse`,
  plus an exclusive event-timestamp window (`after_time` / `before_time`)
  that ANDs with the cursor bounds — backed by a `(namespace, timestamp)`
  index.
- `Range` also takes `session_id` / `role` / `type` **equality predicates**
  (empty = no filter), ANDed with the window — so "reconstruct session X"
  (optionally one role or type) is a bounded, index-backed scan instead of an
  O(namespace) transfer. Backed by a partial `(namespace, session_id, cursor)`
  index.
- `Tail` either starts at the live edge (`include_historical=false`) or
  replays everything with `cursor > after_cursor` before transitioning to
  live.

### Idempotency, revisability & retention

- **`dedup_key`** on `Append` makes writes idempotent under retry. gRPC delivery
  is at-least-once and the log has no update/delete on the write path, so a
  retried `Append` would otherwise duplicate an event. The first `Append` for a
  `(namespace, dedup_key)` writes; any later one is a no-op that returns the
  already-stored event (same `id` and `cursor`). Empty = no dedup.
- **`supersedes` / `source`** carry provenance and revisability. `supersedes`
  lists ids of earlier events this one revises; the server tombstones each named
  live event (`deleted_at = now()`) in the same transaction (self-references and
  already-tombstoned ids are ignored — no inference). `source` is an opaque
  provenance handle, stored and returned verbatim.
- A tombstoned event is **retained** (the log stays append-only) and hidden from
  `Range` unless `include_deleted=true`.
- **`Expire`** is the retention/compaction path: it tombstones or physically
  removes events inside a bounded window (`before_cursor` and/or `before_time` —
  at least one required), oldest-first, capped by a required `max_rows`. Actions
  are `EXPIRE_ACTION_SOFT_DELETE` (set `deleted_at`, retain) and
  `EXPIRE_ACTION_HARD_DELETE` (remove the row). Returns the number affected.

## Drivers

| Driver | Notes |
|---|---|
| `memory` | Per-namespace slice + channel fan-out for live `Tail`. Slow tailers are detached with `ErrSubscriberLagged` rather than blocking writers. |
| `postgres` | Single table `episodic_events`, `(namespace, cursor)` unique, with `role`/`session_id`/`source`/`supersedes`/`deleted_at`/`dedup_key` columns. Indexes: `(namespace, timestamp)` for time windows, a partial `(namespace, session_id, cursor)` for session reconstruction, a partial unique `(namespace, dedup_key)` for idempotent `Append`, and a partial `(namespace, cursor) WHERE deleted_at IS NULL` for the default live scan. Cursor assignment is atomic via a per-namespace counter row (`INSERT … ON CONFLICT DO UPDATE`) that only ever increases. `Tail` polls every 250 ms (configurable). |

## Configuration

```yaml
namespaces:
  - { block: episodic, name: events, backend: pg-main }
```

## gRPC example

```bash
grpcurl -plaintext -H "x-memsidecar-capability: Bearer $TOKEN" \
  -d '{"namespace":"events","type":"tool_call","payload":"aGk="}' \
  127.0.0.1:7777 memsidecar.episodic.v1.Episodic/Append

grpcurl -plaintext -H "x-memsidecar-capability: Bearer $TOKEN" \
  -d '{"namespace":"events","after_cursor":0}' \
  127.0.0.1:7777 memsidecar.episodic.v1.Episodic/Range

# Reconstruct one conversation: all events for session "sess-1".
grpcurl -plaintext -H "x-memsidecar-capability: Bearer $TOKEN" \
  -d '{"namespace":"events","session_id":"sess-1"}' \
  127.0.0.1:7777 memsidecar.episodic.v1.Episodic/Range

# Retention: soft-delete everything older than cursor 1000 (up to 500 at a time).
grpcurl -plaintext -H "x-memsidecar-capability: Bearer $TOKEN" \
  -d '{"namespace":"events","before_cursor":1000,"action":"EXPIRE_ACTION_SOFT_DELETE","max_rows":500}' \
  127.0.0.1:7777 memsidecar.episodic.v1.Episodic/Expire

# Live tail; new appends arrive as a server-stream.
grpcurl -plaintext -H "x-memsidecar-capability: Bearer $TOKEN" \
  -d '{"namespace":"events"}' \
  127.0.0.1:7777 memsidecar.episodic.v1.Episodic/Tail
```

## Python example

```python
import datetime as dt
from memsidecar.episodic.v1 import episodic_pb2

# Idempotent append: retrying with the same dedup_key returns the stored event.
ev = m.episodic.append("events", "message", b"hi",
                       role="user", session_id="sess-1", dedup_key="msg-42")
print(ev.cursor, ev.role, ev.session_id)  # 1 user sess-1

# Revise an earlier event: v2 tombstones v1, which Range hides by default.
v2 = m.episodic.append("events", "fact", b"v2", supersedes=[ev.id], source="corr-7")

# Reconstruct one conversation (equality filter, ANDs with cursor/time bounds).
for e in m.episodic.range("events", session_id="sess-1", role="user"):
    handle(e)

# Replay just this hour's events (exclusive time window, ANDs with cursor bounds).
since = dt.datetime.now(dt.timezone.utc) - dt.timedelta(hours=1)
for e in m.episodic.range("events", after_time=since):
    handle(e)

# Retention: soft-delete everything below cursor 1000, oldest-first, ≤500 rows.
n = m.episodic.expire("events", before_cursor=1000,
                      action=episodic_pb2.EXPIRE_ACTION_SOFT_DELETE, max_rows=500)

# History replay + live tail in one stream.
for e in m.episodic.tail("events", include_historical=True, after_cursor=0):
    handle(e)
```

## Op names

| Op | Method |
|---|---|
| `episodic.append` | `Episodic/Append` |
| `episodic.range` | `Episodic/Range` |
| `episodic.tail` | `Episodic/Tail` |
| `episodic.expire` | `Episodic/Expire` |

## Notes

- Cursors are **per namespace**. Two namespaces in the same backend each
  start at 1 and grow independently.
- The Postgres driver uses `Serializable` isolation only for cursor
  assignment; `Range` and `Tail` reads are at the default isolation.
- LISTEN/NOTIFY is a future optimisation for `Tail`; today the Postgres
  driver polls.
