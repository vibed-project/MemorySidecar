//go:build integration

package postgres_test

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	tcpostgres "github.com/testcontainers/testcontainers-go/modules/postgres"

	"memsidecar/internal/semantic"
	pgdrv "memsidecar/internal/semantic/drivers/postgres"
)

const testDim = 8

func newDriver(t *testing.T) *pgdrv.Driver {
	t.Helper()
	ctx := context.Background()

	container, err := tcpostgres.Run(ctx, "pgvector/pgvector:pg16",
		tcpostgres.WithDatabase("memsidecar_test"),
		tcpostgres.WithUsername("test"),
		tcpostgres.WithPassword("test"),
		tcpostgres.BasicWaitStrategies(),
		tcpostgres.WithSQLDriver("pgx"),
	)
	require.NoError(t, err)
	t.Cleanup(func() { _ = container.Terminate(ctx) })

	dsn, err := container.ConnectionString(ctx, "sslmode=disable")
	require.NoError(t, err)

	d, err := pgdrv.New(ctx, pgdrv.Options{
		DSN: dsn, Namespace: "notes", Dimensions: testDim,
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = d.Close() })
	return d
}

func axisVector(i int) []float32 {
	v := make([]float32, testDim)
	v[i] = 1
	return v
}

func TestPG_UpsertSearch(t *testing.T) {
	d := newDriver(t)
	ctx := context.Background()
	require.NoError(t, d.Upsert(ctx, []semantic.Record{
		{ID: "a", Vector: axisVector(0), Content: "axis 0"},
		{ID: "b", Vector: axisVector(1), Content: "axis 1"},
		{ID: "c", Vector: axisVector(2), Content: "axis 2"},
	}))
	hits, err := d.Search(ctx, semantic.SearchOptions{
		QueryVector: axisVector(0), TopK: 2,
	})
	require.NoError(t, err)
	require.Len(t, hits, 2)
	assert.Equal(t, "a", hits[0].Record.ID)
	assert.InDelta(t, 1.0, hits[0].Score, 1e-4)
}

func TestPG_Upsert_AssignsID(t *testing.T) {
	d := newDriver(t)
	recs := []semantic.Record{{Vector: axisVector(0), Content: "x"}}
	require.NoError(t, d.Upsert(context.Background(), recs))
	assert.NotEmpty(t, recs[0].ID)
}

func TestPG_Search_Filter(t *testing.T) {
	d := newDriver(t)
	ctx := context.Background()
	require.NoError(t, d.Upsert(ctx, []semantic.Record{
		{ID: "a", Vector: axisVector(0), Metadata: map[string]string{"tenant": "acme"}},
		{ID: "b", Vector: axisVector(0), Metadata: map[string]string{"tenant": "other"}},
	}))
	hits, err := d.Search(ctx, semantic.SearchOptions{
		QueryVector: axisVector(0), Filter: map[string]string{"tenant": "acme"},
	})
	require.NoError(t, err)
	require.Len(t, hits, 1)
	assert.Equal(t, "a", hits[0].Record.ID)
}

func TestPG_Delete(t *testing.T) {
	d := newDriver(t)
	ctx := context.Background()
	require.NoError(t, d.Upsert(ctx, []semantic.Record{{ID: "a", Vector: axisVector(0)}}))
	existed, err := d.Delete(ctx, "a")
	require.NoError(t, err)
	assert.True(t, existed)

	existed, err = d.Delete(ctx, "a")
	require.NoError(t, err)
	assert.False(t, existed)
}
