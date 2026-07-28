package lease_test

import (
	"context"
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
	"google.golang.org/protobuf/types/known/durationpb"

	leasev1 "memsidecar/gen/memsidecar/lease/v1"
	"memsidecar/internal/auth"
	"memsidecar/internal/lease"
	memdrv "memsidecar/internal/lease/drivers/memory"
)

func newTestServer(t *testing.T, cap *auth.Capability) leasev1.LeaseClient {
	t.Helper()
	reg := lease.NewRegistry()
	require.NoError(t, reg.Bind("locks", memdrv.New(memdrv.Options{})))

	svc := lease.NewService(reg, false)
	inject := func(ctx context.Context) context.Context {
		if cap == nil {
			return ctx
		}
		return auth.WithCapability(ctx, cap)
	}
	srv := grpc.NewServer(
		grpc.UnaryInterceptor(func(ctx context.Context, req any, _ *grpc.UnaryServerInfo, h grpc.UnaryHandler) (any, error) {
			return h(inject(ctx), req)
		}),
	)
	leasev1.RegisterLeaseServer(srv, svc)
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
	return leasev1.NewLeaseClient(conn)
}

func fullScope() *auth.Capability {
	return &auth.Capability{Scope: auth.Scope{
		Tenant: "acme", NamespacePattern: []string{"lease/locks"}, AllowedOps: []string{"*"},
	}}
}

func TestService_AcquireRenewRelease(t *testing.T) {
	c := newTestServer(t, fullScope())
	ctx := context.Background()

	acq, err := c.Acquire(ctx, &leasev1.AcquireRequest{
		Namespace: "locks", Key: "deploy",
		Ttl: durationpb.New(time.Minute),
	})
	require.NoError(t, err)
	holder := acq.GetHandle().GetHolderId()
	assert.NotEmpty(t, holder)

	insp, err := c.Inspect(ctx, &leasev1.InspectRequest{Namespace: "locks", Key: "deploy"})
	require.NoError(t, err)
	assert.True(t, insp.GetHeld())
	assert.Equal(t, holder, insp.GetHandle().GetHolderId())

	_, err = c.Renew(ctx, &leasev1.RenewRequest{
		Namespace: "locks", Key: "deploy", HolderId: holder, Ttl: durationpb.New(2 * time.Minute),
	})
	require.NoError(t, err)

	rel, err := c.Release(ctx, &leasev1.ReleaseRequest{
		Namespace: "locks", Key: "deploy", HolderId: holder,
	})
	require.NoError(t, err)
	assert.True(t, rel.GetExisted())

	insp, _ = c.Inspect(ctx, &leasev1.InspectRequest{Namespace: "locks", Key: "deploy"})
	assert.False(t, insp.GetHeld())
}

func TestService_AcquireConflict(t *testing.T) {
	c := newTestServer(t, fullScope())
	_, err := c.Acquire(context.Background(), &leasev1.AcquireRequest{
		Namespace: "locks", Key: "x", Ttl: durationpb.New(time.Minute),
	})
	require.NoError(t, err)

	_, err = c.Acquire(context.Background(), &leasev1.AcquireRequest{
		Namespace: "locks", Key: "x", Ttl: durationpb.New(time.Minute),
	})
	require.Error(t, err)
	assert.Equal(t, codes.FailedPrecondition, status.Code(err))
}

func TestService_RenewWrongHolder(t *testing.T) {
	c := newTestServer(t, fullScope())
	_, err := c.Acquire(context.Background(), &leasev1.AcquireRequest{
		Namespace: "locks", Key: "x", Ttl: durationpb.New(time.Minute),
	})
	require.NoError(t, err)
	_, err = c.Renew(context.Background(), &leasev1.RenewRequest{
		Namespace: "locks", Key: "x", HolderId: "imposter", Ttl: durationpb.New(time.Minute),
	})
	require.Error(t, err)
	assert.Equal(t, codes.FailedPrecondition, status.Code(err))
}

func TestService_MissingCapability(t *testing.T) {
	c := newTestServer(t, nil)
	_, err := c.Acquire(context.Background(), &leasev1.AcquireRequest{
		Namespace: "locks", Key: "x", Ttl: durationpb.New(time.Minute),
	})
	require.Error(t, err)
	assert.Equal(t, codes.Unauthenticated, status.Code(err))
}

func TestService_OpNotAllowed(t *testing.T) {
	cap := &auth.Capability{Scope: auth.Scope{
		Tenant: "acme", NamespacePattern: []string{"lease/locks"},
		AllowedOps: []string{"lease.inspect"},
	}}
	c := newTestServer(t, cap)
	_, err := c.Acquire(context.Background(), &leasev1.AcquireRequest{
		Namespace: "locks", Key: "x", Ttl: durationpb.New(time.Minute),
	})
	require.Error(t, err)
	assert.Equal(t, codes.PermissionDenied, status.Code(err))
}

func TestService_TTLRequired(t *testing.T) {
	c := newTestServer(t, fullScope())
	_, err := c.Acquire(context.Background(), &leasev1.AcquireRequest{
		Namespace: "locks", Key: "x",
	})
	require.Error(t, err)
	assert.Equal(t, codes.InvalidArgument, status.Code(err))
}
