// Package semantictest provides a driver-agnostic conformance suite that
// every semantic.Driver implementation must pass.
package semantictest

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"memsidecar/internal/semantic"
)

// Dim is the vector dimension every conformance harness must support. Picked
// large enough to discriminate top-K ordering and to match common test
// fixtures across drivers.
const Dim = 8

// Harness adapts a concrete driver to the conformance suite.
//
// New must return a freshly isolated driver configured for dim-dimensional
// vectors. The suite calls it once per subtest.
type Harness interface {
	New(t *testing.T, dim int) semantic.Driver
}

// RunConformance runs the full conformance battery against the driver
// produced by h.
func RunConformance(t *testing.T, h Harness) {
	t.Helper()
	cases := []struct {
		name string
		fn   func(*testing.T, Harness)
	}{
		{"Upsert_AssignsID", testUpsertAssignsID},
		{"Search_TopKOrdering", testSearchTopKOrdering},
		{"Search_FilterMetadata", testSearchFilterMetadata},
		{"Search_IncludeFlags", testSearchIncludeFlags},
		{"Delete", testDelete},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) { tc.fn(t, h) })
	}
}

// axisVector returns a unit vector along axis i in Dim dimensions.
func axisVector(i int) []float32 {
	v := make([]float32, Dim)
	v[i] = 1
	return v
}

func testUpsertAssignsID(t *testing.T, h Harness) {
	d := h.New(t, Dim)
	recs := []semantic.Record{{Vector: axisVector(0), Content: "x"}}
	require.NoError(t, d.Upsert(context.Background(), recs))
	assert.NotEmpty(t, recs[0].ID)
}

func testSearchTopKOrdering(t *testing.T, h Harness) {
	d := h.New(t, Dim)
	ctx := context.Background()

	require.NoError(t, d.Upsert(ctx, []semantic.Record{
		{ID: "a", Vector: axisVector(0), Content: "a"},
		{ID: "b", Vector: axisVector(1), Content: "b"},
		{ID: "c", Vector: axisVector(2), Content: "c"},
	}))

	hits, err := d.Search(ctx, semantic.SearchOptions{
		QueryVector: axisVector(0),
		TopK:        2,
	})
	require.NoError(t, err)
	require.Len(t, hits, 2)
	assert.Equal(t, "a", hits[0].Record.ID)
	assert.InDelta(t, 1.0, hits[0].Score, 1e-4)
}

func testSearchFilterMetadata(t *testing.T, h Harness) {
	d := h.New(t, Dim)
	ctx := context.Background()
	require.NoError(t, d.Upsert(ctx, []semantic.Record{
		{ID: "a", Vector: axisVector(0), Metadata: map[string]string{"tenant": "acme"}},
		{ID: "b", Vector: axisVector(0), Metadata: map[string]string{"tenant": "other"}},
	}))
	hits, err := d.Search(ctx, semantic.SearchOptions{
		QueryVector: axisVector(0),
		Filter:      map[string]string{"tenant": "acme"},
	})
	require.NoError(t, err)
	require.Len(t, hits, 1)
	assert.Equal(t, "a", hits[0].Record.ID)
}

func testSearchIncludeFlags(t *testing.T, h Harness) {
	d := h.New(t, Dim)
	ctx := context.Background()
	require.NoError(t, d.Upsert(ctx, []semantic.Record{
		{ID: "a", Vector: axisVector(0), Payload: []byte("p"), Content: "c"},
	}))

	bare, err := d.Search(ctx, semantic.SearchOptions{QueryVector: axisVector(0)})
	require.NoError(t, err)
	require.Len(t, bare, 1)
	assert.Nil(t, bare[0].Record.Payload)
	assert.Nil(t, bare[0].Record.Vector)
	assert.Equal(t, "c", bare[0].Record.Content) // content always returned

	full, err := d.Search(ctx, semantic.SearchOptions{
		QueryVector:    axisVector(0),
		IncludePayload: true,
		IncludeVector:  true,
	})
	require.NoError(t, err)
	require.Len(t, full, 1)
	assert.Equal(t, []byte("p"), full[0].Record.Payload)
	assert.Len(t, full[0].Record.Vector, Dim)
}

func testDelete(t *testing.T, h Harness) {
	d := h.New(t, Dim)
	ctx := context.Background()
	require.NoError(t, d.Upsert(ctx, []semantic.Record{{ID: "a", Vector: axisVector(0)}}))

	existed, err := d.Delete(ctx, "missing")
	require.NoError(t, err)
	assert.False(t, existed)

	existed, err = d.Delete(ctx, "a")
	require.NoError(t, err)
	assert.True(t, existed)

	hits, err := d.Search(ctx, semantic.SearchOptions{QueryVector: axisVector(0)})
	require.NoError(t, err)
	assert.Empty(t, hits)
}
