package server_test

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/metadata"

	kvv1 "github.com/vibed-project/mindD/gen/mindd/kv/v1"
	"github.com/vibed-project/mindD/internal/auth"
	"github.com/vibed-project/mindD/internal/config"
	"github.com/vibed-project/mindD/internal/interceptor"
	"github.com/vibed-project/mindD/internal/kv"
	memdrv "github.com/vibed-project/mindD/internal/kv/drivers/memory"
	"github.com/vibed-project/mindD/internal/obs"
	"github.com/vibed-project/mindD/internal/server"
)

// genSelfSignedECDSACert writes a fresh self-signed cert + key to dir and
// returns their paths plus an in-memory tls.Config that trusts the cert as a
// CA. Suitable for in-process round-trip tests.
func genSelfSignedECDSACert(t *testing.T, dir string, host string) (certPath, keyPath string, caPEM []byte) {
	t.Helper()
	priv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	require.NoError(t, err)
	template := x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: host},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth, x509.ExtKeyUsageClientAuth},
		BasicConstraintsValid: true,
		IsCA:                  true,
		IPAddresses:           []net.IP{net.ParseIP("127.0.0.1")},
		DNSNames:              []string{host},
	}
	der, err := x509.CreateCertificate(rand.Reader, &template, &template, &priv.PublicKey, priv)
	require.NoError(t, err)

	certPath = filepath.Join(dir, "cert.pem")
	keyPath = filepath.Join(dir, "key.pem")
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	require.NoError(t, os.WriteFile(certPath, certPEM, 0o600))
	keyDER, err := x509.MarshalECPrivateKey(priv)
	require.NoError(t, err)
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})
	require.NoError(t, os.WriteFile(keyPath, keyPEM, 0o600))
	return certPath, keyPath, certPEM
}

func TestTLS_TCPListenerAcceptsTLSDial(t *testing.T) {
	dir := t.TempDir()
	certPath, keyPath, caPEM := genSelfSignedECDSACert(t, dir, "localhost")

	sec, pub, err := auth.GeneratePASETOKeyPair()
	require.NoError(t, err)
	v, _ := auth.NewPASETOVerifier(config.PASETOConfig{PublicKeyHex: pub})

	logSetup, _ := obs.NewLogger(config.LoggingConfig{Level: "warn", Format: "json"})

	reg := kv.NewRegistry()
	require.NoError(t, reg.Bind("scratchpad", memdrv.New(memdrv.Options{SweeperInterval: -1})))

	addr := freePort(t)
	srv, err := server.New(config.ServerConfig{
		GRPC: config.GRPCConfig{
			TCP: addr,
			TLS: &config.TLSConfig{CertFile: certPath, KeyFile: keyPath},
		},
		ShutdownTimeout: 2 * time.Second,
	}, server.Deps{Logger: logSetup.Logger, Verifier: v, KV: reg})
	require.NoError(t, err)

	_, err = srv.Start()
	require.NoError(t, err)
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = srv.Shutdown(ctx)
		_ = reg.Close()
	})

	// Build a client that trusts the self-signed CA and dials TLS.
	pool := x509.NewCertPool()
	require.True(t, pool.AppendCertsFromPEM(caPEM))
	creds := credentials.NewClientTLSFromCert(pool, "localhost")
	conn, err := grpc.NewClient(addr, grpc.WithTransportCredentials(creds))
	require.NoError(t, err)
	t.Cleanup(func() { _ = conn.Close() })

	tok, err := auth.IssuePASETO(sec, auth.Scope{
		Tenant: "acme", NamespacePattern: []string{"kv/scratchpad"}, AllowedOps: []string{"*"},
	}, time.Minute, "")
	require.NoError(t, err)
	ctx := metadata.AppendToOutgoingContext(context.Background(),
		interceptor.CapabilityHeader, "Bearer "+tok)

	c := kvv1.NewKVClient(conn)
	resp, err := c.Put(ctx, &kvv1.PutRequest{Namespace: "scratchpad", Key: "k", Value: []byte("v")})
	require.NoError(t, err)
	assert.Equal(t, uint64(1), resp.GetVersion())
}

func TestTLS_RequiredClientCertRejectsPlain(t *testing.T) {
	dir := t.TempDir()
	certPath, keyPath, caPEM := genSelfSignedECDSACert(t, dir, "localhost")
	caPath := filepath.Join(dir, "ca.pem")
	require.NoError(t, os.WriteFile(caPath, caPEM, 0o600))

	_, pub, _ := auth.GeneratePASETOKeyPair()
	v, _ := auth.NewPASETOVerifier(config.PASETOConfig{PublicKeyHex: pub})
	logSetup, _ := obs.NewLogger(config.LoggingConfig{Level: "warn", Format: "json"})
	reg := kv.NewRegistry()
	require.NoError(t, reg.Bind("scratchpad", memdrv.New(memdrv.Options{SweeperInterval: -1})))

	addr := freePort(t)
	srv, err := server.New(config.ServerConfig{
		GRPC: config.GRPCConfig{
			TCP: addr,
			TLS: &config.TLSConfig{
				CertFile: certPath, KeyFile: keyPath,
				ClientCAFile:      caPath,
				RequireClientCert: true,
			},
		},
		ShutdownTimeout: 2 * time.Second,
	}, server.Deps{Logger: logSetup.Logger, Verifier: v, KV: reg})
	require.NoError(t, err)
	_, err = srv.Start()
	require.NoError(t, err)
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = srv.Shutdown(ctx)
		_ = reg.Close()
	})

	pool := x509.NewCertPool()
	pool.AppendCertsFromPEM(caPEM)
	// Client trusts the server but presents no certificate of its own.
	creds := credentials.NewClientTLSFromCert(pool, "localhost")
	conn, err := grpc.NewClient(addr, grpc.WithTransportCredentials(creds))
	require.NoError(t, err)
	t.Cleanup(func() { _ = conn.Close() })

	c := kvv1.NewKVClient(conn)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_, err = c.Get(ctx, &kvv1.GetRequest{Namespace: "scratchpad", Key: "k"})
	require.Error(t, err, "expected handshake/tls failure with no client cert")
}
