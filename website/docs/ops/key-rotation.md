---
title: Key rotation
sidebar_position: 2
---

# Key rotation

Both PASETO and JWT (RS256) verifiers accept a **list** of trusted public
keys. The standard rotation flow combines this with
[hot reload](../config/hot-reload.md) and never drops in-flight RPCs.

## The flow

```
stage 1: [old]            ← only the old key trusted
stage 2: [new, old]       ← overlap window; both work
stage 3: [new]            ← old key removed
```

In config:

```yaml
auth:
  verifier: paseto
  paseto:
    # stage 1
    public_key_hex: "<old>"
```

```yaml
auth:
  verifier: paseto
  paseto:
    # stage 2 — add the new key alongside, SIGHUP
    public_key_hexes:
      - "<new>"
      - "<old>"
```

```yaml
auth:
  verifier: paseto
  paseto:
    # stage 3 — drop the old key once tokens minted under it have expired
    public_key_hex: "<new>"
```

After each edit, `kill -HUP $(pgrep mindd)`.

## End-to-end smoke

Walked through against a running server with `kill -HUP` between each
stage:

| stage | configured keys | token signed by `<old>` | token signed by `<new>` |
|---|---|---|---|
| 1 | `[old]` | `OK` | `Unauthenticated` |
| 2 | `[new, old]` | `OK` | `OK` |
| 3 | `[new]` | `Unauthenticated` | `OK` |

The server process never restarted; the PID stayed the same throughout.

## Picking the overlap-window length

Set the overlap to at least the **longest TTL of any token currently
minted under the old key**. If your issuer mints 1-hour tokens, keep the
old key in the list for an hour after switching the issuer's private key.
Drop it on the next SIGHUP.

If you discover a key compromise and need to invalidate **immediately**,
skip the overlap — remove the old key in one SIGHUP. Every in-flight
token signed by it dies on the next request, which is the right
trade-off for that scenario.

## JWT RS256

The same pattern works for JWT with RS256:

```yaml
auth:
  verifier: jwt
  jwt:
    alg: RS256
    public_pems:
      - /etc/mindd/jwt/new-key.pem
      - /etc/mindd/jwt/old-key.pem
```

`HS256` uses a single shared secret and doesn't benefit from multi-key —
rotation there means changing the secret, which invalidates every token
in one step (no overlap is possible with symmetric keys without
coordination).

## See also

- [Capability tokens](../concepts/capabilities.md) — what's in a token.
- [Hot reload](../config/hot-reload.md) — what reloads, what doesn't.
