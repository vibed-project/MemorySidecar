//go:build integration

package postgres_test

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	tcpostgres "github.com/testcontainers/testcontainers-go/modules/postgres"

	"memsidecar/internal/episodic"
	pgdrv "memsidecar/internal/episodic/drivers/postgres"
	"memsidecar/internal/episodic/episodictest"
)

const tailPollInterval = 100 * time.Millisecond

type harness struct{}

func (harness) New(t *testing.T) episodic.Driver {
	t.Helper()
	ctx := context.Background()

	container, err := tcpostgres.Run(ctx, "postgres:16-alpine",
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

	d, err := pgdrv.New(ctx, pgdrv.Options{DSN: dsn, TailInterval: tailPollInterval})
	require.NoError(t, err)
	t.Cleanup(func() { _ = d.Close() })
	return d
}

// Postgres Tail is poll-based, so the settle time must exceed the poll
// interval to be sure a new event has been observed.
func (harness) TailSettleTime() time.Duration { return tailPollInterval + 50*time.Millisecond }

func TestConformance(t *testing.T) {
	episodictest.RunConformance(t, harness{})
}
