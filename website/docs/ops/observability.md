---
title: Observability
sidebar_position: 3
---

# Observability

Three signals come out of memsidecar:

| Signal | Where it lands | Format |
|---|---|---|
| Traces | stdout (default) or an OTLP collector | OpenTelemetry |
| Metrics | `:9090/metrics` HTTP endpoint | Prometheus |
| Access log | stderr | structured JSON via `slog` |

All three flow through the same gRPC instrumentation
([`otelgrpc.NewServerHandler`](https://pkg.go.dev/go.opentelemetry.io/contrib/instrumentation/google.golang.org/grpc/otelgrpc))
plus a thin in-process interceptor, so per-method latency, status codes,
and trace-to-log correlation are automatic.

## Traces

```yaml
observability:
  tracing:
    exporter: stdout              # stdout | otlp | none
    sample_ratio: 1.0
```

`stdout` is the default and dumps full span JSON to stdout — useful for
local debugging, terrible for production volume. Switch to `otlp` to
ship to any OTel-compatible collector (Honeycomb, Lightstep, Tempo,
self-hosted otel-collector, etc.):

```yaml
observability:
  tracing:
    exporter: otlp
    sample_ratio: 0.1
    otlp:
      endpoint: otel-collector.observability.svc.cluster.local:4317
      insecure: true              # within-cluster plaintext
      compression: gzip
      headers_env:
        x-honeycomb-team: HONEYCOMB_API_KEY
```

The exporter is gRPC; headers come either as literals or from env vars
(so API keys never live in the YAML).

Span attributes:

- `rpc.method`, `rpc.system.name`, `rpc.response.status_code` — added by
  `otelgrpc`.
- `memsidecar.method`, `memsidecar.tenant`, `memsidecar.agent` — added by
  the observability interceptor.

## Metrics

```yaml
observability:
  metrics:
    exporter: prometheus
    prometheus:
      addr: ":9090"
      path: /metrics
```

Prometheus is the default (pull, via the `/metrics` endpoint). Set
`exporter: otlp` to **push** the same metrics to an OTLP/gRPC collector via a
periodic reader instead — reusing the identical `otlp` endpoint/headers config
that tracing uses (so metrics and traces land in one collector). In OTLP mode
there is no `/metrics` HTTP endpoint:

```yaml
observability:
  metrics:
    exporter: otlp
    otlp:
      endpoint: otel-collector.observability.svc.cluster.local:4317
      insecure: true
      headers_env:
        x-api-key: MY_API_KEY_ENV
```

memsidecar registers Go runtime + process collectors out of the box plus
the gRPC instrumentation's own histograms:

```
rpc_server_call_duration_seconds_bucket{rpc_method="memsidecar.kv.v1.KV/Put",rpc_response_status_code="OK",le="0.005"} ...
rpc_server_call_duration_seconds_count{rpc_method="memsidecar.kv.v1.KV/Put",rpc_response_status_code="OK"} 3
go_goroutines 14
process_resident_memory_bytes ...
```

Per-method latency and error-rate dashboards work without any extra
instrumentation.

On top of the raw gRPC histogram, memsidecar emits its own memory-aware
duration metric that splits **write/index time from query time** — the
distinction the memory literature cares about most (index-construction vs.
query cost):

```
memsidecar_op_duration_seconds_bucket{block="semantic",op="semantic.upsert",op_class="write",namespace="notes",code="OK",le="0.05"} ...
memsidecar_op_duration_seconds_count{block="semantic",op="semantic.search",op_class="query",namespace="notes",code="OK"} 12
```

Labels: `block`, `op`, `op_class` (`write`|`query`), `namespace`, and `code`.
Only recognized building-block methods are recorded (health/reflection calls
are skipped), so cardinality is bounded by `blocks × ops × namespaces × codes`.
Streaming methods (`episodic.range`/`tail`, `artifact.get`) carry an empty
`namespace` — it is not available at the interceptor boundary.

This makes the write-vs-query cost split queryable directly, e.g. p99 query
latency for one namespace:

```
histogram_quantile(0.99,
  sum by (le) (rate(memsidecar_op_duration_seconds_bucket{op_class="query",namespace="notes"}[5m])))
```

### Backend vs. sidecar overhead

`op.duration` is the whole RPC. The semantic service additionally records how
much of that was spent **inside the engine** (pgvector / the in-memory driver),
so you can see the sidecar's own overhead — the payoff of the "we front engines"
design:

```
memsidecar_backend_duration_seconds{block="semantic",op="semantic.search",namespace="notes"} ...
memsidecar_result_size{block="semantic",op="semantic.search",namespace="notes"}          ...
memsidecar_result_top_score{block="semantic",namespace="notes"}                            ...
```

- `memsidecar.backend.duration` — driver-call time by `block`, `op`, `namespace`.
- `memsidecar.result.size` — records returned (Search) or affected (Upsert / Expire).
- `memsidecar.result.top_score` — top-1 cosine of a Search, a cheap
  evidence-completion proxy (retrieval quality, not just latency).
- `memsidecar.embedder.cache.hits` / `.misses` — embedding-cache effectiveness
  by `namespace` and `model`. Identical content is embedded once; a high hit
  rate means the semantic namespace is avoiding repeat provider calls (see
  [`embedder.cache_size`](../config/reference.md#namespaces)). Miss rate ×
  provider latency is the embed cost the cache is shaving off.

`backend.duration` shares the `block` / `op` / `namespace` labels with
`op.duration`, so **sidecar overhead is a direct subtraction** — no extra
instrument needed:

```
# mean sidecar overhead (s) for one namespace's queries
  sum(rate(memsidecar_op_duration_seconds_sum{op_class="query",namespace="notes"}[5m]))
    / sum(rate(memsidecar_op_duration_seconds_count{op_class="query",namespace="notes"}[5m]))
- sum(rate(memsidecar_backend_duration_seconds_sum{op="semantic.search",namespace="notes"}[5m]))
    / sum(rate(memsidecar_backend_duration_seconds_count{op="semantic.search",namespace="notes"}[5m]))
```

These service-layer metrics are scoped to the `semantic` block, where the
backend/sidecar split matters most (embedding + vector search dominate the RPC).
The same instruments generalise to the other blocks along the shared
`block`/`op`/`namespace` labels when a backend there warrants the same scrutiny.

### Namespace growth & eviction

Two cheap instruments track how much a namespace is holding and how fast it
turns over — the unbounded-growth side of the cost story:

```
memsidecar_namespace_items{block="kv",namespace="scratchpad"}        128
memsidecar_eviction_total{block="kv",namespace="scratchpad",cause="ttl"}  57
```

- `memsidecar.namespace.items` — an observable **gauge** of the live item count
  per `block` / `namespace`, polled on scrape. Memory drivers report map length;
  the pgvector semantic driver uses the planner's `reltuples` estimate, never
  `count(*)` on the scrape path. Shared-table Postgres blocks (`kv`, `episodic`,
  `lease`) and object-store artifact backends (`fs`, `s3`) are intentionally
  omitted — a cheap per-namespace count isn't available there; watch those at
  the datastore layer.
- `memsidecar.eviction.total` — a **counter** of items dropped from a namespace,
  labelled by `cause`. The in-memory KV driver emits `cause="ttl"` at lazy-expiry
  (on `Get`) and on the background sweep, and `cause="capacity"` when a
  cache-tier namespace evicts its coldest keys over capacity (U5);
  `consolidation` is reserved. `rate(memsidecar_eviction_total[5m])` is your
  eviction churn.

### Benchmarks

Each block's in-memory driver ships Go benchmarks that split the **write/index
path** from the **query path** — the construction-vs-query distinction the memory
literature centers on. They're advisory (not a CI gate); run them per backend:

```bash
go test -run '^$' -bench 'Upsert|Search'  ./internal/semantic/drivers/memory/
go test -run '^$' -bench 'Put|Get'        ./internal/kv/drivers/memory/
go test -run '^$' -bench 'UpsertEdges|Traverse' ./internal/graph/drivers/memory/
```

## Logs

slog JSON to stderr. Two lines per RPC:

- The access line emitted by the observability interceptor:
  ```json
  {"msg":"rpc","method":"/memsidecar.kv.v1.KV/Put","code":"OK","latency":803875}
  ```
- Any structured log lines the handler emits explicitly.

Failures from auth, policy, recovery interceptors and from drivers carry
their own log lines at WARN/ERROR — grep `"level":"WARN"` to find
denied requests.

The level is hot-reloadable. Set
`observability.logging.level: debug` and `kill -HUP` to crank verbosity
for one incident without restarting.

## Prometheus + Kubernetes

The Helm chart can render a `ServiceMonitor` for prometheus-operator:

```yaml
# values.yaml
serviceMonitor:
  enabled: true
  interval: 30s
```

Without prometheus-operator, scrape the `metrics` Service port directly
from your scrape config.

## Suggested alerts

A few that fall out cheaply from the metrics above:

- `rate(rpc_server_call_duration_seconds_count{rpc_response_status_code!="OK"}[5m]) > 0.1`
  — sustained gRPC error rate.
- `histogram_quantile(0.99, rate(rpc_server_call_duration_seconds_bucket[5m])) > 0.5`
  — p99 RPC latency over 500ms.
- `rate(rpc_server_call_duration_seconds_count{rpc_response_status_code="ResourceExhausted"}[5m]) > 0`
  — policy rate-limits firing (signal of either misconfig or genuine
  abuse).
