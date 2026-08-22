// @mindd/client — TypeScript SDK for mindd.
export { MindD } from "./client.js";
export type {
  MindDOptions,
  KVPutOptions,
  KVScanOptions,
  EpisodicAppendOptions,
  EpisodicRangeOptions,
  EpisodicTailOptions,
  EpisodicExpireOptions,
  SemanticSearchOptions,
  SemanticExpireOptions,
  SemanticRecordInput,
  FieldPredicateInput,
  ArtifactPutOptions,
  ArtifactGetOptions,
  ArtifactListOptions,
  LeaseAcquireOptions,
  GraphNeighborsOptions,
  GraphTraverseOptions,
  NodeInput,
  EdgeInput,
} from "./client.js";

export { capabilityInterceptor, CAPABILITY_HEADER } from "./auth.js";

// Enums (runtime values).
export { SearchMode, PredicateOp, ExpireAction } from "./gen/mindd/semantic/v1/semantic_pb.js";
// Episodic Expire has its own action enum, aliased to avoid colliding with the
// semantic one above.
export { ExpireAction as EpisodicExpireAction } from "./gen/mindd/episodic/v1/episodic_pb.js";
export { Direction } from "./gen/mindd/graph/v1/graph_pb.js";

// Message types returned by the block clients.
export type {
  GetResponse as KVGetResponse,
  PutResponse as KVPutResponse,
  DeleteResponse as KVDeleteResponse,
  KVItem,
} from "./gen/mindd/kv/v1/kv_pb.js";
export type { Event } from "./gen/mindd/episodic/v1/episodic_pb.js";
export type {
  Record as SemanticRecord,
  Hit,
  FieldPredicate,
  UpsertResponse,
} from "./gen/mindd/semantic/v1/semantic_pb.js";
export type {
  ArtifactRef,
  ArtifactMeta,
  StatResponse as ArtifactStatResponse,
} from "./gen/mindd/artifact/v1/artifact_pb.js";
export type {
  LeaseHandle,
  InspectResponse as LeaseInspectResponse,
} from "./gen/mindd/lease/v1/lease_pb.js";
export type {
  Node as GraphNode,
  Edge as GraphEdge,
  Subgraph,
  NeighborsResponse,
} from "./gen/mindd/graph/v1/graph_pb.js";
export type {
  ListNamespacesResponse,
  NamespaceInfo,
  ServerInfo,
  EmbedderInfo,
} from "./gen/mindd/admin/v1/admin_pb.js";
