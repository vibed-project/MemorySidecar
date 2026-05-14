---
slug: /
sidebar_position: 1
title: Overview
---

# memsidecar

memsidecar is a **self-hosted, OSS, framework-agnostic memory sidecar for
agentic systems**. It runs as a co-process to one or more agents and exposes
a small, opinionated gRPC API over **pluggable backends** for the five kinds
of memory every agent stack ends up reinventing:

| Block | Purpose | Typical backends |
|---|---|---|
| **kv** | Typed, TTL'd key-value for tool-result caching and scratchpads | Redis, Postgres, in-memory |
| **episodic** | Append-only log of agent events, messages, tool calls | Postgres, SQLite, Kafka |
| **semantic** | Embed-and-search over arbitrary records | pgvector, Qdrant, Pinecone |
| **artifact** | Blob storage with metadata for generated files | S3, MinIO, local FS |
| **lease** | Distributed locks for shared-state coordination | Redis, etcd, Postgres advisory locks |

Agents talk to the sidecar; the sidecar talks to the substrate. The analogy
is Dapr's building-block model, narrowed and specialised to memory for
agentic workloads.

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
- **One policy surface** — declarative YAML rules ([allow / deny / rate-limit](./concepts/policy.md))
  enforced at the edge, hot-reloadable via `SIGHUP`.
- **One observability story** — OpenTelemetry traces (stdout or OTLP) plus
  Prometheus metrics on the same gRPC instrumentation.

## What's in the box

- A single static Go binary (`memsidecar`) plus an admin CLI (`memctl`).
- Drivers for in-memory, Postgres / pgvector, local filesystem, S3 / MinIO.
- A [Python SDK](./clients/python.md) that wraps the gRPC stubs and handles
  capability headers for you.
- A [Helm chart](./deploy/helm.md) for Kubernetes and a multi-stage
  [Docker image](./deploy/docker.md) based on distroless `nonroot`.

## Where to go from here

- New to memsidecar? Start with the [Quickstart](./quickstart.md).
- Want to understand the model? Read the
  [Architecture](./concepts/architecture.md) page next.
- Looking for a specific block? Jump straight to
  [KV](./blocks/kv.md), [Episodic](./blocks/episodic.md),
  [Semantic](./blocks/semantic.md), [Artifact](./blocks/artifact.md), or
  [Lease](./blocks/lease.md).
- Deploying to Kubernetes? See [Helm](./deploy/helm.md).

The original design rationale lives in
[ADR-0001](./reference/adr-0001.md).
