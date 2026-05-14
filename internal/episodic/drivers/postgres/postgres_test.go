//go:build integration

package postgres_test

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	tcpostgres "github.com/testcontainers/testcontainers-go/modules/postgres"

	"memsidecar/internal/episodic"
	pgdrv "memsidecar/internal/episodic/drivers/postgres"
)

func newDriver(t *testing.T) *pgdrv.Driver {
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

	d, err := pgdrv.New(ctx, pgdrv.Options{DSN: dsn, TailInterval: 100 * time.Millisecond})
	require.NoError(t, err)
	t.Cleanup(func() { _ = d.Close() })
	return d
}

func TestPG_AppendRange(t *testing.T) {
	d := newDriver(t)
	ctx := context.Background()
	for i := 0; i < 3; i++ {
		ev, err := d.Append(ctx, "ns", episodic.AppendOptions{Type: "tool_call", Payload: []byte("v")})
		require.NoError(t, err)
		assert.Equal(t, uint64(i+1), ev.Cursor)
		assert.NotEmpty(t, ev.ID)
	}
	var cursors []uint64
	err := d.Range(ctx, "ns", episodic.RangeOptions{},
		func(e episodic.Event) error { cursors = append(cursors, e.Cursor); return nil })
	require.NoError(t, err)
	assert.Equal(t, []uint64{1, 2, 3}, cursors)
}

func TestPG_TailLive(t *testing.T) {
	d := newDriver(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	_, _ = d.Append(ctx, "ns", episodic.AppendOptions{Type: "h"})

	got := make(chan uint64, 4)
	done := make(chan error, 1)
	go func() {
		done <- d.Tail(ctx, "ns", episodic.TailOptions{},
			func(e episodic.Event) error { got <- e.Cursor; return nil })
	}()

	time.Sleep(150 * time.Millisecond)
	_, _ = d.Append(ctx, "ns", episodic.AppendOptions{Type: "live"})

	select {
	case c := <-got:
		assert.Equal(t, uint64(2), c)
	case <-time.After(2 * time.Second):
		t.Fatal("Tail did not deliver event")
	}
	cancel()
	<-done
}
