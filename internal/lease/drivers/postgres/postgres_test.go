//go:build integration

package postgres_test

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	tcpostgres "github.com/testcontainers/testcontainers-go/modules/postgres"

	"memsidecar/internal/lease"
	pgdrv "memsidecar/internal/lease/drivers/postgres"
	"memsidecar/internal/lease/leasetest"
)

type harness struct{}

func (harness) New(t *testing.T) lease.Driver {
	t.Helper()
	ctx := context.Background()
	c, err := tcpostgres.Run(ctx, "postgres:16-alpine",
		tcpostgres.WithDatabase("memsidecar_test"),
		tcpostgres.WithUsername("test"),
		tcpostgres.WithPassword("test"),
		tcpostgres.BasicWaitStrategies(),
		tcpostgres.WithSQLDriver("pgx"),
	)
	require.NoError(t, err)
	t.Cleanup(func() { _ = c.Terminate(ctx) })

	dsn, err := c.ConnectionString(ctx, "sslmode=disable")
	require.NoError(t, err)
	d, err := pgdrv.New(ctx, pgdrv.Options{DSN: dsn, PollInterval: 50 * time.Millisecond})
	require.NoError(t, err)
	t.Cleanup(func() { _ = d.Close() })
	return d
}

func TestConformance(t *testing.T) {
	leasetest.RunConformance(t, harness{})
}
