---
title: Observability
sidebar_position: 3
---

# Observability

Three signals come out of mindD:

| Signal | Where it lands | Format |
|---|---|---|
| Traces | stdout (default) or an OTLP collector | OpenTelemetry |
| Metrics | `:9090/metrics`, or pushed over OTLP | Prometheus / OTLP |
| Access log | stderr | structured JSON via `slog` |

All three flow through the same gRPC instrumentation
([`otelgrpc.NewServerHandler`](https://pkg.go.dev/go.opentelemetry.io/contrib/instrumentation/google.golang.org/grpc/otelgrpc))
plus a thin in-process interceptor, so per-method latency, status codes, and
trace-to-log correlation are automatic.

:::warning No request is attributed to a principal
The observability interceptor sits **above** auth in the chain
(`recovery -> observability -> auth -> policy`), so the capability is not yet
on the context when the span attributes and the access-log line are written.
Neither carries a tenant, an agent, or a token id in this release.

Practically: you can see *what* was called and *how it failed*, but not *who
called it*. Correlate at the client, or in front of the sidecar, until this is
fixed. Tracked as a known limitation; see [Security](../security.md).
:::

## Traces

```yaml
observability:
  tracing:
    exporter: stdout              # stdout | otlp | none; default stdout
    sample_ratio: 1.0             # default 1.0
```

`stdout` is the default and dumps full span JSON to stdout, useful for local
debugging and terrible for production volume. Switch to `otlp` to ship to any
OTel-compatible collector (Honeycomb, Lightstep, Tempo, a self-hosted
otel-collector):

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

The exporter is gRPC. `endpoint` is required in `otlp` mode. Headers come
either as literals (`headers`) or from env vars (`headers_env`), so API keys
never live in the YAML; a named env var that is empty is a startup error.
Sampling is `ParentBased(TraceIDRatioBased(sample_ratio))`, and a ratio of zero
or less is treated as `1.0`.

Span attributes:

- `rpc.method`, `rpc.system.name`, `rpc.response.status_code`, added by
  `otelgrpc`.
- `mindd.method`, added by the observability interceptor.

## Metrics

```yaml
observability:
  metrics:
    exporter: prometheus
    prometheus:
      addr: ":9090"               # default ":9090"
      path: /metrics              # default "/metrics"
```

Metrics are **off unless you ask for them**: an unset `metrics.exporter` means
`none`, which installs a no-op meter provider and starts no HTTP server.

Set `exporter: otlp` to **push** the same metrics to an OTLP/gRPC collector via
a periodic reader instead, reusing the identical `otlp` endpoint and header
config that tracing uses, so metrics and traces land in one collector. In OTLP
mode there is no `/metrics` endpoint and `prometheus.addr` / `prometheus.path`
are ignored:

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

In Prometheus mode, mindD registers Go runtime and process collectors out of
the box plus the gRPC instrumentation's own histograms:

```
rpc_server_call_duration_seconds_bucket{rpc_method="mindd.kv.v1.KV/Put",rpc_response_status_code="OK",le="0.005"} ...
rpc_server_call_duration_seconds_count{rpc_method="mindd.kv.v1.KV/Put",rpc_response_status_code="OK"} 3
go_goroutines 14
process_resident_memory_bytes ...
```

Per-method latency and error-rate dashboards work without any extra
instrumentation.

On top of the raw gRPC histogram, mindD emits its own memory-aware duration
metric that splits **write/index time from query time**, the distinction the
memory literature cares about most (index construction vs query cost):

```
mindd_op_duration_seconds_bucket{block="semantic",op="semantic.upsert",op_class="write",namespace="notes",code="OK",le="0.05"} ...
mindd_op_duration_seconds_count{block="semantic",op="semantic.search",op_class="query",namespace="notes",code="OK"} 12
```

Labels: `block`, `op`, `op_class` (`write` or `query`), `namespace`, and
`code`. Only recognized building-block methods are recorded (health and
reflection calls are skipped), so cardinality is bounded by
`blocks x ops x namespaces x codes`. Streaming methods (`kv.scan`,
`episodic.range`, `episodic.tail`, `artifact.put`, `artifact.get`,
`artifact.list`) carry an **empty** `namespace`: it is not available at the
interceptor boundary. That is the same gap that made namespace-scoped policy
rules miss streaming RPCs on v0.1.0.

This makes the write-vs-query cost split queryable directly, for example p99
query latency for one namespace:

```
histogram_quantile(0.99,
  sum by (le) (rate(mindd_op_duration_seconds_bucket{op_class="query",namespace="notes"}[5m])))
```

### Backend vs sidecar overhead

`mindd.op.duration` is the whole RPC. The semantic service additionally records
how much of that was spent **inside the engine** (pgvector or the in-memory
driver), so you can see the sidecar's own overhead:

```
mindd_backend_duration_seconds{block="semantic",op="semantic.search",namespace="notes"} ...
mindd_result_size{block="semantic",op="semantic.search",namespace="notes"}          ...
mindd_result_top_score{block="semantic",namespace="notes"}                            ...
```

- `mindd.backend.duration`, driver-call time by `block`, `op`, `namespace`.
- `mindd.result.size`, records returned (Search) or affected (Upsert, Expire).
- `mindd.result.top_score`, top-1 cosine of a Search, a cheap
  evidence-completion proxy (retrieval quality, not just latency).
- `mindd.embedder.cache.hits` and `.misses`, embedding-cache effectiveness by
  `namespace` and `model`. Identical content is embedded once; a high hit rate
  means the semantic namespace is avoiding repeat provider calls (see
  [`embedder.cache_size`](../config/reference.md#semantic-namespaces)). Miss
  rate times provider latency is the embed cost the cache is shaving off.

`backend.duration` shares the `block` / `op` / `namespace` labels with
`op.duration`, so **sidecar overhead is a direct subtraction**, with no extra
instrument needed:

```
# mean sidecar overhead (s) for one namespace's queries
  sum(rate(mindd_op_duration_seconds_sum{op_class="query",namespace="notes"}[5m]))
    / sum(rate(mindd_op_duration_seconds_count{op_class="query",namespace="notes"}[5m]))
- sum(rate(mindd_backend_duration_seconds_sum{op="semantic.search",namespace="notes"}[5m]))
    / sum(rate(mindd_backend_duration_seconds_count{op="semantic.search",namespace="notes"}[5m]))
```

These service-layer metrics are scoped to the `semantic` block, where the
backend/sidecar split matters most (embedding plus vector search dominate the
RPC). The same instruments generalise to the other blocks along the shared
`block` / `op` / `namespace` labels when a backend there warrants the same
scrutiny.

### Namespace growth and eviction

Two cheap instruments track how much a namespace is holding and how fast it
turns over, the unbounded-growth side of the cost story:

```
mindd_namespace_items{block="kv",namespace="scratchpad"}        128
mindd_eviction_total{block="kv",namespace="scratchpad",cause="ttl"}  57
```

- `mindd.namespace.items`, an observable **gauge** of the live item count per
  `block` and `namespace`, polled on scrape. It is reported only by drivers
  that can answer cheaply: the in-memory drivers for every block, and the
  pgvector semantic driver, which uses the planner's `reltuples` estimate and
  never `count(*)` on the scrape path.

  **Everything else is silently absent**, including every Postgres-backed
  `kv`, `episodic`, `lease` and `graph` namespace (they share tables, so a
  per-namespace count is not cheap) and the `fs` and `s3` artifact backends.
  Watch those at the datastore layer. The same source feeds `item_count` /
  `has_count` on the [Admin block](../blocks/admin.md).
- `mindd.eviction.total`, a **counter** of items dropped from a namespace,
  labelled by `cause`. The in-memory KV driver emits `cause="ttl"` at lazy
  expiry (on `Get`) and on the background sweep (which runs every 30 seconds
  and is not configurable), and `cause="capacity"` when a cache-tier namespace
  evicts its coldest keys over capacity. `consolidation` is reserved.
  `rate(mindd_eviction_total[5m])` is your eviction churn.

### Benchmarks

Each block's in-memory driver ships Go benchmarks that split the **write/index
path** from the **query path**. They are advisory, not a CI gate; run them per
backend:

```bash
go test -run '^$' -bench 'Upsert|Search'  ./internal/semantic/drivers/memory/
go test -run '^$' -bench 'Put|Get'        ./internal/kv/drivers/memory/
go test -run '^$' -bench 'UpsertEdges|Traverse' ./internal/graph/drivers/memory/
```

## Logs

slog JSON to **stderr**. The observability interceptor emits one access line
per RPC:

```json
{"msg":"rpc","method":"/mindd.kv.v1.KV/Put","code":"OK","latency":803875}
```

Those three fields are the whole line: method, gRPC status code, and latency in
nanoseconds. As noted at the top of this page, there is no tenant, agent or
token id. A failed call logs at WARN with the same shape.

Failures from the auth, policy and recovery interceptors, and from drivers,
carry their own log lines at WARN or ERROR, so `"level":"WARN"` finds denied
requests.

The level is hot-reloadable. Set `observability.logging.level: debug` and
`kill -HUP` to crank verbosity for one incident without restarting. See
[Hot reload](../config/hot-reload.md).

## Prometheus and Kubernetes

The Helm chart can render a `ServiceMonitor` for prometheus-operator:

```yaml
# values.yaml
serviceMonitor:
  enabled: true
  interval: 30s
```

Without prometheus-operator, scrape the `metrics` Service port directly from
your scrape config. The chart's default config already sets
`observability.metrics.exporter: prometheus` on `0.0.0.0:9090`.

## Suggested alerts

A few that fall out cheaply from the metrics above:

- `rate(rpc_server_call_duration_seconds_count{rpc_response_status_code!="OK"}[5m]) > 0.1`,
  sustained gRPC error rate.
- `histogram_quantile(0.99, rate(rpc_server_call_duration_seconds_bucket[5m])) > 0.5`,
  p99 RPC latency over 500ms.
- `rate(rpc_server_call_duration_seconds_count{rpc_response_status_code="ResourceExhausted"}[5m]) > 0`,
  policy rate-limits or caps firing, which signals either a misconfiguration or
  genuine abuse.
- `rate(rpc_server_call_duration_seconds_count{rpc_response_status_code="Unauthenticated"}[5m]) > 0`,
  expired or forged tokens. Worth watching given that the access log cannot tell
  you who they came from.
