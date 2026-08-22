package interceptor_test

import (
	"context"
	"net"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/health"
	healthpb "google.golang.org/grpc/health/grpc_health_v1"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"google.golang.org/grpc/test/bufconn"

	kvv1 "github.com/vibed-project/mindD/gen/mindd/kv/v1"
	"github.com/vibed-project/mindD/internal/auth"
	"github.com/vibed-project/mindD/internal/interceptor"
)

type fakeVerifier struct {
	cap *auth.Capability
	err error
}

func (f *fakeVerifier) Verify(_ context.Context, token string) (*auth.Capability, error) {
	if f.err != nil {
		return nil, f.err
	}
	return f.cap, nil
}

type echoKV struct {
	kvv1.UnimplementedKVServer
	gotCap *auth.Capability
}

func (e *echoKV) Get(ctx context.Context, _ *kvv1.GetRequest) (*kvv1.GetResponse, error) {
	e.gotCap, _ = auth.FromContext(ctx)
	return &kvv1.GetResponse{Found: false}, nil
}

func newServer(t *testing.T, v auth.TokenVerifier) (kvv1.KVClient, healthpb.HealthClient, *echoKV) {
	t.Helper()
	echo := &echoKV{}
	srv := grpc.NewServer(
		grpc.UnaryInterceptor(interceptor.AuthUnary(v)),
		grpc.StreamInterceptor(interceptor.AuthStream(v)),
	)
	kvv1.RegisterKVServer(srv, echo)
	healthpb.RegisterHealthServer(srv, health.NewServer())

	lis := bufconn.Listen(1 << 16)
	go func() { _ = srv.Serve(lis) }()
	t.Cleanup(srv.Stop)

	conn, err := grpc.NewClient("passthrough:bufnet",
		grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) { return lis.DialContext(ctx) }),
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	)
	require.NoError(t, err)
	t.Cleanup(func() { _ = conn.Close() })
	return kvv1.NewKVClient(conn), healthpb.NewHealthClient(conn), echo
}

func TestAuth_Missing(t *testing.T) {
	c, _, _ := newServer(t, &fakeVerifier{cap: &auth.Capability{}})
	_, err := c.Get(context.Background(), &kvv1.GetRequest{Namespace: "ns", Key: "k"})
	require.Error(t, err)
	assert.Equal(t, codes.Unauthenticated, status.Code(err))
}

func TestAuth_Invalid(t *testing.T) {
	c, _, _ := newServer(t, &fakeVerifier{err: assertNoError("bad token")})
	ctx := metadata.AppendToOutgoingContext(context.Background(), interceptor.CapabilityHeader, "Bearer abc")
	_, err := c.Get(ctx, &kvv1.GetRequest{Namespace: "ns", Key: "k"})
	require.Error(t, err)
	assert.Equal(t, codes.Unauthenticated, status.Code(err))
}

func TestAuth_ValidBearer(t *testing.T) {
	want := &auth.Capability{Scope: auth.Scope{Tenant: "acme"}}
	c, _, echo := newServer(t, &fakeVerifier{cap: want})
	ctx := metadata.AppendToOutgoingContext(context.Background(), interceptor.CapabilityHeader, "Bearer xxxxx")
	_, err := c.Get(ctx, &kvv1.GetRequest{Namespace: "ns", Key: "k"})
	require.NoError(t, err)
	require.NotNil(t, echo.gotCap)
	assert.Equal(t, "acme", echo.gotCap.Scope.Tenant)
}

func TestAuth_NoBearerPrefix(t *testing.T) {
	want := &auth.Capability{Scope: auth.Scope{Tenant: "acme"}}
	c, _, echo := newServer(t, &fakeVerifier{cap: want})
	ctx := metadata.AppendToOutgoingContext(context.Background(), interceptor.CapabilityHeader, "rawtoken")
	_, err := c.Get(ctx, &kvv1.GetRequest{Namespace: "ns", Key: "k"})
	require.NoError(t, err)
	assert.Equal(t, "acme", echo.gotCap.Scope.Tenant)
}

func TestAuth_HealthExempt(t *testing.T) {
	_, h, _ := newServer(t, &fakeVerifier{err: assertNoError("ignored")})
	resp, err := h.Check(context.Background(), &healthpb.HealthCheckRequest{})
	require.NoError(t, err)
	assert.Equal(t, healthpb.HealthCheckResponse_SERVING, resp.GetStatus())
}

// assertNoError builds a non-nil error for tests where the verifier should
// reject. Naming is intentional: makes the test reader notice that this is the
// "verifier returns error" arm.
func assertNoError(msg string) error { return &simpleErr{msg} }

type simpleErr struct{ s string }

func (e *simpleErr) Error() string { return e.s }
