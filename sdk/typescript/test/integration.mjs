// Live integration test against a running mindd (configs/example.yaml,
// all in-memory backends + the fake embedder). Not part of `npm test` — it
// needs a server. Point it at one and mint a token:
//
//   MINDD_ADDR=127.0.0.1:7777 MINDD_TOKEN=$(mindctl token issue ...) \
//     node --test test/integration.mjs
import assert from "node:assert/strict";
import { test } from "node:test";

import { MindD } from "../dist/index.js";

const addr = process.env.MINDD_ADDR ?? "127.0.0.1:7777";
const token = process.env.MINDD_TOKEN;
if (!token) {
  throw new Error("set MINDD_TOKEN");
}

const m = new MindD(addr, { token });
const enc = new TextEncoder();
const dec = new TextDecoder();

test("kv: put / get / scan (unary + server-stream)", async () => {
  await m.kv.put("scratchpad", "greeting", enc.encode("hello world"), {
    contentType: "text/plain",
    metadata: { lang: "en" },
  });
  const got = await m.kv.get("scratchpad", "greeting");
  assert.equal(got.found, true);
  assert.equal(dec.decode(got.value), "hello world");
  assert.equal(got.metadata.lang, "en");

  const keys = [];
  for await (const item of m.kv.scan("scratchpad", { includeValues: true })) {
    keys.push(item.key);
  }
  assert.ok(keys.includes("greeting"), `scan missing key; saw ${keys}`);
});

test("episodic: append / range (server-stream)", async () => {
  const ev = await m.episodic.append("events", "tool_call", {
    payload: enc.encode("{}"),
    role: "assistant",
  });
  assert.ok(ev.cursor >= 1n, `cursor=${ev.cursor}`);

  const seen = [];
  for await (const e of m.episodic.range("events", { limit: 10 })) {
    seen.push(e.type);
  }
  assert.ok(seen.includes("tool_call"), `range missing event; saw ${seen}`);
});

test("semantic: upsert / search (fake embedder)", async () => {
  await m.semantic.upsert("notes", [
    { id: "a", content: "apple" },
    { id: "b", content: "banana" },
  ]);
  const hits = await m.semantic.search("notes", { queryText: "apple", topK: 2 });
  assert.ok(hits.length >= 1, "expected at least one hit");
  assert.equal(hits[0].record?.id, "a", `top hit was ${hits[0].record?.id}`);
});

test("artifact: put / get / stat (client-stream + server-stream)", async () => {
  const payload = enc.encode("x".repeat(200_000)); // spans multiple 64 KiB chunks
  const ref = await m.artifact.put("blobs", payload, { contentType: "application/octet-stream" });
  assert.ok(ref.id, "no artifact id");
  assert.equal(ref.size, BigInt(payload.length));

  const back = await m.artifact.get("blobs", ref.id);
  assert.equal(back.length, payload.length);
  assert.deepEqual(back, payload);

  const stat = await m.artifact.stat("blobs", ref.id);
  assert.equal(stat.found, true);
});

test("lease: acquire / inspect / release", async () => {
  const handle = await m.lease.acquire("locks", "job-42", { ttlSeconds: 30 });
  assert.ok(handle.holderId, "no holder id");

  const insp = await m.lease.inspect("locks", "job-42");
  assert.equal(insp.held, true);

  const released = await m.lease.release(handle.holderId, "locks", "job-42");
  assert.equal(released, true);
});

test("graph: upsert nodes/edges / neighbors", async () => {
  await m.graph.upsertNodes("knowledge", [
    { id: "n-a", labels: ["Person"] },
    { id: "n-b", labels: ["Person"] },
  ]);
  await m.graph.upsertEdges("knowledge", [
    { id: "e-ab", type: "KNOWS", from: "n-a", to: "n-b" },
  ]);
  const res = await m.graph.neighbors("knowledge", "n-a");
  assert.ok(res.nodes.some((n) => n.id === "n-b"), "neighbor n-b not found");
});
