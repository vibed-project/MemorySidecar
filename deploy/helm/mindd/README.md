# mindD Helm chart

Installs the mindD gRPC server into Kubernetes.

## TL;DR

```bash
# Mint a keypair first. The chart has no default key and will not render
# without one.
mindctl token gen-keypair

helm install mindd deploy/helm/mindd \
  --set config.auth.paseto.public_key_hex=<public-half-hex>
```

`image.repository` defaults to `ghcr.io/vibed-project/mindd`, so no registry
override is needed unless you mirror the image.

The chart deliberately ships **no default signing key**. Earlier versions
defaulted to the development keypair from `configs/example.yaml`, whose private
half is published in this repository, alongside `policy.default: allow`. A
default install therefore trusted tokens that anyone who had read the repository
could mint, for any tenant. The chart now fails at template time if
`config.auth.paseto.public_key_hex` is unset, and fails again if it is set to
that known keypair. Keep the private half outside the cluster.

## Common overrides

```yaml
# values.example.yaml
image:
  repository: ghcr.io/your-org/mindd
  tag: v0.1.0

# Postgres-backed kv namespace via injected DSN.
env:
  - name: MINDD_PG_DSN
    valueFrom:
      secretKeyRef:
        name: mindd-pg
        key: dsn

config:
  auth:
    paseto:
      public_key_hex: "<your-public-key-hex>"
  backends:
    - name: pg-main
      driver: postgres
      options:
        dsn_env: MINDD_PG_DSN
  namespaces:
    - { block: kv, name: scratchpad, backend: pg-main }

# Terminate TLS on the pod (Pass an existing cert/key Secret instead of inlining).
tls:
  enabled: true
  existingSecret: mindd-server-tls
```

## What the chart renders

| Object | Always | Purpose |
|---|---|---|
| `Deployment` | yes | runs the mindD binary |
| `Service` | yes | exposes ports `grpc/http/metrics` |
| `ConfigMap` | yes | mounts `config.yaml` at `/etc/mindd/config.yaml` |
| `ServiceAccount` | when `serviceAccount.create=true` (default) | pod identity |
| `Secret` (type `kubernetes.io/tls`) | when `tls.enabled=true` AND no `existingSecret` | cert + key |
| `ServiceMonitor` | when `serviceMonitor.enabled=true` | prometheus-operator scrape |

## Probes

Liveness and readiness use the standard gRPC health protocol on the `grpc`
port. Probing the `mindd.kv.v1.KV` service ensures the wiring is fully
up before traffic flows.

## Notes

- The `Deployment` includes `checksum/config` on the pod template, so a
  `helm upgrade` after a values change re-rolls the pod even when only the
  `ConfigMap` content changed.
- The default `podSecurityContext` / `securityContext` follow the
  restricted Pod Security Standard (non-root, read-only root FS, no extra
  capabilities). The distroless `nonroot` image runs as UID 65532.
- This chart **does not** provision certificates. Use cert-manager,
  external-secrets, etc. to populate the TLS Secret; the chart just
  references it.
- See [`../../../README.md`](../../../README.md) and
  [`../../../docs/architecture.md`](../../../docs/architecture.md) for the
  full mindD story.
