package memory

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"memsidecar/internal/semantic"
)

func TestUpsertSearch_TopKOrdering(t *testing.T) {
	d, err := New(Options{Dimensions: 3})
	require.NoError(t, err)
	ctx := context.Background()

	// Orthogonal axis-aligned vectors so cosine scores are unambiguous.
	require.NoError(t, d.Upsert(ctx, []semantic.Record{
		{ID: "a", Vector: []float32{1, 0, 0}, Content: "a"},
		{ID: "b", Vector: []float32{0, 1, 0}, Content: "b"},
		{ID: "c", Vector: []float32{0, 0, 1}, Content: "c"},
		{ID: "ab", Vector: []float32{1, 1, 0}, Content: "ab"},
	}))

	hits, err := d.Search(ctx, semantic.SearchOptions{
		QueryVector: []float32{1, 0, 0},
		TopK:        2,
	})
	require.NoError(t, err)
	require.Len(t, hits, 2)
	assert.Equal(t, "a", hits[0].Record.ID)
	assert.InDelta(t, float32(1), hits[0].Score, 1e-5)
	assert.Equal(t, "ab", hits[1].Record.ID)
	assert.InDelta(t, float32(0.7071), hits[1].Score, 1e-3)
}

func TestUpsert_AssignsID(t *testing.T) {
	d, _ := New(Options{Dimensions: 4})
	recs := []semantic.Record{{Vector: []float32{1, 0, 0, 0}, Content: "x"}}
	require.NoError(t, d.Upsert(context.Background(), recs))
	assert.NotEmpty(t, recs[0].ID)
}

func TestUpsert_DimMismatch(t *testing.T) {
	d, _ := New(Options{Dimensions: 3})
	err := d.Upsert(context.Background(), []semantic.Record{
		{ID: "x", Vector: []float32{1, 2}},
	})
	require.Error(t, err)
}

func TestSearch_FilterMetadata(t *testing.T) {
	d, _ := New(Options{Dimensions: 2})
	ctx := context.Background()
	require.NoError(t, d.Upsert(ctx, []semantic.Record{
		{ID: "a", Vector: []float32{1, 0}, Metadata: map[string]string{"tenant": "acme"}},
		{ID: "b", Vector: []float32{1, 0}, Metadata: map[string]string{"tenant": "other"}},
	}))
	hits, err := d.Search(ctx, semantic.SearchOptions{
		QueryVector: []float32{1, 0},
		Filter:      map[string]string{"tenant": "acme"},
	})
	require.NoError(t, err)
	require.Len(t, hits, 1)
	assert.Equal(t, "a", hits[0].Record.ID)
}

func TestSearch_IncludeFlags(t *testing.T) {
	d, _ := New(Options{Dimensions: 2})
	ctx := context.Background()
	_ = d.Upsert(ctx, []semantic.Record{
		{ID: "a", Vector: []float32{1, 0}, Payload: []byte("p"), Content: "c"},
	})

	bare, _ := d.Search(ctx, semantic.SearchOptions{QueryVector: []float32{1, 0}})
	assert.Nil(t, bare[0].Record.Payload)
	assert.Nil(t, bare[0].Record.Vector)
	assert.Equal(t, "c", bare[0].Record.Content) // content always returned

	full, _ := d.Search(ctx, semantic.SearchOptions{
		QueryVector:    []float32{1, 0},
		IncludePayload: true,
		IncludeVector:  true,
	})
	assert.Equal(t, []byte("p"), full[0].Record.Payload)
	assert.Len(t, full[0].Record.Vector, 2)
}

func TestDelete(t *testing.T) {
	d, _ := New(Options{Dimensions: 2})
	ctx := context.Background()
	_ = d.Upsert(ctx, []semantic.Record{{ID: "a", Vector: []float32{1, 0}}})

	existed, err := d.Delete(ctx, "missing")
	require.NoError(t, err)
	assert.False(t, existed)

	existed, err = d.Delete(ctx, "a")
	require.NoError(t, err)
	assert.True(t, existed)

	hits, _ := d.Search(ctx, semantic.SearchOptions{QueryVector: []float32{1, 0}})
	assert.Empty(t, hits)
}

func TestUpsert_NormalizesVectors(t *testing.T) {
	d, _ := New(Options{Dimensions: 2})
	ctx := context.Background()
	_ = d.Upsert(ctx, []semantic.Record{{ID: "a", Vector: []float32{5, 0}}})
	hits, _ := d.Search(ctx, semantic.SearchOptions{QueryVector: []float32{2, 0}, IncludeVector: true})
	// Stored vector should be normalised to (1, 0).
	assert.InDelta(t, float32(1), hits[0].Record.Vector[0], 1e-5)
	assert.InDelta(t, float32(0), hits[0].Record.Vector[1], 1e-5)
	assert.InDelta(t, float32(1), hits[0].Score, 1e-5)
}
