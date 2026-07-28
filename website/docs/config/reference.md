---
title: YAML reference
sidebar_position: 1
---

# YAML reference

memsidecar takes a single YAML file via `--config`. The example shipped at
`configs/example.yaml` is annotated; this page is the per-field reference.

Environment variables override fields via the `MEMSIDECAR_` prefix with
double-underscore as section separator:

```bash
MEMSIDECAR_SERVER__GRPC__TCP="0.0.0.0:9000"
```

## Top-level shape

```yaml
server: {...}
observability: {...}
auth: {...}
policy: {...}
tenant_isolation: false   # optional; see below
backends: [...]
namespaces: [...]
```

## tenant_isolation

```yaml
tenant_isolation: true    # default false
```

Scopes every block's storage to the caller's capability `tenant`, so two tenants
sharing a namespace name get physically separate data. Off by default
(single-tenant behavior; existing data unaffected). Covers all six blocks (kv, episodic, lease, graph, artifact, semantic). Restart required to change. See
[Tenant isolation](../concepts/tenant-isolation.md).

## server

```yaml
server:
  grpc:
    tcp: "127.0.0.1:7777"        # leave empty to disable
    uds: "/tmp/memsidecar.sock"
    tls:                          # optional; UDS stays plaintext either way
      cert_file: /etc/.../server.crt
      key_file:  /etc/.../server.key
      client_ca_file: /etc/.../client-ca.crt   # set → mTLS
      require_client_cert: true
  http:
    addr: "127.0.0.1:8080"        # grpc-gateway; empty = disabled
  shutdown_timeout: 10s
```

At least one of `tcp` and `uds` must be set.

## observability

```yaml
observability:
  tracing:
    exporter: stdout              # stdout | otlp | none
    sample_ratio: 1.0
    otlp:                         # only when exporter=otlp
      endpoint: localhost:4317
      insecure: true              # plaintext for localhost
      compression: gzip
      headers:
        x-some-team: literal
      headers_env:
        x-api-key: MY_API_KEY_ENV  # value comes from env at start
  metrics:
    exporter: prometheus          # prometheus | otlp | none
    prometheus:                   # only when exporter=prometheus
      addr: ":9090"
      path: /metrics
    otlp:                         # only when exporter=otlp (push; no /metrics endpoint)
      endpoint: localhost:4317    # same shape as tracing.otlp
      insecure: true
      compression: gzip
      headers_env:
        x-api-key: MY_API_KEY_ENV
  logging:
    level: info                   # debug | info | warn | error (hot-reloadable)
    format: json                  # json | text
```

## auth

```yaml
auth:
  verifier: paseto                # paseto | jwt
  paseto:
    public_key_hex: "..."         # singular; back-compat
    public_key_hexes:             # rotation list — all keys are trusted
      - "<new>"
      - "<old>"
  jwt:
    alg: HS256                    # HS256 | RS256
    secret_env: MEMSIDECAR_JWT_SECRET   # HS256 only
    public_pem: /etc/.../jwt.pem        # RS256 singular
    public_pems:                        # RS256 rotation
      - /etc/.../jwt-new.pem
      - /etc/.../jwt-old.pem
```

Only one verifier is active at a time. `auth.verifier=paseto` requires at
least one PASETO public key; `auth.verifier=jwt` requires `alg`.

## policy

See [Policy](../concepts/policy.md) for semantics.

```yaml
policy:
  default: allow                  # allow | deny
  rules:
    - name: block-secrets
      effect: deny                # allow | deny | rate_limit | cap
      reason: "secret-* namespaces are off-limits"
      match:
        tenant:    ["acme"]       # any field optional; empty = match anything
        agent:     ["agent-1"]
        block:     ["kv"]
        namespace: ["secret-*"]   # glob, single trailing *
        op:        ["put", "delete"]   # dotted or verb-only
      bucket:                     # only used when effect=rate_limit
        per_tenant: true
        per_agent: false
        per_namespace: false
        per_op: true
        rate_per_second: 5.0
        burst: 10
    - name: cap-search-topk
      effect: cap                 # bound the magnitude of a single request
      match: { op: ["semantic.search"] }
      max:                        # only used when effect=cap; ≥1 bound required
        top_k:  200               # semantic Search result count
        limit:  0                 # scan/range page size (0 = no bound)
        depth:  0                 # graph traversal depth
        fan_out: 0                # graph traversal fan-out
        rerank_candidate_k: 0     # semantic hybrid per-lane candidate depth
```

`deny` (and `default: deny`) surface as `PermissionDenied`; `rate_limit` and
`cap` rejections surface as `ResourceExhausted` so clients can back off.

The whole `policy` block is reloaded on `SIGHUP`. See
[Hot reload](./hot-reload.md).

## backends

```yaml
backends:
  - name: mem-default
    driver: memory                 # always available
  - name: pg-main
    driver: postgres
    options:
      dsn: "postgres://..."        # OR
      dsn_env: MEMSIDECAR_PG_DSN
      max_conns: 10
      sweeper_interval: 5m         # kv: kv_items expiry sweep
      tail_interval: 250ms         # episodic: Tail poll cadence
      poll_interval: 100ms         # lease: wait_for poll cadence
  - name: blob-local
    driver: fs
    options:
      base_dir: /var/lib/memsidecar/blobs
  - name: blob-s3
    driver: s3
    options:
      endpoint: s3.amazonaws.com
      bucket: my-bucket
      use_ssl: true
      region: eu-west-1
      prefix: "memsidecar/"
      access_key_env: AWS_ACCESS_KEY_ID
      secret_key_env: AWS_SECRET_ACCESS_KEY
```

Not every driver fits every block — `fs` and `s3` only serve `artifact`;
`memory` and `postgres` serve everything except artifact-on-`postgres`
(which isn't implemented). The `graph` block runs on `memory` or `postgres`
(the Postgres driver stores nodes/edges in shared `graph_*` tables and runs
bounded traversal in Go).

## namespaces

```yaml
namespaces:
  - { block: kv,       name: scratchpad, backend: mem-default }
  - { block: episodic, name: events,     backend: pg-main }
  - { block: artifact, name: blobs,      backend: blob-local }
  - { block: lease,    name: locks,      backend: pg-main }
  - { block: graph,    name: knowledge,  backend: mem-default }
  - block: semantic
    name: notes
    backend: pg-main
    text_search: english           # optional; Postgres FTS config for hybrid's sparse lane (default: simple)
    embedder:
      provider: openai             # fake | ollama | openai
      model: text-embedding-3-small
      dimensions: 1536
      cache_size: 4096             # optional; embed-once cache (see below)
      options:
        api_key_env: OPENAI_API_KEY
        timeout: 30s
```

Semantic namespaces require an `embedder` block. The driver chosen for the
backend must support the block (e.g. you can't use `driver: fs` for
`block: kv`).

In-memory `kv` namespaces may add an optional `access` block (cache-tier
tracking, read-through TTL, and heat-based capacity eviction) — off by default.
See [KV → cache-tier access policy](../blocks/kv.md#cache-tier-access-policy-in-memory-only).

`embedder.cache_size` bounds a per-namespace **embedding cache**: identical
content (same `(namespace, model, content)`) is embedded once and served from a
bounded LRU thereafter, cutting provider calls and cost on repeated or duplicate
text. Omit it or set `0` for the default (4096 entries); set a **negative** value
to disable caching for the namespace. Hit/miss rates are exported as
`memsidecar.embedder.cache.{hits,misses}` — see
[Observability](../ops/observability.md).

## What hot-reloads

| Section | Hot-reloadable? |
|---|---|
| `auth.verifier` / keys | yes |
| `policy.*` | yes |
| `observability.logging.level` | yes |
| `server.*`, `observability.tracing/metrics`, `backends`, `namespaces` | restart required |

See [Hot reload](./hot-reload.md) for the contract.
