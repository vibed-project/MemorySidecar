// High-level TypeScript client for memsidecar.
//
// Usage:
//
//   import { MemSidecar } from "@memsidecar/client";
//
//   const m = new MemSidecar("127.0.0.1:7777", { token: process.env.MEMSIDECAR_TOKEN! });
//   await m.kv.put("scratchpad", "hello", new TextEncoder().encode("world"));
//   const rec = await m.kv.get("scratchpad", "hello");
//   console.log(new TextDecoder().decode(rec.value));
//
// The block clients (`m.kv`, `m.episodic`, `m.semantic`, `m.artifact`,
// `m.lease`, `m.graph`) wrap the generated stubs with idiomatic types and
// inject the capability token on every call.
import { create } from "@bufbuild/protobuf";
import type { MessageInitShape } from "@bufbuild/protobuf";
import type { Client, Interceptor, Transport } from "@connectrpc/connect";
import { createClient } from "@connectrpc/connect";
import { createGrpcTransport } from "@connectrpc/connect-node";

import { capabilityInterceptor } from "./auth.js";
import { toDuration, toTimestamp, toU64 } from "./convert.js";

import { KV } from "./gen/memsidecar/kv/v1/kv_pb.js";
import type {
  DeleteResponse as KVDeleteResponse,
  GetResponse as KVGetResponse,
  KVItem,
  PutResponse as KVPutResponse,
} from "./gen/memsidecar/kv/v1/kv_pb.js";
import { Episodic } from "./gen/memsidecar/episodic/v1/episodic_pb.js";
import type { Event } from "./gen/memsidecar/episodic/v1/episodic_pb.js";
import { Semantic } from "./gen/memsidecar/semantic/v1/semantic_pb.js";
import type {
  FieldPredicateSchema,
  Hit,
  RecordSchema,
  SearchMode,
  ExpireAction,
  UpsertResponse,
} from "./gen/memsidecar/semantic/v1/semantic_pb.js";
import { Artifact } from "./gen/memsidecar/artifact/v1/artifact_pb.js";
import type {
  ArtifactRef,
  PutRequest as ArtifactPutRequest,
  StatResponse as ArtifactStatResponse,
} from "./gen/memsidecar/artifact/v1/artifact_pb.js";
import { PutRequestSchema as ArtifactPutRequestSchema } from "./gen/memsidecar/artifact/v1/artifact_pb.js";
import { Lease } from "./gen/memsidecar/lease/v1/lease_pb.js";
import type {
  InspectResponse as LeaseInspectResponse,
  LeaseHandle,
} from "./gen/memsidecar/lease/v1/lease_pb.js";
import { Graph } from "./gen/memsidecar/graph/v1/graph_pb.js";
import type {
  Direction,
  EdgeSchema,
  NeighborsResponse,
  Node as GraphNode,
  NodeSchema,
  Subgraph,
} from "./gen/memsidecar/graph/v1/graph_pb.js";

/** A caller-supplied record for `semantic.upsert`. */
export type SemanticRecordInput = MessageInitShape<typeof RecordSchema>;
/** A caller-supplied structured predicate for `semantic.search`. */
export type FieldPredicateInput = MessageInitShape<typeof FieldPredicateSchema>;
/** A caller-supplied node for `graph.upsertNodes`. */
export type NodeInput = MessageInitShape<typeof NodeSchema>;
/** A caller-supplied edge for `graph.upsertEdges`. */
export type EdgeInput = MessageInitShape<typeof EdgeSchema>;

const CHUNK_SIZE = 64 * 1024;

// ---------------------------------------------------------------------------
// KV

export interface KVPutOptions {
  /** Time-to-live in seconds; omit for no expiry. */
  ttlSeconds?: number;
  contentType?: string;
  metadata?: Record<string, string>;
  /** Optimistic concurrency: write only if the stored version equals this. */
  ifVersion?: number | bigint;
}

export interface KVScanOptions {
  keyPrefix?: string;
  limit?: number;
  includeValues?: boolean;
}

class KVClient {
  constructor(private readonly stub: Client<typeof KV>) {}

  put(
    namespace: string,
    key: string,
    value: Uint8Array,
    opts: KVPutOptions = {},
  ): Promise<KVPutResponse> {
    return this.stub.put({
      namespace,
      key,
      value,
      contentType: opts.contentType ?? "",
      metadata: opts.metadata,
      ttl: toDuration(opts.ttlSeconds),
      ifVersion: toU64(opts.ifVersion),
    });
  }

  get(namespace: string, key: string): Promise<KVGetResponse> {
    return this.stub.get({ namespace, key });
  }

  delete(
    namespace: string,
    key: string,
    opts: { ifVersion?: number | bigint } = {},
  ): Promise<KVDeleteResponse> {
    return this.stub.delete({ namespace, key, ifVersion: toU64(opts.ifVersion) });
  }

  /** Stream every item in a namespace (optionally under a key prefix). */
  scan(namespace: string, opts: KVScanOptions = {}): AsyncIterable<KVItem> {
    return this.stub.scan({
      namespace,
      keyPrefix: opts.keyPrefix ?? "",
      limit: opts.limit ?? 0,
      includeValues: opts.includeValues ?? false,
    });
  }
}

// ---------------------------------------------------------------------------
// Episodic

export interface EpisodicAppendOptions {
  payload?: Uint8Array;
  metadata?: Record<string, string>;
  role?: string;
  sessionId?: string;
}

export interface EpisodicRangeOptions {
  afterCursor?: number | bigint;
  beforeCursor?: number | bigint;
  limit?: number;
  reverse?: boolean;
  /** Exclusive lower bound on event timestamp (ANDed with the cursor bounds). */
  afterTime?: Date;
  /** Exclusive upper bound on event timestamp. */
  beforeTime?: Date;
}

export interface EpisodicTailOptions {
  afterCursor?: number | bigint;
  includeHistorical?: boolean;
}

class EpisodicClient {
  constructor(private readonly stub: Client<typeof Episodic>) {}

  async append(
    namespace: string,
    type: string,
    opts: EpisodicAppendOptions = {},
  ): Promise<Event> {
    const resp = await this.stub.append({
      namespace,
      type,
      payload: opts.payload,
      metadata: opts.metadata,
      role: opts.role ?? "",
      sessionId: opts.sessionId ?? "",
    });
    return resp.event!;
  }

  /** Replay historical events by cursor and/or timestamp window. */
  range(namespace: string, opts: EpisodicRangeOptions = {}): AsyncIterable<Event> {
    return this.stub.range({
      namespace,
      afterCursor: toU64(opts.afterCursor) ?? 0n,
      beforeCursor: toU64(opts.beforeCursor) ?? 0n,
      limit: opts.limit ?? 0,
      reverse: opts.reverse ?? false,
      afterTime: toTimestamp(opts.afterTime),
      beforeTime: toTimestamp(opts.beforeTime),
    });
  }

  /** Follow the log live from a cursor (optionally replaying history first). */
  tail(namespace: string, opts: EpisodicTailOptions = {}): AsyncIterable<Event> {
    return this.stub.tail({
      namespace,
      afterCursor: toU64(opts.afterCursor) ?? 0n,
      includeHistorical: opts.includeHistorical ?? false,
    });
  }
}

// ---------------------------------------------------------------------------
// Semantic

export interface SemanticSearchOptions {
  queryText?: string;
  queryVector?: number[];
  topK?: number;
  /** Exact-match metadata filter. */
  filter?: Record<string, string>;
  /** Structured range/set predicates, ANDed with `filter`. */
  predicates?: FieldPredicateInput[];
  createdAfter?: Date;
  createdBefore?: Date;
  mode?: SearchMode;
  rerankCandidateK?: number;
  includePayload?: boolean;
  includeVector?: boolean;
  /** Evaluate validity at this instant instead of now (point-in-time recall). */
  asOf?: Date;
  includeInvalidated?: boolean;
  /** Return only each hit's record id and score. */
  idsOnly?: boolean;
}

export interface SemanticExpireOptions {
  action: ExpireAction;
  /** Required upper bound on affected records. */
  maxRows: number;
  filter?: Record<string, string>;
}

class SemanticClient {
  constructor(private readonly stub: Client<typeof Semantic>) {}

  upsert(namespace: string, records: SemanticRecordInput[]): Promise<UpsertResponse> {
    return this.stub.upsert({ namespace, records });
  }

  async search(namespace: string, opts: SemanticSearchOptions = {}): Promise<Hit[]> {
    const resp = await this.stub.search({
      namespace,
      queryText: opts.queryText ?? "",
      queryVector: opts.queryVector,
      topK: opts.topK ?? 0,
      filter: opts.filter,
      predicates: opts.predicates,
      mode: opts.mode,
      rerankCandidateK: opts.rerankCandidateK ?? 0,
      includePayload: opts.includePayload ?? false,
      includeVector: opts.includeVector ?? false,
      includeInvalidated: opts.includeInvalidated ?? false,
      idsOnly: opts.idsOnly ?? false,
      asOf: toTimestamp(opts.asOf),
      createdAfter: toTimestamp(opts.createdAfter),
      createdBefore: toTimestamp(opts.createdBefore),
    });
    return resp.hits;
  }

  /** Soft-delete (default) or hard-delete a record; returns whether it existed. */
  async delete(namespace: string, id: string, opts: { hard?: boolean } = {}): Promise<boolean> {
    const resp = await this.stub.delete({ namespace, id, hard: opts.hard ?? false });
    return resp.existed;
  }

  /** Apply a bounded lifecycle action to all records matching a filter. */
  async expire(namespace: string, opts: SemanticExpireOptions): Promise<bigint> {
    const resp = await this.stub.expire({
      namespace,
      filter: opts.filter,
      action: opts.action,
      maxRows: opts.maxRows,
    });
    return resp.affected;
  }
}

// ---------------------------------------------------------------------------
// Artifact

export interface ArtifactPutOptions {
  id?: string;
  contentType?: string;
  metadata?: Record<string, string>;
}

export interface ArtifactGetOptions {
  offset?: number | bigint;
  length?: number | bigint;
}

class ArtifactClient {
  constructor(private readonly stub: Client<typeof Artifact>) {}

  /** Store a blob (client-streamed in 64 KiB chunks); returns its ref. */
  async put(
    namespace: string,
    data: Uint8Array,
    opts: ArtifactPutOptions = {},
  ): Promise<ArtifactRef> {
    async function* body(): AsyncGenerator<ArtifactPutRequest> {
      yield create(ArtifactPutRequestSchema, {
        body: {
          case: "init",
          value: {
            namespace,
            id: opts.id ?? "",
            contentType: opts.contentType ?? "",
            metadata: opts.metadata,
          },
        },
      });
      for (let off = 0; off < data.length; off += CHUNK_SIZE) {
        yield create(ArtifactPutRequestSchema, {
          body: { case: "chunk", value: { data: data.subarray(off, off + CHUNK_SIZE) } },
        });
      }
    }
    const resp = await this.stub.put(body());
    return resp.ref!;
  }

  /** Fetch a blob (optionally a byte range), buffered into a single array. */
  async get(namespace: string, id: string, opts: ArtifactGetOptions = {}): Promise<Uint8Array> {
    const chunks: Uint8Array[] = [];
    let total = 0;
    for await (const msg of this.stub.get({
      namespace,
      id,
      offset: toU64(opts.offset) ?? 0n,
      length: toU64(opts.length) ?? 0n,
    })) {
      if (msg.body.case === "chunk") {
        chunks.push(msg.body.value.data);
        total += msg.body.value.data.length;
      }
    }
    const out = new Uint8Array(total);
    let o = 0;
    for (const c of chunks) {
      out.set(c, o);
      o += c.length;
    }
    return out;
  }

  stat(namespace: string, id: string): Promise<ArtifactStatResponse> {
    return this.stub.stat({ namespace, id });
  }

  async delete(namespace: string, id: string): Promise<boolean> {
    return (await this.stub.delete({ namespace, id })).existed;
  }
}

// ---------------------------------------------------------------------------
// Lease

export interface LeaseAcquireOptions {
  /** Time until the lease auto-expires if not renewed. Required. */
  ttlSeconds: number;
  /** If held, wait up to this long for it to free. Omit to fail fast. */
  waitForSeconds?: number;
  metadata?: Record<string, string>;
}

class LeaseClient {
  constructor(private readonly stub: Client<typeof Lease>) {}

  async acquire(namespace: string, key: string, opts: LeaseAcquireOptions): Promise<LeaseHandle> {
    const ttl = toDuration(opts.ttlSeconds);
    if (ttl === undefined) {
      throw new Error("ttlSeconds is required");
    }
    const resp = await this.stub.acquire({
      namespace,
      key,
      ttl,
      waitFor: toDuration(opts.waitForSeconds),
      metadata: opts.metadata,
    });
    return resp.handle!;
  }

  async renew(
    holderId: string,
    namespace: string,
    key: string,
    opts: { ttlSeconds: number },
  ): Promise<LeaseHandle> {
    const ttl = toDuration(opts.ttlSeconds);
    if (ttl === undefined) {
      throw new Error("ttlSeconds is required");
    }
    return (await this.stub.renew({ holderId, namespace, key, ttl })).handle!;
  }

  async release(holderId: string, namespace: string, key: string): Promise<boolean> {
    return (await this.stub.release({ holderId, namespace, key })).existed;
  }

  inspect(namespace: string, key: string): Promise<LeaseInspectResponse> {
    return this.stub.inspect({ namespace, key });
  }
}

// ---------------------------------------------------------------------------
// Graph

export interface GraphNeighborsOptions {
  edgeTypes?: string[];
  direction?: Direction;
  nodeLabels?: string[];
  limit?: number;
  asOf?: Date;
}

export interface GraphTraverseOptions {
  edgeTypes?: string[];
  direction?: Direction;
  depth?: number;
  maxNodes?: number;
  asOf?: Date;
}

class GraphClient {
  constructor(private readonly stub: Client<typeof Graph>) {}

  async upsertNodes(namespace: string, nodes: NodeInput[]): Promise<string[]> {
    return (await this.stub.upsertNodes({ namespace, nodes })).ids;
  }

  async upsertEdges(namespace: string, edges: EdgeInput[]): Promise<string[]> {
    return (await this.stub.upsertEdges({ namespace, edges })).ids;
  }

  getNode(namespace: string, id: string): Promise<GraphNode> {
    return this.stub.getNode({ namespace, id });
  }

  neighbors(
    namespace: string,
    nodeId: string,
    opts: GraphNeighborsOptions = {},
  ): Promise<NeighborsResponse> {
    return this.stub.neighbors({
      namespace,
      nodeId,
      edgeTypes: opts.edgeTypes,
      direction: opts.direction,
      nodeLabels: opts.nodeLabels,
      limit: opts.limit ?? 0,
      asOf: toTimestamp(opts.asOf),
    });
  }

  traverse(namespace: string, startId: string, opts: GraphTraverseOptions = {}): Promise<Subgraph> {
    return this.stub.traverse({
      namespace,
      startId,
      edgeTypes: opts.edgeTypes,
      direction: opts.direction,
      depth: opts.depth ?? 0,
      maxNodes: opts.maxNodes ?? 0,
      asOf: toTimestamp(opts.asOf),
    });
  }

  async deleteNode(
    namespace: string,
    id: string,
    opts: { cascade?: boolean } = {},
  ): Promise<boolean> {
    return (await this.stub.deleteNode({ namespace, id, cascade: opts.cascade ?? false })).existed;
  }

  async deleteEdge(namespace: string, id: string): Promise<boolean> {
    return (await this.stub.deleteEdge({ namespace, id })).existed;
  }
}

// ---------------------------------------------------------------------------
// Top-level client

export interface MemSidecarOptions {
  /** Capability token sent on every call. */
  token: string;
  /** Use TLS (https/h2). Default false (plaintext h2c), matching a local sidecar. */
  tls?: boolean;
  /** Extra interceptors, applied after the capability interceptor. */
  interceptors?: Interceptor[];
}

function baseUrlFor(address: string, tls: boolean): string {
  if (/^https?:\/\//i.test(address)) {
    return address;
  }
  return `${tls ? "https" : "http"}://${address}`;
}

/**
 * Connection plus per-block clients sharing a single capability token.
 *
 * `address` is a `host:port` (or a full `http(s)://` URL). The transport speaks
 * standard gRPC over HTTP/2, so it interoperates with the same server the Go,
 * `memctl`, and Python clients use.
 */
export class MemSidecar {
  readonly transport: Transport;
  readonly kv: KVClient;
  readonly episodic: EpisodicClient;
  readonly semantic: SemanticClient;
  readonly artifact: ArtifactClient;
  readonly lease: LeaseClient;
  readonly graph: GraphClient;

  constructor(address: string, options: MemSidecarOptions) {
    this.transport = createGrpcTransport({
      baseUrl: baseUrlFor(address, options.tls ?? false),
      interceptors: [capabilityInterceptor(options.token), ...(options.interceptors ?? [])],
    });
    this.kv = new KVClient(createClient(KV, this.transport));
    this.episodic = new EpisodicClient(createClient(Episodic, this.transport));
    this.semantic = new SemanticClient(createClient(Semantic, this.transport));
    this.artifact = new ArtifactClient(createClient(Artifact, this.transport));
    this.lease = new LeaseClient(createClient(Lease, this.transport));
    this.graph = new GraphClient(createClient(Graph, this.transport));
  }
}
