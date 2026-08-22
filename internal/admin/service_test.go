package admin

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	adminv1 "github.com/vibed-project/mindD/gen/mindd/admin/v1"
	"github.com/vibed-project/mindD/internal/auth"
	"github.com/vibed-project/mindD/internal/config"
	"github.com/vibed-project/mindD/internal/obs"
)

func testConfig() *config.Config {
	return &config.Config{
		Backends: []config.BackendConfig{
			{Name: "mem", Driver: "memory"},
			{Name: "pg", Driver: "postgres"},
		},
		Namespaces: []config.NamespaceConfig{
			{Block: "semantic", Name: "notes", Backend: "pg", Embedder: config.EmbedderConfig{Provider: "openai", Model: "text-embedding-3-small", Dimensions: 1536}},
			{Block: "kv", Name: "scratch", Backend: "mem"},
			{Block: "artifact", Name: "blobs", Backend: "mem"}, // no count source → HasCount false
		},
	}
}

func sources() []obs.NamespaceItemSource {
	return []obs.NamespaceItemSource{
		{Block: "kv", Items: func(context.Context) map[string]int64 { return map[string]int64{"scratch": 7} }},
		{Block: "semantic", Items: func(context.Context) map[string]int64 { return map[string]int64{"notes": 42} }},
		// artifact intentionally omitted to model a driver with no cheap Size.
	}
}

func adminCtx(ops ...string) context.Context {
	return auth.WithCapability(context.Background(), &auth.Capability{
		Scope: auth.Scope{Tenant: "t", AllowedOps: ops},
	})
}

func TestListNamespaces(t *testing.T) {
	s := NewService(testConfig(), sources(), "1.2.3", "abc123")
	resp, err := s.ListNamespaces(adminCtx("admin.inspect"), &adminv1.ListNamespacesRequest{})
	require.NoError(t, err)

	require.Equal(t, "1.2.3", resp.GetServer().GetVersion())
	require.Equal(t, "abc123", resp.GetServer().GetCommit())

	// Ordered by (block, name): artifact/blobs, kv/scratch, semantic/notes.
	require.Len(t, resp.GetNamespaces(), 3)
	assert.Equal(t, []string{"artifact", "kv", "semantic"}, []string{
		resp.Namespaces[0].GetBlock(), resp.Namespaces[1].GetBlock(), resp.Namespaces[2].GetBlock(),
	})

	// artifact/blobs: driver resolved from backend, no count source → HasCount false.
	blobs := resp.Namespaces[0]
	assert.Equal(t, "memory", blobs.GetDriver())
	assert.False(t, blobs.GetHasCount())

	// kv/scratch: count wired through.
	scratch := resp.Namespaces[1]
	assert.Equal(t, "memory", scratch.GetDriver())
	assert.True(t, scratch.GetHasCount())
	assert.Equal(t, int64(7), scratch.GetItemCount())

	// semantic/notes: postgres driver, count, and embedder populated.
	notes := resp.Namespaces[2]
	assert.Equal(t, "postgres", notes.GetDriver())
	assert.Equal(t, int64(42), notes.GetItemCount())
	require.NotNil(t, notes.GetEmbedder())
	assert.Equal(t, "openai", notes.GetEmbedder().GetProvider())
	assert.Equal(t, uint32(1536), notes.GetEmbedder().GetDimensions())
	// Non-semantic namespaces carry no embedder.
	assert.Empty(t, scratch.GetEmbedder().GetProvider())
}

func TestListNamespaces_AuthGate(t *testing.T) {
	s := NewService(testConfig(), sources(), "v", "c")

	// No capability at all.
	_, err := s.ListNamespaces(context.Background(), &adminv1.ListNamespacesRequest{})
	assert.Equal(t, codes.Unauthenticated, status.Code(err))

	// Capability without the admin op.
	_, err = s.ListNamespaces(adminCtx("kv.get"), &adminv1.ListNamespacesRequest{})
	assert.Equal(t, codes.PermissionDenied, status.Code(err))

	// Wildcard op grants it.
	_, err = s.ListNamespaces(adminCtx("*"), &adminv1.ListNamespacesRequest{})
	assert.NoError(t, err)
}
