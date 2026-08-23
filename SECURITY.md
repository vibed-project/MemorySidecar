# Security Policy

mindD stores an agent's memory: cached tool results, conversation history,
embeddings, blobs and locks. A compromised sidecar therefore exposes
everything the agents around it have read or written. We take reports
seriously and would rather hear about a problem early than politely.

## Reporting a Vulnerability

**Please do not open a public GitHub issue for security vulnerabilities.**

Report privately via
[GitHub Security Advisories](https://github.com/vibed-project/mindD/security/advisories/new)
("Report a vulnerability"). That is the only private channel we monitor.

If you cannot use that channel, open a public issue saying only that you have a
security report and asking for a private contact — no details, no reproduction
steps — and a maintainer will follow up.

Please include, where you can:

- A description of the issue and the impact you believe it has
- The affected component — a block (`kv`, `episodic`, `semantic`, `artifact`,
  `lease`, `graph`), a driver (`memory`, `postgres`, `fs`, `s3`), or the auth,
  policy or server layer
- Steps to reproduce, or a proof of concept
- The version or commit you tested against, and the relevant configuration

We aim to acknowledge a report within a few business days and will keep you
updated as we investigate. Please give us a reasonable window to ship a fix
before public disclosure. We are happy to credit reporters in the advisory
unless you prefer to remain anonymous.

## Supported Versions

mindD is experimental and pre-1.0. Security fixes land on `main` and ship in
the next tagged release; there are no backports to earlier tags.

| Version | Supported |
|---------|-----------|
| `main` | ✅ |
| `v0.1.x` (latest tag) | ✅ |
| Older tags | ❌ |

## Deploying mindD safely

### Generate your own signing keypair

The keypair in this repository's examples — `configs/example.yaml`,
`docker-compose.yml`, the README and the quickstart — is a **development key
whose private half is published with it**. Anyone who has read this repository
can mint a valid token for any tenant against a server that trusts it.

```
mindctl token gen-keypair
```

The Helm chart has no default key and refuses to render without one, and
refuses to render if it is given the published development key.

### Scope tokens narrowly

Mint tokens with fully-qualified ops (`kv.get`, `kv.put`) rather than bare
verbs. Capability matching currently also accepts the bare verb, so a token
minted with `--ops inspect` for leases additionally satisfies `admin.inspect`,
which is not namespace-scoped and exposes cross-namespace introspection. This
is tracked as a known limitation for v0.1.0.

### Do not rely on namespace deny rules for streaming RPCs

In v0.1.0 the policy engine receives an empty namespace for the streaming
methods (`KV/Scan`, `Episodic/Range`, `Episodic/Tail`, and the three
`Artifact` streams), so a rule matching on `namespace` does not apply to them.
Enforce those boundaries with the token's namespace pattern, which is checked
on every RPC.

### Turn on tenant isolation if you are multi-tenant

`tenant_isolation` defaults to `false`, which puts every tenant in one physical
partition and leaves only the token's namespace pattern between them. Set it to
`true` for any deployment serving more than one trust domain.

### Do not expose the sidecar

mindD is designed to run as a co-process next to its agents. The gRPC
reflection service is registered unconditionally and is exempt from
authentication, so an internet-reachable sidecar reveals its full schema to
anonymous callers. Bind to loopback or a private network, and terminate TLS at
an ingress if you need the HTTP/JSON gateway remotely — the gateway itself has
no TLS option in this release.

## What we consider in scope

- Authentication or capability bypass, including token forgery and scope
  escalation
- Cross-tenant or cross-namespace data access
- Path or key traversal in any storage driver
- Flaws in the encryption-at-rest envelope (key handling, nonce reuse, AEAD
  misuse)
- Remote crashes or unbounded resource consumption reachable by an
  authenticated caller

## What we already know

The **Known limitations** section of [CHANGELOG.md](CHANGELOG.md) lists the
weaknesses we have already found and documented for v0.1.0. Reports that match
those are still welcome — especially with a sharper exploit than we expect —
but they will not be treated as new findings.
