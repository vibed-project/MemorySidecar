---
title: Tenant isolation
sidebar_position: 5
---

# Tenant isolation

A namespace is a physical store. By default every capability that is scoped to a
namespace shares it — so if two tenants both hold tokens for `kv/scratchpad`,
they read and write the **same** data. That's fine for a single-tenant
deployment, but in a multi-tenant one it's a data-leak.

**Tenant isolation** scopes each request's storage to the caller's capability
`tenant`, so two tenants sharing a namespace name get physically separate data.

```yaml
# config.yaml
tenant_isolation: true
```

## Behavior

- **Off (default).** Storage is byte-for-byte identical to how it worked before
  isolation existed. A single-tenant deployment is unaffected and existing data
  keeps working. This is why it's opt-in.
- **On.** Every block's storage key is prefixed with the caller's tenant
  (from the capability token). Enforcement lives in **one place** — the service
  layer, before the driver is ever touched — so a driver can't leak across
  tenants even by accident. All tokens must carry a `tenant`.

```
tenant "acme" ─┐
               ├─ kv.put("scratchpad", "k", …)   → stored under acme
tenant "beta" ─┘                                → stored under beta (separate)
```

Tenant A cannot read, scan, overwrite, or delete tenant B's records in the same
namespace, and vice-versa.

## Coverage

Isolation covers **all six blocks** — kv, episodic, lease, graph, artifact, and
semantic. The first five qualify the storage namespace string; the semantic
block, which is bound per-namespace, carries a `tenant` column so `(tenant, id)`
is a record's identity and every search/delete/expire is tenant-scoped. Two
tenants can reuse the same semantic record id without colliding.

## Enabling it on an existing deployment

Turning isolation on changes where data is addressed. Data written **before**
enabling it lives under the un-prefixed namespace and won't be visible to
tenant-scoped reads afterward — which is the point (it was previously shared).
Enable it from the start of a multi-tenant deployment, or migrate deliberately.

The `tenant` comes from the capability token, so issue every token with one:

```bash
mindctl token issue --tenant acme --agent a1 --ns 'kv/*' --ops '*'
```

See [Capabilities](./capabilities.md) for how tokens carry the tenant.
