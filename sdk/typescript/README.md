# @mindd/client — TypeScript client

Thin TypeScript/JavaScript client for the [mindD](../../README.md) agent
memory sidecar. Wraps the generated [protobuf-es](https://github.com/bufbuild/protobuf-es)
/ [Connect](https://connectrpc.com/) stubs with idiomatic types and injects the
capability token on every call. It speaks standard gRPC over HTTP/2, so it talks
to the same server as the Go, `mindctl`, and Python clients.

## Install

```bash
cd sdk/typescript
npm install
npm run build
```

The package is ESM and ships type declarations. It targets Node.js ≥ 20
(the gRPC transport uses `node:http2`).

## Quickstart

Assumes the sidecar is running with `configs/example.yaml` and you've minted a
token:

```bash
export MINDD_PASETO_SECRET_HEX=...   # from configs/example.yaml comment
export MINDD_TOKEN=$(./bin/mindctl token issue \
    --tenant acme --agent agent-1 \
    --ns 'kv/*,episodic/events,semantic/notes,artifact/blobs,lease/locks,graph/knowledge' \
    --ops '*' --ttl 1h)
```

Then:

```ts
import { MindD, SearchMode } from "@mindd/client";

const m = new MindD("127.0.0.1:7777", { token: process.env.MINDD_TOKEN! });
const enc = new TextEncoder();
const dec = new TextDecoder();

// KV
await m.kv.put("scratchpad", "hello", enc.encode("world"), { ttlSeconds: 300 });
const rec = await m.kv.get("scratchpad", "hello");
console.log(dec.decode(rec.value)); // "world"

// Episodic
await m.episodic.append("events", "tool_call", { payload: enc.encode("...") });
for await (const ev of m.episodic.range("events", { limit: 10 })) {
  console.log(ev.cursor, ev.type);
}

// Semantic — bitemporal & revisable. Default search returns only records live
// and valid *now*; pass `asOf` or `includeInvalidated` for point-in-time /
// audit reads. `supersedes` retires the ids it revises.
await m.semantic.upsert("notes", [{ id: "a", content: "apple" }]);
const hits = await m.semantic.search("notes", { queryText: "apple", topK: 3 });
for (const hit of hits) {
  console.log(hit.record?.id, hit.score);
}

// Artifact
const ref = await m.artifact.put("blobs", enc.encode("binary bytes"), {
  contentType: "application/octet-stream",
});
const got = await m.artifact.get("blobs", ref.id);

// Lease
const handle = await m.lease.acquire("locks", "job-42", { ttlSeconds: 30 });
await m.lease.release(handle.holderId, "locks", "job-42");

// Graph
await m.graph.upsertNodes("knowledge", [{ id: "a", labels: ["Person"] }]);
const neighbors = await m.graph.neighbors("knowledge", "a");
```

### TLS

Pass `tls: true` to use `https`/h2 (or give a full `https://host:port` address):

```ts
const m = new MindD("mem.internal:443", { token, tls: true });
```

## Vercel AI SDK — chat persistence

`@mindd/client/ai` adapts mindD to the [Vercel AI SDK](https://ai-sdk.dev)'s
chat-persistence convention: a chat is its full `UIMessage[]`, loaded by id and
re-saved after each turn. The store keeps one kv key per chat. `ai` is an
optional peer dependency — importing the base client never pulls it in.

```ts
import { MindD } from "@mindd/client";
import { createChatStore } from "@mindd/client/ai";

const m = new MindD("127.0.0.1:7777", { token });
const store = createChatStore(m, { namespace: "chats" });

// New conversation, or resume an existing one.
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

The store also exposes `deleteChat(id)` and `listChats()`. It needs a token with
`kv` ops on the chat namespace (`get,put,delete,scan`).

## Codegen

The stubs under `src/gen` are generated from the protos in `../../proto` with
the buf-native protobuf-es plugin. Regenerate from the repo root:

```bash
make proto-ts   # buf generate --template buf.gen.ts.yaml
```

## Test

```bash
npm test        # builds, then runs the offline smoke test
```
