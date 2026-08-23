---
title: Lease
sidebar_position: 5
---

# Lease

Distributed locks with TTL. The right block when two cooperating agents
need bounded exclusive access to a shared resource, and you want the
lock to expire automatically if the holder dies.

## API

```proto
service Lease {
  rpc Acquire(AcquireRequest) returns (AcquireResponse);
  rpc Renew  (RenewRequest)   returns (RenewResponse);
  rpc Release(ReleaseRequest) returns (ReleaseResponse);
  rpc Inspect(InspectRequest) returns (InspectResponse);
  rpc List   (ListRequest)    returns (ListResponse);   // held leases in a namespace
}

message AcquireRequest {
  string namespace = 1;
  string key = 2;
  google.protobuf.Duration ttl = 3;       // required, > 0
  google.protobuf.Duration wait_for = 4;  // 0 = fail fast
  map<string, string> metadata = 5;
}

message LeaseHandle {
  string holder_id = 1;            // server-assigned UUID; fences Renew/Release
  string namespace = 2;
  string key = 3;
  google.protobuf.Timestamp acquired_at = 4;
  google.protobuf.Timestamp expires_at = 5;
  map<string, string> metadata = 6;
}
```

- `Acquire` either returns a `LeaseHandle` or fails with
  `FailedPrecondition: already held`. With `wait_for > 0` the caller
  blocks up to that duration for the key to free.
- `Renew` and `Release` require the `holder_id` from `Acquire`.
  Presenting the wrong one returns `FailedPrecondition: not held by this
  holder`.
- `Inspect` is a read-only peek at one key that never blocks.
- `List` returns every currently-held (unexpired) lease in a namespace, ordered
  by key. `Inspect` is single-key, so `List` is what you use for deadlock /
  orphan-lease discovery and cleanup.

## Drivers

| Driver | Notes |
|---|---|
| `memory` | Mutex + `sync.Cond`. Atomic acquire (taking over expired leases is one step), poll-with-broadcast for `wait_for`. |
| `postgres` | Single table `leases`. Acquire is one `INSERT … ON CONFLICT (namespace, key) DO UPDATE … WHERE leases.expires_at <= now()`, atomic and observable as a SQL row. `wait_for` polls every 100 ms (configurable). |

## Configuration

```yaml
namespaces:
  - { block: lease, name: locks, backend: pg-main }
```

## gRPC example

```bash
# Acquire a lease.
ACQ=$(grpcurl -plaintext -H "x-mindd-capability: Bearer $TOKEN" \
  -d '{"namespace":"locks","key":"deploy","ttl":"60s"}' \
  127.0.0.1:7777 mindd.lease.v1.Lease/Acquire)
HOLDER=$(echo "$ACQ" | jq -r '.handle.holderId')

# Renew it.
grpcurl -plaintext -H "x-mindd-capability: Bearer $TOKEN" \
  -d "{\"namespace\":\"locks\",\"key\":\"deploy\",\"holder_id\":\"$HOLDER\",\"ttl\":\"300s\"}" \
  127.0.0.1:7777 mindd.lease.v1.Lease/Renew

# Release.
grpcurl -plaintext -H "x-mindd-capability: Bearer $TOKEN" \
  -d "{\"namespace\":\"locks\",\"key\":\"deploy\",\"holder_id\":\"$HOLDER\"}" \
  127.0.0.1:7777 mindd.lease.v1.Lease/Release
```

## Python example

```python
import datetime as dt

handle = m.lease.acquire("locks", "deploy", ttl=dt.timedelta(seconds=60))
try:
    do_work()
    m.lease.renew(handle.holder_id, "locks", "deploy", ttl=dt.timedelta(seconds=300))
finally:
    m.lease.release(handle.holder_id, "locks", "deploy")

# Who holds what right now (for cleanup / observability).
for h in m.lease.list("locks"):
    print(h.key, h.holder_id, h.expires_at.ToDatetime())
```

## Op names

| Op | Method |
|---|---|
| `lease.acquire` | `Lease/Acquire` |
| `lease.renew` | `Lease/Renew` |
| `lease.release` | `Lease/Release` |
| `lease.inspect` | `Lease/Inspect` |
| `lease.list` | `Lease/List` |

## Notes

- The `holder_id` is the **fencing token**. Long-running holders should
  reuse the same handle for `Renew` rather than re-`Acquire` (which would
  fail until the existing lease expires).
- A renew that arrives **after** the deadline returns
  `FailedPrecondition`. The lease can't be revived. Holders should renew
  comfortably ahead of `expires_at`.
- Cross-process correctness depends on the driver. The memory driver is
  in-process only. Use the Postgres (or future Redis/etcd) driver for
  multi-pod coordination.
