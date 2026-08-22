#!/usr/bin/env python3
"""A whistle-stop tour of mindd's building blocks, framed as one agent turn.

A "research assistant" agent handles a task and uses the sidecar for every kind
of memory it needs:

  - episodic : an append-only log of what happened this session
  - kv       : a short-lived cache for an expensive tool result
  - semantic : durable facts it can recall by meaning
  - artifact : a generated file, stored by id
  - graph    : relationships between entities
  - lease    : a lock so two workers don't collide

Run it against a running sidecar (see examples/README.md):

    export MINDD_TOKEN=$(mindctl token issue --tenant demo \
        --ns 'kv/*,episodic/*,semantic/*,artifact/*,lease/*,graph/*' --ops '*')
    python examples/agent_tour.py

Only the standard config namespaces are used (configs/example.yaml):
kv/scratchpad, episodic/events, semantic/notes, artifact/blobs, lease/locks,
graph/knowledge.
"""
from __future__ import annotations

import datetime as dt
import os
import sys
import uuid

from mindd import MindD
from mindd.semantic.v1 import semantic_pb2
from mindd.graph.v1 import graph_pb2


def h(title: str) -> None:
    print(f"\n\033[1m{title}\033[0m")


def main() -> int:
    target = os.environ.get("MINDD_TARGET", "127.0.0.1:7777")
    token = os.environ.get("MINDD_TOKEN")
    if not token:
        print("set MINDD_TOKEN (see examples/README.md)", file=sys.stderr)
        return 2

    session = f"tour-{uuid.uuid4().hex[:8]}"

    with MindD(target, token=token) as m:
        h("1. episodic — log what happens this session")
        # A conversation turn plus the tool call it triggered.
        m.episodic.append("events", "message", b"What's the capital of France?",
                          role="user", session_id=session)
        m.episodic.append("events", "tool_call", b'{"tool":"search","q":"capital of France"}',
                          role="assistant", session_id=session)
        turns = list(m.episodic.range("events", limit=100))
        mine = [e for e in turns if e.session_id == session]
        print(f"   logged {len(mine)} events for session {session}")

        h("2. kv — cache an expensive tool result with a TTL")
        m.kv.put("scratchpad", f"search:{session}", b"Paris",
                 ttl=dt.timedelta(minutes=5), content_type="text/plain")
        cached = m.kv.get("scratchpad", f"search:{session}")
        print(f"   cache hit: {cached.value.decode()} (found={cached.found})")

        h("3. semantic — remember a fact, then recall it by meaning")
        fact_id = f"fact-{session}"
        m.semantic.upsert("notes", [
            semantic_pb2.Record(id=fact_id, content="Paris is the capital of France",
                                metadata={"topic": "geography", "session": session}),
        ])
        hits = m.semantic.search("notes", query_text="Paris is the capital of France",
                                 top_k=3, filter={"session": session})
        print(f"   recalled {len(hits)} note(s); top id={hits[0].record.id} "
              f"score={hits[0].score:.3f}" if hits else "   no hits")

        h("4. artifact — store a generated file, fetch it back by id")
        report = f"# Answer\n\nThe capital of France is Paris.\n".encode()
        ref = m.artifact.put("blobs", report, id=f"report-{session}", content_type="text/markdown")
        got = m.artifact.get("blobs", ref.id)
        print(f"   stored {ref.size} bytes (sha256={ref.sha256[:12]}…); "
              f"read back {len(got)} bytes, match={got == report}")

        h("5. graph — link the entities we learned about")
        m.graph.upsert_nodes("knowledge", [
            graph_pb2.Node(id="paris", labels=["City"]),
            graph_pb2.Node(id="france", labels=["Country"]),
        ])
        m.graph.upsert_edges("knowledge", [
            graph_pb2.Edge(id=f"cap-{session}", type="CAPITAL_OF",
                           **{"from": "paris"}, to="france"),
        ])
        nb = m.graph.neighbors("knowledge", "paris", edge_types=["CAPITAL_OF"])
        print(f"   paris → {[e.type + '→' + e.to for e in nb.edges]}")

        h("6. lease — coordinate exclusive work")
        lock = m.lease.acquire("locks", f"finalize-{session}", ttl=dt.timedelta(seconds=30))
        print(f"   acquired lock, holder={lock.holder_id[:8]}…")
        info = m.lease.inspect("locks", f"finalize-{session}")
        print(f"   inspect: held={info.held}")
        m.lease.release(lock.holder_id, "locks", f"finalize-{session}")
        print("   released")

        h("done — the agent's whole memory footprint lived in the sidecar")
        print("   Try `mindctl ns ls` to see item counts, or `mindctl episodic tail events`.")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
