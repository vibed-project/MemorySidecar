# ADR-0002 — Graph Building Block

> **Status:** Accepted  ·  **Decision drivers:** Self-hosted OSS; bring-your-own backends; framework-agnostic protocol; relationship-aware recall as a first-class primitive  ·  **Date:** 2026-05-24
>
> Implemented: `internal/graph/` with the in-memory reference driver + conformance suite, structured `Neighbors`/`Traverse` with server-side hard caps, and the standard auth/policy/config wiring. A production driver remains a follow-up (§6).
>
> Extends [ADR-0001](adr-0001-memory-sidecar.md). This ADR adds a sixth
> building block; it does not revisit the cross-cutting decisions (auth,
> policy, observability, config) established there.

## 1. Context and problem statement

ADR-0001 established five building blocks: `kv`, `episodic`, `semantic`,
`artifact`, and `lease`. Between them they cover caching, history, similarity
recall, blobs, and coordination — but none of them models **relationships
between entities**.

Agentic workloads increasingly need this. An agent that has accumulated facts
about people, documents, tools, and tasks wants to ask *connected* questions:
"what is reachable from this entity," "what links these two records," "expand
the neighborhood around the records semantic search just returned." The
`semantic` block answers *similarity* ("what is like this?"); it cannot answer
*relationship* ("what is connected to this, and how?"). `kv`/`episodic` can
encode edges by convention, but neither offers indexed multi-hop traversal, so
every agent that needs a graph ends up standing up a graph database directly —
reintroducing exactly the per-agent backend coupling, absent policy
enforcement, and namespace sprawl that ADR-0001 set out to remove.

We propose a **`graph` building block**: a small, opinionated set of node/edge
write and bounded-traversal read primitives, exposed over the same gRPC
protocol, scoped by the same namespace + capability model, and backed by
pluggable graph backends chosen at deploy time. Agents store and traverse a
graph through the sidecar; the sidecar talks to the graph substrate and
enforces policy on the way through — identical in spirit to every existing
block.

## 2. Design goals and non-goals

### 2.1 Goals

- **Relationship-aware recall as a first-class primitive.** Nodes, typed
  edges, and bounded traversal, not relationships smuggled through KV keys or
  flat metadata.
- **Same contracts as the other blocks.** Namespace-scoped, capability-secured,
  policy-hooked, OTel-instrumented. A `graph` namespace looks and behaves like a
  `kv` or `semantic` namespace from the operator's seat.
- **Bring-your-own graph backend.** A driver interface with an in-memory
  reference driver plus production drivers, selected in YAML like every other
  backend.
- **Backend-agnostic protocol.** The wire surface exposes structured traversal
  primitives, not a vendor query dialect, so swapping the backend never
  rewrites agent code.
- **Composable with `semantic`.** Designed so an agent can seed a traversal from
  the ids returned by a semantic search, enabling hybrid recall **assembled by
  the agent** — not by the sidecar.

### 2.2 Non-goals

- **Not a graph database.** As with `semantic` and vector DBs, the block *fronts*
  graph engines; it does not implement its own storage, indexing, or query
  planner.
- **Not a graph query language.** No Cypher/Gremlin/SPARQL surface as the
  primary interface. A backend-native pass-through is an explicit open question
  (§8), not a v1 commitment.
- **Not a reasoning or inference engine.** No ontology reasoning, entity
  resolution, or automated relationship inference. The agent decides what nodes
  and edges to write; the sidecar stores and serves them.
- **Not a context-window compiler.** Consistent with ADR-0001 §2.2, hybrid
  retrieval orchestration (semantic seed → graph expand → rank → assemble)
  stays in the agent, not the sidecar.

## 3. Where it fits

The block slots into the existing request path with no new cross-cutting
machinery. The interceptor chain (recovery → observability → auth → policy →
service) and the `Registry` → `Driver` dispatch are reused verbatim:

```
Agent ──gRPC──▶ recovery → observability → auth → policy → Graph service
                                                              │
                                                              ▼
                                              graph.Registry → graph.Driver
                                              (memory │ <backend> │ …)
```

Capability scoping is unchanged: a token carries `graph/<namespace>` patterns
and `graph.*` ops, checked in the service layer before any driver call, exactly
as `kv` and `semantic` do today.

## 4. API surface (illustrative)

```proto
service Graph {
  // Writes — idempotent upserts keyed by caller-supplied ids.
  rpc UpsertNodes(UpsertNodesRequest) returns (UpsertNodesResponse);
  rpc UpsertEdges(UpsertEdgesRequest) returns (UpsertEdgesResponse);

  // Reads.
  rpc GetNode(GetNodeRequest) returns (Node);
  rpc Neighbors(NeighborsRequest) returns (NeighborsResponse); // 1-hop, filtered
  rpc Traverse(TraverseRequest) returns (Subgraph);            // bounded multi-hop

  // Deletes.
  rpc DeleteNode(DeleteNodeRequest) returns (Ack);  // with edge-cascade option
  rpc DeleteEdge(DeleteEdgeRequest) returns (Ack);
}
```

Every request carries the capability header and a `namespace` field, validated
before the backend is touched — identical to the other services.

`Neighbors` and `Traverse` are deliberately **structured**: caller specifies
node/edge type filters, direction, depth, and a fan-out/result cap. There is no
free-form query string in v1. This keeps the protocol portable across backends
and keeps traversal cost bounded (§8).

## 5. Driver interface

Mirrors the established driver style (context-first, `namespace` parameter,
`*Options` structs, sentinel errors, `Close()`):

```go
// ErrNotFound is returned when a node or edge id is absent.
var ErrNotFound = errors.New("graph: not found")

type Node struct {
    ID        string
    Labels    []string          // entity types, e.g. ["Person"]
    Props     map[string]string
    CreatedAt time.Time
}

type Edge struct {
    ID        string
    Type      string            // relationship type, e.g. "AUTHORED"
    From, To  string            // node ids
    Props     map[string]string
    CreatedAt time.Time
}

type NeighborOptions struct {
    NodeID     string
    EdgeTypes  []string // empty = any
    Direction  Direction // out | in | both
    NodeLabels []string // filter returned neighbors
    Limit      uint32
}

type TraverseOptions struct {
    StartID   string
    EdgeTypes []string
    Direction Direction
    MaxDepth  uint32 // hard-capped server-side
    MaxNodes  uint32 // hard-capped server-side
}

// Driver is the contract every graph backend implements.
// Implementations must be safe for concurrent use.
type Driver interface {
    UpsertNodes(ctx context.Context, namespace string, nodes []Node) error
    UpsertEdges(ctx context.Context, namespace string, edges []Edge) error
    GetNode(ctx context.Context, namespace, id string) (Node, error)
    Neighbors(ctx context.Context, namespace string, opts NeighborOptions) ([]Node, []Edge, error)
    Traverse(ctx context.Context, namespace string, opts TraverseOptions) (Subgraph, error)
    DeleteNode(ctx context.Context, namespace, id string, cascade bool) (existed bool, err error)
    DeleteEdge(ctx context.Context, namespace, id string) (existed bool, err error)
    Close() error
}
```

A `graphtest/conformance.go` suite (Harness + `RunConformance`, as in every
other block) defines the behavioral contract; a backend earns coverage by
implementing the harness, not by writing per-driver tests.

## 6. Candidate backends

| Driver | Role | Rationale |
|---|---|---|
| **in-memory** | Reference + conformance baseline | Mandatory first driver; zero-dependency dev/test, matches every other block. |
| **Embedded graph engine** | Single-binary self-hosted | Keeps the "one process, no external service" story for small deployments. Trade-off: may introduce cgo / static-binary friction (§8). |
| **Standalone graph DB** | Production / shared | The de-facto choice for teams already running a graph engine; speaks its native protocol behind the driver. |
| **Graph-over-Postgres** | Reuse existing substrate | Attractive because Postgres is already a first-class backend (pgx is a dependency). Lets a deployment add the block without a new datastore, at the cost of traversal performance. |

Exact products are a v1 implementation choice, not an ADR commitment. The
in-memory driver ships first; at least one production driver follows before the
block is considered ready.

## 7. Data model concepts

Extends ADR-0001 §6 rather than replacing it:

| Concept | Definition |
|---|---|
| **Node** | A unit within a `graph` namespace: `(id, labels, props, created_at)`. The `id` is caller-supplied so it can be shared with a `semantic` record id for hybrid recall. |
| **Edge** | A typed, directed relationship `(id, type, from, to, props, created_at)` between two nodes in the same namespace. |
| **Namespace** | Isolation/grouping boundary, scoped to the `graph` block — same semantics as for every other block. |
| **Traversal** | A bounded, structured read (1-hop neighbors or depth-limited expansion), always subject to server-side depth/fan-out caps. |

Edges do not cross namespaces. Cross-namespace or cross-tenant linking is out of
scope and likely violates the tenant isolation boundary; revisit only with an
explicit decision.

## 8. Risks and open questions

- **Query expressiveness vs. backend-agnosticism.** The central tension.
  Structured traversal stays portable but cannot express everything a native
  query language can. Do we eventually add an opt-in, capability-gated
  backend-native query pass-through (clearly marked non-portable), or hold the
  line on structured primitives only?
- **Traversal cost.** Graph queries can explode. Depth and fan-out **must** be
  hard-capped server-side, surfaced as `ResourceExhausted`, and integrated with
  the existing policy `rate_limit` buckets. What are sane defaults?
- **Id coupling with `semantic`.** Sharing ids across the two blocks enables
  hybrid recall but creates an implicit contract. Do we document a convention
  only, or offer a helper? (Leaning: convention only — orchestration stays in
  the agent.)
- **Backend feature parity.** The in-memory and Postgres drivers will not match
  a dedicated engine's traversal semantics. The conformance suite defines the
  guaranteed subset; anything beyond it is backend-specific and undocumented.
- **Embedded-engine build cost.** An embedded graph driver may pull in cgo,
  complicating the distroless static-binary image. Acceptable only if it can be
  a build-tagged/optional driver that doesn't burden the default build.
- **Schema.** Schemaless labels/props (proposed) vs. a declared per-namespace
  schema. Start schemaless; reconsider if validation/policy needs it.

## 9. Alternatives considered

| Option | Why rejected as primary |
|---|---|
| Encode edges in `kv`/`episodic` by convention | No indexed multi-hop traversal; every consumer re-implements graph logic client-side — the coupling ADR-0001 removes. |
| Add adjacency to `semantic` record metadata | Metadata is flat key/value; no edge types, no direction, no traversal. Conflates similarity and relationship. |
| Agents talk to a graph DB directly | Reintroduces per-agent backend lock-in, bypasses central policy/namespace isolation, and breaks the framework-agnostic goal. |
| Expose a native query language (Cypher/Gremlin) as the primary surface | Couples the protocol to one backend dialect; swapping backends rewrites agent code. Demoted to a possible opt-in escape hatch (§8). |
| Build our own graph storage/indexing | Contradicts the "not a database, we front engines" stance (ADR-0001 §2.2). High cost, no differentiation. |

## 10. Decision

Add a sixth building block, **`graph`**, to the memsidecar surface:

- Exposes structured **node/edge upsert** and **bounded traversal** primitives
  over gRPC (plus the HTTP/JSON mirror), scoped by the existing namespace +
  capability + policy model.
- Defines a `graph.Driver` interface with an **in-memory reference driver**
  shipping first and a conformance suite establishing the behavioral contract;
  at least one production-grade driver follows before the block is marked ready.
- **Does not** implement graph storage itself, **does not** expose a
  backend-native query language as the primary interface, and **does not**
  perform hybrid retrieval orchestration in v1 — agents compose `semantic` +
  `graph` results themselves.

This is additive: it introduces no new cross-cutting subsystems and reuses the
auth, policy, observability, config, and registry machinery from ADR-0001
unchanged.

## 11. How it slots into the codebase

Follows the "Adding a building block" recipe in
[`docs/architecture.md`](../architecture.md):

1. Add `proto/memsidecar/graph/v1/graph.proto`; run `make proto`.
2. Create `internal/graph/`: `driver.go`, `registry.go`, `service.go`,
   `drivers/memory/`, `graphtest/conformance.go` — mirroring `internal/semantic/`.
3. Add op constants to `internal/auth/capability.go`:
   `OpGraphUpsert` (`graph.upsert`), `OpGraphGet` (`graph.get`),
   `OpGraphQuery` (`graph.query` — neighbors/traverse), `OpGraphDelete`
   (`graph.delete`).
4. Map each gRPC method → op in `internal/interceptor/policy.go` (`methodToOp`),
   marking upserts/deletes as writes.
5. Wire the service in `internal/server/server.go` and add a
   `buildGraphRegistry` to `cmd/memsidecar/main.go`.
6. Extend `internal/config/config.go` validation for `block: graph`.
7. Add a `graph` page under `website/docs/blocks/` and a Python client under
   `sdk/python/`, matching the other blocks.

## 12. Roadmap placement

This extends the ADR-0001 §10 roadmap. The `graph` block is targeted **after the
core five blocks stabilize** (post-v0.2), as a v0.3/v0.4-class capability,
sequenced behind multi-tenant hardening. Delivery is incremental:

1. proto + `internal/graph` skeleton + in-memory driver + conformance suite.
2. One production driver + Python SDK + docs.
3. Bounded-traversal limits wired into the policy engine.
4. (Revisit) hybrid-recall convenience and/or native-query escape hatch, pending
   the §8 open questions.
