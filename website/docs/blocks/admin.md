---
title: Admin
sidebar_position: 7
---

# Admin

Read-only server introspection: which namespaces exist, how they're configured,
and their live item counts. Unlike the building blocks, `Admin` is
**cross-namespace** — it answers "what is this sidecar serving?" rather than
operating on one namespace's data. It's a prerequisite for backup/export tooling
and for discovering contents without an external registry.

## API

```proto
service Admin {
  rpc ListNamespaces(ListNamespacesRequest) returns (ListNamespacesResponse);
}

message NamespaceInfo {
  string block = 1;      // kv | episodic | semantic | artifact | lease | graph
  string name = 2;
  string backend = 3;    // configured backend name
  string driver = 4;     // memory | postgres | fs | s3
  int64 item_count = 5;  // live count, when the driver reports one cheaply
  bool has_count = 6;    // false → item_count is unavailable, ignore it
  EmbedderInfo embedder = 7;  // semantic namespaces only
}
```

- `ListNamespaces` returns every configured namespace (ordered by
  `(block, name)`) plus a `ServerInfo` with the server `version` / `commit`.
- `item_count` reuses each block registry's cheap per-namespace count — the same
  source that feeds the `mindd.namespace.items` gauge (O3). Drivers with no
  cheap size (the `fs` / `s3` artifact drivers) report **`has_count = false`**
  rather than paying a `count(*)`; treat `item_count` as valid only when
  `has_count` is true.
- `embedder` (provider / model / dimensions) is populated for semantic
  namespaces only.

## Authorization

`Admin` is gated on a single **cross-namespace** op, `admin.inspect` — it does
**not** check a per-namespace scope, so a token grants it via `AllowedOps`
(`admin.inspect`, the verb-only `inspect`, or `*`). Issue an admin token with:

```bash
mindctl token issue --tenant acme --agent ops --ops admin.inspect --ttl 1h
```

The service is optional in the server wiring: when it isn't registered the RPC
simply isn't served.

## gRPC example

```bash
grpcurl -plaintext -H "x-mindd-capability: Bearer $TOKEN" \
  -d '{}' 127.0.0.1:7777 mindd.admin.v1.Admin/ListNamespaces
```

## Python example

```python
resp = m.admin.list_namespaces()
print(resp.server.version, resp.server.commit)
for ns in resp.namespaces:
    count = ns.item_count if ns.has_count else "?"
    print(f"{ns.block}/{ns.name}  backend={ns.backend} ({ns.driver})  items={count}")
    if ns.embedder.provider:
        print(f"    embedder: {ns.embedder.provider} {ns.embedder.model} dim={ns.embedder.dimensions}")
```

## Op names

| Op | Method |
|---|---|
| `admin.inspect` | `Admin/ListNamespaces` |
