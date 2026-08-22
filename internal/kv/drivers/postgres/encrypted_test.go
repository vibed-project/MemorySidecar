//go:build integration

package postgres_test

import (
	"bytes"
	"context"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	tcpostgres "github.com/testcontainers/testcontainers-go/modules/postgres"

	"github.com/vibed-project/mindD/internal/encryption"
	"github.com/vibed-project/mindD/internal/kv"
	pgdrv "github.com/vibed-project/mindD/internal/kv/drivers/postgres"
	"github.com/vibed-project/mindD/internal/kv/kvtest"
)

func testRing(t *testing.T) *encryption.Keyring {
	t.Helper()
	kr, err := encryption.NewKeyring([]encryption.KeySpec{
		{ID: "k1", Secret: bytes.Repeat([]byte{1}, 32)},
	})
	require.NoError(t, err)
	return kr
}

// startPG spins up a Postgres and returns its DSN.
func startPG(t *testing.T) string {
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
	return dsn
}

// encPGHarness runs the conformance suite against a real Postgres through the
// encrypting decorator. The suite must pass unchanged — encryption is meant to
// be invisible above the driver.
type encPGHarness struct{}

func (encPGHarness) New(t *testing.T) kv.Driver {
	t.Helper()
	d, err := pgdrv.New(context.Background(), pgdrv.Options{DSN: startPG(t), SweeperInterval: -1})
	require.NoError(t, err)
	t.Cleanup(func() { _ = d.Close() })
	return kv.NewEncryptedDriver(d, testRing(t), kv.EncryptOptions{})
}

func (encPGHarness) NewWithClock(t *testing.T, _ *kvtest.FakeClock) (kv.Driver, bool) {
	return nil, false // Postgres decides expiry server-side via NOW().
}

func TestEncryptedConformance_Postgres(t *testing.T) {
	kvtest.RunConformance(t, encPGHarness{})
}

// The point of the feature: what is actually on disk must be ciphertext, while
// the columns Postgres queries on stay readable.
func TestCiphertextOnDisk_Postgres(t *testing.T) {
	ctx := context.Background()
	dsn := startPG(t)

	raw, err := pgdrv.New(ctx, pgdrv.Options{DSN: dsn, SweeperInterval: -1})
	require.NoError(t, err)
	t.Cleanup(func() { _ = raw.Close() })
	enc := kv.NewEncryptedDriver(raw, testRing(t), kv.EncryptOptions{})

	secret := []byte("card number 4111111111111111")
	_, err = enc.Put(ctx, "ns", "k", kv.PutOptions{
		Value:       secret,
		ContentType: "text/plain",
		Metadata:    map[string]string{"owner": "alice"},
	})
	require.NoError(t, err)

	conn, err := pgx.Connect(ctx, dsn)
	require.NoError(t, err)
	defer func() { _ = conn.Close(ctx) }()

	var (
		key         string
		value       []byte
		contentType string
	)
	err = conn.QueryRow(ctx,
		`SELECT key, value, content_type FROM kv_items WHERE namespace = $1 AND key = $2`,
		"ns", "k",
	).Scan(&key, &value, &contentType)
	require.NoError(t, err)

	assert.NotContains(t, string(value), "4111111111111111", "plaintext reached the table")
	assert.True(t, encryption.IsEnvelope(value), "stored value is not an envelope")
	// Queryable columns are untouched, so indexes and scans still work.
	assert.Equal(t, "k", key)
	assert.Equal(t, "text/plain", contentType)

	got, err := enc.Get(ctx, "ns", "k")
	require.NoError(t, err)
	assert.Equal(t, secret, got.Value)
}
