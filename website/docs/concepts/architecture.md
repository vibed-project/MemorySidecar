---
title: Architecture
sidebar_position: 1
---

# Architecture

memsidecar runs as a co-process to one or more agents — typically in the
same Kubernetes pod, the same VM, or the same container network in local
dev. Agents make local calls over Unix domain socket or loopback gRPC; the
sidecar fans out to configured backends and enforces policy on the way
through.

## Request flow

```
┌──────────┐ gRPC ┌───────────────────────────────────────────────────────────┐
│  Agent   │─────▶│ recovery → observability → auth → policy → block service  │
│ (client) │      │            (slog+OTel)    (token  (YAML rules,             │
└──────────┘      │                            check) hot-reloadable)          │
                  │                                              │              │
                  │                                              ▼              │
                  │                              ┌──────────────────────────┐   │
                  │                              │ kv / episodic / semantic │   │
                  │                              │ artifact / lease / graph │   │
                  │                              │       ↓ registry          │   │
                  │                              │       ↓ driver            │   │
                  │                              │   backend (mem│pg│s3│…)   │   │
                  │                              └──────────────────────────┘   │
                  └───────────────────────────────────────────────────────────┘
```

The interceptor chain is intentional:

1. **Recovery** wraps everything so panics become `Internal` errors
   without taking down the process.
2. **Observability** records a span and an access-log line at the wire
   boundary. OTel metrics flow through the same `otelgrpc.StatsHandler`,
   so `/metrics` reports per-method latency histograms and counters for
   free — plus a memory-aware `memsidecar.op.duration` split by
   write/query op-class and per-block backend-latency / result-shape
   metrics. See [Observability](../ops/observability.md).
3. **Auth** reads `x-memsidecar-capability` from gRPC metadata, verifies
   the token (PASETO or JWT), and attaches a typed `*auth.Capability` to
   the request context. Missing → `Unauthenticated`; out-of-scope →
   `PermissionDenied`. See [Capabilities](./capabilities.md).
4. **Policy** consults the configured engine. The default `NoopEngine`
   allows everything; the YAML rule engine (allow / deny / rate-limit /
   cost-cap) slots in seamlessly. See [Policy](./policy.md).
5. **Service** dispatches to the building-block implementation, which
   resolves the namespace through a per-block **registry** to the
   appropriate **driver**.

## Per-block layout

Every block follows the same internal shape, which makes adding a new one
(or a new driver) cheap:

```
internal/<block>/
├── driver.go      # Driver interface — every backend must satisfy this
├── registry.go    # namespace → Driver lookup
├── service.go     # gRPC service: scope checks, request → Driver call
└── drivers/
    ├── memory/    # always present — used in tests and dev
    ├── postgres/  # //go:build integration tests + embedded migrations
    └── ...
```

The driver interface is deliberately narrow — five to seven methods per
block. New backends only need to satisfy that interface; the rest of the
stack (auth, policy, observability, HTTP gateway) costs them nothing.

## Listeners

memsidecar opens up to four listeners side by side:

| Listener | Purpose | TLS? |
|---|---|---|
| gRPC TCP | Cross-host RPC | yes (optional + mTLS) |
| gRPC UDS | Same-pod RPC | no (filesystem perms) |
| HTTP gateway | JSON-over-HTTP via grpc-gateway | no (terminate at ingress) |
| Prometheus `/metrics` | Scrape endpoint | no |

All four are configurable in [`server` config](../config/reference.md#server).

## Hot reload

A `SIGHUP` re-reads the YAML config and atomically swaps the subsystems
that can change without restarting: the auth verifier, the policy engine,
and the log level. Backends, namespaces, and listeners still require a
full restart. See [Hot reload](../config/hot-reload.md).

## What's *not* in the sidecar

- **No agent framework.** memsidecar doesn't orchestrate, plan, or invoke
  LLMs.
- **No prompt assembly.** Final context-window construction stays in the
  agent or framework.
- **No inference cache.** Prompt/response caching for LLM calls is a
  future concern (could fit as another block).
- **No vector index or graph engine of its own.** The semantic block fronts
  pgvector / Qdrant / Pinecone (it doesn't implement ANN search), and the
  graph block fronts graph engines (it doesn't implement storage, a query
  planner, or a query language) — both stay thin substrate layers.
- **No retrieval orchestration.** "Semantic-search then graph-expand" hybrid
  recall is composed by the agent (sharing ids across blocks), not by the
  sidecar.
