//go:build integration

package postgres_test

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	tcpostgres "github.com/testcontainers/testcontainers-go/modules/postgres"
	"github.com/testcontainers/testcontainers-go/wait"

	"memsidecar/internal/kv"
	pgdrv "memsidecar/internal/kv/drivers/postgres"
)

func newPGDriver(t *testing.T) *pgdrv.Driver {
	t.Helper()
	ctx := context.Background()

	container, err := tcpostgres.Run(ctx, "postgres:16-alpine",
		tcpostgres.WithDatabase("memsidecar_test"),
		tcpostgres.WithUsername("test"),
		tcpostgres.WithPassword("test"),
		tcpostgres.BasicWaitStrategies(),
		tcpostgres.WithSQLDriver("pgx"),
	)
	_ = wait.Strategy(nil) // keep import used
	require.NoError(t, err)
	t.Cleanup(func() { _ = container.Terminate(ctx) })

	dsn, err := container.ConnectionString(ctx, "sslmode=disable")
	require.NoError(t, err)

	d, err := pgdrv.New(ctx, pgdrv.Options{DSN: dsn, SweeperInterval: -1})
	require.NoError(t, err)
	t.Cleanup(func() { _ = d.Close() })
	return d
}

func TestPG_PutGetDelete(t *testing.T) {
	d := newPGDriver(t)
	ctx := context.Background()

	_, err := d.Get(ctx, "ns", "missing")
	require.ErrorIs(t, err, kv.ErrNotFound)

	rec, err := d.Put(ctx, "ns", "k", kv.PutOptions{Value: []byte("v"), ContentType: "text/plain"})
	require.NoError(t, err)
	assert.Equal(t, uint64(1), rec.Version)

	got, err := d.Get(ctx, "ns", "k")
	require.NoError(t, err)
	assert.Equal(t, []byte("v"), got.Value)
	assert.Equal(t, "text/plain", got.ContentType)

	existed, err := d.Delete(ctx, "ns", "k", kv.DeleteOptions{})
	require.NoError(t, err)
	assert.True(t, existed)
}

func TestPG_TTL(t *testing.T) {
	d := newPGDriver(t)
	ctx := context.Background()
	_, err := d.Put(ctx, "ns", "k", kv.PutOptions{Value: []byte("x"), TTL: time.Second})
	require.NoError(t, err)
	time.Sleep(2 * time.Second)
	_, err = d.Get(ctx, "ns", "k")
	require.ErrorIs(t, err, kv.ErrNotFound)
}

func TestPG_CAS(t *testing.T) {
	d := newPGDriver(t)
	ctx := context.Background()
	zero := uint64(0)
	_, err := d.Put(ctx, "ns", "k", kv.PutOptions{Value: []byte("a"), IfVersion: &zero})
	require.NoError(t, err)
	wrong := uint64(99)
	_, err = d.Put(ctx, "ns", "k", kv.PutOptions{Value: []byte("b"), IfVersion: &wrong})
	require.ErrorIs(t, err, kv.ErrVersionMismatch)
	one := uint64(1)
	rec, err := d.Put(ctx, "ns", "k", kv.PutOptions{Value: []byte("c"), IfVersion: &one})
	require.NoError(t, err)
	assert.Equal(t, uint64(2), rec.Version)
}

func TestPG_ScanPrefix(t *testing.T) {
	d := newPGDriver(t)
	ctx := context.Background()
	for _, k := range []string{"a/1", "a/2", "a/3", "b/1"} {
		_, _ = d.Put(ctx, "ns", k, kv.PutOptions{Value: []byte(k)})
	}
	var keys []string
	err := d.Scan(ctx, "ns", kv.ScanOptions{KeyPrefix: "a/", Limit: 2, IncludeValues: true},
		func(r kv.Record) error { keys = append(keys, r.Key); return nil })
	require.NoError(t, err)
	assert.Equal(t, []string{"a/1", "a/2"}, keys)
}
