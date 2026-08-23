---
id: encryption-at-rest
title: Encryption at rest
sidebar_label: Encryption at rest
---

mindd can encrypt stored values so that anyone reading the substrate
directly (a database dump, a stolen disk, a backup, a curious DBA) sees
ciphertext instead of your agents' data.

It is **opt-in per namespace** and off by default. A deployment that doesn't
configure it behaves exactly as before.

## What is and isn't encrypted

| Block | Encrypted | Left plaintext |
|---|---|---|
| `kv` | `value` | key, content type, metadata, version, timestamps, TTL |
| `episodic` | `payload` | cursor, timestamp, type, role, session id, metadata, dedup key |

Everything in the right-hand column is what the backend **queries** on. Keys are
ordered and prefix-scanned, TTLs are swept, and `role`/`session_id`/`type` are
index-backed `Range` predicates. Encrypting them would turn every lookup into a
full scan, so the payload is the boundary.

:::caution Not yet covered
`semantic`, `artifact`, `lease` and `graph` do **not** support `encrypt: true`
and the server refuses to start if you set it on one. Better a clear error than
a namespace that looks encrypted and isn't.

`semantic` is the interesting case: its sparse and hybrid search lanes rank on a
full-text index over the stored content, so sealing that column would silently
break search rather than merely cost performance. It needs a design that
separates the searchable projection from the stored payload.
:::

## Configuration

Keys are declared once, globally. Secrets never appear in YAML. Each key names
an environment variable holding 32 bytes as hex (64 characters) or base64.

```yaml
encryption:
  keys:
    - id: primary-2026-08
      secret_env: MINDD_ENC_PRIMARY

namespaces:
  - block: kv
    name: secrets
    backend: pg-main
    encrypt: true
  - block: kv
    name: tool-cache      # same backend, deliberately not encrypted
    backend: pg-main
  - block: episodic
    name: transcript
    backend: pg-main
    encrypt: true
```

Generate a key with:

```bash
export MINDD_ENC_PRIMARY=$(openssl rand -hex 32)
```

Encryption keys are **not** hot-reloadable. `SIGHUP` swaps the auth verifier,
policy engine and log level only; changing the keyring needs a restart.

## How it works

Each value is sealed with AES-256-GCM into a self-describing envelope:

```
version(1) | keyTag(4) | nonce(12) | ciphertext+tag
```

`keyTag` is derived from the key's `id`, so a ciphertext names the key that
sealed it and decryption never depends on ring order.

Every seal is bound to **where it lives** through AES-GCM's associated data:
the block name, the namespace, and (for `kv`) the record key. That associated
data is authenticated but not stored. The practical effect is that ciphertext
copied to a different key, a different namespace, or a different block will not
open. An attacker with write access to the substrate cannot relocate values to
somewhere they're allowed to read. Under
[tenant isolation](./tenant-isolation.md) the namespace is already
tenant-qualified, so the binding covers the tenant too.

For `episodic`, the binding stops at the namespace: an event's id and cursor are
assigned by the driver during `Append`, so they don't exist yet when the payload
is sealed.

## Rotating keys

The keyring is ordered. The **first** key is active and seals new writes; every
key remains a decryption candidate.

To rotate, prepend the new key and keep the old one:

```yaml
encryption:
  keys:
    - id: primary-2026-08     # new, seals all new writes
      secret_env: MINDD_ENC_PRIMARY
    - id: retired-2026-02     # old, still opens existing values
      secret_env: MINDD_ENC_RETIRED
```

Restart, then let writes migrate the data. Drop the retired key only once no
ciphertext still references it. A value whose key is gone is unrecoverable, and
reads of it fail loudly rather than returning anything partial.

Changing a key's `id` has the same effect as deleting it: the tag changes, and
everything sealed under the old id is orphaned.

## Migrating an existing namespace

Turning on `encrypt: true` for a namespace that already holds plaintext makes
those existing values unreadable. They aren't envelopes, so opening them fails.

`allow_plaintext_reads` bridges that:

```yaml
encryption:
  allow_plaintext_reads: true
  keys:
    - id: primary-2026-08
      secret_env: MINDD_ENC_PRIMARY
```

With it on, a value that isn't a well-formed envelope is returned as-is, while
every write seals. Once the data has been rewritten, turn it back off.

:::warning
While `allow_plaintext_reads` is on, an attacker who can write to the substrate
can strip an envelope and have the plaintext served back to clients. Treat it as
a migration window, not a setting.
:::

## What this does and doesn't protect

**Does:** offline access to the substrate (dumps, backups, snapshots, disks,
and a compromised database account that can read rows but not the sidecar's
environment).

**Doesn't:** anyone who can reach the API with a valid capability token gets
plaintext, because that's the whole point of the service. It also doesn't
protect against an attacker who has the sidecar's process environment, since
that's where the keys are. Values are in plaintext in the sidecar's memory while
in use.

Encryption is a layer under [capabilities](./capabilities.md),
[policy](./policy.md) and [tenant isolation](./tenant-isolation.md), not a
replacement for any of them.

## Operational notes

- **Size.** Each value grows by 33 bytes (17-byte header + 16-byte GCM tag).
  Namespace item *counts* are unaffected, so `kv` capacity limits and quotas
  behave the same.
- **Failure mode.** A value that can't be opened (wrong key, tampering,
  truncation, relocation) returns an error. It never silently yields partial or
  corrupted data.
- **Backends.** Encryption sits above the driver, so it works identically on
  memory, Postgres and Redis without any schema change.
