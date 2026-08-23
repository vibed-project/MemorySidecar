---
title: TLS & mTLS
sidebar_position: 1
---

# Transport security

The gRPC TCP listener supports TLS and mTLS. UDS connections are always
plaintext. Filesystem permissions are the authz boundary, and TLS on a
unix socket is just overhead.

## Modes

| Setting | Behaviour |
|---|---|
| `cert_file` + `key_file` only | TLS-only. Server presents the cert; client validates it. |
| Add `client_ca_file` + `require_client_cert: true` | **mTLS.** Every client must present a cert chained to `client_ca_file`. |
| Add `client_ca_file` only (no require) | "If a client presents a cert, validate it; otherwise allow." |

The listener log reports the active mode:

```json
{"msg":"listening","transport":"tcp","addr":"...","security":"mtls"}
```

## Config

```yaml
server:
  grpc:
    tcp: "0.0.0.0:7777"
    tls:
      cert_file: /etc/mindd/tls/server.crt
      key_file:  /etc/mindd/tls/server.key
      client_ca_file: /etc/mindd/tls/client-ca.crt
      require_client_cert: true
```

`MinVersion` is forced to TLS 1.3. `NextProtos` is set to `["h2"]` so
grpc-go's ALPN check is satisfied (without it the handshake fails with
"missing selected ALPN property"; see grpc/grpc-go#434).

## Generating a self-signed cert for dev

A 30-line Go program in `internal/server/tls_test.go` produces an ECDSA
P-256 self-signed cert valid for an hour. For quick local testing,
generate one with:

```go
// gen.go
package main

import (
	"crypto/ecdsa"; "crypto/elliptic"; "crypto/rand"
	"crypto/x509"; "crypto/x509/pkix"; "encoding/pem"
	"math/big"; "net"; "os"; "time"
)

func main() {
	priv, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	serial, _ := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	tmpl := x509.Certificate{
		SerialNumber: serial,
		Subject:      pkix.Name{CommonName: "localhost"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(24 * time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth, x509.ExtKeyUsageClientAuth},
		BasicConstraintsValid: true, IsCA: true,
		IPAddresses: []net.IP{net.ParseIP("127.0.0.1")},
		DNSNames:    []string{"localhost"},
	}
	der, _ := x509.CreateCertificate(rand.Reader, &tmpl, &tmpl, &priv.PublicKey, priv)
	os.WriteFile("server.crt", pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o600)
	keyDER, _ := x509.MarshalECPrivateKey(priv)
	os.WriteFile("server.key", pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER}), 0o600)
}
```

```bash
go run gen.go
```

Then dial with `grpcurl`:

```bash
grpcurl -cacert server.crt -authority localhost \
  -H "x-mindd-capability: Bearer $TOKEN" \
  -d '{"namespace":"scratchpad","key":"hello"}' \
  127.0.0.1:7777 mindd.kv.v1.KV/Get
```

## In Kubernetes

The Helm chart accepts either an inline cert + key (rendered into a
`kubernetes.io/tls` Secret) or a pre-existing Secret name:

```yaml
# values.yaml
tls:
  enabled: true
  existingSecret: mindd-server-tls   # managed by cert-manager
```

The Deployment mounts that Secret at `/etc/mindd/tls`, where the
referenced `cert_file` / `key_file` live.

## TLS and capability tokens are orthogonal

mTLS authenticates the **transport peer**; the capability token still
scopes **what they can do**. A connection authenticated by mTLS but
missing a valid token is still rejected with `Unauthenticated` at the
auth interceptor.

## Known limits

- The HTTP/JSON gateway runs on its own listener; chart support for
  terminating TLS on the gateway itself is not built in. Terminate at
  the ingress.
- The OTLP exporter to a remote collector uses its own TLS settings under
  `observability.tracing.otlp`; the server-side TLS config above doesn't
  affect outbound exports.
