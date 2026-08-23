---
title: Namespaces & backends
sidebar_position: 3
---

# Namespaces & backends

mindD's data plane is configured around three concepts:

| Concept | Definition |
|---|---|
| **Backend** | A connection: Postgres pool, S3 client, filesystem path, in-memory store. Defined once at top level, named, then referenced. |
| **Namespace** | A logical grouping within a building block. Maps to a Postgres table prefix, an S3 key prefix, a pgvector table, etc. |
| **Block** | One of `kv` / `episodic` / `semantic` / `artifact` / `lease` / `graph`. |

## Mapping in config

```yaml
backends:
  - name: mem-default
    driver: memory
  - name: pg-main
    driver: postgres
    options:
      dsn_env: MINDD_PG_DSN

namespaces:
  - { block: kv,       name: scratchpad,  backend: mem-default }
  - { block: kv,       name: tool-cache,  backend: pg-main }
  - { block: episodic, name: events,      backend: pg-main }
  - { block: artifact, name: blobs,       backend: blob-local }
  - { block: lease,    name: locks,       backend: mem-default }
  - { block: graph,    name: knowledge,   backend: mem-default }
```

A backend can serve **multiple namespaces** across **multiple blocks**.
The chart's default config shares one in-memory backend across kv,
episodic, artifact, and lease. Each namespace gets its own driver instance
under the hood; the backend config just supplies the connection parameters.

## How namespaces reach the right driver

Inside the sidecar, every block owns a `Registry`:

```
namespace name ── Registry.Resolve ──▶ Driver
```

The KV service looks up `Resolve("scratchpad")`, gets back a
`kv.Driver`, and calls `Put` / `Get` / etc. on it. The driver itself
doesn't know what namespace it's serving except as a string parameter
on each call.

For the **semantic** block, the registry returns a
`BoundNamespace { Driver, Embedder }` because each semantic namespace has
its own embedder configured at startup. See
[Semantic](../blocks/semantic.md).

## Scope and namespaces

[Capability tokens](./capabilities.md) carry **namespace glob patterns**,
not raw namespace strings. A token with `ns: ["kv/tool-*"]` covers any
existing or future namespace whose name starts with `tool-`. This lets
issuers grant a scope without knowing every namespace name up front.

The match runs against the qualified form `<block>/<name>`, so
`semantic/notes` and `kv/notes` are distinct, and a single pattern
`*/notes` is *not* supported (intentional, to keep the matcher easy to
reason about).

## Adding a new namespace

1. Edit the `namespaces:` list in the YAML config.
2. Restart the sidecar (namespaces aren't currently hot-reloadable; adding
   one requires building a fresh driver instance against the backend).
3. Mint tokens whose `ns` pattern covers the new name.

For semantic namespaces, also declare the embedder. See
[Semantic](../blocks/semantic.md#configuration).
