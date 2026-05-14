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
