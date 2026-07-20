# examples

Runnable tours of memsidecar.

## `agent_tour.py`

A whistle-stop tour of all six building blocks, framed as a single agent turn: a
"research assistant" logs the session (**episodic**), caches a tool result
(**kv**), remembers and recalls a fact (**semantic**), stores a generated report
(**artifact**), links entities (**graph**), and takes a lock (**lease**).

### Run it

**1. Start a sidecar** — the fastest way is Docker Compose from the repo root:

```bash
docker compose up --build -d
export MEMSIDECAR_TOKEN=$(docker compose run --rm -T token)
```

Or run a local build against the example config:

```bash
./bin/memsidecar --config configs/example.yaml &
export MEMSIDECAR_PASETO_SECRET_HEX=38fb82e74985d41969ce39904d7cbe01dd31ea0b573dc8fc35c5689b8212ccc961a2d0067233cf8d6570c76f37573cbc31d33032ab256fe0c8032c0987d0fbf9
export MEMSIDECAR_TOKEN=$(./bin/memctl token issue --tenant demo \
  --ns 'kv/*,episodic/*,semantic/*,artifact/*,lease/*,graph/*' --ops '*' --ttl 1h)
```

**2. Install the SDK and run the tour:**

```bash
pip install ./sdk/python
python examples/agent_tour.py
```

It prints each step and what the sidecar returned. `MEMSIDECAR_TARGET` overrides
the address (default `127.0.0.1:7777`).

The tour uses only the namespaces in `configs/example.yaml`
(`kv/scratchpad`, `episodic/events`, `semantic/notes`, `artifact/blobs`,
`lease/locks`, `graph/knowledge`), so it works against either setup unchanged.
