---
slug: /
sidebar_position: 1
title: Overview
---

# mindD

mindD is a **self-hosted, OSS, framework-agnostic memory sidecar for
agentic systems**. It runs as a co-process to one or more agents and exposes
a small, opinionated gRPC API over **pluggable backends** for the kinds
of memory every agent stack ends up reinventing:

| Block | What it's for | Drivers in v0.1.0 |
|---|---|---|
| **kv** | Tool-result caching and scratchpads: TTL'd, typed, with an optional heat-based cache tier | `memory`, `postgres` |
| **episodic** | Append-only event log (messages, tool calls, observations), replayable *and* live-tailable, with first-class roles and sessions | `memory`, `postgres` |
| **semantic** | Embed-and-search over records: **bitemporal and revisable**, with hybrid (dense + sparse) retrieval | `memory`, `postgres` (pgvector) |
| **artifact** | Blob storage with metadata for generated files, streamed in and out | `memory`, `fs`, `s3` (S3 / MinIO / R2) |
| **lease** | Distributed locks with TTL for multi-agent coordination | `memory`, `postgres` |
| **graph** | Typed nodes/edges with bounded traversal: **bitemporal**, as-of queryable | `memory`, `postgres` |

Those are the drivers that ship and are covered by the conformance suite and
the integration tests. The `Driver` interface per block is deliberately narrow
(five to seven methods), so adding Redis, Qdrant, Kafka or a native graph
engine is a self-contained piece of work rather than a rewrite. None of them
exist yet.

Agents talk to the sidecar; the sidecar talks to the substrate. The analogy
is Dapr's building-block model, narrowed and specialised to memory for
agentic workloads.

New here? The [Use cases](./guides/use-cases.md) page is the fastest way to
see what these blocks let you build: a hot-tier tool cache, a knowledge base
that stays correct as facts change, hybrid retrieval, a temporal knowledge
graph, and cost-governed autonomous agents.

## Status

**v0.1.0 is tagged.** Container images, cross-platform binaries and
`go install` all work; see [Install](./install.md).

mindD is pre-1.0. Proto shapes under `proto/mindd/*/v1` may still change
between minor versions. Each release also documents what it does **not**
enforce yet: read [Security](./security.md) before you deploy, and
[CHANGELOG.md](https://github.com/vibed-project/mindD/blob/main/CHANGELOG.md)
for the full notes.

:::warning The example keypair is public
Every quickstart, example config and compose file in this project uses a
development PASETO keypair whose private half is committed to the repository.
Anyone who has read it can mint a token for any tenant. Run
`mindctl token gen-keypair` before you deploy. See [Security](./security.md).
:::

## Why a sidecar

Every popular agent framework (LangGraph, CrewAI, Autogen, Mastra, bespoke
stacks) re-implements memory plumbing in incompatible ways. Agent code ends
up tightly coupled to specific backends, multi-agent memory sharing is
ad-hoc, access control is either absent or framework-specific, and swapping
one store for another means rewriting agent code.

mindD moves that plumbing out of the agent and into a sidecar with:

- **One protocol** (gRPC, with an HTTP/JSON gateway for most of it).
- **One auth model.** Every request carries a signed
  [capability token](./concepts/capabilities.md) scoped to a tenant, agent,
  namespace pattern, and op set.
- **One policy surface.** Declarative YAML rules
  ([allow / deny / rate-limit / cost-cap](./concepts/policy.md)) enforced at
  the edge, hot-reloadable via `SIGHUP`.
- **One observability story.** OpenTelemetry traces (stdout or OTLP) plus
  Prometheus metrics on the same gRPC instrumentation, including a
  [write-vs-query cost split](./ops/observability.md) the memory layer is
  uniquely placed to expose.

## What's in the box

- A single static Go binary (`mindd`) plus a CLI (`mindctl`) that mints
  tokens, drives part of the data plane, and runs an
  [MCP stdio server](./clients/mcp.md).
- Drivers for in-memory, Postgres / pgvector, local filesystem, S3 / MinIO.
- Opt-in [encryption at rest](./concepts/encryption-at-rest.md) for `kv` and
  `episodic` payloads, and opt-in
  [tenant isolation](./concepts/tenant-isolation.md) across all six blocks.
- A [Python SDK](./clients/python.md) and a
  [TypeScript SDK](./clients/typescript.md), both of which handle capability
  headers for you, plus
  [framework adapters](./guides/framework-adapters.md) for LangGraph, CrewAI
  and the Vercel AI SDK.
- A [Helm chart](./deploy/helm.md) for Kubernetes and a multi-stage
  [Docker image](./deploy/docker.md) based on distroless `nonroot`, published
  to `ghcr.io/vibed-project/mindd`.

## Where to go from here

- Want it running? [Install](./install.md), then the
  [Quickstart](./quickstart.md).
- Deploying it? Read [Security](./security.md) first. It is short and it
  matters.
- Wondering what it's *for*? Skim the [Use cases](./guides/use-cases.md),
  problem-first recipes that map real agent needs onto the blocks.
- Want to understand the model? Read the
  [Architecture](./concepts/architecture.md) page.
- Looking for a specific block? Jump straight to
  [KV](./blocks/kv.md), [Episodic](./blocks/episodic.md),
  [Semantic](./blocks/semantic.md), [Artifact](./blocks/artifact.md),
  [Lease](./blocks/lease.md), or [Graph](./blocks/graph.md), or
  [Admin](./blocks/admin.md) for cross-namespace introspection.
- Deploying to Kubernetes? See [Helm](./deploy/helm.md).
