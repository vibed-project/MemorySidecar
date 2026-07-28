package artifact_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"net"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
	"google.golang.org/grpc/test/bufconn"

	artifactv1 "memsidecar/gen/memsidecar/artifact/v1"
	"memsidecar/internal/artifact"
	memdrv "memsidecar/internal/artifact/drivers/memory"
	"memsidecar/internal/auth"
)

func newTestServer(t *testing.T, cap *auth.Capability) artifactv1.ArtifactClient {
	t.Helper()
	reg := artifact.NewRegistry()
	require.NoError(t, reg.Bind("blobs", memdrv.New(memdrv.Options{})))

	svc := artifact.NewService(reg, false)

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
	artifactv1.RegisterArtifactServer(srv, svc)

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
	return artifactv1.NewArtifactClient(conn)
}

type wrappedStream struct {
	grpc.ServerStream
	ctx context.Context
}

func (w *wrappedStream) Context() context.Context { return w.ctx }

func fullScope() *auth.Capability {
	return &auth.Capability{Scope: auth.Scope{
		Tenant:           "acme",
		NamespacePattern: []string{"artifact/blobs"},
		AllowedOps:       []string{"*"},
	}}
}

func TestService_Put_Get_Round(t *testing.T) {
	c := newTestServer(t, fullScope())
	ctx := context.Background()

	stream, err := c.Put(ctx)
	require.NoError(t, err)
	require.NoError(t, stream.Send(&artifactv1.PutRequest{
		Body: &artifactv1.PutRequest_Init{Init: &artifactv1.PutInit{
			Namespace: "blobs", Id: "obj1", ContentType: "text/plain",
			Metadata: map[string]string{"k": "v"},
		}},
	}))
	payload := []byte("hello world")
	require.NoError(t, stream.Send(&artifactv1.PutRequest{
		Body: &artifactv1.PutRequest_Chunk{Chunk: &artifactv1.PutChunk{Data: payload[:5]}},
	}))
	require.NoError(t, stream.Send(&artifactv1.PutRequest{
		Body: &artifactv1.PutRequest_Chunk{Chunk: &artifactv1.PutChunk{Data: payload[5:]}},
	}))
	putResp, err := stream.CloseAndRecv()
	require.NoError(t, err)
	assert.Equal(t, "obj1", putResp.GetRef().GetId())
	assert.Equal(t, uint64(11), putResp.GetRef().GetSize())
	wantHash := sha256.Sum256(payload)
	assert.Equal(t, hex.EncodeToString(wantHash[:]), putResp.GetRef().GetSha256())

	// Get back the bytes.
	gstream, err := c.Get(ctx, &artifactv1.GetRequest{Namespace: "blobs", Id: "obj1"})
	require.NoError(t, err)

	first, err := gstream.Recv()
	require.NoError(t, err)
	header := first.GetHeader()
	require.NotNil(t, header)
	assert.Equal(t, uint64(11), header.GetMeta().GetSize())
	assert.Equal(t, hex.EncodeToString(wantHash[:]), header.GetMeta().GetSha256())

	var got bytes.Buffer
	for {
		msg, err := gstream.Recv()
		if err == io.EOF {
			break
		}
		require.NoError(t, err)
		got.Write(msg.GetChunk().GetData())
	}
	assert.Equal(t, payload, got.Bytes())
}

func TestService_Put_AssignsID(t *testing.T) {
	c := newTestServer(t, fullScope())
	stream, _ := c.Put(context.Background())
	_ = stream.Send(&artifactv1.PutRequest{Body: &artifactv1.PutRequest_Init{Init: &artifactv1.PutInit{Namespace: "blobs"}}})
	_ = stream.Send(&artifactv1.PutRequest{Body: &artifactv1.PutRequest_Chunk{Chunk: &artifactv1.PutChunk{Data: []byte("x")}}})
	resp, err := stream.CloseAndRecv()
	require.NoError(t, err)
	assert.NotEmpty(t, resp.GetRef().GetId())
}

func TestService_Stat_Delete(t *testing.T) {
	c := newTestServer(t, fullScope())
	ctx := context.Background()
	stream, _ := c.Put(ctx)
	_ = stream.Send(&artifactv1.PutRequest{Body: &artifactv1.PutRequest_Init{Init: &artifactv1.PutInit{Namespace: "blobs", Id: "a"}}})
	_ = stream.Send(&artifactv1.PutRequest{Body: &artifactv1.PutRequest_Chunk{Chunk: &artifactv1.PutChunk{Data: []byte("hi")}}})
	_, _ = stream.CloseAndRecv()

	stat, err := c.Stat(ctx, &artifactv1.StatRequest{Namespace: "blobs", Id: "a"})
	require.NoError(t, err)
	assert.True(t, stat.GetFound())
	assert.Equal(t, uint64(2), stat.GetMeta().GetSize())

	del, err := c.Delete(ctx, &artifactv1.DeleteRequest{Namespace: "blobs", Id: "a"})
	require.NoError(t, err)
	assert.True(t, del.GetExisted())

	stat, _ = c.Stat(ctx, &artifactv1.StatRequest{Namespace: "blobs", Id: "a"})
	assert.False(t, stat.GetFound())
}

func TestService_Put_MissingCapability(t *testing.T) {
	c := newTestServer(t, nil)
	stream, _ := c.Put(context.Background())
	_ = stream.Send(&artifactv1.PutRequest{Body: &artifactv1.PutRequest_Init{Init: &artifactv1.PutInit{Namespace: "blobs"}}})
	_, err := stream.CloseAndRecv()
	require.Error(t, err)
	assert.Equal(t, codes.Unauthenticated, status.Code(err))
}

func TestService_Put_OpNotAllowed(t *testing.T) {
	cap := &auth.Capability{Scope: auth.Scope{
		Tenant: "acme", NamespacePattern: []string{"artifact/blobs"},
		AllowedOps: []string{"artifact.get"},
	}}
	c := newTestServer(t, cap)
	stream, _ := c.Put(context.Background())
	_ = stream.Send(&artifactv1.PutRequest{Body: &artifactv1.PutRequest_Init{Init: &artifactv1.PutInit{Namespace: "blobs"}}})
	_, err := stream.CloseAndRecv()
	require.Error(t, err)
	assert.Equal(t, codes.PermissionDenied, status.Code(err))
}

func TestService_Get_NotFound(t *testing.T) {
	c := newTestServer(t, fullScope())
	gstream, err := c.Get(context.Background(), &artifactv1.GetRequest{Namespace: "blobs", Id: "missing"})
	require.NoError(t, err)
	_, err = gstream.Recv()
	require.Error(t, err)
	assert.Equal(t, codes.NotFound, status.Code(err))
}

func TestService_Get_RangeRead(t *testing.T) {
	c := newTestServer(t, fullScope())
	ctx := context.Background()
	stream, _ := c.Put(ctx)
	_ = stream.Send(&artifactv1.PutRequest{Body: &artifactv1.PutRequest_Init{Init: &artifactv1.PutInit{Namespace: "blobs", Id: "r"}}})
	_ = stream.Send(&artifactv1.PutRequest{Body: &artifactv1.PutRequest_Chunk{Chunk: &artifactv1.PutChunk{Data: []byte("abcdefghij")}}})
	_, _ = stream.CloseAndRecv()

	gstream, _ := c.Get(ctx, &artifactv1.GetRequest{Namespace: "blobs", Id: "r", Offset: 3, Length: 4})
	_, _ = gstream.Recv() // header
	var got bytes.Buffer
	for {
		msg, err := gstream.Recv()
		if err == io.EOF {
			break
		}
		require.NoError(t, err)
		got.Write(msg.GetChunk().GetData())
	}
	assert.Equal(t, "defg", got.String())
}
