---
title: Policy
sidebar_position: 4
---

# Policy

The policy engine sits in the interceptor chain right after auth. Its job
is to decide whether a call that's correctly scoped should still be
allowed — and, optionally, rate-limited or cost-capped.

memsidecar ships two engines:

- `NoopEngine` — allows everything. The default when `policy:` is omitted.
- `RuleEngine` — driven by declarative YAML rules, hot-reloadable.

## Rule shape

```yaml
policy:
  default: allow       # allow | deny — fallback when no rule matches
  rules:
    - name: block-secret-namespaces
      effect: deny
      reason: "secret-* namespaces are off-limits"
      match:
        namespace: ["secret-*"]

    - name: throttle-semantic-search
      effect: rate_limit
      match:
        op: ["semantic.search"]
      bucket:
        per_tenant: true
        rate_per_second: 5
        burst: 10

    - name: cap-search-topk
      effect: cap
      reason: "top_k over 200 is not allowed"
      match:
        op: ["semantic.search"]
      max:
        top_k: 200
```

Each rule has:

- `name` — required, unique.
- `effect` — `allow`, `deny`, `rate_limit`, or `cap`.
- `reason` — optional string surfaced in the rejection status message and
  audit logs.
- `match` — filter on `tenant`, `agent`, `block`, `namespace` (glob),
  `op`. Empty fields match anything.
- `bucket` (rate_limit only) — `rate_per_second`, `burst`, plus boolean
  axes (`per_tenant`, `per_agent`, `per_namespace`, `per_op`) that scope
  the limiter.
- `max` (cap only) — per-request magnitude bounds: `top_k` (semantic
  Search), `limit` (scan/range page size), `depth` / `fan_out` (graph
  traversal), `rerank_candidate_k` (semantic hybrid per-lane candidate
  depth). At least one bound is required;
  a zero bound imposes no limit on that dimension. This is how bounded
  traversal is enforced server-side.

## Evaluation order

Rules are scanned **in declaration order**. The first matching rule's
effect decides:

- `allow` → request proceeds; no further rules consulted.
- `deny` → reject with `PermissionDenied: policy: <reason>`.
- `rate_limit` → consume a token from the rule's bucket. If a token is
  available, **fall through** to subsequent rules. If exhausted, reject.
- `cap` → check the request magnitude against `max`. If within bounds,
  **fall through**; if any bound is breached, reject.

If nothing matches, the engine returns `policy.default` (default `allow`).

Rejection status codes distinguish the two failure classes: a `deny`
(or `default: deny`) surfaces as **`PermissionDenied`**, while a
`rate_limit` or `cap` rejection surfaces as **`ResourceExhausted`** — so
clients can back off on the latter without treating it as an auth failure.

The fall-through on rate-limit/cap success is the useful bit: you can chain
"rate-limit AND then allow" or "cap top_k AND allow" with a single small
ruleset.

## Hot reload

A `SIGHUP` re-reads the policy section and atomically swaps the active
engine via [`policy.Holder`](https://github.com/vibed-project/MemorySidecar/blob/main/internal/policy/holder.go).
In-flight requests keep using the engine they were dispatched under; the
next request after the SIGHUP sees the new rules.

See [Hot reload](../config/hot-reload.md) for the full reload contract.

## What policy is *not*

The policy engine handles **access and cost decisions** —
allow / deny / rate-limit / cost-cap. A larger policy story (PII redaction,
post-read transforms, retention enforcement) is intentionally deferred;
the [HookCtx](https://github.com/vibed-project/MemorySidecar/blob/main/internal/policy/engine.go)
type already carries the fields a richer engine would need.
