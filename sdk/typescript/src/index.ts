// @memsidecar/client — TypeScript SDK for memsidecar.
export { MemSidecar } from "./client.js";
export type {
  MemSidecarOptions,
  KVPutOptions,
  KVScanOptions,
  EpisodicAppendOptions,
  EpisodicRangeOptions,
  EpisodicTailOptions,
  SemanticSearchOptions,
  SemanticExpireOptions,
  SemanticRecordInput,
  FieldPredicateInput,
  ArtifactPutOptions,
  ArtifactGetOptions,
  LeaseAcquireOptions,
  GraphNeighborsOptions,
  GraphTraverseOptions,
  NodeInput,
  EdgeInput,
} from "./client.js";

export { capabilityInterceptor, CAPABILITY_HEADER } from "./auth.js";

// Enums (runtime values).
export { SearchMode, PredicateOp, ExpireAction } from "./gen/memsidecar/semantic/v1/semantic_pb.js";
export { Direction } from "./gen/memsidecar/graph/v1/graph_pb.js";

// Message types returned by the block clients.
export type {
  GetResponse as KVGetResponse,
  PutResponse as KVPutResponse,
  DeleteResponse as KVDeleteResponse,
  KVItem,
} from "./gen/memsidecar/kv/v1/kv_pb.js";
export type { Event } from "./gen/memsidecar/episodic/v1/episodic_pb.js";
export type {
  Record as SemanticRecord,
  Hit,
  FieldPredicate,
  UpsertResponse,
} from "./gen/memsidecar/semantic/v1/semantic_pb.js";
export type {
  ArtifactRef,
  ArtifactMeta,
  StatResponse as ArtifactStatResponse,
} from "./gen/memsidecar/artifact/v1/artifact_pb.js";
export type {
  LeaseHandle,
  InspectResponse as LeaseInspectResponse,
} from "./gen/memsidecar/lease/v1/lease_pb.js";
export type {
  Node as GraphNode,
  Edge as GraphEdge,
  Subgraph,
  NeighborsResponse,
} from "./gen/memsidecar/graph/v1/graph_pb.js";
