// Offline smoke test: verifies the module loads (generated stubs + wiring are
// valid), the block clients are present, the capability interceptor guards an
// empty token, and the enums are re-exported. Constructing a client does not
// open a connection, so no server is required. Run against the built package:
//
//   npm run build && node --test test/
import assert from "node:assert/strict";
import { test } from "node:test";

import {
  MemSidecar,
  capabilityInterceptor,
  CAPABILITY_HEADER,
  SearchMode,
  Direction,
} from "../dist/index.js";

test("MemSidecar exposes all six block clients", () => {
  const m = new MemSidecar("127.0.0.1:7777", { token: "test-token" });
  for (const block of ["kv", "episodic", "semantic", "artifact", "lease", "graph"]) {
    assert.equal(typeof m[block], "object", `missing block: ${block}`);
  }
  assert.ok(m.transport, "transport should be set");
});

test("full http(s) URLs are accepted as the address", () => {
  assert.doesNotThrow(() => new MemSidecar("https://mem.internal:443", { token: "t", tls: true }));
});

test("an empty token is rejected", () => {
  assert.throws(() => capabilityInterceptor(""), /must not be empty/);
  assert.throws(() => new MemSidecar("127.0.0.1:7777", { token: "" }), /must not be empty/);
});

test("capability interceptor stamps the header", async () => {
  const interceptor = capabilityInterceptor("abc123");
  const seen = new Headers();
  const next = async (req) => ({ header: req.header });
  await interceptor(next)({ header: seen });
  assert.equal(seen.get(CAPABILITY_HEADER), "Bearer abc123");
});

test("enums are re-exported with proto values", () => {
  assert.equal(SearchMode.HYBRID, 3);
  assert.equal(Direction.OUT, 1);
});
