---
title: MCP server
sidebar_position: 4
---

# MCP server

`mindctl mcp` runs a [Model Context Protocol](https://modelcontextprotocol.io)
server over stdio, so an **LLM host** (Claude Desktop, Cursor, …) can call
mindD's memory as tools — the "let the assistant manage its own long-term
memory" pattern.

It's pure protocol translation: every tool proxies to the same gRPC services
under a single capability token, so the **capability check and policy engine run
server-side unchanged**. The LLM's judgement stays client-side; the sidecar
still just does CRUD. This is a **secondary** surface — code that runs *alongside*
an agent should use the [SDKs](./python.md) or gRPC directly; MCP is for
LLM-driven, no-code hosts.

## Run it

The MCP server connects to a **running** mindD. It reads the target from
`--addr` (or `$MINDD_ADDR`, default `127.0.0.1:7777`) and the capability
token from `--token` (or `$MINDD_TOKEN`):

```bash
export MINDD_TOKEN=$(mindctl token issue --tenant demo \
  --ns 'kv/*,semantic/*,episodic/*,graph/*' --ops '*' --ttl 24h)

mindctl mcp            # serves MCP on stdio; the host launches this for you
```

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

The token's scope **is** the tool's authority: it can only touch the namespaces
and ops the token allows, and every call still passes through the policy engine.
Issue a narrowly-scoped token per host.

## Tools

A curated set mapped from the core blocks:

| Tool | Proxies to |
|---|---|
| `kv_get` / `kv_put` | `KV/Get`, `KV/Put` |
| `semantic_search` / `semantic_upsert` | `Semantic/Search`, `Semantic/Upsert` |
| `episodic_append` / `episodic_range` | `Episodic/Append`, `Episodic/Range` |
| `graph_neighbors` / `graph_traverse` | `Graph/Neighbors`, `Graph/Traverse` |

Binary artifacts are intentionally **not** exposed over MCP — blobs don't fit the
LLM-tool model; use the SDK/gRPC for the [artifact block](../blocks/artifact.md).
Streaming reads are surfaced as bounded `episodic_range` rather than a live tail.
