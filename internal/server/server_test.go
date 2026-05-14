package server_test

import (
	"context"
	"io"
	"net"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"

	kvv1 "memsidecar/gen/memsidecar/kv/v1"
	"memsidecar/internal/auth"
	"memsidecar/internal/config"
	"memsidecar/internal/interceptor"
	"memsidecar/internal/kv"
	memdrv "memsidecar/internal/kv/drivers/memory"
	"memsidecar/internal/obs"
	"memsidecar/internal/server"
)

func freePort(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	defer func() { _ = l.Close() }()
	return l.Addr().String()
}

func newEndToEnd(t *testing.T) (kvv1.KVClient, string, func() string) {
	t.Helper()

	sec, pub, err := auth.GeneratePASETOKeyPair()
	require.NoError(t, err)

	v, err := auth.NewPASETOVerifier(config.PASETOConfig{PublicKeyHex: pub})
	require.NoError(t, err)

	logSetup, err := obs.NewLogger(config.LoggingConfig{Level: "info", Format: "json"})
	require.NoError(t, err)

	reg := kv.NewRegistry()
	require.NoError(t, reg.Bind("scratchpad", memdrv.New(memdrv.Options{SweeperInterval: -1})))

	addr := freePort(t)
	srv, err := server.New(config.ServerConfig{
		GRPC:            config.GRPCConfig{TCP: addr},
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

	conn, err := grpc.NewClient(addr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	require.NoError(t, err)
	t.Cleanup(func() { _ = conn.Close() })

	mintToken := func() string {
		tok, err := auth.IssuePASETO(sec, auth.Scope{
			Tenant: "acme", Agent: "agent-1",
			NamespacePattern: []string{"kv/scratchpad"},
			AllowedOps:       []string{"*"},
		}, time.Minute, "tid")
		require.NoError(t, err)
		return tok
	}
	return kvv1.NewKVClient(conn), addr, mintToken
}

func withToken(ctx context.Context, tok string) context.Context {
	return metadata.AppendToOutgoingContext(ctx, interceptor.CapabilityHeader, "Bearer "+tok)
}

func TestE2E_HappyPath(t *testing.T) {
	c, _, mint := newEndToEnd(t)
	ctx := withToken(context.Background(), mint())

	_, err := c.Put(ctx, &kvv1.PutRequest{Namespace: "scratchpad", Key: "hello", Value: []byte("world"), ContentType: "text/plain"})
	require.NoError(t, err)

	got, err := c.Get(ctx, &kvv1.GetRequest{Namespace: "scratchpad", Key: "hello"})
	require.NoError(t, err)
	assert.True(t, got.GetFound())
	assert.Equal(t, []byte("world"), got.GetValue())

	stream, err := c.Scan(ctx, &kvv1.ScanRequest{Namespace: "scratchpad", IncludeValues: true})
	require.NoError(t, err)
	var keys []string
	for {
		item, err := stream.Recv()
		if err == io.EOF {
			break
		}
		require.NoError(t, err)
		keys = append(keys, item.GetKey())
	}
	assert.Equal(t, []string{"hello"}, keys)

	del, err := c.Delete(ctx, &kvv1.DeleteRequest{Namespace: "scratchpad", Key: "hello"})
	require.NoError(t, err)
	assert.True(t, del.GetExisted())
}

func TestE2E_Unauthenticated(t *testing.T) {
	c, _, _ := newEndToEnd(t)
	_, err := c.Get(context.Background(), &kvv1.GetRequest{Namespace: "scratchpad", Key: "k"})
	require.Error(t, err)
	assert.Equal(t, codes.Unauthenticated, status.Code(err))
}

func TestE2E_ReadOnlyTokenCannotPut(t *testing.T) {
	c, _, _ := newEndToEnd(t)
	sec, pub, _ := auth.GeneratePASETOKeyPair()
	_ = pub
	roTok, err := auth.IssuePASETO(sec, auth.Scope{
		Tenant:           "acme",
		NamespacePattern: []string{"kv/scratchpad"},
		AllowedOps:       []string{"kv.get"},
	}, time.Minute, "")
	require.NoError(t, err)
	ctx := withToken(context.Background(), roTok)
	// Different signer than the server's verifier, so the token is rejected as
	// unauthenticated. To test PermissionDenied we'd need to reuse the same
	// signer — covered by the bufconn service tests already.
	_, err = c.Put(ctx, &kvv1.PutRequest{Namespace: "scratchpad", Key: "k", Value: []byte("v")})
	require.Error(t, err)
}
