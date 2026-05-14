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
}
```

- Server assigns each event a UUID `id` and a monotonic `cursor` per
  namespace (the first event has `cursor=1`).
- `Range` supports `after_cursor` / `before_cursor` / `limit` / `reverse`.
- `Tail` either starts at the live edge (`include_historical=false`) or
  replays everything with `cursor > after_cursor` before transitioning to
  live.

## Drivers

| Driver | Notes |
|---|---|
| `memory` | Per-namespace slice + channel fan-out for live `Tail`. Slow tailers are detached with `ErrSubscriberLagged` rather than blocking writers. |
| `postgres` | Single table `episodic_events`, `(namespace, cursor)` unique. Cursor assignment is atomic via `SELECT MAX … FOR UPDATE` inside a `Serializable` tx. `Tail` polls every 250 ms (configurable). |

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

# Live tail; new appends arrive as a server-stream.
grpcurl -plaintext -H "x-memsidecar-capability: Bearer $TOKEN" \
  -d '{"namespace":"events"}' \
  127.0.0.1:7777 memsidecar.episodic.v1.Episodic/Tail
```

## Python example

```python
ev = m.episodic.append("events", "tool_call", b"...")
print(ev.cursor)  # 1, 2, 3, …

# History replay + live tail in one stream.
for ev in m.episodic.tail("events", include_historical=True, after_cursor=0):
    handle(ev)
```

## Op names

| Op | Method |
|---|---|
| `episodic.append` | `Episodic/Append` |
| `episodic.range` | `Episodic/Range` |
| `episodic.tail` | `Episodic/Tail` |

## Notes

- Cursors are **per namespace**. Two namespaces in the same backend each
  start at 1 and grow independently.
- The Postgres driver uses `Serializable` isolation only for cursor
  assignment; `Range` and `Tail` reads are at the default isolation.
- LISTEN/NOTIFY is a future optimisation for `Tail`; today the Postgres
  driver polls.
