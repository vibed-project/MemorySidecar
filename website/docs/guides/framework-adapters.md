---
title: Framework adapters
sidebar_position: 4
---

# Framework adapters

mindD is framework-agnostic on purpose: the server knows nothing about
LangGraph, CrewAI or the Vercel AI SDK. The adapters below live in the SDKs and
translate each framework's persistence interface onto blocks that already
exist, so you get durable, multi-tenant, policy-governed memory without
bespoke glue.

Each adapter is optional and pulls in its framework only when you install it.

| Adapter | Framework | SDK | Blocks used |
|---|---|---|---|
| `MindDSaver` | LangGraph checkpointer | Python | `kv` |
| `MindDStorage` | CrewAI memory storage | Python | `semantic` + `kv` |
| `createChatStore` | Vercel AI SDK chat persistence | TypeScript | `kv` |

## LangGraph checkpointer

`mindd.ext.langgraph.MindDSaver` implements LangGraph's
`BaseCheckpointSaver` on top of the `kv` block, so a graph gets durable thread
state by pointing at a running sidecar.

```bash
pip install 'mindd[langgraph]'
```

```python
from mindd import MindD
from mindd.ext.langgraph import MindDSaver

client = MindD("127.0.0.1:7777", token=TOKEN)
saver = MindDSaver(client, namespace="checkpoints")

graph = builder.compile(checkpointer=saver)
graph.invoke({"messages": [...]}, {"configurable": {"thread_id": "t1"}})
```

| Argument | Default | Meaning |
|---|---|---|
| `client` | required | a `MindD` instance |
| `namespace` | `"checkpoints"` | the `kv` namespace holding all state |
| `serde` | LangGraph's default | serializer protocol |

The storage model mirrors LangGraph's reference `InMemorySaver`, so runtime
behaviour is identical. Everything lives under one `kv` namespace, keyed by
role:

- `cp/<thread>/<ns>/<checkpoint_id>`: the checkpoint (channel values popped
  out) plus its metadata and parent id, as one JSON envelope.
- `bl/<thread>/<ns>/<channel>/<version>`: one channel value per version.
  Checkpoints reference these by `channel_versions`, so unchanged channels are
  shared across checkpoints.
- `wr/<thread>/<ns>/<checkpoint_id>/<task_id>/<idx>`: a pending write.

Per-thread queries (get-latest, list) are served by a `kv` prefix scan. The
`episodic` block is deliberately not used: its cursor stream is per namespace
with no server-side per-thread filter, which is the wrong access pattern for a
checkpointer.

**Config.** Declare the namespace and mint a token that covers it:

```yaml
namespaces:
  - { block: kv, name: checkpoints, backend: pg-main }
```

```bash
mindctl token issue --tenant acme --agent graph-1 \
  --ns 'kv/checkpoints' --ops 'kv.get,kv.put,kv.delete,kv.scan' --ttl 1h
```

Turn on [tenant isolation](../concepts/tenant-isolation.md) if different
tenants share the deployment, and consider `encrypt: true` on the namespace
since checkpoints carry conversation state. See
[Encryption at rest](../concepts/encryption-at-rest.md).

## CrewAI memory storage

`mindd.ext.crewai.MindDStorage` implements CrewAI 1.x's `StorageBackend`
protocol, so a crew's memories live on a running mindD instead of a local
vector file.

```bash
pip install 'mindd[crewai]'
```

```python
from crewai import Crew
from crewai.memory.unified_memory import Memory
from mindd import MindD
from mindd.ext.crewai import MindDStorage

client = MindD("127.0.0.1:7777", token=TOKEN)
memory = Memory(storage=MindDStorage(client, namespace="memories"))
crew = Crew(agents=[...], tasks=[...], memory=memory)
```

Or register it process-wide:

```python
from crewai.memory.storage.factory import set_memory_storage_factory
set_memory_storage_factory(lambda spec: MindDStorage(client))
```

| Argument | Default | Meaning |
|---|---|---|
| `client` | required | a `MindD` instance |
| `namespace` | `"memories"` | name used for **both** the semantic and the kv namespace |
| `semantic_namespace` | falls back to `namespace` | override the vector namespace |
| `kv_namespace` | falls back to `namespace` | override the record namespace |
| `over_fetch` | `3` | candidate multiplier before scope filtering |

Two blocks are used under one shared namespace name:

- **semantic**, the vector index. `save` upserts `(id, content, vector)`;
  `search` runs an ids-only ANN query, then records are rehydrated from `kv`.
- **kv**, the record store and scope index. Each record is written under
  `id/<id>` (direct lookup) and `rec<scope>/<id>` (prefix scan), which backs
  `get_record`, list/count, and the scope and category introspection the memory
  runtime calls during encode and recall.

:::caution CrewAI owns the embedding
CrewAI populates `MemoryRecord.embedding` before `save` and hands `search` a
`query_embedding`. The adapter stores those vectors as-is and searches by
vector, so the mindD semantic namespace's `dimensions` **must equal** the
CrewAI embedder's output dimension. The server-side embedder is never invoked
on the write path, but it still has to be declared, because every semantic
namespace requires an `embedder` block.
:::

**Config.**

```yaml
namespaces:
  - { block: kv, name: memories, backend: pg-main }
  - block: semantic
    name: memories
    backend: pg-main
    embedder:
      provider: fake          # never called; dimensions must match CrewAI's
      model: unused
      dimensions: 1536
```

```bash
mindctl token issue --tenant acme --agent crew-1 \
  --ns 'kv/memories,semantic/memories' \
  --ops 'kv.get,kv.put,kv.delete,kv.scan,semantic.upsert,semantic.search,semantic.delete' \
  --ttl 1h
```

`kv/memories` and `semantic/memories` are distinct namespaces: the capability
match runs against the qualified `<block>/<name>` form.

## Vercel AI SDK chat persistence

`@mindd/client/ai` implements the AI SDK's persistence convention (a chat is
its full `UIMessage[]`, loaded by id and re-saved after each turn) on the `kv`
block, one key per chat.

```bash
npm install @mindd/client ai
```

```ts
import { MindD } from "@mindd/client";
import { createChatStore } from "@mindd/client/ai";

const m = new MindD("127.0.0.1:7777", { token: process.env.MINDD_TOKEN! });
const store = createChatStore(m, { namespace: "chats" });

const chatId = await store.createChat();
const messages = await store.loadChat(chatId);
```

```ts
import { streamText, convertToModelMessages } from "ai";

const result = streamText({ model, messages: convertToModelMessages(messages) });
return result.toUIMessageStreamResponse({
  originalMessages: messages,
  onFinish: ({ messages }) => store.saveChat({ chatId, messages }),
});
```

| Option | Default | Meaning |
|---|---|---|
| `namespace` | `"chats"` | the `kv` namespace holding the chats |
| `generateId` | `crypto.randomUUID()` | new-chat id generator |

The store also exposes `deleteChat(id)` and `listChats()`. `ai` is an optional
peer dependency, so importing the base client never pulls it in.

**Config.**

```yaml
namespaces:
  - { block: kv, name: chats, backend: pg-main }
```

```bash
mindctl token issue --tenant acme --agent web \
  --ns 'kv/chats' --ops 'kv.get,kv.put,kv.delete,kv.scan' --ttl 1h
```

## Why the adapters are thin

Everything an adapter needs already exists at the block level: TTLs, keyset
pagination, optimistic concurrency, bitemporal semantic records. What the
adapters add is a mapping from one framework's interface to those primitives.
That means the [policy engine](../concepts/policy.md),
[capability scoping](../concepts/capabilities.md),
[tenant isolation](../concepts/tenant-isolation.md),
[encryption at rest](../concepts/encryption-at-rest.md) and
[observability](../ops/observability.md) all apply unchanged, which is the
point of putting memory behind a sidecar in the first place.

Adding an adapter for another framework is a single file against a public SDK.
