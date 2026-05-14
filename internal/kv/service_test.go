package kv_test

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
	"google.golang.org/grpc/status"
	"google.golang.org/grpc/test/bufconn"

	kvv1 "memsidecar/gen/memsidecar/kv/v1"
	"memsidecar/internal/auth"
	"memsidecar/internal/kv"
	memdrv "memsidecar/internal/kv/drivers/memory"
)

func newTestServer(t *testing.T, cap *auth.Capability) kvv1.KVClient {
	t.Helper()

	reg := kv.NewRegistry()
	require.NoError(t, reg.Bind("scratchpad", memdrv.New(memdrv.Options{SweeperInterval: -1})))

	svc := kv.NewService(reg)

	injectCap := func(ctx context.Context) context.Context {
		if cap == nil {
			return ctx
		}
		return auth.WithCapability(ctx, cap)
	}

	srv := grpc.NewServer(
		grpc.UnaryInterceptor(func(ctx context.Context, req any, _ *grpc.UnaryServerInfo, h grpc.UnaryHandler) (any, error) {
			return h(injectCap(ctx), req)
		}),
		grpc.StreamInterceptor(func(srv any, ss grpc.ServerStream, _ *grpc.StreamServerInfo, h grpc.StreamHandler) error {
			return h(srv, &wrappedStream{ServerStream: ss, ctx: injectCap(ss.Context())})
		}),
	)
	kvv1.RegisterKVServer(srv, svc)

	lis := bufconn.Listen(1 << 16)
	go func() { _ = srv.Serve(lis) }()
	t.Cleanup(func() {
		srv.GracefulStop()
		_ = reg.Close()
	})

	conn, err := grpc.NewClient("passthrough:bufnet",
		grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) { return lis.DialContext(ctx) }),
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	)
	require.NoError(t, err)
	t.Cleanup(func() { _ = conn.Close() })

	return kvv1.NewKVClient(conn)
}

type wrappedStream struct {
	grpc.ServerStream
	ctx context.Context
}

func (w *wrappedStream) Context() context.Context { return w.ctx }

func fullScope() *auth.Capability {
	return &auth.Capability{
		Scope: auth.Scope{
			Tenant:           "acme",
			NamespacePattern: []string{"kv/scratchpad"},
			AllowedOps:       []string{"*"},
		},
	}
}

func TestService_PutGetDelete(t *testing.T) {
	c := newTestServer(t, fullScope())
	ctx := context.Background()

	put, err := c.Put(ctx, &kvv1.PutRequest{Namespace: "scratchpad", Key: "k", Value: []byte("v"), ContentType: "text/plain"})
	require.NoError(t, err)
	assert.Equal(t, uint64(1), put.GetVersion())

	get, err := c.Get(ctx, &kvv1.GetRequest{Namespace: "scratchpad", Key: "k"})
	require.NoError(t, err)
	assert.True(t, get.GetFound())
	assert.Equal(t, []byte("v"), get.GetValue())

	del, err := c.Delete(ctx, &kvv1.DeleteRequest{Namespace: "scratchpad", Key: "k"})
	require.NoError(t, err)
	assert.True(t, del.GetExisted())

	get, err = c.Get(ctx, &kvv1.GetRequest{Namespace: "scratchpad", Key: "k"})
	require.NoError(t, err)
	assert.False(t, get.GetFound())
}

func TestService_MissingCapability(t *testing.T) {
	c := newTestServer(t, nil)
	_, err := c.Get(context.Background(), &kvv1.GetRequest{Namespace: "scratchpad", Key: "k"})
	require.Error(t, err)
	assert.Equal(t, codes.Unauthenticated, status.Code(err))
}

func TestService_NamespaceNotAllowed(t *testing.T) {
	cap := &auth.Capability{Scope: auth.Scope{Tenant: "acme", NamespacePattern: []string{"kv/other"}, AllowedOps: []string{"*"}}}
	c := newTestServer(t, cap)
	_, err := c.Get(context.Background(), &kvv1.GetRequest{Namespace: "scratchpad", Key: "k"})
	require.Error(t, err)
	assert.Equal(t, codes.PermissionDenied, status.Code(err))
}

func TestService_OpNotAllowed(t *testing.T) {
	cap := &auth.Capability{Scope: auth.Scope{Tenant: "acme", NamespacePattern: []string{"kv/scratchpad"}, AllowedOps: []string{"kv.get"}}}
	c := newTestServer(t, cap)
	_, err := c.Put(context.Background(), &kvv1.PutRequest{Namespace: "scratchpad", Key: "k", Value: []byte("v")})
	require.Error(t, err)
	assert.Equal(t, codes.PermissionDenied, status.Code(err))
}

func TestService_Scan(t *testing.T) {
	c := newTestServer(t, fullScope())
	ctx := context.Background()
	for _, k := range []string{"a", "b", "c"} {
		_, err := c.Put(ctx, &kvv1.PutRequest{Namespace: "scratchpad", Key: k, Value: []byte(k)})
		require.NoError(t, err)
	}
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	st, err := c.Scan(ctx, &kvv1.ScanRequest{Namespace: "scratchpad", IncludeValues: true})
	require.NoError(t, err)
	var keys []string
	for {
		item, err := st.Recv()
		if err == io.EOF {
			break
		}
		require.NoError(t, err)
		keys = append(keys, item.GetKey())
	}
	assert.Equal(t, []string{"a", "b", "c"}, keys)
}

func TestService_NamespaceNotConfigured(t *testing.T) {
	cap := &auth.Capability{Scope: auth.Scope{Tenant: "acme", NamespacePattern: []string{"kv/missing"}, AllowedOps: []string{"*"}}}
	c := newTestServer(t, cap)
	_, err := c.Get(context.Background(), &kvv1.GetRequest{Namespace: "missing", Key: "k"})
	require.Error(t, err)
	assert.Equal(t, codes.NotFound, status.Code(err))
}
