---
title: KV
sidebar_position: 1
---

# KV

Typed, TTL'd key-value storage scoped by namespace. The right block for
tool-result caching, scratchpads, and short-lived agent state.

## API

```proto
service KV {
  rpc Get    (GetRequest)    returns (GetResponse);
  rpc Put    (PutRequest)    returns (PutResponse);   // with TTL, CAS
  rpc Delete (DeleteRequest) returns (DeleteResponse);
  rpc Scan   (ScanRequest)   returns (stream KVItem);
}
```

Notable fields:

- `ttl` — `google.protobuf.Duration`; 0 means no expiry.
- `if_version` — optional uint64 for optimistic concurrency (CAS).
- `Scan.key_prefix` + `limit` — prefix scan with ordered results.
- Server returns a monotonic `version` on every write.

## Drivers

| Driver | Notes |
|---|---|
| `memory` | `sync.RWMutex`-guarded map. Lazy expiry on read + background sweeper (default 30 s). |
| `postgres` | Single table `kv_items` keyed by `(namespace, key)`, partial index on `expires_at`, periodic sweeper deletes expired rows. CAS via `WHERE version = $if_version`. Embedded migrations applied at startup. |

## Configuration

```yaml
backends:
  - name: pg-main
    driver: postgres
    options:
      dsn_env: MEMSIDECAR_PG_DSN
      max_conns: 10
      sweeper_interval: 5m

namespaces:
  - { block: kv, name: scratchpad, backend: pg-main }
```

### Cache-tier access policy (in-memory only)

An in-memory namespace can opt into cache-tier behaviour. It's **off by
default** — a namespace without an `access` block never writes on a read:

```yaml
namespaces:
  - block: kv
    name: tool-cache
    backend: mem-default
    access:
      track: true                 # record last_accessed/access_count on Get
      slide_ttl_seconds: 300      # >0: each Get extends a TTL'd key's expiry to now+300s
      capacity: 10000             # >0: cap live keys; on Put over cap, evict the coldest
      heat_half_life_seconds: 3600  # eviction heat = access_count · 2^(−age/half_life)
```

- **`track`** maintains per-key `access_count` / `last_accessed` (used only for
  eviction; not returned on the wire).
- **`slide_ttl_seconds`** gives read-through residency: frequently-read keys
  keep getting their TTL extended, so hot entries survive while idle ones
  expire. Keys written without a TTL are unaffected.
- **`capacity`** + **`heat_half_life_seconds`** bound a namespace's size by
  evicting the **coldest** keys first (lowest `access_count · 2^(−age/half_life)`),
  so a burst of one-shot writes can't push out actively-used entries. Capacity
  evictions increment `memsidecar.eviction.total{cause="capacity"}`.

Durations are expressed in seconds. This policy is honoured by the `memory`
driver only; on a Postgres-backed namespace it is ignored.

## gRPC example

```bash
TOKEN=$(memctl token issue --tenant acme --agent a1 \
  --ns 'kv/scratchpad' --ops put,get,delete,scan --ttl 1h)

grpcurl -plaintext -H "x-memsidecar-capability: Bearer $TOKEN" \
  -d '{"namespace":"scratchpad","key":"hello","value":"d29ybGQ=","ttl":"60s"}' \
  127.0.0.1:7777 memsidecar.kv.v1.KV/Put

grpcurl -plaintext -H "x-memsidecar-capability: Bearer $TOKEN" \
  -d '{"namespace":"scratchpad","key":"hello"}' \
  127.0.0.1:7777 memsidecar.kv.v1.KV/Get
```

## Python example

```python
import datetime as dt
from memsidecar import MemSidecar

with MemSidecar("127.0.0.1:7777", token=TOKEN) as m:
    m.kv.put("scratchpad", "hello", b"world", ttl=dt.timedelta(seconds=60))
    rec = m.kv.get("scratchpad", "hello")
    assert rec.found and rec.value == b"world"
```

## Op names (for capability + policy)

| Op | Method |
|---|---|
| `kv.get` | `KV/Get` |
| `kv.put` | `KV/Put` |
| `kv.delete` | `KV/Delete` |
| `kv.scan` | `KV/Scan` |
