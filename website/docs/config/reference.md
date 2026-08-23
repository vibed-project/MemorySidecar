---
title: YAML reference
sidebar_position: 1
---

# YAML reference

mindD takes a single YAML file via `--config`. The flag is **required**; the
binary has no default path. (The container image supplies
`/etc/mindd/config.yaml` through its `CMD`.) The example shipped at
`configs/example.yaml` is annotated; this page is the per-field reference.

Environment variables override fields via the `MINDD_` prefix with
double-underscore as section separator. The name is lowercased and `__` becomes
`.`, then merged over the YAML:

```bash
MINDD_SERVER__GRPC__TCP="0.0.0.0:9000"
```

## Top-level shape

```yaml
server: {...}
observability: {...}
auth: {...}
policy: {...}
tenant_isolation: false   # optional; see below
encryption: {...}         # optional; see below
backends: [...]
namespaces: [...]
```

There is no top-level `logging:` or `telemetry:`; logging lives under
`observability.logging`.

## tenant_isolation

```yaml
tenant_isolation: true    # default false
```

Scopes every block's storage to the caller's capability `tenant`, so two
tenants sharing a namespace name get physically separate data. Off by default
(single-tenant behavior; existing data unaffected). Covers all six blocks (kv,
episodic, lease, graph, artifact, semantic). Restart required to change.

Enabling it changes where data is addressed, so set it before the first write.
See [Tenant isolation](../concepts/tenant-isolation.md) and
[Security](../security.md).

## encryption

```yaml
encryption:
  keys:                              # ordered; the first key is active
    - id: primary-2026-08            # stable label, hashed into the envelope
      secret_env: MINDD_ENC_PRIMARY
    - id: retired-2026-02
      secret_env: MINDD_ENC_RETIRED
  allow_plaintext_reads: false       # default false; migration only
```

| Key | Meaning |
|---|---|
| `keys[].id` | Stable key label. Hashed into each envelope so ciphertext names its own key. Changing an id orphans everything sealed under it. Required, and must be unique. |
| `keys[].secret_env` | Name of an env var holding 32 bytes as hex (64 chars) or base64. Required; secrets must not appear in YAML. |
| `allow_plaintext_reads` | Return values that aren't well-formed envelopes as-is, to migrate a namespace that already holds plaintext. See the warning in [Encryption at rest](../concepts/encryption-at-rest.md). |

Declaring keys does nothing on its own; a namespace opts in with
`encrypt: true`. Supported on `kv` and `episodic` only; setting it on any other
block is a startup error, as is `encrypt: true` with no keys configured, or
`allow_plaintext_reads: true` with no keys. Restart required to change (keys are
not hot-reloadable).

## server

```yaml
server:
  grpc:
    tcp: "127.0.0.1:7777"        # default when unset or empty
    uds: "/tmp/mindd.sock"        # optional; no default
    tls:                          # optional; UDS stays plaintext either way
      cert_file: /etc/.../server.crt
      key_file:  /etc/.../server.key
      client_ca_file: /etc/.../client-ca.crt   # set -> mTLS
      require_client_cert: true
  http:
    addr: "127.0.0.1:8080"        # grpc-gateway; unset/empty = disabled
  shutdown_timeout: 10s           # default 10s
```

| Key | Default |
|---|---|
| `server.grpc.tcp` | `127.0.0.1:7777` |
| `server.grpc.uds` | empty (no UDS listener) |
| `server.grpc.tls` | absent (plaintext) |
| `server.http.addr` | empty (gateway disabled) |
| `server.shutdown_timeout` | `10s` |

:::note The TCP listener cannot be turned off
An empty or absent `server.grpc.tcp` is rewritten to `127.0.0.1:7777` before
validation, so a UDS-only deployment is not expressible in this release. Bind
it to loopback if you only want same-host traffic.
:::

TLS engages only when **both** `cert_file` and `key_file` are set. A `tls:`
block containing only `client_ca_file` is silently ignored and the listener
stays plaintext. `MinVersion` is forced to TLS 1.3 and ALPN is `h2`. With
`client_ca_file` plus `require_client_cert: true` the server uses
`RequireAndVerifyClientCert`; with `require_client_cert: false` it uses
`VerifyClientCertIfGiven`.

Setting `server.http.addr` with no gRPC listener is a startup error. The
gateway dials the local gRPC listener with **insecure** credentials, so
enabling `server.grpc.tls` breaks the gateway's own dial. The gateway mirrors
`kv`, `episodic`, `semantic`, `artifact` and `lease` only; `graph` and `admin`
have no HTTP route. See [HTTP / JSON gateway](../clients/http-gateway.md).

## observability

```yaml
observability:
  tracing:
    exporter: stdout              # stdout | otlp | none; default stdout
    sample_ratio: 1.0             # default 1.0; <= 0 is treated as 1.0
    otlp:                         # only when exporter=otlp
      endpoint: localhost:4317    # required in otlp mode
      insecure: true              # plaintext for localhost
      compression: gzip           # only "gzip" has an effect
      headers:
        x-some-team: literal
      headers_env:
        x-api-key: MY_API_KEY_ENV  # value comes from env at start; must be non-empty
  metrics:
    exporter: prometheus          # prometheus | otlp | none; default none
    prometheus:                   # only when exporter=prometheus
      addr: ":9090"               # default ":9090"
      path: /metrics              # default "/metrics"
    otlp:                         # only when exporter=otlp (push; no /metrics endpoint)
      endpoint: localhost:4317    # same shape as tracing.otlp
      insecure: true
      compression: gzip
      headers_env:
        x-api-key: MY_API_KEY_ENV
  logging:
    level: info                   # debug | info | warn | error; default info (hot-reloadable)
    format: json                  # json | text; default json
```

Logs go to **stderr**. An unset `metrics.exporter` means `none`: no meter
provider and no `/metrics` server. In `otlp` metrics mode there is no
`/metrics` endpoint and `prometheus.addr` / `prometheus.path` are ignored.

## auth

```yaml
auth:
  verifier: paseto                # paseto | jwt; default paseto
  paseto:
    public_key_hex: "..."         # singular; back-compat
    public_key_hexes:             # rotation list; all keys are trusted
      - "<new>"
      - "<old>"
  jwt:
    alg: HS256                    # HS256 | RS256
    secret_env: MINDD_JWT_SECRET   # HS256 only
    public_pem: /etc/.../jwt.pem        # RS256 singular
    public_pems:                        # RS256 rotation
      - /etc/.../jwt-new.pem
      - /etc/.../jwt-old.pem
```

Only one verifier is active at a time. `verifier: paseto` requires at least one
PASETO public key; `verifier: jwt` requires `alg`, and an unrecognised `alg`
fails later, when the verifier is built. `public_key_hexes` is consulted first,
then `public_key_hex` if not already present; `public_pem`/`public_pems` are
each either an inline PEM (a string starting `-----BEGIN`) or a file path.

mindD only verifies, so it never needs a private key. `auth.paseto` has a
`private_key_hex` field in the struct that nothing reads; `mindctl token issue`
takes the signing key from `--secret-key-hex` or `$MINDD_PASETO_SECRET_HEX`
instead. Do not put a private key in this file.

:::danger Do not use the example key
`configs/example.yaml`, `configs/compose.yaml` and the quickstart all set
`public_key_hex` to a development key whose private half is published in this
repository. Run `mindctl token gen-keypair`. See [Security](../security.md).
:::

## policy

See [Policy](../concepts/policy.md) for semantics.

```yaml
policy:
  default: allow                  # allow | deny
  rules:
    - name: block-secrets         # required, unique
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
        rate_per_second: 5.0      # required and > 0 for rate_limit
        burst: 10                 # <= 0 becomes 1
    - name: cap-search-topk
      effect: cap                 # bound the magnitude of a single request
      match: { op: ["semantic.search"] }
      max:                        # only used when effect=cap; at least one bound required
        top_k:  200               # semantic Search result count
        limit:  0                 # scan/range page size (0 = no bound)
        depth:  0                 # graph traversal depth
        fan_out: 0                # graph traversal fan-out
        rerank_candidate_k: 0     # semantic hybrid per-lane candidate depth
```

Note the singular key names under `match:` even though each takes a list.

Omitting the whole `policy` block (no `default`, no `rules`) installs the
no-op engine, which allows everything without evaluating anything. Setting
`default:` with an empty `rules` list installs the real rule engine with no
rules, which behaves the same but goes through the evaluation path.

`deny` (and `default: deny`) surface as `PermissionDenied`; `rate_limit` and
`cap` rejections surface as `ResourceExhausted` so clients can back off.

Policy is validated when the engine is built, not by the config loader, so a
typo in `effect` or a missing `bucket.rate_per_second` fails at startup with a
`policy: ...` error.

The whole `policy` block is reloaded on `SIGHUP`. See
[Hot reload](./hot-reload.md).

:::warning Namespace rules and streaming RPCs on v0.1.0
On v0.1.0, `match.namespace` and `cap` rules did not apply to `KV/Scan`,
`Episodic/Range`, `Episodic/Tail`, `Artifact/Put`, `Artifact/Get` or
`Artifact/List`. Fixed after v0.1.0. See [Security](../security.md).
:::

## backends

```yaml
backends:
  - name: mem-default
    driver: memory                 # reads no options
  - name: pg-main
    driver: postgres
    options:
      dsn: "postgres://..."        # OR
      dsn_env: MINDD_PG_DSN
      max_conns: 10                # default 10
      sweeper_interval: 5m         # kv only: kv_items expiry sweep (default 5m)
      tail_interval: 250ms         # episodic only: Tail poll cadence (default 250ms)
      poll_interval: 100ms         # lease only: wait_for poll cadence (default 100ms)
  - name: blob-local
    driver: fs
    options:
      base_dir: /var/lib/mindd/blobs   # required
  - name: blob-s3
    driver: s3
    options:
      endpoint: s3.amazonaws.com   # required
      bucket: my-bucket            # required
      use_ssl: true                # default false
      region: eu-west-1
      prefix: "mindd/"
      access_key: ""               # literal wins over the _env form
      access_key_env: AWS_ACCESS_KEY_ID
      secret_key: ""
      secret_key_env: AWS_SECRET_ACCESS_KEY
```

`name` is required and must be unique. `driver` must be one of `memory`,
`postgres`, `fs`, `s3`.

`options` is an untyped map, so **unknown keys are silently ignored**, and a
duration written as a bare number (`sweeper_interval: 300`) silently falls back
to the default rather than erroring.

### Which drivers serve which blocks

| Block | `memory` | `postgres` | `fs` | `s3` |
|---|---|---|---|---|
| `kv` | yes | yes | no | no |
| `episodic` | yes | yes | no | no |
| `semantic` | yes | yes (pgvector) | no | no |
| `artifact` | yes | **no** | yes | yes |
| `lease` | yes | yes | no | no |
| `graph` | yes | yes | no | no |

A mismatch is a startup error, not a runtime one. The `graph` Postgres driver
stores nodes and edges in shared `graph_*` tables and runs the bounded walk in
Go inside a read transaction.

Postgres migrations always run at startup; there is no config key to skip them.

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
      dimensions: 1536             # required, > 0
      cache_size: 4096             # optional; embed-once cache (see below)
      options:
        api_key_env: OPENAI_API_KEY
        timeout: 30s
```

`block` must be one of `kv`, `episodic`, `semantic`, `artifact`, `lease`,
`graph`. `name` is required, `backend` must reference a declared backend, and
`block/name` pairs must be unique (`kv/notes` and `semantic/notes` can coexist).

### Semantic namespaces

`embedder` is required. `provider` is `fake`, `ollama` or `openai`;
`dimensions` must be positive; `model` is required for `ollama` and `openai`
but not for `fake`. Provider options:

| Provider | Options |
|---|---|
| `fake` | none |
| `ollama` | `base_url` (default `http://localhost:11434`), `timeout` (default `30s`) |
| `openai` | `api_key_env` (required, and the named env var must be non-empty), `base_url` (default `https://api.openai.com`), `timeout` (default `30s`) |

`text_search` is read **only** for semantic namespaces on the Postgres driver;
it is ignored elsewhere. It defaults to `simple` and must match
`^[a-z][a-z0-9_]{0,62}$`.

`embedder.cache_size` bounds a per-namespace **embedding cache**: identical
content (same `(namespace, model, content)`) is embedded once and served from a
bounded LRU thereafter. Omit it or set `0` for the default (4096 entries); set a
**negative** value to disable caching for the namespace. Hit and miss rates are
exported as `mindd.embedder.cache.{hits,misses}`; see
[Observability](../ops/observability.md).

### Cache-tier access policy (in-memory kv only)

```yaml
namespaces:
  - block: kv
    name: tool-cache
    backend: mem-default
    access:
      track: true                    # record last_accessed/access_count on Get
      slide_ttl_seconds: 300         # integer seconds; each Get extends a TTL'd key
      capacity: 10000                # cap live keys; over cap, evict the coldest
      heat_half_life_seconds: 3600   # integer seconds; default 3600
```

All four values are **integer seconds or counts, not duration strings**. The
block is honoured only by the in-memory `kv` driver, and only for `block: kv`;
it is ignored on a Postgres-backed namespace and on every other block. A block
where all four fields are zero is dropped entirely. See
[KV cache-tier access policy](../blocks/kv.md#cache-tier-access-policy-in-memory-only).

### Encryption

`kv` and `episodic` namespaces may set `encrypt: true` to seal stored values
with the [`encryption`](#encryption) keyring. Namespaces sharing a backend can
differ, so one can be encrypted while another stays plaintext. See
[Encryption at rest](../concepts/encryption-at-rest.md).

## What hot-reloads

| Section | Hot-reloadable? |
|---|---|
| `auth.verifier` and keys | yes |
| `policy.*` | yes |
| `observability.logging.level` | yes |
| `server.*`, `observability.tracing`/`metrics`, `backends`, `namespaces`, `encryption`, `tenant_isolation` | restart required |

See [Hot reload](./hot-reload.md) for the contract.
