---
title: TypeScript SDK
sidebar_position: 2
---

# TypeScript SDK

`@mindd/client` is a thin TypeScript/JavaScript client. It wraps the generated
[protobuf-es](https://github.com/bufbuild/protobuf-es) /
[Connect](https://connectrpc.com/) stubs with idiomatic types and injects the
capability token on every call. It speaks standard gRPC over HTTP/2, so it
talks to the same server as `mindctl`, the Go stubs, and the
[Python SDK](./python.md).

Source lives in
[`sdk/typescript/`](https://github.com/vibed-project/mindD/tree/main/sdk/typescript).

## Install

The package is ESM only and targets Node.js 20 or newer (the gRPC transport
uses `node:http2`). It ships type declarations.

```bash
npm install @mindd/client
```

Or from the repository:

```bash
cd sdk/typescript
npm install
npm run build
```

Runtime dependencies are `@bufbuild/protobuf`, `@connectrpc/connect` and
`@connectrpc/connect-node`. `ai` is an **optional** peer dependency, needed
only for the Vercel AI SDK adapter under the `/ai` subpath.

The SDK versions and releases independently of the server, from the
`typescript-sdk-v*` tag prefix.

## Hello world

```ts
import { MindD } from "@mindd/client";

const m = new MindD("127.0.0.1:7777", { token: process.env.MINDD_TOKEN! });
const enc = new TextEncoder();
const dec = new TextDecoder();

await m.kv.put("scratchpad", "hello", enc.encode("world"), { ttlSeconds: 300 });
const rec = await m.kv.get("scratchpad", "hello");
console.log(dec.decode(rec.value)); // "world"
```

`address` is a `host:port` or a full `http(s)://` URL.

| Option | Default | Meaning |
|---|---|---|
| `token` | none, **required** | capability token sent on every call |
| `tls` | `false` | use `https`/h2 instead of plaintext h2c |
| `interceptors` | `[]` | extra Connect interceptors, applied after the capability one |

```ts
const m = new MindD("mem.internal:443", { token, tls: true });
```

The token is shared across the seven sub-clients: `m.kv`, `m.episodic`,
`m.semantic`, `m.artifact`, `m.lease`, `m.graph` and `m.admin`. The transport
is on `m.transport` if you need to build a raw Connect client.

## Per-block surface

Values are `Uint8Array`. Durations are plain numbers of seconds
(`ttlSeconds`, `waitForSeconds`), not `Duration` messages. Streaming reads are
`AsyncIterable`, so `for await` works directly.

### KV

```ts
await m.kv.put(ns, key, value, { ttlSeconds, contentType, metadata, ifVersion });
const rec = await m.kv.get(ns, key);           // { found, value, version, ... }
const items = await m.kv.multiGet(ns, ["k1", "k2"]);
await m.kv.delete(ns, key, { ifVersion });

for await (const item of m.kv.scan(ns, { keyPrefix: "", limit: 100, startAfter: "" })) {
  // startAfter is the keyset resume cursor
}
```

### Episodic

```ts
const ev = await m.episodic.append("events", "tool_call", {
  payload: enc.encode("..."),
  role: "assistant",
  sessionId: "sess-1",
  dedupKey: "msg-42",     // idempotent under retry
  supersedes: [],
  source: "",
});

for await (const e of m.episodic.range("events", { limit: 10, sessionId: "sess-1" })) { }
for await (const e of m.episodic.tail("events", { includeHistorical: true, afterCursor: 0n })) { }

const affected = await m.episodic.expire("events", {
  beforeCursor: 1000n,
  action: EpisodicExpireAction.SOFT_DELETE,
  maxRows: 500,
});
```

### Semantic

```ts
import { SearchMode, PredicateOp } from "@mindd/client";

await m.semantic.upsert("notes", [{ id: "a", content: "apple" }]);

const hits = await m.semantic.search("notes", {
  queryText: "apple",
  topK: 3,
  mode: SearchMode.HYBRID,
  rerankCandidateK: 50,
});

await m.semantic.delete("notes", "a");            // soft delete by default
await m.semantic.delete("notes", "a", { hard: true });
const n = await m.semantic.expire("notes", { filter: { topic: "food" }, action, maxRows: 100 });
```

The lifecycle fields (`validFrom`, `validTo`, `supersedes`, `source`,
`ifVersion`) go on the record objects passed to `upsert`; `asOf` and
`includeInvalidated` go on `search`. See [Semantic](../blocks/semantic.md).

### Artifact

```ts
const ref = await m.artifact.put("blobs", bytes, { contentType: "image/png", id: "render-1" });
const got = await m.artifact.get("blobs", ref.id, { offset: 0, length: 0 });
const meta = await m.artifact.stat("blobs", ref.id);
await m.artifact.delete("blobs", ref.id);

for await (const meta of m.artifact.list("blobs", { filter: { kind: "render" }, limit: 100 })) { }
```

`put` handles the client-streaming chunking; `get` reassembles the server
stream into one `Uint8Array`.

### Lease

```ts
const handle = await m.lease.acquire("locks", "job-42", { ttlSeconds: 30 });
await m.lease.renew(handle.holderId, "locks", "job-42", { ttlSeconds: 300 });
await m.lease.release(handle.holderId, "locks", "job-42");

await m.lease.inspect("locks", "job-42");
await m.lease.list("locks");
```

### Graph

```ts
import { Direction } from "@mindd/client";

await m.graph.upsertNodes("knowledge", [{ id: "alice", labels: ["Person"] }]);
await m.graph.upsertEdges("knowledge", [
  { id: "e1", type: "AUTHORED", from: "alice", to: "doc-1" },
]);

const node = await m.graph.getNode("knowledge", "alice");
const nb = await m.graph.neighbors("knowledge", "alice", {
  edgeTypes: ["AUTHORED"],
  direction: Direction.OUT,
  limit: 50,
});
const sub = await m.graph.traverse("knowledge", "alice", { depth: 2, maxNodes: 100 });

await m.graph.deleteNode("knowledge", "doc-1", { cascade: true });
await m.graph.deleteEdge("knowledge", "e1");
```

Unlike Python, `from` is not a reserved word here, so `EdgeInput` uses a plain
`from` field.

### Admin

```ts
const resp = await m.admin.listNamespaces();   // requires the admin.inspect op
console.log(resp.server?.version);
```

## Vercel AI SDK adapter

`@mindd/client/ai` implements the AI SDK's chat-persistence convention on the
`kv` block: one key per chat holding the full `UIMessage[]`. Importing the base
client never pulls in `ai`.

```ts
import { MindD } from "@mindd/client";
import { createChatStore } from "@mindd/client/ai";

const store = createChatStore(new MindD("127.0.0.1:7777", { token }), {
  namespace: "chats",
});

const chatId = await store.createChat();
const messages = await store.loadChat(chatId);
```

Wire `saveChat` into a streamed response so every turn is persisted:

```ts
import { streamText, convertToModelMessages } from "ai";

const result = streamText({ model, messages: convertToModelMessages(messages) });
return result.toUIMessageStreamResponse({
  originalMessages: messages,
  onFinish: ({ messages }) => store.saveChat({ chatId, messages }),
});
```

The store also exposes `deleteChat(id)` and `listChats()`. It needs a token
with `kv.get`, `kv.put`, `kv.delete` and `kv.scan` on the chat namespace. See
[Framework adapters](../guides/framework-adapters.md).

## Exports

Beyond `MindD`, the package exports `capabilityInterceptor` and
`CAPABILITY_HEADER` for building your own Connect clients, the runtime enums
`SearchMode`, `PredicateOp`, `ExpireAction`, `EpisodicExpireAction` and
`Direction`, and the generated message types (`KVItem`, `Event`,
`SemanticRecord`, `Hit`, `ArtifactRef`, `ArtifactMeta`, `LeaseHandle`,
`GraphNode`, `GraphEdge`, `Subgraph`, `NamespaceInfo`, and others).

## Regenerating stubs

The stubs under `src/gen` come from `proto/` via the buf-native protobuf-es
plugin:

```bash
make proto-ts     # buf generate --template buf.gen.ts.yaml
```

## Test

```bash
cd sdk/typescript
npm test          # builds, then runs the offline smoke test
```
