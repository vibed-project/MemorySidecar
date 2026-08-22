---
title: Helm chart
sidebar_position: 2
---

# Helm chart

The chart at `deploy/helm/mindd/` installs mindD into
Kubernetes with sensible defaults: Deployment + Service + ConfigMap +
ServiceAccount, optional Secret (TLS) and ServiceMonitor.

## Install

```bash
helm install msc deploy/helm/mindd \
  --set image.repository=ghcr.io/your-org/mindd \
  --set image.tag=v0.1.0
```

Default `image.repository` is `mindd`; default tag is the chart's
`appVersion`. You'll almost always want to point this at your own
registry.

## What gets rendered

| Object | When |
|---|---|
| `ServiceAccount` | always (unless `serviceAccount.create=false`) |
| `ConfigMap` | always — holds `config.yaml` from `.Values.config` |
| `Service` | always — three ports: grpc / http / metrics (the latter two are gated on the config) |
| `Deployment` | always |
| `Secret` (type `kubernetes.io/tls`) | when `tls.enabled=true` and no `existingSecret` is set |
| `ServiceMonitor` | when `serviceMonitor.enabled=true` |

## Pod template

- Non-root (`runAsNonRoot: true`, `seccompProfile.type: RuntimeDefault`)
- `readOnlyRootFilesystem: true`, no privilege escalation, all
  capabilities dropped — matches the restricted Pod Security Standard
- gRPC liveness + readiness probes on port 7777 (readiness aimed at the
  `mindd.kv.v1.KV` service)
- `checksum/config` annotation on the pod template, so `helm upgrade`
  re-rolls the pod automatically on any ConfigMap change

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
  backends:
    - name: pg-main
      driver: postgres
      options:
        dsn_env: MINDD_PG_DSN
  namespaces:
    - { block: kv, name: scratchpad, backend: pg-main }
```

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

The chart mounts the named Secret at `/etc/mindd/tls/`. cert-manager
produces Secrets in exactly the same `kubernetes.io/tls` shape.

### Multiple replicas

mindD is stateless (state lives in the backends). For HA, scale up:

```yaml
replicaCount: 3
```

Note: the in-memory drivers shipped as defaults are **per-replica**.
Multiple replicas reading the same in-memory namespace will *not* see
each other's data — point those namespaces at Postgres/S3/etc.

### Prometheus operator scrape

```yaml
serviceMonitor:
  enabled: true
  interval: 30s
```

## Probes and rollouts

The readiness probe uses the gRPC health protocol, so a pod doesn't
receive traffic until the gRPC server is actually accepting calls —
including DB connection pools, drivers, and registries finishing
startup. Use that as your rollout signal.

## What the chart does NOT do

- **Provision certificates.** Use cert-manager / external-secrets /
  whatever; the chart only references Secrets.
- **Run Postgres/Redis/MinIO.** Bring your own backends; the chart
  injects connection strings via env from your existing Secrets.
- **Configure the gateway with TLS.** Terminate TLS on the gateway port
  at your ingress.

See [`deploy/helm/mindd/README.md`](https://github.com/vibed-project/mindD/blob/main/deploy/helm/mindd/README.md) for
the full values reference.
