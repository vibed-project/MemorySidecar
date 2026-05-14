---
title: Hot reload
sidebar_position: 2
---

# Hot reload

A `SIGHUP` to the running process re-reads the YAML config and atomically
swaps the subsystems that can change without restarting. In-flight RPCs
finish under the old configuration; the next request after the signal
sees the new one.

## What reloads

| Subsystem | Reloads on SIGHUP? | Notes |
|---|---|---|
| **Policy rules** | yes | The active `policy.Engine` is wrapped in an `atomic.Pointer` holder. |
| **Auth verifier** | yes | PASETO/JWT verifier is in a `VerifierHolder`. Add or remove keys, rotate algorithms, swap PASETO for JWT — all live. |
| **Log level** | yes | Backed by an `slog.LevelVar`. |
| Listeners (TCP/UDS/HTTP/metrics) | no | Restart required. |
| Tracing & metrics exporters | no | Restart required. |
| Backends & namespaces | no | Restart required. |

## How to trigger

```bash
kill -HUP $(pgrep memsidecar)
```

On Kubernetes, you can `exec` into the pod for a single-pod tweak, or
restart the deployment (`kubectl rollout restart`) when you want the
config change to propagate to every replica with the same Helm-rendered
`ConfigMap`.

## Verification

The reload logs a single JSON line you can grep for:

```json
{"msg":"reloaded","policy_default":"allow","policy_rules":3,"verifier":"paseto","log_level":"info"}
```

A failure (e.g. invalid YAML, bad token format) leaves the previous
configuration in place and logs at ERROR:

```json
{"level":"ERROR","msg":"reload failed","err":"load: ..."}
```

## Behind the scenes

Inside the process, each hot-reloadable subsystem hides behind a thin
holder that satisfies the same interface the rest of the code already
talks to:

- [`policy.Holder`](https://github.com/m-koerbaecher/memsidecar/blob/main/internal/policy/holder.go)
  — wraps an `Engine` in `atomic.Pointer`. The policy interceptor calls
  `holder.PreRead` / `holder.PreWrite`; the holder loads the current
  engine and delegates.
- [`auth.VerifierHolder`](https://github.com/m-koerbaecher/memsidecar/blob/main/internal/auth/holder.go)
  — mirrors the pattern for `TokenVerifier`.
- `slog.LevelVar` — the standard library's own atomic level.

No reads are blocked during a swap; no in-flight request can land in a
half-swapped state.

## What you can do with this

- **Tighten policy in response to an incident.** Add a `deny` rule, drop
  in a SIGHUP, mitigation is live without a deploy.
- **Rotate signing keys.** Add the new public key to the list, SIGHUP,
  flip the issuer to the new private key, wait for old tokens to expire,
  drop the old key, SIGHUP again. See [Key rotation](../ops/key-rotation.md).
- **Crank logs to debug for one incident.** Set `observability.logging.level: debug`,
  SIGHUP, get the trace, set it back to `info`, SIGHUP. No restart.

## Limitations

- A reload that flips `auth.verifier` from `paseto` to `jwt` immediately
  invalidates every PASETO-signed token in flight. There's no graceful
  cross-verifier transition; do this only in maintenance windows.
- Removing a public key invalidates tokens still signed by it. Always
  use the overlap-window pattern in [Key rotation](../ops/key-rotation.md).
