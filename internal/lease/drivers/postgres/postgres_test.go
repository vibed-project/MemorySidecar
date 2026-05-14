//go:build integration

package postgres_test

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	tcpostgres "github.com/testcontainers/testcontainers-go/modules/postgres"

	"memsidecar/internal/lease"
	pgdrv "memsidecar/internal/lease/drivers/postgres"
)

func newDriver(t *testing.T) *pgdrv.Driver {
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

func TestPG_AcquireRelease(t *testing.T) {
	d := newDriver(t)
	ctx := context.Background()
	l, err := d.Acquire(ctx, "ns", "k", lease.AcquireOptions{TTL: time.Minute})
	require.NoError(t, err)
	assert.NotEmpty(t, l.HolderID)

	cur, held, err := d.Inspect(ctx, "ns", "k")
	require.NoError(t, err)
	assert.True(t, held)
	assert.Equal(t, l.HolderID, cur.HolderID)

	existed, err := d.Release(ctx, "ns", "k", l.HolderID)
	require.NoError(t, err)
	assert.True(t, existed)
}

func TestPG_ConflictAndTakeoverAfterExpiry(t *testing.T) {
	d := newDriver(t)
	ctx := context.Background()
	_, err := d.Acquire(ctx, "ns", "k", lease.AcquireOptions{TTL: time.Second})
	require.NoError(t, err)
	_, err = d.Acquire(ctx, "ns", "k", lease.AcquireOptions{TTL: time.Minute})
	require.ErrorIs(t, err, lease.ErrAlreadyHeld)

	time.Sleep(1500 * time.Millisecond)
	l, err := d.Acquire(ctx, "ns", "k", lease.AcquireOptions{TTL: time.Minute})
	require.NoError(t, err)
	assert.NotEmpty(t, l.HolderID)
}

func TestPG_Renew(t *testing.T) {
	d := newDriver(t)
	ctx := context.Background()
	l, _ := d.Acquire(ctx, "ns", "k", lease.AcquireOptions{TTL: time.Minute})
	exp := l.ExpiresAt
	time.Sleep(50 * time.Millisecond)
	l2, err := d.Renew(ctx, "ns", "k", l.HolderID, 2*time.Minute)
	require.NoError(t, err)
	assert.True(t, l2.ExpiresAt.After(exp))

	_, err = d.Renew(ctx, "ns", "k", "wrong", time.Minute)
	require.ErrorIs(t, err, lease.ErrNotHeld)
}

func TestPG_WaitForUnblocksOnRelease(t *testing.T) {
	d := newDriver(t)
	ctx := context.Background()
	first, err := d.Acquire(ctx, "ns", "k", lease.AcquireOptions{TTL: time.Hour})
	require.NoError(t, err)
	got := make(chan lease.Lease, 1)
	go func() {
		l, err := d.Acquire(ctx, "ns", "k", lease.AcquireOptions{TTL: time.Minute, WaitFor: 2 * time.Second})
		if err == nil {
			got <- l
		}
	}()
	time.Sleep(100 * time.Millisecond)
	_, _ = d.Release(ctx, "ns", "k", first.HolderID)
	select {
	case l := <-got:
		assert.NotEqual(t, first.HolderID, l.HolderID)
	case <-time.After(3 * time.Second):
		t.Fatal("Acquire did not unblock after Release")
	}
}
