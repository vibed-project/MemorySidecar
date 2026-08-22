//go:build integration

package postgres_test

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
	tcpostgres "github.com/testcontainers/testcontainers-go/modules/postgres"

	"github.com/vibed-project/mindD/internal/kv"
	pgdrv "github.com/vibed-project/mindD/internal/kv/drivers/postgres"
	"github.com/vibed-project/mindD/internal/kv/kvtest"
)

type harness struct{}

func (harness) New(t *testing.T) kv.Driver {
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

	d, err := pgdrv.New(ctx, pgdrv.Options{DSN: dsn, SweeperInterval: -1})
	require.NoError(t, err)
	t.Cleanup(func() { _ = d.Close() })
	return d
}

// Postgres decides expiry server-side via NOW(); no clock injection.
// The conformance suite falls back to a real-time sleep for TTL and skips
// Scan_ExpiredFiltered.
func (harness) NewWithClock(t *testing.T, _ *kvtest.FakeClock) (kv.Driver, bool) {
	return nil, false
}

func TestConformance(t *testing.T) {
	kvtest.RunConformance(t, harness{})
}
