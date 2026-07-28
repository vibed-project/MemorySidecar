package episodic_test

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

	episodicv1 "memsidecar/gen/memsidecar/episodic/v1"
	"memsidecar/internal/auth"
	"memsidecar/internal/episodic"
	memdrv "memsidecar/internal/episodic/drivers/memory"
)

func newTestServer(t *testing.T, cap *auth.Capability) episodicv1.EpisodicClient {
	t.Helper()
	reg := episodic.NewRegistry()
	require.NoError(t, reg.Bind("events", memdrv.New(memdrv.Options{})))

	svc := episodic.NewService(reg, false)

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
		grpc.StreamInterceptor(func(srv any, ss grpc.ServerStream, _ *grpc.StreamServerInfo, h grpc.StreamHandler) error {
			return h(srv, &wrappedStream{ServerStream: ss, ctx: inject(ss.Context())})
		}),
	)
	episodicv1.RegisterEpisodicServer(srv, svc)

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

	return episodicv1.NewEpisodicClient(conn)
}

type wrappedStream struct {
	grpc.ServerStream
	ctx context.Context
}

func (w *wrappedStream) Context() context.Context { return w.ctx }

func fullScope() *auth.Capability {
	return &auth.Capability{Scope: auth.Scope{
		Tenant:           "acme",
		NamespacePattern: []string{"episodic/events"},
		AllowedOps:       []string{"*"},
	}}
}

func TestService_AppendRange(t *testing.T) {
	c := newTestServer(t, fullScope())
	ctx := context.Background()

	for i := 0; i < 3; i++ {
		_, err := c.Append(ctx, &episodicv1.AppendRequest{
			Namespace: "events", Type: "tool_call", Payload: []byte("x"),
		})
		require.NoError(t, err)
	}

	st, err := c.Range(ctx, &episodicv1.RangeRequest{Namespace: "events"})
	require.NoError(t, err)
	var cursors []uint64
	for {
		ev, err := st.Recv()
		if err == io.EOF {
			break
		}
		require.NoError(t, err)
		cursors = append(cursors, ev.GetCursor())
	}
	assert.Equal(t, []uint64{1, 2, 3}, cursors)
}

func TestService_TailHistoricalThenLive(t *testing.T) {
	c := newTestServer(t, fullScope())
	ctx := context.Background()
	for i := 0; i < 2; i++ {
		_, err := c.Append(ctx, &episodicv1.AppendRequest{Namespace: "events", Type: "h", Payload: []byte("h")})
		require.NoError(t, err)
	}

	tctx, cancel := context.WithCancel(ctx)
	defer cancel()
	st, err := c.Tail(tctx, &episodicv1.TailRequest{Namespace: "events", IncludeHistorical: true})
	require.NoError(t, err)

	// Receive historical 1, 2.
	for _, want := range []uint64{1, 2} {
		ev, err := st.Recv()
		require.NoError(t, err)
		assert.Equal(t, want, ev.GetCursor())
	}

	// Produce one more, expect to receive it.
	time.Sleep(30 * time.Millisecond)
	_, err = c.Append(ctx, &episodicv1.AppendRequest{Namespace: "events", Type: "live", Payload: []byte("l")})
	require.NoError(t, err)

	ev, err := st.Recv()
	require.NoError(t, err)
	assert.Equal(t, uint64(3), ev.GetCursor())
}

func TestService_MissingCapability(t *testing.T) {
	c := newTestServer(t, nil)
	_, err := c.Append(context.Background(), &episodicv1.AppendRequest{Namespace: "events", Type: "x"})
	require.Error(t, err)
	assert.Equal(t, codes.Unauthenticated, status.Code(err))
}

func TestService_OpNotAllowed(t *testing.T) {
	cap := &auth.Capability{Scope: auth.Scope{
		Tenant:           "acme",
		NamespacePattern: []string{"episodic/events"},
		AllowedOps:       []string{"episodic.range"},
	}}
	c := newTestServer(t, cap)
	_, err := c.Append(context.Background(), &episodicv1.AppendRequest{Namespace: "events", Type: "x"})
	require.Error(t, err)
	assert.Equal(t, codes.PermissionDenied, status.Code(err))
}

func TestService_AppendRequiresType(t *testing.T) {
	c := newTestServer(t, fullScope())
	_, err := c.Append(context.Background(), &episodicv1.AppendRequest{Namespace: "events"})
	require.Error(t, err)
	assert.Equal(t, codes.InvalidArgument, status.Code(err))
}
