//go:build integration

package redis_test

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
	tcredis "github.com/testcontainers/testcontainers-go/modules/redis"

	"memsidecar/internal/kv"
	redisdrv "memsidecar/internal/kv/drivers/redis"
	"memsidecar/internal/kv/kvtest"
)

type harness struct{}

func (harness) New(t *testing.T) kv.Driver {
	t.Helper()
	ctx := context.Background()

	container, err := tcredis.Run(ctx, "redis:7-alpine")
	require.NoError(t, err)
	t.Cleanup(func() { _ = container.Terminate(ctx) })

	dsn, err := container.ConnectionString(ctx)
	require.NoError(t, err)

	d, err := redisdrv.New(ctx, redisdrv.Options{DSN: dsn})
	require.NoError(t, err)
	t.Cleanup(func() { _ = d.Close() })
	return d
}

// Redis decides record memory reclamation server-side and the driver treats its
// own wall clock as authoritative for expiry; no fake-clock injection. The
// suite falls back to a real-time sleep for TTL and skips Scan_ExpiredFiltered.
func (harness) NewWithClock(t *testing.T, _ *kvtest.FakeClock) (kv.Driver, bool) {
	return nil, false
}

func TestConformance(t *testing.T) {
	kvtest.RunConformance(t, harness{})
}

// Sanity check that the client type is what we depend on (keeps the go-redis
// import meaningful even if the suite is skipped).
var _ = redis.Nil
