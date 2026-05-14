---
title: Capability tokens
sidebar_position: 2
---

# Capability tokens

Every request to memsidecar carries a signed bearer token in the gRPC
metadata key `x-memsidecar-capability` (or the HTTP header of the same
name). The token encodes **who** is allowed to do **what**:

- `tenant` — top-level isolation boundary
- `agent` — informational id, useful in audit
- `namespaces` — list of glob patterns (e.g. `kv/scratchpad`, `kv/tool-*`)
- `ops` — list of permitted operations (`kv.put`, `episodic.append`, `*`, …)
- `exp` — expiration

Two signature formats are supported: **PASETO v4.public** (the default) and
**JWT** (HS256 or RS256). Both are pluggable behind a `TokenVerifier`
interface.

## Issuing a token

Use `memctl` for dev. In production, the issuer is your IdP — memsidecar
only verifies.

```bash
memctl token issue \
  --tenant acme --agent agent-1 \
  --ns 'kv/scratchpad,episodic/events' \
  --ops 'get,put,append,range' --ttl 1h
```

- `--ns` accepts comma-separated **glob** patterns. A trailing `*` matches
  any suffix: `kv/tool-*` covers `kv/tool-cache`, `kv/tool-results`, etc.
  `*` alone matches every namespace.
- `--ops` accepts either dotted form (`kv.put`) or verb-only
  (`put` — matches the verb across any block). `*` allows all ops.
- `--format paseto|jwt` switches the signing format; default is PASETO.

## Authority chain

The auth interceptor enforces this on every request:

```
metadata → verify signature → decode scope → namespace match? → op match? → expired?
```

Failures map to gRPC codes:

| Cause | Code |
|---|---|
| Header missing | `Unauthenticated` |
| Bad signature, malformed token | `Unauthenticated` |
| Token expired | `Unauthenticated` |
| Namespace not covered by scope | `PermissionDenied` |
| Op not in scope | `PermissionDenied` |

The capability check happens **before** the per-block service is invoked,
which means an unscoped peer never reaches the driver — even when a real
backend would have served the request.

## Key rotation

Both verifiers accept a list of trusted public keys, not just one. See
[Key rotation](../ops/key-rotation.md) for the textbook overlap-window
flow that never drops in-flight RPCs.

```yaml
auth:
  verifier: paseto
  paseto:
    public_key_hexes:
      - "<new>"   # add the new key first
      - "<old>"   # keep the old one until tokens minted under it expire
```

## mTLS adds a second factor

When the gRPC TCP listener runs with `client_ca_file` + `require_client_cert`,
peers must also present an X.509 cert chained to that CA. Capability tokens
still apply on top — mTLS authenticates the *peer*, the token scopes
*what they can do*. See [Transport security](../ops/tls.md).
