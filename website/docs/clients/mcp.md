---
title: MCP server
sidebar_position: 5
---

# MCP server

`mindctl mcp` runs a [Model Context Protocol](https://modelcontextprotocol.io)
server over stdio, so an **LLM host** (Claude Desktop, Cursor, and others) can
call mindD's memory as tools. This is the "let the assistant manage its own
long-term memory" pattern.

It is pure protocol translation: every tool proxies to the same gRPC services
under a single capability token, so the **capability check and policy engine
run server-side unchanged**. The LLM's judgement stays client-side; the sidecar
still just does CRUD.

This is a **secondary** surface. Code that runs alongside an agent should use
the [Python](./python.md) or [TypeScript](./typescript.md) SDK, or gRPC
directly. MCP is for LLM-driven, no-code hosts.

## Run it

The MCP server connects to a **running** mindD. It is a stdio server: stdout is
the JSON-RPC channel and every diagnostic goes to stderr, so the host launches
it rather than you.

| Flag | Env fallback | Default | Meaning |
|---|---|---|---|
| `--addr` | `MINDD_ADDR` | `127.0.0.1:7777` | target gRPC address |
| `--token` | `MINDD_TOKEN` | none, **required** | the capability token |
| `--tls` | none | `false` | dial with TLS (minimum TLS 1.2) |

```bash
export MINDD_TOKEN=$(mindctl token issue --tenant demo \
  --ns 'kv/*,semantic/*,episodic/*,graph/*' \
  --ops 'kv.get,kv.put,semantic.search,semantic.upsert,episodic.append,episodic.range,graph.query' \
  --ttl 24h)

mindctl mcp            # serves MCP on stdio
```

Without a token it exits 2 with `mcp: no token: pass --token or set
$MINDD_TOKEN`. Each proxied call gets a 20 second deadline. `SIGINT` and
`SIGTERM` shut it down cleanly.

### Claude Desktop / Cursor

Add mindD to the host's MCP config (Claude Desktop:
`claude_desktop_config.json`):

```json
{
  "mcpServers": {
    "mindd": {
      "command": "mindctl",
      "args": ["mcp"],
      "env": {
        "MINDD_ADDR": "127.0.0.1:7777",
        "MINDD_TOKEN": "<a capability token>"
      }
    }
  }
}
```

:::warning The token is the tool's entire authority
The MCP server holds one long-lived token and hands its full authority to the
LLM host. Scope it to exactly the namespaces and ops the host should have, use
fully-qualified ops (never bare verbs, which match across every block), and
give each host its own token so you can revoke one without touching the others.
See [Security](../security.md).
:::

## Tools

Eight tools, mapped from four of the six blocks. Required parameters are
marked; everything else is optional.

| Tool | Proxies to | Parameters |
|---|---|---|
| `kv_get` | `KV/Get` | `namespace`\*, `key`\* |
| `kv_put` | `KV/Put` | `namespace`\*, `key`\*, `value`\*, `ttl_seconds` |
| `semantic_search` | `Semantic/Search` | `namespace`\*, `query`\*, `top_k` |
| `semantic_upsert` | `Semantic/Upsert` | `namespace`\*, `content`\*, `id` |
| `episodic_append` | `Episodic/Append` | `namespace`\*, `type`\*, `payload`, `role`, `session_id` |
| `episodic_range` | `Episodic/Range` | `namespace`\*, `after_cursor`, `limit`, `session_id`, `role`, `type` |
| `graph_neighbors` | `Graph/Neighbors` | `namespace`\*, `node_id`\*, `edge_types`, `limit` |
| `graph_traverse` | `Graph/Traverse` | `namespace`\*, `start_id`\*, `depth`, `max_nodes` |

Notes on behaviour:

- `kv_get` returns the raw value as text, or the literal `(not found)`.
- `kv_put` sends a TTL only when `ttl_seconds > 0`; `0` means no expiry.
- `semantic_search` returns a JSON array of `{id, score, content}`. There is no
  metadata `filter` parameter on this surface.
- `episodic_range` drains the server stream into one JSON array of
  `{cursor, type, role, session_id, payload}`.
- `graph_neighbors` and `graph_traverse` return
  `{nodes: [{id, labels}], edges: [{id, type, from, to}]}`. Depth and fan-out
  are hard-capped server-side regardless of what the model asks for.

The server exposes **no MCP resources and no prompts**, only these tools.

## What is deliberately not exposed

- **Artifact**, the whole block. Blobs do not fit the LLM-tool model; use an
  SDK or gRPC for the [artifact block](../blocks/artifact.md).
- **Lease.** Locks held by a model that may stop mid-turn are a bad idea.
- **Admin.** `admin.inspect` is cross-namespace; keep it off this surface.
- **`kv` delete and scan**, and the live `Episodic/Tail`. Streaming reads are
  surfaced as bounded `episodic_range` instead.
