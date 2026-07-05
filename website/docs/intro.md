---
slug: /
sidebar_position: 1
title: Overview
---

# memsidecar

memsidecar is a **self-hosted, OSS, framework-agnostic memory sidecar for
agentic systems**. It runs as a co-process to one or more agents and exposes
a small, opinionated gRPC API over **pluggable backends** for the kinds
of memory every agent stack ends up reinventing:

| Block | What it's for | Typical backends |
|---|---|---|
| **kv** | Tool-result caching and scratchpads — TTL'd, typed, with an optional heat-based cache tier | Redis, Postgres, in-memory |
| **episodic** | Append-only event log — messages, tool calls, observations; replayable *and* live-tailable, with first-class roles & sessions | Postgres, SQLite, Kafka |
| **semantic** | Embed-and-search over records — **bitemporal & revisable**, with hybrid (dense + sparse) retrieval | pgvector, Qdrant, Pinecone |
| **artifact** | Blob storage with metadata for generated files — streamed in and out | S3, MinIO, local FS |
| **lease** | Distributed locks with TTL for multi-agent coordination | Redis, etcd, Postgres advisory locks |
| **graph** | Typed nodes/edges with bounded traversal — **bitemporal**, as-of queryable | graph DB, Postgres, in-memory |

Agents talk to the sidecar; the sidecar talks to the substrate. The analogy
is Dapr's building-block model, narrowed and specialised to memory for
agentic workloads.

New here? The [Use cases](./guides/use-cases.md) page is the fastest way to
see what these blocks let you build — a hot-tier tool cache, a knowledge base
that stays correct as facts change, hybrid retrieval, a temporal knowledge
graph, and cost-governed autonomous agents.

## Why a sidecar

Every popular agent framework (LangGraph, CrewAI, Autogen, Mastra, bespoke
stacks) re-implements memory plumbing in incompatible ways. Agent code ends
up tightly coupled to specific backends, multi-agent memory sharing is
ad-hoc, access control is either absent or framework-specific, and swapping
Pinecone for Qdrant — or Redis for Postgres — means rewriting agent code.

memsidecar moves that plumbing out of the agent and into a sidecar with:

- **One protocol** (gRPC, with an HTTP/JSON gateway for everything that
  isn't gRPC-native).
- **One auth model** — every request carries a signed
  [capability token](./concepts/capabilities.md) scoped to a tenant, agent,
  namespace pattern, and op set.
- **One policy surface** — declarative YAML rules
  ([allow / deny / rate-limit / cost-cap](./concepts/policy.md)) enforced at
  the edge, hot-reloadable via `SIGHUP`.
- **One observability story** — OpenTelemetry traces (stdout or OTLP) plus
  Prometheus metrics on the same gRPC instrumentation, including a
  [write-vs-query cost split](./ops/observability.md) the memory layer is
  uniquely placed to expose.

## What's in the box

- A single static Go binary (`memsidecar`) plus an admin CLI (`memctl`).
- Drivers for in-memory, Postgres / pgvector, local filesystem, S3 / MinIO.
- A [Python SDK](./clients/python.md) that wraps the gRPC stubs and handles
  capability headers for you.
- A [Helm chart](./deploy/helm.md) for Kubernetes and a multi-stage
  [Docker image](./deploy/docker.md) based on distroless `nonroot`.

## Where to go from here

- New to memsidecar? Start with the [Quickstart](./quickstart.md).
- Wondering what it's *for*? Skim the [Use cases](./guides/use-cases.md) —
  problem-first recipes that map real agent needs onto the blocks.
- Want to understand the model? Read the
  [Architecture](./concepts/architecture.md) page next.
- Looking for a specific block? Jump straight to
  [KV](./blocks/kv.md), [Episodic](./blocks/episodic.md),
  [Semantic](./blocks/semantic.md), [Artifact](./blocks/artifact.md),
  [Lease](./blocks/lease.md), or [Graph](./blocks/graph.md).
- Deploying to Kubernetes? See [Helm](./deploy/helm.md).
