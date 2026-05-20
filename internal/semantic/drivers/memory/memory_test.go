package memory

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"memsidecar/internal/semantic"
	"memsidecar/internal/semantic/semantictest"
)

type harness struct{}

func (harness) New(t *testing.T, dim int) semantic.Driver {
	t.Helper()
	d, err := New(Options{Dimensions: dim})
	require.NoError(t, err)
	return d
}

func TestConformance(t *testing.T) {
	semantictest.RunConformance(t, harness{})
}

// Driver-specific: the in-memory driver normalises stored vectors. Other
// backends may rely on the storage layer (e.g. pgvector) to handle cosine
// distance differently, so this invariant is asserted only here.
func TestUpsert_NormalizesVectors(t *testing.T) {
	d, _ := New(Options{Dimensions: 2})
	ctx := context.Background()
	require.NoError(t, d.Upsert(ctx, []semantic.Record{{ID: "a", Vector: []float32{5, 0}}}))
	hits, err := d.Search(ctx, semantic.SearchOptions{QueryVector: []float32{2, 0}, IncludeVector: true})
	require.NoError(t, err)
	require.Len(t, hits, 1)
	assert.InDelta(t, float32(1), hits[0].Record.Vector[0], 1e-5)
	assert.InDelta(t, float32(0), hits[0].Record.Vector[1], 1e-5)
	assert.InDelta(t, float32(1), hits[0].Score, 1e-5)
}

// Driver-specific: the in-memory driver validates dimensions at Upsert.
// Postgres/pgvector rejects at the SQL layer with a different error shape; we
// only assert the contract that *some* error is returned, in-package here.
func TestUpsert_DimMismatch(t *testing.T) {
	d, _ := New(Options{Dimensions: 3})
	err := d.Upsert(context.Background(), []semantic.Record{
		{ID: "x", Vector: []float32{1, 2}},
	})
	require.Error(t, err)
}
