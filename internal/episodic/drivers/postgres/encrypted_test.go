//go:build integration

package postgres_test

import (
	"bytes"
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	tcpostgres "github.com/testcontainers/testcontainers-go/modules/postgres"

	"github.com/vibed-project/mindD/internal/encryption"
	"github.com/vibed-project/mindD/internal/episodic"
	eppgdrv "github.com/vibed-project/mindD/internal/episodic/drivers/postgres"
	"github.com/vibed-project/mindD/internal/episodic/episodictest"
)

func testRing(t *testing.T) *encryption.Keyring {
	t.Helper()
	kr, err := encryption.NewKeyring([]encryption.KeySpec{
		{ID: "k1", Secret: bytes.Repeat([]byte{1}, 32)},
	})
	require.NoError(t, err)
	return kr
}

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
// encrypting decorator. The suite must pass unchanged.
type encPGHarness struct{}

func (encPGHarness) New(t *testing.T) episodic.Driver {
	t.Helper()
	d, err := eppgdrv.New(context.Background(), eppgdrv.Options{DSN: startPG(t)})
	require.NoError(t, err)
	t.Cleanup(func() { _ = d.Close() })
	return episodic.NewEncryptedDriver(d, testRing(t), episodic.EncryptOptions{})
}

func (encPGHarness) TailSettleTime() time.Duration { return 500 * time.Millisecond }

func TestEncryptedConformance_Postgres(t *testing.T) {
	episodictest.RunConformance(t, encPGHarness{})
}

// Payload must be ciphertext on disk while the columns Range filters and orders
// on stay readable.
func TestCiphertextOnDisk_Postgres(t *testing.T) {
	ctx := context.Background()
	dsn := startPG(t)

	raw, err := eppgdrv.New(ctx, eppgdrv.Options{DSN: dsn})
	require.NoError(t, err)
	t.Cleanup(func() { _ = raw.Close() })
	enc := episodic.NewEncryptedDriver(raw, testRing(t), episodic.EncryptOptions{})

	secret := []byte(`{"ssn":"078-05-1120"}`)
	_, err = enc.Append(ctx, "ns", episodic.AppendOptions{
		Type:      "message",
		Payload:   secret,
		Role:      "user",
		SessionID: "s1",
	})
	require.NoError(t, err)

	conn, err := pgx.Connect(ctx, dsn)
	require.NoError(t, err)
	defer func() { _ = conn.Close(ctx) }()

	var (
		payload   []byte
		eventType string
		role      string
		sessionID string
	)
	err = conn.QueryRow(ctx,
		`SELECT payload, type, role, session_id FROM episodic_events WHERE namespace = $1`,
		"ns",
	).Scan(&payload, &eventType, &role, &sessionID)
	require.NoError(t, err)

	assert.NotContains(t, string(payload), "078-05-1120", "plaintext reached the table")
	assert.True(t, encryption.IsEnvelope(payload), "stored payload is not an envelope")
	// Index-backed Range predicates are untouched.
	assert.Equal(t, "message", eventType)
	assert.Equal(t, "user", role)
	assert.Equal(t, "s1", sessionID)

	var got []string
	require.NoError(t, enc.Range(ctx, "ns", episodic.RangeOptions{}, func(ev episodic.Event) error {
		got = append(got, string(ev.Payload))
		return nil
	}))
	assert.Equal(t, []string{string(secret)}, got)
}
