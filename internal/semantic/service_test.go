package semantic_test

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

	semanticv1 "memsidecar/gen/memsidecar/semantic/v1"
	"memsidecar/internal/auth"
	"memsidecar/internal/semantic"
	memdrv "memsidecar/internal/semantic/drivers/memory"
	"memsidecar/internal/semantic/embedder"
)

const testDim = 64

func newTestServer(t *testing.T, cap *auth.Capability) semanticv1.SemanticClient {
	t.Helper()
	emb, err := embedder.NewFake(embedder.FakeOptions{Dimensions: testDim})
	require.NoError(t, err)
	drv, err := memdrv.New(memdrv.Options{Dimensions: testDim})
	require.NoError(t, err)

	reg := semantic.NewRegistry()
	require.NoError(t, reg.Bind("notes", semantic.BoundNamespace{Driver: drv, Embedder: emb}))

	svc := semantic.NewService(reg, false)

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
	semanticv1.RegisterSemanticServer(srv, svc)

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
	return semanticv1.NewSemanticClient(conn)
}

func fullScope() *auth.Capability {
	return &auth.Capability{Scope: auth.Scope{
		Tenant:           "acme",
		NamespacePattern: []string{"semantic/notes"},
		AllowedOps:       []string{"*"},
	}}
}

func TestService_UpsertSearch_TextQuery(t *testing.T) {
	c := newTestServer(t, fullScope())
	ctx := context.Background()

	upsert, err := c.Upsert(ctx, &semanticv1.UpsertRequest{
		Namespace: "notes",
		Records: []*semanticv1.Record{
			{Id: "a", Content: "apple"},
			{Id: "b", Content: "banana"},
			{Content: "cherry"}, // server-assigned id
		},
	})
	require.NoError(t, err)
	require.Len(t, upsert.GetIds(), 3)
	assert.Equal(t, "a", upsert.GetIds()[0])
	assert.Equal(t, "b", upsert.GetIds()[1])
	assert.NotEmpty(t, upsert.GetIds()[2])

	// Querying with the exact same content should put that record at the top
	// with cosine ~ 1.
	res, err := c.Search(ctx, &semanticv1.SearchRequest{
		Namespace: "notes",
		QueryText: "apple",
		TopK:      3,
	})
	require.NoError(t, err)
	require.NotEmpty(t, res.GetHits())
	assert.Equal(t, "a", res.GetHits()[0].GetRecord().GetId())
	assert.InDelta(t, 1.0, res.GetHits()[0].GetScore(), 1e-3)
}

func TestService_Search_VectorQuery(t *testing.T) {
	c := newTestServer(t, fullScope())
	ctx := context.Background()

	// Direct vector record.
	vec := make([]float32, testDim)
	vec[0] = 1
	_, err := c.Upsert(ctx, &semanticv1.UpsertRequest{
		Namespace: "notes",
		Records:   []*semanticv1.Record{{Id: "v1", Vector: vec}},
	})
	require.NoError(t, err)

	res, err := c.Search(ctx, &semanticv1.SearchRequest{
		Namespace:   "notes",
		QueryVector: vec,
		TopK:        1,
	})
	require.NoError(t, err)
	require.Len(t, res.GetHits(), 1)
	assert.Equal(t, "v1", res.GetHits()[0].GetRecord().GetId())
}

func TestService_Search_BothQueriesIsError(t *testing.T) {
	c := newTestServer(t, fullScope())
	vec := make([]float32, testDim)
	vec[0] = 1
	_, err := c.Search(context.Background(), &semanticv1.SearchRequest{
		Namespace: "notes", QueryText: "x", QueryVector: vec,
	})
	require.Error(t, err)
	assert.Equal(t, codes.InvalidArgument, status.Code(err))
}

func TestService_Search_FilterAndIncludes(t *testing.T) {
	c := newTestServer(t, fullScope())
	ctx := context.Background()
	_, err := c.Upsert(ctx, &semanticv1.UpsertRequest{
		Namespace: "notes",
		Records: []*semanticv1.Record{
			{Id: "a", Content: "apple", Metadata: map[string]string{"color": "red"}, Payload: []byte("p")},
			{Id: "b", Content: "apple", Metadata: map[string]string{"color": "green"}, Payload: []byte("q")},
		},
	})
	require.NoError(t, err)

	res, err := c.Search(ctx, &semanticv1.SearchRequest{
		Namespace:      "notes",
		QueryText:      "apple",
		Filter:         map[string]string{"color": "red"},
		IncludePayload: true,
	})
	require.NoError(t, err)
	require.Len(t, res.GetHits(), 1)
	assert.Equal(t, "a", res.GetHits()[0].GetRecord().GetId())
	assert.Equal(t, []byte("p"), res.GetHits()[0].GetRecord().GetPayload())
}

func TestService_Delete(t *testing.T) {
	c := newTestServer(t, fullScope())
	ctx := context.Background()
	_, _ = c.Upsert(ctx, &semanticv1.UpsertRequest{
		Namespace: "notes",
		Records:   []*semanticv1.Record{{Id: "a", Content: "apple"}},
	})
	del, err := c.Delete(ctx, &semanticv1.DeleteRequest{Namespace: "notes", Id: "a"})
	require.NoError(t, err)
	assert.True(t, del.GetExisted())

	del, _ = c.Delete(ctx, &semanticv1.DeleteRequest{Namespace: "notes", Id: "a"})
	assert.False(t, del.GetExisted())
}

func TestService_MissingCapability(t *testing.T) {
	c := newTestServer(t, nil)
	_, err := c.Search(context.Background(), &semanticv1.SearchRequest{Namespace: "notes", QueryText: "x"})
	require.Error(t, err)
	assert.Equal(t, codes.Unauthenticated, status.Code(err))
}

func TestService_OpNotAllowed(t *testing.T) {
	cap := &auth.Capability{Scope: auth.Scope{
		Tenant:           "acme",
		NamespacePattern: []string{"semantic/notes"},
		AllowedOps:       []string{"semantic.search"},
	}}
	c := newTestServer(t, cap)
	_, err := c.Upsert(context.Background(), &semanticv1.UpsertRequest{
		Namespace: "notes",
		Records:   []*semanticv1.Record{{Content: "x"}},
	})
	require.Error(t, err)
	assert.Equal(t, codes.PermissionDenied, status.Code(err))
}

func TestService_Upsert_DimMismatch(t *testing.T) {
	c := newTestServer(t, fullScope())
	_, err := c.Upsert(context.Background(), &semanticv1.UpsertRequest{
		Namespace: "notes",
		Records:   []*semanticv1.Record{{Id: "a", Vector: []float32{1, 2, 3}}},
	})
	require.Error(t, err)
	assert.Equal(t, codes.InvalidArgument, status.Code(err))
}
