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
	t.Cleanup(func() { _ = reg.Close() })
	return newTestServerOn(t, reg, cap, false)
}

// newTestServerOn serves the given registry with a fixed capability and
// isolation setting, so multiple servers can share one backing store to test
// cross-tenant behavior.
func newTestServerOn(t *testing.T, reg *kv.Registry, cap *auth.Capability, isolate bool) kvv1.KVClient {
	return newTestServerOnQ(t, reg, cap, isolate, nil)
}

// newTestServerOnQ is newTestServerOn with an explicit per-namespace quota map.
func newTestServerOnQ(t *testing.T, reg *kv.Registry, cap *auth.Capability, isolate bool, quotas map[string]int) kvv1.KVClient {
	t.Helper()

	svc := kv.NewService(reg, isolate, quotas)

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
	t.Cleanup(func() { srv.GracefulStop() }) // the registry is owned by the caller

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

func tenantScope(tenant string) *auth.Capability {
	return &auth.Capability{Scope: auth.Scope{
		Tenant: tenant, NamespacePattern: []string{"kv/scratchpad"}, AllowedOps: []string{"*"},
	}}
}

// sharedStore serves the same registry to two tenants at the given isolation
// setting, returning a client for each.
func sharedStore(t *testing.T, isolate bool) (acme, beta kvv1.KVClient) {
	t.Helper()
	reg := kv.NewRegistry()
	require.NoError(t, reg.Bind("scratchpad", memdrv.New(memdrv.Options{SweeperInterval: -1})))
	t.Cleanup(func() { _ = reg.Close() })
	return newTestServerOn(t, reg, tenantScope("acme"), isolate),
		newTestServerOn(t, reg, tenantScope("beta"), isolate)
}

// With isolation ON, two tenants writing the same namespace/key must not see or
// clobber each other's data, even though they share the backing store.
func TestService_TenantIsolation(t *testing.T) {
	acme, beta := sharedStore(t, true)
	ctx := context.Background()

	_, err := acme.Put(ctx, &kvv1.PutRequest{Namespace: "scratchpad", Key: "k", Value: []byte("acme-val")})
	require.NoError(t, err)
	_, err = beta.Put(ctx, &kvv1.PutRequest{Namespace: "scratchpad", Key: "k", Value: []byte("beta-val")})
	require.NoError(t, err)

	// Each reads only its own value.
	g, err := acme.Get(ctx, &kvv1.GetRequest{Namespace: "scratchpad", Key: "k"})
	require.NoError(t, err)
	assert.Equal(t, []byte("acme-val"), g.GetValue())
	g, err = beta.Get(ctx, &kvv1.GetRequest{Namespace: "scratchpad", Key: "k"})
	require.NoError(t, err)
	assert.Equal(t, []byte("beta-val"), g.GetValue())

	// acme's scan sees only acme's keys.
	_, err = acme.Put(ctx, &kvv1.PutRequest{Namespace: "scratchpad", Key: "acme-only", Value: []byte("x")})
	require.NoError(t, err)
	assert.Equal(t, []string{"acme-only", "k"}, scanKeys(t, acme))
	assert.Equal(t, []string{"k"}, scanKeys(t, beta))

	// acme deleting its key leaves beta's untouched.
	del, err := acme.Delete(ctx, &kvv1.DeleteRequest{Namespace: "scratchpad", Key: "k"})
	require.NoError(t, err)
	assert.True(t, del.GetExisted())
	g, err = beta.Get(ctx, &kvv1.GetRequest{Namespace: "scratchpad", Key: "k"})
	require.NoError(t, err)
	assert.True(t, g.GetFound())
	assert.Equal(t, []byte("beta-val"), g.GetValue())
}

// With isolation OFF (the default), the namespace is shared — behavior is
// unchanged from before tenant isolation existed (last write wins).
func TestService_NoIsolationShares(t *testing.T) {
	acme, beta := sharedStore(t, false)
	ctx := context.Background()

	_, err := acme.Put(ctx, &kvv1.PutRequest{Namespace: "scratchpad", Key: "k", Value: []byte("acme-val")})
	require.NoError(t, err)
	_, err = beta.Put(ctx, &kvv1.PutRequest{Namespace: "scratchpad", Key: "k", Value: []byte("beta-val")})
	require.NoError(t, err)

	g, err := acme.Get(ctx, &kvv1.GetRequest{Namespace: "scratchpad", Key: "k"})
	require.NoError(t, err)
	assert.Equal(t, []byte("beta-val"), g.GetValue()) // shared: last write wins
}

// A namespace with max_items caps its live keys: a new key past the cap is
// rejected with ResourceExhausted, overwrites are always allowed, and deleting
// a key frees a slot.
func TestService_Quota(t *testing.T) {
	reg := kv.NewRegistry()
	require.NoError(t, reg.Bind("scratchpad", memdrv.New(memdrv.Options{SweeperInterval: -1})))
	t.Cleanup(func() { _ = reg.Close() })
	c := newTestServerOnQ(t, reg, fullScope(), false, map[string]int{"scratchpad": 2})
	ctx := context.Background()

	_, err := c.Put(ctx, &kvv1.PutRequest{Namespace: "scratchpad", Key: "a", Value: []byte("1")})
	require.NoError(t, err)
	_, err = c.Put(ctx, &kvv1.PutRequest{Namespace: "scratchpad", Key: "b", Value: []byte("2")})
	require.NoError(t, err)

	// A third new key is over the cap.
	_, err = c.Put(ctx, &kvv1.PutRequest{Namespace: "scratchpad", Key: "c", Value: []byte("3")})
	require.Error(t, err)
	assert.Equal(t, codes.ResourceExhausted, status.Code(err))

	// Overwriting an existing key never grows the count, so it's allowed.
	_, err = c.Put(ctx, &kvv1.PutRequest{Namespace: "scratchpad", Key: "a", Value: []byte("1b")})
	require.NoError(t, err)

	// Deleting frees a slot for a new key.
	_, err = c.Delete(ctx, &kvv1.DeleteRequest{Namespace: "scratchpad", Key: "b"})
	require.NoError(t, err)
	_, err = c.Put(ctx, &kvv1.PutRequest{Namespace: "scratchpad", Key: "c", Value: []byte("3")})
	require.NoError(t, err)
}

// With tenant isolation on, max_items is enforced against the tenant-qualified
// storage namespace, so each tenant gets its own quota over the shared store.
func TestService_QuotaPerTenant(t *testing.T) {
	reg := kv.NewRegistry()
	require.NoError(t, reg.Bind("scratchpad", memdrv.New(memdrv.Options{SweeperInterval: -1})))
	t.Cleanup(func() { _ = reg.Close() })
	q := map[string]int{"scratchpad": 2}
	acme := newTestServerOnQ(t, reg, tenantScope("acme"), true, q)
	beta := newTestServerOnQ(t, reg, tenantScope("beta"), true, q)
	ctx := context.Background()

	// acme fills its own quota, then is capped.
	_, err := acme.Put(ctx, &kvv1.PutRequest{Namespace: "scratchpad", Key: "a", Value: []byte("1")})
	require.NoError(t, err)
	_, err = acme.Put(ctx, &kvv1.PutRequest{Namespace: "scratchpad", Key: "b", Value: []byte("2")})
	require.NoError(t, err)
	_, err = acme.Put(ctx, &kvv1.PutRequest{Namespace: "scratchpad", Key: "c", Value: []byte("3")})
	assert.Equal(t, codes.ResourceExhausted, status.Code(err))

	// beta's quota is independent — the same key names still fit.
	_, err = beta.Put(ctx, &kvv1.PutRequest{Namespace: "scratchpad", Key: "a", Value: []byte("1")})
	require.NoError(t, err)
	_, err = beta.Put(ctx, &kvv1.PutRequest{Namespace: "scratchpad", Key: "b", Value: []byte("2")})
	require.NoError(t, err)
	_, err = beta.Put(ctx, &kvv1.PutRequest{Namespace: "scratchpad", Key: "c", Value: []byte("3")})
	assert.Equal(t, codes.ResourceExhausted, status.Code(err))
}

func scanKeys(t *testing.T, c kvv1.KVClient) []string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	st, err := c.Scan(ctx, &kvv1.ScanRequest{Namespace: "scratchpad"})
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
	return keys
}
