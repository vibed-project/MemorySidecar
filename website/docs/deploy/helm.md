---
title: Helm chart
sidebar_position: 2
---

# Helm chart

The chart at `deploy/helm/mindd/` installs mindD into Kubernetes: Deployment,
Service, ConfigMap and ServiceAccount, plus an optional Secret (TLS) and
ServiceMonitor.

## Install

:::danger A signing key is required
The chart ships **no** default `config.auth.paseto.public_key_hex`. It fails at
template time if the key is unset, and fails again if it is set to this
repository's published development key.

That is deliberate. The chart used to default to the development key, whose
private half is committed alongside it, together with `policy.default: allow`.
A default `helm install` therefore accepted tokens that anyone who had read the
repository could mint, for any tenant.
:::

Generate a keypair, then install with the public half:

```bash
mindctl token gen-keypair
# paseto:
#   secret_key_hex: <keep this outside the cluster; it mints tokens>
#   public_key_hex: <give this to the chart>

helm install mindd deploy/helm/mindd \
  --set config.auth.paseto.public_key_hex=<public_key_hex>
```

If you leave it out, `helm install` fails before anything is created:

```
Error: execution error at (mindd/templates/configmap.yaml:2:4):
config.auth.paseto.public_key_hex is required. Generate a keypair with
`mindctl token gen-keypair` and pass the public half ...
```

For anything beyond a one-liner, put the key in a values file, or template it
in from a Secret you manage separately. mindD only ever verifies tokens, so the
cluster never needs the private half.

### Image

`image.repository` defaults to `ghcr.io/vibed-project/mindd` and `image.tag`
defaults to the chart's `appVersion`, so a plain install pulls the published
multi-arch image. Override only if you mirror it:

```bash
helm install mindd deploy/helm/mindd \
  --set config.auth.paseto.public_key_hex=<hex> \
  --set image.repository=registry.internal/mindd \
  --set image.tag=v0.1.0
```

## What gets rendered

| Object | When |
|---|---|
| `ServiceAccount` | always (unless `serviceAccount.create=false`) |
| `ConfigMap` | always; holds `config.yaml` from `.Values.config` |
| `Service` | always; three ports: grpc / http / metrics (the latter two are gated on the config) |
| `Deployment` | always |
| `Secret` (type `kubernetes.io/tls`) | when `tls.enabled=true` and no `existingSecret` is set |
| `ServiceMonitor` | when `serviceMonitor.enabled=true` |

## Default config

`.Values.config` is rendered verbatim into the ConfigMap. Out of the box it
binds gRPC on `0.0.0.0:7777`, the HTTP gateway on `0.0.0.0:8080` and Prometheus
on `0.0.0.0:9090`, sets `policy.default: allow` with no rules, and declares one
in-memory backend serving four namespaces: `kv/scratchpad`, `episodic/events`,
`artifact/blobs` and `lease/locks`.

Note what that default does **not** include: no `semantic` or `graph`
namespace, no `tenant_isolation`, no `encryption`, and a policy that allows
everything. Replace `.Values.config` wholesale for a real deployment.

## Pod template

- Non-root (`runAsNonRoot: true`, `seccompProfile.type: RuntimeDefault`)
- `readOnlyRootFilesystem: true`, no privilege escalation, all capabilities
  dropped, matching the restricted Pod Security Standard
- gRPC liveness and readiness probes on port 7777 (readiness aimed at the
  `mindd.kv.v1.KV` service)
- `checksum/config` annotation on the pod template, so `helm upgrade` re-rolls
  the pod automatically on any ConfigMap change

## Common overrides

### Postgres backend via secret

```yaml
# values.yaml
env:
  - name: MINDD_PG_DSN
    valueFrom:
      secretKeyRef:
        name: mindd-pg
        key: dsn

config:
  auth:
    paseto:
      public_key_hex: "<your public key hex>"
  backends:
    - name: pg-main
      driver: postgres
      options:
        dsn_env: MINDD_PG_DSN
  namespaces:
    - { block: kv, name: scratchpad, backend: pg-main }
```

### Multi-tenant

`tenant_isolation` defaults to `false`, which puts every tenant in one physical
partition. Set it before the first write:

```yaml
config:
  tenant_isolation: true
```

See [Tenant isolation](../concepts/tenant-isolation.md).

### Encryption at rest

Keys come from the environment, never from the ConfigMap:

```yaml
env:
  - name: MINDD_ENC_PRIMARY
    valueFrom:
      secretKeyRef:
        name: mindd-enc
        key: primary

config:
  encryption:
    keys:
      - id: primary-2026-08
        secret_env: MINDD_ENC_PRIMARY
  namespaces:
    - { block: kv, name: secrets, backend: pg-main, encrypt: true }
```

See [Encryption at rest](../concepts/encryption-at-rest.md).

### TLS with cert-manager

```yaml
# values.yaml
tls:
  enabled: true
  existingSecret: mindd-server-tls   # populated by a Certificate resource

# Also tell the inner mindd config to actually use those mounted files:
config:
  server:
    grpc:
      tls:
        cert_file: /etc/mindd/tls/tls.crt
        key_file:  /etc/mindd/tls/tls.key
```

The chart mounts the named Secret at `/etc/mindd/tls/`. cert-manager produces
Secrets in exactly the same `kubernetes.io/tls` shape. Both `cert_file` and
`key_file` must be set; a `tls:` block with only `client_ca_file` is ignored and
the listener stays plaintext.

### Multiple replicas

mindD is stateless (state lives in the backends). For HA, scale up:

```yaml
replicaCount: 3
```

The in-memory drivers shipped as defaults are **per-replica**. Multiple
replicas reading the same in-memory namespace will not see each other's data;
point those namespaces at Postgres or S3.

### Prometheus operator scrape

```yaml
serviceMonitor:
  enabled: true
  interval: 30s
```

## Probes and rollouts

The readiness probe uses the gRPC health protocol, so a pod doesn't receive
traffic until the gRPC server is actually accepting calls, including DB
connection pools, drivers, and registries finishing startup. Use that as your
rollout signal.

## Config changes

`SIGHUP` reloads only the auth verifier, the policy engine and the log level,
and the chart gives you no hook to send one. In practice a values change means
`helm upgrade`, which changes the ConfigMap checksum and re-rolls the pods. That
is also required for anything `SIGHUP` cannot swap: listeners, backends,
namespaces, exporters and encryption keys. See
[Hot reload](../config/hot-reload.md).

## What the chart does NOT do

- **Provision certificates.** Use cert-manager, external-secrets, or whatever
  you already run; the chart only references Secrets.
- **Run Postgres, MinIO or anything else.** Bring your own backends; the chart
  injects connection strings via env from your existing Secrets.
- **Configure the gateway with TLS.** The HTTP gateway has no TLS option in
  this release. Terminate at your ingress.
- **Restrict network reach.** The gRPC reflection service is unauthenticated
  (see [Security](../security.md)); keep the Service `ClusterIP` and add a
  NetworkPolicy.

See
[`deploy/helm/mindd/README.md`](https://github.com/vibed-project/mindD/blob/main/deploy/helm/mindd/README.md)
for the full values reference.
