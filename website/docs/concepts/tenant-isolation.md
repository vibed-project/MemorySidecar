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

Isolation currently covers **kv, episodic, lease, graph, and artifact**.

The **semantic** block is bound per-namespace and needs a driver-level tenant
column, which isn't implemented yet. To avoid silently sharing semantic data,
the server **refuses to start** when `tenant_isolation` is enabled and a
`semantic` namespace is configured:

```
namespace "semantic/notes": tenant_isolation does not yet isolate the
semantic block; remove semantic namespaces or disable tenant_isolation
until semantic isolation lands
```

## Enabling it on an existing deployment

Turning isolation on changes where data is addressed. Data written **before**
enabling it lives under the un-prefixed namespace and won't be visible to
tenant-scoped reads afterward — which is the point (it was previously shared).
Enable it from the start of a multi-tenant deployment, or migrate deliberately.

The `tenant` comes from the capability token, so issue every token with one:

```bash
memctl token issue --tenant acme --agent a1 --ns 'kv/*' --ops '*'
```

See [Capabilities](./capabilities.md) for how tokens carry the tenant.
