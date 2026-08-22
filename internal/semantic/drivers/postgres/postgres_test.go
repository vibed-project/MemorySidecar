//go:build integration

package postgres_test

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
	tcpostgres "github.com/testcontainers/testcontainers-go/modules/postgres"

	"github.com/vibed-project/mindD/internal/semantic"
	pgdrv "github.com/vibed-project/mindD/internal/semantic/drivers/postgres"
	"github.com/vibed-project/mindD/internal/semantic/semantictest"
)

type harness struct{}

func (harness) New(t *testing.T, dim int) semantic.Driver {
	t.Helper()
	ctx := context.Background()

	container, err := tcpostgres.Run(ctx, "pgvector/pgvector:pg16",
		tcpostgres.WithDatabase("mindd_test"),
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
		DSN: dsn, Namespace: "notes", Dimensions: dim,
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = d.Close() })
	return d
}

func TestConformance(t *testing.T) {
	semantictest.RunConformance(t, harness{})
}
