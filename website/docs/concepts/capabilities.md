---
title: Capability tokens
sidebar_position: 2
---

# Capability tokens

Every request to mindD carries a signed bearer token in the gRPC metadata key
`x-mindd-capability` (or the HTTP header of the same name). The token encodes
**who** is allowed to do **what**:

- `tenant`, the top-level isolation boundary. Required.
- `agent`, an informational id, useful in audit.
- `namespaces`, a list of glob patterns (for example `kv/scratchpad`,
  `kv/tool-*`).
- `ops`, a list of permitted operations (`kv.put`, `episodic.append`, `*`).
- `exp`, the expiration.

Two signature formats are supported: **PASETO v4.public** (the default) and
**JWT** (HS256 or RS256). Both are pluggable behind a `TokenVerifier`
interface.

## Issuing a token

Use `mindctl` for development. In production the issuer is your IdP; mindD only
ever verifies, and never needs a private key.

```bash
mindctl token issue \
  --tenant acme --agent agent-1 \
  --ns 'kv/scratchpad,episodic/events' \
  --ops 'kv.get,kv.put,episodic.append,episodic.range' --ttl 1h
```

| Flag | Default | Meaning |
|---|---|---|
| `--tenant` | none, **required** | tenant the token acts as |
| `--ns` | none, **required** | comma-separated namespace glob patterns |
| `--agent` | empty | informational agent id |
| `--ops` | `get,put,delete,scan` | comma-separated ops |
| `--ttl` | `1h` | token lifetime |
| `--format` | `paseto` | `paseto` or `jwt` |
| `--secret-key-hex` | `$MINDD_PASETO_SECRET_HEX` | PASETO signing key |
| `--jwt-secret` | `$MINDD_JWT_SECRET` | JWT HS256 secret |
| `--jti` | empty | optional token id |

- `--ns` accepts comma-separated **glob** patterns. A trailing `*` matches any
  suffix: `kv/tool-*` covers `kv/tool-cache`, `kv/tool-results`, and so on.
  `*` alone matches every namespace.
- `--ops` accepts either the dotted form (`kv.put`) or a verb-only form
  (`put`). `*` allows all ops.

Note that the default `--ops get,put,delete,scan` is verb-only, and that
`mindctl` itself has no `kv delete` or `kv scan` command. The default exists for
convenience with the SDKs; it is not a scope you should ship.

:::warning Verb-only ops match across every block
Op matching accepts the bare verb as well as the dotted form, so a token minted
with `--ops inspect` for the lease block **also** satisfies `admin.inspect`.
`admin.inspect` is not namespace scoped, so that token gains cross-namespace
introspection of every namespace the server serves.

Mint fully-qualified ops (`lease.inspect`, not `inspect`) until this is
tightened. Tracked as a known limitation for v0.1.0; see
[Security](../security.md).
:::

## Op names

| Block | Ops |
|---|---|
| `kv` | `kv.get`, `kv.put`, `kv.delete`, `kv.scan` |
| `episodic` | `episodic.append`, `episodic.range`, `episodic.tail`, `episodic.expire` |
| `semantic` | `semantic.upsert`, `semantic.search`, `semantic.delete`, `semantic.expire` |
| `artifact` | `artifact.put`, `artifact.get`, `artifact.stat`, `artifact.delete`, `artifact.list` |
| `lease` | `lease.acquire`, `lease.renew`, `lease.release`, `lease.inspect`, `lease.list` |
| `graph` | `graph.upsert`, `graph.get`, `graph.query`, `graph.delete` |
| `admin` | `admin.inspect` (cross-namespace; not tied to any `--ns` pattern) |

## Authority chain

The auth interceptor enforces this on every request:

```
metadata -> verify signature -> decode scope -> namespace match? -> op match? -> expired?
```

Failures map to gRPC codes:

| Cause | Code |
|---|---|
| Header missing | `Unauthenticated` |
| Bad signature, malformed token | `Unauthenticated` |
| Token expired | `Unauthenticated` |
| Namespace not covered by scope | `PermissionDenied` |
| Op not in scope | `PermissionDenied` |

The capability check happens **before** the per-block service is invoked, and
again inside each service before the driver is touched, which means an unscoped
peer never reaches storage even when a real backend would have served the
request. That second check is why the token's namespace pattern is the
boundary you can rely on most; unlike the policy engine, it applies uniformly
to unary and streaming RPCs.

## Key rotation

Both verifiers accept a list of trusted public keys, not just one. See
[Key rotation](../ops/key-rotation.md) for the overlap-window flow that never
drops in-flight RPCs.

```yaml
auth:
  verifier: paseto
  paseto:
    public_key_hexes:
      - "<new>"   # add the new key first
      - "<old>"   # keep the old one until tokens minted under it expire
```

`mindctl token gen-keypair` generates a fresh pair. Do not use the one in this
repository's examples: its private half is published with it. See
[Security](../security.md).

## Tenant isolation

The `tenant` claim only separates data physically when `tenant_isolation: true`
is set on the server. It defaults to `false`, in which case every tenant shares
one partition and only the namespace pattern separates them. See
[Tenant isolation](./tenant-isolation.md).

## mTLS adds a second factor

When the gRPC TCP listener runs with `client_ca_file` and
`require_client_cert`, peers must also present an X.509 cert chained to that
CA. Capability tokens still apply on top: mTLS authenticates the *peer*, the
token scopes *what they can do*. See
[Transport security](../ops/tls.md).
