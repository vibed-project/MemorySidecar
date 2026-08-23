---
title: Graph
sidebar_position: 6
---

# Graph

Relationship-aware recall: typed nodes and edges with **bounded, structured
traversal**, scoped by namespace. The `semantic` block answers
*"what is like this?"*; `graph` answers *"what is connected to this, and how?"*.

Like every block it **fronts** an engine. It does not implement graph storage,
indexing, a query planner, a query language, or any reasoning / entity
resolution. The agent decides which nodes and edges to write; the sidecar stores
and serves them, and hard-caps traversal cost.

## API

```proto
service Graph {
  rpc UpsertNodes(UpsertNodesRequest) returns (UpsertNodesResponse);
  rpc UpsertEdges(UpsertEdgesRequest) returns (UpsertEdgesResponse);
  rpc GetNode(GetNodeRequest) returns (Node);
  rpc Neighbors(NeighborsRequest) returns (NeighborsResponse);  // 1-hop, filtered
  rpc Traverse(TraverseRequest) returns (Subgraph);             // bounded multi-hop
  rpc DeleteNode(DeleteNodeRequest) returns (DeleteNodeResponse);
  rpc DeleteEdge(DeleteEdgeRequest) returns (DeleteEdgeResponse);
}

message Node {
  string id = 1;                       // caller-supplied; shareable with a semantic id
  repeated string labels = 2;          // entity types, e.g. ["Person"]
  map<string, string> props = 3;
  google.protobuf.Timestamp created_at = 4;
}

message Edge {
  string id = 1;
  string type = 2;                     // relationship type, e.g. "AUTHORED"
  string from = 3;                     // source node id
  string to = 4;                       // target node id
  map<string, string> props = 5;
  google.protobuf.Timestamp created_at = 6;
  google.protobuf.Timestamp valid_from = 7;  // bitemporal validity (default reads skip
  google.protobuf.Timestamp valid_to = 8;    //   edges not valid now; both unset = always)
}

enum Direction { DIRECTION_UNSPECIFIED = 0; DIRECTION_OUT = 1; DIRECTION_IN = 2; DIRECTION_BOTH = 3; }

message NeighborsRequest {
  string namespace = 1;
  string node_id = 2;
  repeated string edge_types = 3;   // empty = any
  Direction direction = 4;
  repeated string node_labels = 5;  // filter returned neighbors
  uint32 limit = 6;                 // fan-out cap; 0 = server default; hard-capped
  google.protobuf.Timestamp as_of = 7;  // evaluate edge validity at this instant
}

message TraverseRequest {
  string namespace = 1;
  string start_id = 2;
  repeated string edge_types = 3;
  Direction direction = 4;
  uint32 depth = 5;      // max hops; 0 = server default (1); hard-capped
  uint32 max_nodes = 6;  // result cap; 0 = server default; hard-capped
  google.protobuf.Timestamp as_of = 7;  // evaluate edge validity at this instant
}
```

**Bitemporal edges.** An edge holds while `valid_from <= t < valid_to`; unset
bounds are open (both unset ⇒ always valid, so existing edges are unaffected).
`Neighbors`/`Traverse` skip edges not valid *now* by default and never cross an
invalidated relationship: the same "hallucinations of the past" fix the
[semantic block](semantic.md) applies to facts, applied to relationships
(`Alice WORKS_AT Acme` until she leaves). Pass `as_of` for point-in-time recall.

Reads are **structured, not free-form**: the caller specifies node/edge type
filters, direction, depth, and a fan-out/result cap. There is no query string.
This keeps the protocol portable across backends and traversal cost bounded.

## Bounded traversal

Traversal cost is capped in two layers:

1. **Server-side hard caps (always on).** `Neighbors.limit`, `Traverse.depth`,
   and `Traverse.max_nodes` are clamped to safe maximums (and defaulted when 0)
   before the driver is touched. `Traverse` additionally enforces fixed,
   non-tunable bounds on the total **edges** returned and the **fan-out**
   examined per node, so even a dense or high-degree region can never blow up
   the response or the scan, with no policy configured.
2. **Policy caps (operator-configurable).** A policy `cap` rule can additionally
   reject over-budget requests up front with `ResourceExhausted`, e.g. cap
   `depth` on `graph.query`. See [Policy](../concepts/policy.md).

## Drivers

| Driver | Notes |
|---|---|
| `memory` | Reference driver: nodes/edges in maps with out/in adjacency and bounded BFS traversal. Zero-dependency; the conformance baseline. |
| `postgres` | Production driver. Two shared tables (`graph_nodes`, `graph_edges`) keyed by `(namespace, id)`, adjacency indexes on `(namespace, from_id)` / `(namespace, to_id)`. `Neighbors`/`Traverse` fetch adjacency and run the same bounded walk in Go inside a read transaction (consistent snapshot). It fronts Postgres rather than pushing a recursive query down, trading traversal throughput for a faithful, portable implementation of the hard-capped contract. Passes the same conformance suite as `memory`. |

## Composing with `semantic`

Because node ids are caller-supplied, an agent can **seed** a recall with a
dense `semantic.Search`, then **expand** the returned ids via `Neighbors` /
`Traverse`, a hybrid "search then walk" pattern. The orchestration lives in the
agent, not the sidecar.

## Configuration

```yaml
backends:
  - name: mem-default
    driver: memory
  - name: pg-main            # durable, shared across processes
    driver: postgres
    options:
      dsn_env: MINDD_PG_DSN
      max_conns: 10
namespaces:
  - block: graph
    name: knowledge
    backend: mem-default     # or pg-main for a durable graph
```

The Postgres driver creates its `graph_nodes` / `graph_edges` tables on
startup; a namespace is a column, so one backend serves many graph namespaces.

## gRPC example

```bash
grpcurl -plaintext -H "x-mindd-capability: Bearer $TOKEN" \
  -d '{"namespace":"knowledge","nodes":[
        {"id":"kim","labels":["Person"]},{"id":"paris","labels":["City"]}]}' \
  127.0.0.1:7777 mindd.graph.v1.Graph/UpsertNodes

grpcurl -plaintext -H "x-mindd-capability: Bearer $TOKEN" \
  -d '{"namespace":"knowledge","edges":[
        {"id":"e1","type":"LIVES_IN","from":"kim","to":"paris"}]}' \
  127.0.0.1:7777 mindd.graph.v1.Graph/UpsertEdges

grpcurl -plaintext -H "x-mindd-capability: Bearer $TOKEN" \
  -d '{"namespace":"knowledge","start_id":"kim","direction":"DIRECTION_OUT","depth":2}' \
  127.0.0.1:7777 mindd.graph.v1.Graph/Traverse
```

## Python example

```python
from mindd.graph.v1 import graph_pb2

m.graph.upsert_nodes("knowledge", [
    graph_pb2.Node(id="kim", labels=["Person"]),
    graph_pb2.Node(id="paris", labels=["City"]),
])
m.graph.upsert_edges("knowledge", [
    graph_pb2.Edge(id="e1", type="LIVES_IN", **{"from": "kim"}, to="paris"),
])
sub = m.graph.traverse("knowledge", "kim",
                       direction=graph_pb2.DIRECTION_OUT, depth=2)
print([n.id for n in sub.nodes])
```

## Op names

| Op | Method |
|---|---|
| `graph.upsert` | `Graph/UpsertNodes`, `Graph/UpsertEdges` |
| `graph.get` | `Graph/GetNode` |
| `graph.query` | `Graph/Neighbors`, `Graph/Traverse` |
| `graph.delete` | `Graph/DeleteNode`, `Graph/DeleteEdge` |

## Notes

- `DeleteNode` rejects with `FailedPrecondition` when the node still has
  incident edges, unless `cascade=true` (which removes them too).
- Edges do not cross namespaces; cross-namespace linking is out of scope
  (tenant isolation boundary).
- Not a graph query language, not a reasoning engine, not a hybrid-retrieval
  orchestrator. Those stay out of the sidecar by design.
