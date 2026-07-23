package graph_test

import (
	"context"
	"net"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
	"google.golang.org/grpc/test/bufconn"

	graphv1 "memsidecar/gen/memsidecar/graph/v1"
	"memsidecar/internal/auth"
	"memsidecar/internal/graph"
	memdrv "memsidecar/internal/graph/drivers/memory"
)

func newTestServer(t *testing.T, cap *auth.Capability) graphv1.GraphClient {
	t.Helper()
	reg := graph.NewRegistry()
	require.NoError(t, reg.Bind("knowledge", memdrv.New(memdrv.Options{})))
	svc := graph.NewService(reg, false)

	inject := func(ctx context.Context) context.Context {
		if cap == nil {
			return ctx
		}
		return auth.WithCapability(ctx, cap)
	}
	srv := grpc.NewServer(grpc.UnaryInterceptor(
		func(ctx context.Context, req any, _ *grpc.UnaryServerInfo, h grpc.UnaryHandler) (any, error) {
			return h(inject(ctx), req)
		},
	))
	graphv1.RegisterGraphServer(srv, svc)

	lis := bufconn.Listen(1 << 16)
	go func() { _ = srv.Serve(lis) }()
	t.Cleanup(func() {
		srv.GracefulStop()
		_ = reg.Close()
	})

	conn, err := grpc.NewClient(
		"passthrough:bufnet",
		grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) { return lis.DialContext(ctx) }),
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	)
	require.NoError(t, err)
	t.Cleanup(func() { _ = conn.Close() })
	return graphv1.NewGraphClient(conn)
}

func fullScope() *auth.Capability {
	return &auth.Capability{Scope: auth.Scope{
		Tenant:           "acme",
		NamespacePattern: []string{"graph/knowledge"},
		AllowedOps:       []string{"*"},
	}}
}

func TestGraph_UpsertTraverseDelete(t *testing.T) {
	c := newTestServer(t, fullScope())
	ctx := context.Background()

	_, err := c.UpsertNodes(ctx, &graphv1.UpsertNodesRequest{Namespace: "knowledge", Nodes: []*graphv1.Node{
		{Id: "a", Labels: []string{"Person"}},
		{Id: "b", Labels: []string{"Person"}},
	}})
	require.NoError(t, err)
	_, err = c.UpsertEdges(ctx, &graphv1.UpsertEdgesRequest{Namespace: "knowledge", Edges: []*graphv1.Edge{
		{Id: "e1", Type: "KNOWS", From: "a", To: "b"},
	}})
	require.NoError(t, err)

	n, err := c.GetNode(ctx, &graphv1.GetNodeRequest{Namespace: "knowledge", Id: "a"})
	require.NoError(t, err)
	assert.Equal(t, "a", n.GetId())

	sub, err := c.Traverse(ctx, &graphv1.TraverseRequest{
		Namespace: "knowledge", StartId: "a", Direction: graphv1.Direction_DIRECTION_OUT, Depth: 2, MaxNodes: 100,
	})
	require.NoError(t, err)
	assert.Len(t, sub.GetNodes(), 2)

	del, err := c.DeleteNode(ctx, &graphv1.DeleteNodeRequest{Namespace: "knowledge", Id: "a", Cascade: true})
	require.NoError(t, err)
	assert.True(t, del.GetExisted())
}

func TestGraph_InvalidArgs(t *testing.T) {
	c := newTestServer(t, fullScope())
	ctx := context.Background()

	_, err := c.UpsertNodes(ctx, &graphv1.UpsertNodesRequest{Namespace: "knowledge", Nodes: []*graphv1.Node{{Id: ""}}})
	assert.Equal(t, codes.InvalidArgument, status.Code(err))

	_, err = c.UpsertEdges(ctx, &graphv1.UpsertEdgesRequest{Namespace: "knowledge", Edges: []*graphv1.Edge{
		{Id: "e", Type: "KNOWS", From: "a"}, // missing To
	}})
	assert.Equal(t, codes.InvalidArgument, status.Code(err))
}

func TestGraph_DepthClampedServerSide(t *testing.T) {
	c := newTestServer(t, fullScope())
	ctx := context.Background()

	// Chain n0 -> n1 -> ... -> n7 (8 nodes).
	nodes := make([]*graphv1.Node, 8)
	edges := make([]*graphv1.Edge, 7)
	for i := 0; i < 8; i++ {
		nodes[i] = &graphv1.Node{Id: id(i)}
	}
	for i := 0; i < 7; i++ {
		edges[i] = &graphv1.Edge{Id: "e" + id(i), Type: "NEXT", From: id(i), To: id(i + 1)}
	}
	_, err := c.UpsertNodes(ctx, &graphv1.UpsertNodesRequest{Namespace: "knowledge", Nodes: nodes})
	require.NoError(t, err)
	_, err = c.UpsertEdges(ctx, &graphv1.UpsertEdgesRequest{Namespace: "knowledge", Edges: edges})
	require.NoError(t, err)

	// Requesting depth 100 is clamped to the server max (5), so only n0..n5.
	sub, err := c.Traverse(ctx, &graphv1.TraverseRequest{
		Namespace: "knowledge", StartId: "n0", Direction: graphv1.Direction_DIRECTION_OUT, Depth: 100, MaxNodes: 1000,
	})
	require.NoError(t, err)
	assert.Len(t, sub.GetNodes(), 6, "depth clamped to 5 hops → 6 nodes")
}

func TestGraph_AuthDenied(t *testing.T) {
	// Capability scoped to a different namespace.
	cap := &auth.Capability{Scope: auth.Scope{
		Tenant: "acme", NamespacePattern: []string{"graph/other"}, AllowedOps: []string{"*"},
	}}
	c := newTestServer(t, cap)
	_, err := c.GetNode(context.Background(), &graphv1.GetNodeRequest{Namespace: "knowledge", Id: "a"})
	assert.Equal(t, codes.PermissionDenied, status.Code(err))
}

func id(i int) string { return "n" + string(rune('0'+i)) }
