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
	"google.golang.org/grpc/status"
	"google.golang.org/grpc/test/bufconn"
	"google.golang.org/protobuf/types/known/durationpb"

	artifactv1 "github.com/vibed-project/mindD/gen/mindd/artifact/v1"
	episodicv1 "github.com/vibed-project/mindD/gen/mindd/episodic/v1"
	kvv1 "github.com/vibed-project/mindD/gen/mindd/kv/v1"
	leasev1 "github.com/vibed-project/mindD/gen/mindd/lease/v1"
	semanticv1 "github.com/vibed-project/mindD/gen/mindd/semantic/v1"
	"github.com/vibed-project/mindD/internal/artifact"
	artifactmem "github.com/vibed-project/mindD/internal/artifact/drivers/memory"
	"github.com/vibed-project/mindD/internal/auth"
	"github.com/vibed-project/mindD/internal/config"
	"github.com/vibed-project/mindD/internal/episodic"
	episodicmem "github.com/vibed-project/mindD/internal/episodic/drivers/memory"
	"github.com/vibed-project/mindD/internal/kv"
	kvmem "github.com/vibed-project/mindD/internal/kv/drivers/memory"
	"github.com/vibed-project/mindD/internal/lease"
	leasemem "github.com/vibed-project/mindD/internal/lease/drivers/memory"
	"github.com/vibed-project/mindD/internal/obs"
	"github.com/vibed-project/mindD/internal/semantic"
	semanticmem "github.com/vibed-project/mindD/internal/semantic/drivers/memory"
	"github.com/vibed-project/mindD/internal/semantic/embedder"
	"github.com/vibed-project/mindD/internal/server"
)

// bufFixture is the full-server fixture for end-to-end tests over bufconn.
// It binds every block to in-memory drivers, builds a real Server with the
// production interceptor chain (recovery → observability → auth → policy),
// and exposes per-block clients plus a token-minting helper that uses the
// server's trusted signer.
type bufFixture struct {
	mintToken func(auth.Scope) string

	KV       kvv1.KVClient
	Episodic episodicv1.EpisodicClient
	Semantic semanticv1.SemanticClient
	Artifact artifactv1.ArtifactClient
	Lease    leasev1.LeaseClient

	// trustedSigner is the PASETO secret-key hex that the server's verifier
	// trusts. mintToken uses it. Tests that need to forge a bad-signer token
	// can mint with auth.IssuePASETO directly against a different secret.
	trustedSigner string
}

func newBufFixture(t *testing.T) *bufFixture {
	t.Helper()

	sec, pub, err := auth.GeneratePASETOKeyPair()
	require.NoError(t, err)

	verifier, err := auth.NewPASETOVerifier(config.PASETOConfig{PublicKeyHex: pub})
	require.NoError(t, err)

	logSetup, err := obs.NewLogger(config.LoggingConfig{Level: "info", Format: "json"})
	require.NoError(t, err)

	// KV: two namespaces so we can exercise namespace-scope denial.
	kvReg := kv.NewRegistry()
	require.NoError(t, kvReg.Bind("scratchpad", kvmem.New(kvmem.Options{SweeperInterval: -1})))
	require.NoError(t, kvReg.Bind("other", kvmem.New(kvmem.Options{SweeperInterval: -1})))

	epReg := episodic.NewRegistry()
	require.NoError(t, epReg.Bind("events", episodicmem.New(episodicmem.Options{})))

	const semDim = 16
	fakeEmb, err := embedder.NewFake(embedder.FakeOptions{Dimensions: semDim})
	require.NoError(t, err)
	semDriver, err := semanticmem.New(semanticmem.Options{Dimensions: semDim})
	require.NoError(t, err)
	semReg := semantic.NewRegistry()
	require.NoError(t, semReg.Bind("notes", semantic.BoundNamespace{Driver: semDriver, Embedder: fakeEmb}))

	artReg := artifact.NewRegistry()
	require.NoError(t, artReg.Bind("blobs", artifactmem.New(artifactmem.Options{})))

	leaseReg := lease.NewRegistry()
	require.NoError(t, leaseReg.Bind("locks", leasemem.New(leasemem.Options{PollInterval: 10 * time.Millisecond})))

	srv, err := server.New(config.ServerConfig{
		ShutdownTimeout: 2 * time.Second,
	}, server.Deps{
		Logger:   logSetup.Logger,
		Verifier: verifier,
		KV:       kvReg,
		Episodic: epReg,
		Semantic: semReg,
		Artifact: artReg,
		Lease:    leaseReg,
	})
	require.NoError(t, err)

	lis := bufconn.Listen(1 << 20)
	serveErrs := srv.ServeListener(lis)
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = srv.Shutdown(ctx)
		<-serveErrs
		_ = kvReg.Close()
		_ = epReg.Close()
		_ = semReg.Close()
		_ = artReg.Close()
		_ = leaseReg.Close()
	})

	conn, err := grpc.NewClient("passthrough:bufnet",
		grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) { return lis.DialContext(ctx) }),
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	)
	require.NoError(t, err)
	t.Cleanup(func() { _ = conn.Close() })

	mint := func(scope auth.Scope) string {
		tok, err := auth.IssuePASETO(sec, scope, time.Minute, "tid")
		require.NoError(t, err)
		return tok
	}

	return &bufFixture{
		mintToken:     mint,
		KV:            kvv1.NewKVClient(conn),
		Episodic:      episodicv1.NewEpisodicClient(conn),
		Semantic:      semanticv1.NewSemanticClient(conn),
		Artifact:      artifactv1.NewArtifactClient(conn),
		Lease:         leasev1.NewLeaseClient(conn),
		trustedSigner: sec,
	}
}

// fullScope grants every op on every namespace. Most happy-path subtests use it.
func fullScope() auth.Scope {
	return auth.Scope{
		Tenant:           "acme",
		Agent:            "agent-1",
		NamespacePattern: []string{"*"},
		AllowedOps:       []string{"*"},
	}
}

// TestE2E_Bufconn_AllBlocks exercises every block through the full server
// stack (real auth + policy interceptors) over a single in-memory connection.
func TestE2E_Bufconn_AllBlocks(t *testing.T) {
	f := newBufFixture(t)
	ctx := withToken(context.Background(), f.mintToken(fullScope()))

	t.Run("KV", func(t *testing.T) {
		_, err := f.KV.Put(ctx, &kvv1.PutRequest{
			Namespace: "scratchpad", Key: "hello", Value: []byte("world"), ContentType: "text/plain",
		})
		require.NoError(t, err)

		got, err := f.KV.Get(ctx, &kvv1.GetRequest{Namespace: "scratchpad", Key: "hello"})
		require.NoError(t, err)
		assert.True(t, got.GetFound())
		assert.Equal(t, []byte("world"), got.GetValue())

		stream, err := f.KV.Scan(ctx, &kvv1.ScanRequest{Namespace: "scratchpad", IncludeValues: true})
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

		del, err := f.KV.Delete(ctx, &kvv1.DeleteRequest{Namespace: "scratchpad", Key: "hello"})
		require.NoError(t, err)
		assert.True(t, del.GetExisted())
	})

	t.Run("Episodic", func(t *testing.T) {
		for i := 0; i < 3; i++ {
			_, err := f.Episodic.Append(ctx, &episodicv1.AppendRequest{
				Namespace: "events", Type: "tool_call", Payload: []byte("p"),
			})
			require.NoError(t, err)
		}
		stream, err := f.Episodic.Range(ctx, &episodicv1.RangeRequest{Namespace: "events"})
		require.NoError(t, err)
		var cursors []uint64
		for {
			ev, err := stream.Recv()
			if err == io.EOF {
				break
			}
			require.NoError(t, err)
			cursors = append(cursors, ev.GetCursor())
		}
		assert.Equal(t, []uint64{1, 2, 3}, cursors)
	})

	t.Run("Semantic", func(t *testing.T) {
		_, err := f.Semantic.Upsert(ctx, &semanticv1.UpsertRequest{
			Namespace: "notes",
			Records: []*semanticv1.Record{
				{Id: "a", Content: "hello world"},
				{Id: "b", Content: "goodbye world"},
			},
		})
		require.NoError(t, err)

		resp, err := f.Semantic.Search(ctx, &semanticv1.SearchRequest{
			Namespace: "notes", QueryText: "hello world", TopK: 2,
		})
		require.NoError(t, err)
		require.NotEmpty(t, resp.GetHits())
		// Deterministic fake embedder → "hello world" embeds identically, so
		// "a" should be the top hit with score very close to 1.0.
		assert.Equal(t, "a", resp.GetHits()[0].GetRecord().GetId())
		assert.InDelta(t, 1.0, resp.GetHits()[0].GetScore(), 1e-4)

		_, err = f.Semantic.Delete(ctx, &semanticv1.DeleteRequest{Namespace: "notes", Id: "a"})
		require.NoError(t, err)
	})

	t.Run("Artifact", func(t *testing.T) {
		putStream, err := f.Artifact.Put(ctx)
		require.NoError(t, err)
		require.NoError(t, putStream.Send(&artifactv1.PutRequest{
			Body: &artifactv1.PutRequest_Init{Init: &artifactv1.PutInit{
				Namespace: "blobs", Id: "obj1", ContentType: "text/plain",
				Metadata: map[string]string{"tag": "v1"},
			}},
		}))
		require.NoError(t, putStream.Send(&artifactv1.PutRequest{
			Body: &artifactv1.PutRequest_Chunk{Chunk: &artifactv1.PutChunk{Data: []byte("hello world")}},
		}))
		putResp, err := putStream.CloseAndRecv()
		require.NoError(t, err)
		assert.Equal(t, "obj1", putResp.GetRef().GetId())
		assert.Equal(t, uint64(11), putResp.GetRef().GetSize())

		stat, err := f.Artifact.Stat(ctx, &artifactv1.StatRequest{Namespace: "blobs", Id: "obj1"})
		require.NoError(t, err)
		assert.True(t, stat.GetFound())
		assert.Equal(t, "v1", stat.GetMeta().GetMetadata()["tag"])

		getStream, err := f.Artifact.Get(ctx, &artifactv1.GetRequest{Namespace: "blobs", Id: "obj1"})
		require.NoError(t, err)
		var body []byte
		for {
			msg, err := getStream.Recv()
			if err == io.EOF {
				break
			}
			require.NoError(t, err)
			if c := msg.GetChunk(); c != nil {
				body = append(body, c.GetData()...)
			}
		}
		assert.Equal(t, "hello world", string(body))

		del, err := f.Artifact.Delete(ctx, &artifactv1.DeleteRequest{Namespace: "blobs", Id: "obj1"})
		require.NoError(t, err)
		assert.True(t, del.GetExisted())
	})

	t.Run("Lease", func(t *testing.T) {
		acq, err := f.Lease.Acquire(ctx, &leasev1.AcquireRequest{
			Namespace: "locks", Key: "k", Ttl: durationpb.New(time.Minute),
		})
		require.NoError(t, err)
		holderID := acq.GetHandle().GetHolderId()
		require.NotEmpty(t, holderID)

		ins, err := f.Lease.Inspect(ctx, &leasev1.InspectRequest{Namespace: "locks", Key: "k"})
		require.NoError(t, err)
		assert.True(t, ins.GetHeld())

		rel, err := f.Lease.Release(ctx, &leasev1.ReleaseRequest{
			Namespace: "locks", Key: "k", HolderId: holderID,
		})
		require.NoError(t, err)
		assert.True(t, rel.GetExisted())
	})
}

// TestE2E_Bufconn_MissingToken confirms the auth interceptor rejects calls
// without a capability header.
func TestE2E_Bufconn_MissingToken(t *testing.T) {
	f := newBufFixture(t)
	_, err := f.KV.Get(context.Background(), &kvv1.GetRequest{Namespace: "scratchpad", Key: "k"})
	require.Error(t, err)
	assert.Equal(t, codes.Unauthenticated, status.Code(err))
}

// TestE2E_Bufconn_BadSigner confirms a syntactically valid PASETO from an
// untrusted signer is rejected.
func TestE2E_Bufconn_BadSigner(t *testing.T) {
	f := newBufFixture(t)

	// Generate a fresh keypair the server's verifier does NOT know about.
	rogueSec, _, err := auth.GeneratePASETOKeyPair()
	require.NoError(t, err)
	tok, err := auth.IssuePASETO(rogueSec, fullScope(), time.Minute, "rogue")
	require.NoError(t, err)

	ctx := withToken(context.Background(), tok)
	_, err = f.KV.Get(ctx, &kvv1.GetRequest{Namespace: "scratchpad", Key: "k"})
	require.Error(t, err)
	assert.Equal(t, codes.Unauthenticated, status.Code(err))
}

// TestE2E_Bufconn_ScopeDenied_Op confirms a token scoped to read-only ops
// cannot perform a write op.
func TestE2E_Bufconn_ScopeDenied_Op(t *testing.T) {
	f := newBufFixture(t)
	ctx := withToken(context.Background(), f.mintToken(auth.Scope{
		Tenant:           "acme",
		NamespacePattern: []string{"kv/scratchpad"},
		AllowedOps:       []string{string(auth.OpKVGet)},
	}))
	_, err := f.KV.Put(ctx, &kvv1.PutRequest{Namespace: "scratchpad", Key: "k", Value: []byte("v")})
	require.Error(t, err)
	assert.Equal(t, codes.PermissionDenied, status.Code(err))
}

// TestE2E_Bufconn_ScopeDenied_Namespace confirms a token scoped to one
// namespace cannot reach another, even if both are bound on the server.
func TestE2E_Bufconn_ScopeDenied_Namespace(t *testing.T) {
	f := newBufFixture(t)
	ctx := withToken(context.Background(), f.mintToken(auth.Scope{
		Tenant:           "acme",
		NamespacePattern: []string{"kv/scratchpad"},
		AllowedOps:       []string{"*"},
	}))
	_, err := f.KV.Get(ctx, &kvv1.GetRequest{Namespace: "other", Key: "k"})
	require.Error(t, err)
	assert.Equal(t, codes.PermissionDenied, status.Code(err))
}
