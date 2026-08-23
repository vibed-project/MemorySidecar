---
title: Security
sidebar_position: 4
---

# Security

mindD stores an agent's memory: cached tool results, conversation history,
embeddings, blobs and locks. A compromised sidecar therefore exposes everything
the agents around it have read or written. This page is the deployment
checklist, the list of weaknesses we already know about in v0.1.0, and how to
report one we do not.

## Generate your own signing keypair

:::danger The keypair in the examples is public
The PASETO keypair used throughout this repository is a **development key whose
private half is published alongside it**. It appears in `configs/example.yaml`,
`configs/compose.yaml`, `docker-compose.yml`, the repository `README.md`, this
site's [Quickstart](./quickstart.md) and `examples/README.md`.

Anyone who has read the repository can mint a valid token for any tenant, any
namespace and any op against a server that trusts it. Treat it as if it were
printed on the front page of the project.
:::

Before any deployment that is not a laptop, mint your own:

```bash
mindctl token gen-keypair
```

Put the **public** half in `auth.paseto.public_key_hex` (or
`public_key_hexes`), and keep the private half wherever your token issuer
lives, outside the cluster that runs mindD. mindD only ever verifies; it never
needs the private key.

The Helm chart ships **no** default key. It fails at template time if
`config.auth.paseto.public_key_hex` is unset, and fails again if it is set to
the published development key, so a `helm install` cannot accidentally trust
it. See [Helm](./deploy/helm.md).

## Scope tokens narrowly

Mint tokens with fully-qualified ops (`kv.get`, `kv.put`) rather than bare
verbs.

:::warning Bare verbs match across blocks
Capability op matching accepts a bare verb as well as the dotted form, so a
token minted with `--ops inspect` for the lease block **also** satisfies
`admin.inspect`. `admin.inspect` is not namespace scoped, so that token gains
cross-namespace introspection of every namespace the server serves. Use
`--ops lease.inspect` instead.
:::

The same applies to `--ops '*'`, which every example on this site uses for
convenience. It is a development shortcut, not a production scope.

Namespace patterns are enforced on **every** RPC inside each service, before
the driver is touched, so the token's `--ns` pattern is the boundary you can
rely on most. See [Capability tokens](./concepts/capabilities.md).

## Turn on tenant isolation if you are multi-tenant

`tenant_isolation` defaults to `false`. With it off, every tenant shares one
physical partition of each namespace, and only the token's namespace pattern
separates them: two tenants holding tokens for `kv/scratchpad` read and write
the same data.

```yaml
tenant_isolation: true
```

Set it to `true` for any deployment serving more than one trust domain, and set
it **before** the first write. Enabling it later changes where data is
addressed, so anything written under the un-prefixed namespace stops being
visible to tenant-scoped reads. See
[Tenant isolation](./concepts/tenant-isolation.md).

## Do not expose the sidecar

mindD is designed to run as a co-process next to its agents, on loopback or a
unix domain socket.

:::warning gRPC reflection is unauthenticated
The gRPC reflection service is registered unconditionally and is exempt from
the auth interceptor, so an internet-reachable sidecar reveals its full schema
to anonymous callers.
:::

- Bind the gRPC listener to loopback, a UDS, or a private network.
- The HTTP/JSON gateway has **no TLS option** in this release. Terminate TLS at
  an ingress if you need it remotely.
- Turn on TLS or mTLS on the gRPC TCP listener when it crosses a host boundary.
  See [TLS and mTLS](./ops/tls.md).

## Encrypt values at rest when the substrate is not trusted

[Encryption at rest](./concepts/encryption-at-rest.md) is opt-in per namespace
and covers `kv` and `episodic` payloads with AES-256-GCM. It protects against
offline access to the substrate: dumps, backups, snapshots, disks, and a
database account that can read rows but not the sidecar's environment.

It does **not** protect against anyone holding a valid capability token, or
against an attacker who has the sidecar's process environment, since that is
where the keys are.

## Rotate signing keys without downtime

Both the PASETO and the JWT (RS256) verifiers accept a list of trusted keys,
and the list is hot-reloadable on `SIGHUP`. Add the new key, switch the issuer,
wait out the longest live token TTL, then drop the old key. On a suspected
compromise, skip the overlap and remove the old key in one `SIGHUP`. See
[Key rotation](./ops/key-rotation.md).

## Known limitations in v0.1.0

These are the weaknesses we have found and documented. They are listed under
**Known limitations** in
[CHANGELOG.md](https://github.com/vibed-project/mindD/blob/main/CHANGELOG.md)
as well.

| Limitation | Impact | What to do instead |
|---|---|---|
| Namespace-scoped policy rules did not apply to streaming RPCs. `KV/Scan`, `Episodic/Range`, `Episodic/Tail`, `Artifact/Put`, `Artifact/Get` and `Artifact/List` reached the policy hook with an empty namespace, so a rule like `deny namespace: ["secret-*"]` never matched them and they fell through to `policy.default`. `cap` rules were inert on the same methods. **Fixed after v0.1.0.** | On v0.1.0, a deny rule you believe is protecting a namespace does not protect its streaming reads. | Enforce the boundary with the token's namespace pattern, which is checked on every RPC regardless. Upgrade past v0.1.0 for the fix. |
| Capability ops match on the bare verb. | `--ops inspect` for `lease` also satisfies `admin.inspect`, which is not namespace scoped. | Mint fully-qualified ops (`lease.inspect`, not `inspect`). |
| `tenant_isolation` defaults to `false`. | Every tenant shares one physical store; only the namespace pattern separates them. | Set `tenant_isolation: true` from the start of a multi-tenant deployment. |
| The access log and traces carry no principal. | The observability interceptor runs above auth, so no request is attributed to a tenant, agent or token id. | Correlate at the client, or in front of the sidecar, until this is fixed. |
| `artifact` on `s3` does not persist `sha256` or `size`. | `Stat` and `List` report an empty digest on that driver. The metadata patch hook is unimplemented for s3. | Use the `Put` response's server-computed digest, which is correct on every driver, and record it yourself. |
| Leases compare an application-clock expiry against the database clock. | Skew between the two can expire or extend a lease. | Keep NTP running on both, and renew comfortably ahead of `expires_at`. |
| Expired leases are never reclaimed from storage. | Rows accumulate in the `leases` table. Correctness is unaffected: expired leases are not honoured. | Prune out of band if the table grows. |
| The gRPC reflection service is registered unconditionally and is exempt from auth. | An exposed sidecar reveals its schema to anonymous callers. | Do not expose the sidecar. Bind to loopback, UDS, or a private network. |

## Supported versions

mindD is experimental and pre-1.0. Proto shapes under `proto/mindd/*/v1` may
still change between minor versions. Security fixes land on `main` and ship in
the next tagged release; there are no backports to earlier tags.

| Version | Supported |
|---------|-----------|
| `main` | yes |
| `v0.1.x` (latest tag) | yes |
| Older tags | no |

## Reporting a vulnerability

**Please do not open a public GitHub issue for a security vulnerability.**

Report privately through
[GitHub Security Advisories](https://github.com/vibed-project/mindD/security/advisories/new)
("Report a vulnerability"). That is the only private channel the maintainers
monitor. If you cannot use it, open a public issue saying only that you have a
security report and asking for a private contact, with no details and no
reproduction steps, and a maintainer will follow up.

Please include, where you can:

- A description of the issue and the impact you believe it has.
- The affected component: a block (`kv`, `episodic`, `semantic`, `artifact`,
  `lease`, `graph`), a driver (`memory`, `postgres`, `fs`, `s3`), or the auth,
  policy or server layer.
- Steps to reproduce, or a proof of concept.
- The version or commit you tested against, and the relevant configuration.

### In scope

- Authentication or capability bypass, including token forgery and scope
  escalation.
- Cross-tenant or cross-namespace data access.
- Path or key traversal in any storage driver.
- Flaws in the encryption-at-rest envelope: key handling, nonce reuse, AEAD
  misuse.
- Remote crashes or unbounded resource consumption reachable by an
  authenticated caller.

Reports matching the **Known limitations** above are still welcome, especially
with a sharper exploit than we expect, but they will not be treated as new
findings.

The full policy lives in
[SECURITY.md](https://github.com/vibed-project/mindD/blob/main/SECURITY.md).
