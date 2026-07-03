// Package semantictest provides a driver-agnostic conformance suite that
// every semantic.Driver implementation must pass.
package semantictest

import (
	"context"
	"testing"
	"time"

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
		{"Lifecycle_ValidityWindow", testLifecycleValidityWindow},
		{"Lifecycle_SoftDeleteVisibility", testLifecycleSoftDeleteVisibility},
		{"Lifecycle_UpsertResurrects", testLifecycleUpsertResurrects},
		{"Supersedes_Invalidates", testSupersedesInvalidates},
		{"Expire_ByFilter", testExpireByFilter},
		{"Versioning", testVersioning},
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

	existed, err := d.Delete(ctx, "missing", semantic.DeleteOptions{})
	require.NoError(t, err)
	assert.False(t, existed)

	existed, err = d.Delete(ctx, "a", semantic.DeleteOptions{})
	require.NoError(t, err)
	assert.True(t, existed)

	hits, err := d.Search(ctx, semantic.SearchOptions{QueryVector: axisVector(0)})
	require.NoError(t, err)
	assert.Empty(t, hits)
}

// testLifecycleValidityWindow checks that a record whose valid_to has passed is
// excluded from a default (as-of now) search, but recoverable via as_of within
// its window and via include_invalidated (ADR-0003).
func testLifecycleValidityWindow(t *testing.T, h Harness) {
	d := h.New(t, Dim)
	ctx := context.Background()

	now := time.Now()
	require.NoError(t, d.Upsert(ctx, []semantic.Record{{
		ID:        "a",
		Vector:    axisVector(0),
		ValidFrom: now.Add(-2 * time.Hour),
		ValidTo:   now.Add(-1 * time.Hour), // expired an hour ago
	}}))

	// Default search evaluates validity as of now -> expired, excluded.
	hits, err := d.Search(ctx, semantic.SearchOptions{QueryVector: axisVector(0)})
	require.NoError(t, err)
	assert.Empty(t, hits, "expired record must be hidden by default")

	// as_of inside the validity window -> visible.
	hits, err = d.Search(ctx, semantic.SearchOptions{
		QueryVector: axisVector(0),
		AsOf:        now.Add(-90 * time.Minute),
	})
	require.NoError(t, err)
	require.Len(t, hits, 1)
	assert.Equal(t, "a", hits[0].Record.ID)

	// include_invalidated bypasses the lifecycle filter entirely.
	hits, err = d.Search(ctx, semantic.SearchOptions{
		QueryVector:        axisVector(0),
		IncludeInvalidated: true,
	})
	require.NoError(t, err)
	require.Len(t, hits, 1)
	assert.Equal(t, "a", hits[0].Record.ID)
}

// testLifecycleSoftDeleteVisibility checks soft delete tombstones a row
// (hidden by default, visible with include_invalidated) while hard delete
// removes it entirely (ADR-0003).
func testLifecycleSoftDeleteVisibility(t *testing.T, h Harness) {
	d := h.New(t, Dim)
	ctx := context.Background()
	require.NoError(t, d.Upsert(ctx, []semantic.Record{{ID: "a", Vector: axisVector(0)}}))

	existed, err := d.Delete(ctx, "a", semantic.DeleteOptions{}) // soft
	require.NoError(t, err)
	assert.True(t, existed)

	// Re-soft-deleting a tombstoned row is a no-op.
	existed, err = d.Delete(ctx, "a", semantic.DeleteOptions{})
	require.NoError(t, err)
	assert.False(t, existed)

	// Hidden by default, visible with include_invalidated.
	hits, err := d.Search(ctx, semantic.SearchOptions{QueryVector: axisVector(0)})
	require.NoError(t, err)
	assert.Empty(t, hits)

	hits, err = d.Search(ctx, semantic.SearchOptions{QueryVector: axisVector(0), IncludeInvalidated: true})
	require.NoError(t, err)
	require.Len(t, hits, 1)
	assert.False(t, hits[0].Record.DeletedAt.IsZero(), "tombstone timestamp must be surfaced")

	// Hard delete removes it even from include_invalidated.
	existed, err = d.Delete(ctx, "a", semantic.DeleteOptions{Hard: true})
	require.NoError(t, err)
	assert.True(t, existed)

	hits, err = d.Search(ctx, semantic.SearchOptions{QueryVector: axisVector(0), IncludeInvalidated: true})
	require.NoError(t, err)
	assert.Empty(t, hits)
}

// testLifecycleUpsertResurrects checks that re-upserting a soft-deleted id
// clears the tombstone (ADR-0003: upsert is authoritative).
func testLifecycleUpsertResurrects(t *testing.T, h Harness) {
	d := h.New(t, Dim)
	ctx := context.Background()
	require.NoError(t, d.Upsert(ctx, []semantic.Record{{ID: "a", Vector: axisVector(0)}}))

	_, err := d.Delete(ctx, "a", semantic.DeleteOptions{})
	require.NoError(t, err)

	hits, err := d.Search(ctx, semantic.SearchOptions{QueryVector: axisVector(0)})
	require.NoError(t, err)
	require.Empty(t, hits)

	// Re-upsert without a tombstone resurrects the record.
	require.NoError(t, d.Upsert(ctx, []semantic.Record{{ID: "a", Vector: axisVector(0)}}))
	hits, err = d.Search(ctx, semantic.SearchOptions{QueryVector: axisVector(0)})
	require.NoError(t, err)
	require.Len(t, hits, 1)
	assert.Equal(t, "a", hits[0].Record.ID)
}

// testSupersedesInvalidates checks that upserting a record that supersedes an
// earlier one invalidates the earlier one as of the new record's valid_from,
// while point-in-time and include_invalidated reads still recover it (U2).
func testSupersedesInvalidates(t *testing.T, h Harness) {
	d := h.New(t, Dim)
	ctx := context.Background()

	past := time.Now().Add(-time.Hour)
	// A becomes valid an hour ago; B (default valid_from ~= now) supersedes it.
	require.NoError(t, d.Upsert(ctx, []semantic.Record{{ID: "a", Vector: axisVector(0), ValidFrom: past}}))
	require.NoError(t, d.Upsert(ctx, []semantic.Record{{ID: "b", Vector: axisVector(0), Supersedes: []string{"a"}}}))

	// Default (as-of now): only the superseding record remains.
	hits, err := d.Search(ctx, semantic.SearchOptions{QueryVector: axisVector(0), TopK: 10})
	require.NoError(t, err)
	require.Len(t, hits, 1)
	assert.Equal(t, "b", hits[0].Record.ID)

	// include_invalidated surfaces both; A now carries a valid_to.
	hits, err = d.Search(ctx, semantic.SearchOptions{QueryVector: axisVector(0), TopK: 10, IncludeInvalidated: true})
	require.NoError(t, err)
	require.Len(t, hits, 2)
	byID := map[string]semantic.Record{}
	for _, hh := range hits {
		byID[hh.Record.ID] = hh.Record
	}
	assert.False(t, byID["a"].ValidTo.IsZero(), "superseded record must have valid_to set")

	// A point-in-time read within A's original window recovers only A.
	hits, err = d.Search(ctx, semantic.SearchOptions{QueryVector: axisVector(0), TopK: 10, AsOf: past})
	require.NoError(t, err)
	require.Len(t, hits, 1)
	assert.Equal(t, "a", hits[0].Record.ID)
}

// testExpireByFilter checks the bounded, filter-scoped lifecycle op for each
// action and that max_rows caps the affected set (U3).
func testExpireByFilter(t *testing.T, h Harness) {
	d := h.New(t, Dim)
	ctx := context.Background()
	require.NoError(t, d.Upsert(ctx, []semantic.Record{
		{ID: "a1", Vector: axisVector(0), Metadata: map[string]string{"tenant": "acme"}},
		{ID: "a2", Vector: axisVector(0), Metadata: map[string]string{"tenant": "acme"}},
		{ID: "o1", Vector: axisVector(0), Metadata: map[string]string{"tenant": "other"}},
	}))

	// Invalidate every acme record in one bounded op.
	affected, err := d.Expire(ctx, semantic.ExpireOptions{
		Filter: map[string]string{"tenant": "acme"}, Action: semantic.ExpireInvalidate, MaxRows: 10,
	})
	require.NoError(t, err)
	assert.Equal(t, uint64(2), affected)

	hits, err := d.Search(ctx, semantic.SearchOptions{QueryVector: axisVector(0), TopK: 10})
	require.NoError(t, err)
	require.Len(t, hits, 1)
	assert.Equal(t, "o1", hits[0].Record.ID)

	hits, err = d.Search(ctx, semantic.SearchOptions{QueryVector: axisVector(0), TopK: 10, IncludeInvalidated: true})
	require.NoError(t, err)
	assert.Len(t, hits, 3)

	// max_rows caps the affected set: two matching rows, cap of 1.
	require.NoError(t, d.Upsert(ctx, []semantic.Record{
		{ID: "b1", Vector: axisVector(0), Metadata: map[string]string{"grp": "x"}},
		{ID: "b2", Vector: axisVector(0), Metadata: map[string]string{"grp": "x"}},
	}))
	affected, err = d.Expire(ctx, semantic.ExpireOptions{
		Filter: map[string]string{"grp": "x"}, Action: semantic.ExpireSoftDelete, MaxRows: 1,
	})
	require.NoError(t, err)
	assert.Equal(t, uint64(1), affected, "max_rows must cap the affected set")

	// Hard delete removes the remaining grp=x rows entirely.
	affected, err = d.Expire(ctx, semantic.ExpireOptions{
		Filter: map[string]string{"grp": "x"}, Action: semantic.ExpireHardDelete, MaxRows: 10,
	})
	require.NoError(t, err)
	assert.Positive(t, affected)

	hits, err = d.Search(ctx, semantic.SearchOptions{
		QueryVector: axisVector(0), TopK: 10, IncludeInvalidated: true,
		Filter: map[string]string{"grp": "x"},
	})
	require.NoError(t, err)
	assert.Empty(t, hits, "hard-deleted rows must be gone even with include_invalidated")
}

// testVersioning checks the monotonic per-id version counter and the if_version
// optimistic-concurrency precondition on Upsert (U4).
func testVersioning(t *testing.T, h Harness) {
	d := h.New(t, Dim)
	ctx := context.Background()

	// Create -> version 1; unconditional update -> version 2.
	rec := []semantic.Record{{ID: "k", Vector: axisVector(0)}}
	require.NoError(t, d.Upsert(ctx, rec))
	assert.Equal(t, uint64(1), rec[0].Version)

	rec = []semantic.Record{{ID: "k", Vector: axisVector(0)}}
	require.NoError(t, d.Upsert(ctx, rec))
	assert.Equal(t, uint64(2), rec[0].Version)

	// CAS with the wrong expected version fails and changes nothing.
	wrong := uint64(1)
	err := d.Upsert(ctx, []semantic.Record{{ID: "k", Vector: axisVector(0), IfVersion: &wrong}})
	require.ErrorIs(t, err, semantic.ErrVersionMismatch)

	// CAS with the correct expected version succeeds and bumps to 3.
	two := uint64(2)
	rec = []semantic.Record{{ID: "k", Vector: axisVector(0), IfVersion: &two}}
	require.NoError(t, d.Upsert(ctx, rec))
	assert.Equal(t, uint64(3), rec[0].Version)

	// Create-only (if_version = 0) succeeds for a fresh id but conflicts on an
	// existing one.
	zero := uint64(0)
	rec = []semantic.Record{{ID: "fresh", Vector: axisVector(0), IfVersion: &zero}}
	require.NoError(t, d.Upsert(ctx, rec))
	assert.Equal(t, uint64(1), rec[0].Version)

	err = d.Upsert(ctx, []semantic.Record{{ID: "k", Vector: axisVector(0), IfVersion: &zero}})
	require.ErrorIs(t, err, semantic.ErrVersionMismatch)
}
