# ADR-0001 — Memory Sidecar for Agentic Systems

> **Status:** Proposed  ·  **Decision drivers:** Self-hosted OSS; bring-your-own backends; framework-agnostic protocol  ·  **Date:** 2026-05-11
>
> The authoritative version lives in [`ADR-0001_Memory_Sidecar.docx`](ADR-0001_Memory_Sidecar.docx).
> This markdown copy exists so the document is discoverable from the source tree and renders in GitHub.

## 1. Context and problem statement

Agentic systems — single agents, multi-agent crews, long-running autonomous workflows — all need persistence. In practice this means a stack of disparate stores: a KV cache for tool results, a vector database for semantic recall, an append-only log for episodic history, blob storage for generated artifacts, and some form of working memory that outlives a single inference call. Every popular framework (LangGraph, CrewAI, Autogen, Mastra, bespoke stacks) re-implements this plumbing in incompatible ways.

The result: agent code is tightly coupled to specific backends, multi-agent memory sharing is ad-hoc, access control is either absent or framework-specific, and swapping Pinecone for Qdrant — or Redis for Postgres — means rewriting agent code. Operations teams have no uniform place to enforce retention, PII policy, encryption, or tenancy.

We propose a **Memory Sidecar**: a co-located process that exposes a small, opinionated, framework-agnostic API over pluggable backends. Agents talk to the sidecar; the sidecar talks to the substrate. The analogy is Dapr's building-block model, narrowed and specialised to memory and state for agentic workloads.

## 2. Design goals and non-goals

### 2.1 Goals

- **Self-hosted, OSS-first.** Single static binary or container. No mandatory SaaS dependency.
- **Bring-your-own backends.** Postgres+pgvector, Redis, S3-compatible blob stores, Qdrant, Pinecone, etc. — chosen at deployment time, not by the agent author.
- **Framework-agnostic protocol.** gRPC primary, HTTP/JSON mirror. Any language with a gRPC client can target it.
- **Multi-tenant and capability-secured.** Every call is scoped by an identity token; ACLs at the namespace and operation level.
- **Observable by default.** Every memory operation emits structured traces, metrics, and audit events.
- **Policy at the edge.** Retention, PII redaction, encryption-at-rest, tenant isolation enforced in the sidecar, not the agent.

### 2.2 Non-goals

- **Not an agent framework.** Does not orchestrate, plan, or invoke LLMs.
- **Not an inference cache.** Prompt/response caching for LLM calls is out of scope.
- **Not a context-window compiler.** Final prompt assembly stays in the agent or framework.
- **Not a vector database.** It fronts vector DBs; it does not implement ANN search itself.

## 3. Architectural overview

The sidecar runs as a co-process to one or more agents — typically in the same pod (Kubernetes), the same host (VM), or the same container network (local dev). Agents make local calls over Unix domain socket or loopback gRPC; the sidecar fans out to configured backends and enforces policy on the way through.

### 3.1 Deployment topology

```
+--------------------+         +--------------------+
|     Agent A        |         |     Agent B        |
|  (any language)    |         |  (any language)    |
+---------+----------+         +----------+---------+
          |   gRPC/uds                    |
          +------------+   +--------------+
                       v   v
              +------------------+
              |  Memory Sidecar  |   <-- per-pod or per-host
              |  (single binary) |
              +---+----+----+----+
                  |    |    |
        Postgres  |    |    |  Redis
        pgvector  |    |    |
                  v    v    v
              Qdrant   S3   ...   (plug-in backends)
```

### 3.2 Core building blocks

| Building block | Purpose | Typical backend(s) |
|---|---|---|
| **kv** | Typed, TTL'd key-value for tool-result caching and scratchpads | Redis, Postgres, in-memory |
| **episodic** | Append-only log of agent events, messages, tool calls | Postgres, SQLite, Kafka |
| **semantic** | Embed-and-search over arbitrary records | pgvector, Qdrant, Pinecone, Weaviate |
| **artifact** | Blob storage with metadata for generated files | S3, MinIO, local FS |
| **lease** | Distributed locks and leases for shared-state coordination | Redis, etcd, Postgres advisory locks |

### 3.3 Cross-cutting concerns

- **Identity & capability** — every request carries a token scoped to (tenant, agent, namespaces, operations).
- **Policy engine** — declarative rules for retention, PII redaction, encryption, rate limiting, tenant isolation.
- **Observability** — OpenTelemetry traces, Prometheus metrics, structured audit log.
- **Backend abstraction** — driver interface per building block; backends configured in YAML, hot-reloadable.

## 4. Required components

- **4.1 Protocol layer** — gRPC primary, HTTP/JSON gateway via grpc-gateway, UDS transport for same-pod co-location.
- **4.2 Auth and identity** — short-lived signed capability tokens (PASETO or JWT) issued by an external IdP or dev-mode issuer, capability model `(tenant, agent_id, namespace_pattern, allowed_ops, ttl)`, optional mTLS.
- **4.3 Policy engine** — declarative YAML or Rego. Hooks: pre-write, pre-read, post-read, on-eviction.
- **4.4 Backend drivers** — per building block; reference + optional drivers (see ADR table).
- **4.5 Embedding service abstraction** — pluggable embedders (OpenAI, Voyage, Cohere, Ollama, llama.cpp).
- **4.6 Configuration** — single YAML, hot-reload via SIGHUP / fsnotify.
- **4.7 Observability** — OTel tracing, Prometheus metrics, structured audit log.
- **4.8 Admin surface** — admin gRPC/HTTP API, `memctl` CLI, optional read-only web UI.

## 5. API surface (illustrative)

```proto
service KV {
  rpc Get(GetRequest) returns (Value);
  rpc Put(PutRequest) returns (PutResponse);   // with TTL, CAS
  rpc Delete(DeleteRequest) returns (Ack);
  rpc Scan(ScanRequest) returns (stream KVItem);
}
service Episodic {
  rpc Append(AppendRequest) returns (Cursor);
  rpc Range(RangeRequest) returns (stream Event);
  rpc Tail(TailRequest) returns (stream Event);
}
service Semantic {
  rpc Upsert(UpsertRequest) returns (UpsertResponse);
  rpc Search(SearchRequest) returns (SearchResponse);
  rpc Delete(DeleteRequest) returns (Ack);
}
service Artifact {
  rpc Put(stream PutChunk) returns (ArtifactRef);
  rpc Get(GetRequest) returns (stream Chunk);
  rpc Stat(StatRequest) returns (ArtifactMeta);
}
service Lease {
  rpc Acquire(AcquireRequest) returns (LeaseHandle);
  rpc Renew(RenewRequest) returns (LeaseHandle);
  rpc Release(ReleaseRequest) returns (Ack);
}
```

Every request carries a Capability header containing the bearer token. The sidecar validates, decodes the scope, and rejects out-of-scope namespace or operation access before the backend driver is touched.

## 6. Data model concepts

| Concept | Definition |
|---|---|
| **Tenant** | Top-level isolation boundary. Tenants cannot read each other's data. |
| **Namespace** | Logical grouping within a tenant, scoped to one building block. |
| **Record** | A unit of data within a namespace. Always has `(id, created_at, metadata)`. |
| **Capability** | Signed bearer asserting `(tenant, agent, ns_pattern, ops, ttl)`. |
| **Policy** | Rule attached to a namespace or tenant. Evaluated at hook points around each op. |

## 7. Alternatives considered

| Option | Why rejected as primary |
|---|---|
| Library, not sidecar | Forces every agent runtime to embed it. Loses central policy enforcement and per-deploy backend swap. |
| Use Dapr directly | No semantic-search primitive, no agent-shaped episodic log, no embedder abstraction. |
| Adopt Letta / mem0 / Zep as-is | Bake in opinionated memory models, are framework-coupled or SaaS-leaning. |
| Build on one vector DB's API | Locks users into that vendor. |

## 8. Risks and open questions

- Latency budget — every memory op adds a hop.
- Context assembly boundary — where retrieval stops and prompt construction begins.
- Embedding model drift — changing embedder invalidates vectors.
- Multi-agent consistency — last-write-wins, CRDTs, or explicit leases? v0.1 says leases for shared state, LWW elsewhere.
- Policy expressiveness vs. simplicity.
- Competitive positioning.

## 9. Decision

Build a self-hosted, OSS, framework-agnostic Memory Sidecar implementing the five building blocks (kv, episodic, semantic, artifact, lease) over pluggable backends, with capability-based auth and a policy engine. Ship a v0.1 reference distribution with Postgres + pgvector + S3-compatible blob store as the default substrate. Provide thin client SDKs in Python and TypeScript; rely on gRPC and HTTP for everything else.

## 10. Roadmap outline

| Milestone | Scope |
|---|---|
| **v0.1 (MVP)** | kv + episodic + semantic; pgvector + Postgres backends; capability tokens; YAML policy; Python SDK; OTel traces. |
| **v0.2** | artifact + lease blocks; S3 driver; Qdrant driver; TypeScript SDK; admin CLI; hot-reload config. |
| **v0.3** | Multi-tenant hardening; Rego policy support; second embedder providers; reference Helm chart; Grafana dashboards. |
| **v0.4** | Streaming primitives (Kafka episodic); CRDT-backed shared working memory experiment; framework integrations (LangGraph, CrewAI adapters). |

## 11. Open decisions to resolve before v0.1

- **Implementation language.** Go (confirmed for this build).
- **Capability token format.** PASETO default, JWT optional (confirmed: ship both via `TokenVerifier` interface).
- **Default embedding provider for dev.** Local (Ollama) vs OpenAI — TBD.
- **Naming.** Working title only — the project needs a real name before public OSS release.
