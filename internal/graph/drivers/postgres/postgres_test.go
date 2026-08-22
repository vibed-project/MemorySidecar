//go:build integration

package postgres_test

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
	tcpostgres "github.com/testcontainers/testcontainers-go/modules/postgres"

	"github.com/vibed-project/mindD/internal/graph"
	pgdrv "github.com/vibed-project/mindD/internal/graph/drivers/postgres"
	"github.com/vibed-project/mindD/internal/graph/graphtest"
)

type harness struct{}

func (harness) New(t *testing.T) graph.Driver {
	t.Helper()
	ctx := context.Background()

	container, err := tcpostgres.Run(ctx, "postgres:16-alpine",
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

	d, err := pgdrv.New(ctx, pgdrv.Options{DSN: dsn})
	require.NoError(t, err)
	t.Cleanup(func() { _ = d.Close() })
	return d
}

func TestConformance(t *testing.T) {
	graphtest.RunConformance(t, harness{})
}
