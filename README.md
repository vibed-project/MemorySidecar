# mindD

> A self-hosted, OSS, framework-agnostic **memory sidecar for agentic systems**.

[![License: Apache 2.0](https://img.shields.io/badge/License-Apache_2.0-blue.svg)](LICENSE)
[![Go](https://img.shields.io/badge/Go-1.26-00ADD8?logo=go&logoColor=white)](go.mod)
[![Release](https://img.shields.io/badge/release-v0.1.0-blue)](https://github.com/vibed-project/mindD/releases/tag/v0.1.0)
[![Docs](https://img.shields.io/badge/docs-vibed--project.github.io%2FmindD-blue)](https://vibed-project.github.io/mindD/)

mindD runs as a co-process to one or more agents and exposes a small,
opinionated gRPC API over **pluggable backends** for the kinds of memory
every agent stack ends up reinventing. Agents talk to the sidecar; the
sidecar talks to the substrate.

| Block | What it's for | Backends |
|---|---|---|
| **kv** | Tool-result caching and scratchpads — TTL'd, typed, optional heat-based cache tier | in-memory, Postgres |
| **episodic** | Append-only event log — replayable *and* live-tailable, with roles & sessions | in-memory, Postgres |
| **semantic** | Embed-and-search over records — bitemporal & revisable, hybrid (dense + sparse) retrieval | in-memory, pgvector |
| **artifact** | Blob storage with metadata for generated files — streamed in and out | in-memory, local FS, S3/MinIO |
| **lease** | Distributed locks with TTL for multi-agent coordination | in-memory, Postgres |
| **graph** | Typed nodes/edges with bounded traversal — bitemporal, as-of queryable | in-memory, Postgres |

Every popular agent framework (LangGraph, CrewAI, Autogen, …) re-implements
this plumbing in incompatible ways. mindD moves it out of the agent
and into a sidecar with one protocol, one auth model, one policy surface,
one observability story.

[**Documentation site →**](website/)

---

## Status

**Feature-complete against the roadmap, pre-release.** All six building
blocks are implemented — with the memory-optimisation work (bitemporal
lifecycle, hybrid retrieval, cache-tier eviction, temporal graph, cost
observability) landed on top — and the whole thing runs in dev and on a real
Kubernetes cluster. It is not yet versioned or released, and the proto shape
may still change before a tagged `v0.1.0`.

New to it? The [Use cases](website/docs/guides/use-cases.md) page is the
fastest tour of what the blocks let you build.

What works:

- All 6 building blocks (KV, Episodic, Semantic, Artifact, Lease, **Graph**)
  over gRPC.
- **Semantic is bitemporal & revisable**: `valid_from`/`valid_to` validity,
  soft-delete, `supersedes`/`source` provenance, "as-of" reads, bulk
  `Expire`-by-filter, and optimistic-concurrency versions (`if_version`).
- **Graph**: typed nodes/edges with bounded, structured recall
  (`Neighbors`/`Traverse`), hard-capped server-side.
- Drivers: in-memory, Postgres / pgvector, local filesystem, S3 / MinIO.
- Real embedders: fake (tests), Ollama (local dev), OpenAI (prod).
- gRPC TCP + UDS listeners, optional TLS / mTLS, plus an HTTP/JSON
  gateway via grpc-gateway.
- Capability tokens (PASETO v4.public default, JWT optional) with
  multi-key rotation.
- YAML policy engine — `allow` / `deny` / `rate_limit` / `cap` (per-request
  cost bounds), hot-reloadable.
- OpenTelemetry tracing (stdout / OTLP), Prometheus `/metrics` — including a
  memory-aware write-vs-query op-latency split plus backend-latency and
  result-shape metrics — and a structured JSON access log.
- Python SDK with idiomatic per-block clients.
- Multi-stage Docker image (distroless `nonroot`) + Helm chart.

What's not in yet: bidi-streaming RPCs. See the Known limitations section of
[CHANGELOG.md](CHANGELOG.md) for what v0.1.0 does not enforce yet.

## Quickstart

```bash
# 1. install codegen toolchain (one-time)
brew install bufbuild/buf/buf grpcurl
go install google.golang.org/protobuf/cmd/protoc-gen-go@latest
go install google.golang.org/grpc/cmd/protoc-gen-go-grpc@latest

# 2. build
make proto
make build

# 3. run
./bin/mindd --config configs/example.yaml
```

In another shell, mint a token and talk to it:

```bash
# The dev keypair in configs/example.yaml is for local use only.
export MINDD_PASETO_SECRET_HEX=38fb82e74985d41969ce39904d7cbe01dd31ea0b573dc8fc35c5689b8212ccc961a2d0067233cf8d6570c76f37573cbc31d33032ab256fe0c8032c0987d0fbf9

TOKEN=$(./bin/mindctl token issue --tenant acme --agent agent-1 \
        --ns 'kv/scratchpad,episodic/events,semantic/notes' \
        --ops '*' --ttl 1h)

# KV over gRPC
grpcurl -plaintext -H "x-mindd-capability: Bearer $TOKEN" \
  -d '{"namespace":"scratchpad","key":"hello","value":"d29ybGQ="}' \
  127.0.0.1:7777 mindd.kv.v1.KV/Put

# Or via the HTTP/JSON gateway
curl -sS -X POST http://127.0.0.1:8080/mindd.kv.v1.KV/Put \
  -H "x-mindd-capability: Bearer $TOKEN" \
  -d '{"namespace":"scratchpad","key":"hello","value":"d29ybGQ="}'
```

The [Quickstart docs page](website/docs/quickstart.md) covers all six
blocks and the Python SDK.

## Python

```python
import datetime as dt
from mindd import MindD

with MindD("127.0.0.1:7777", token=MINDD_TOKEN) as m:
    m.kv.put("scratchpad", "hello", b"world", ttl=dt.timedelta(minutes=5))
    print(m.kv.get("scratchpad", "hello").value)
```

```bash
cd sdk/python && pip install -e ".[dev]"
```

## Docker

```bash
make docker DOCKER_TAG=v0.1.0
docker run --rm -p 7777:7777 -p 8080:8080 -p 9090:9090 \
  -v $(pwd)/configs/example.yaml:/etc/mindd/config.yaml:ro mindd:v0.1.0
```

Multi-stage build, distroless `nonroot` runtime — no shell, no package
manager, UID 65532.

## Kubernetes

```bash
helm install msc deploy/helm/mindd \
  --set image.repository=ghcr.io/your-org/mindd \
  --set image.tag=v0.1.0
```

The chart applies the restricted Pod Security Standard, mounts the YAML
config from a `ConfigMap`, exposes gRPC + HTTP + metrics ports, and uses
gRPC-native liveness/readiness probes. See
[`deploy/helm/mindd/README.md`](deploy/helm/mindd/README.md) for
overrides (TLS/mTLS, Postgres backend, ServiceMonitor, …).

## Documentation

A full Docusaurus docs site lives under [`website/`](website/) — concepts,
per-block reference, configuration, operations, and deployment.

```bash
make docs-dev      # http://localhost:3000
make docs-build    # static site → website/build/
```

Highlights:

- [Overview](website/docs/intro.md)
- [Use cases](website/docs/guides/use-cases.md) — what the blocks let you build
- [Architecture](website/docs/concepts/architecture.md)
- [Capability tokens](website/docs/concepts/capabilities.md) and [Policy](website/docs/concepts/policy.md)
- Per-block: [KV](website/docs/blocks/kv.md), [Episodic](website/docs/blocks/episodic.md), [Semantic](website/docs/blocks/semantic.md), [Artifact](website/docs/blocks/artifact.md), [Lease](website/docs/blocks/lease.md), [Graph](website/docs/blocks/graph.md)
- [YAML config reference](website/docs/config/reference.md), [Hot reload](website/docs/config/hot-reload.md)
- [TLS & mTLS](website/docs/ops/tls.md), [Key rotation](website/docs/ops/key-rotation.md), [Observability](website/docs/ops/observability.md)

## Repo layout

```
proto/         protobuf service definitions
gen/           generated protobuf Go code (checked in)
cmd/
  mindd/  the server
  mindctl/      admin CLI (token issue, gen-keypair)
internal/      everything else
  auth/        TokenVerifier (PASETO + JWT, multi-key)
  config/      koanf-based YAML config + validation
  interceptor/ gRPC interceptors: recovery, observability, auth, policy
  obs/         OTel + Prometheus + slog bootstrap
  policy/      rule engine (allow/deny/rate-limit/cap) + atomic.Pointer holder
  server/      lifecycle, listeners, TLS, HTTP gateway
  kv/, episodic/, semantic/, artifact/, lease/, graph/
                each block: driver interface, registry, gRPC service, drivers
sdk/python/    Python client (`pip install -e .[dev]`)
deploy/helm/   Helm chart for Kubernetes
website/       Docusaurus docs site
configs/       example YAML config
Dockerfile     multi-stage build → distroless nonroot
```

## Development

```bash
make test                  # unit tests (-race not enabled by default; CI runs it)
make test-integration      # integration tests (require Docker)
make lint                  # golangci-lint
make run-dev               # run with configs/example.yaml
make proto                 # regenerate Go gRPC stubs from proto/
make proto-python          # regenerate Python stubs (uses buf remote plugins)
```

CI runs `go vet`, the race-enabled unit suite, the integration suite against
real Postgres and MinIO (`-tags=integration`), `golangci-lint`, and `buf lint`
/ `buf format` on every push and pull request.

## Contributing

Contributions welcome via GitHub Issues and Pull Requests. Please:

- Open an issue first if you're planning a larger change so we can align
  on scope.
- Keep PRs focused — one logical change per PR.
- Run `make test`, `go vet ./...`, `buf lint`, and `helm lint
  deploy/helm/mindd` before opening a PR.
- By submitting a contribution you agree to license it under the project
  license (Apache 2.0) per the inbound-equals-outbound model.

For security-sensitive issues, please **do not** open a public issue —
contact the maintainers privately. See `NOTICE` for attribution
boundaries.

## License

mindD is licensed under the [Apache License 2.0](LICENSE).

Third-party components and their licenses are listed in [NOTICE](NOTICE).

## Acknowledgments

The design is heavily inspired by Dapr's building-block model, narrowed
and specialised to memory and state for agentic workloads.
