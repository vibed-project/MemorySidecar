package episodic_test

import (
	"bytes"
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/vibed-project/mindD/internal/encryption"
	"github.com/vibed-project/mindD/internal/episodic"
	"github.com/vibed-project/mindD/internal/episodic/drivers/memory"
	"github.com/vibed-project/mindD/internal/episodic/episodictest"
)

func ring(t *testing.T, ids ...string) *encryption.Keyring {
	t.Helper()
	specs := make([]encryption.KeySpec, 0, len(ids))
	for i, id := range ids {
		specs = append(specs, encryption.KeySpec{
			ID:     id,
			Secret: bytes.Repeat([]byte{byte(i + 1)}, 32),
		})
	}
	kr, err := encryption.NewKeyring(specs)
	require.NoError(t, err)
	return kr
}

// encHarness runs the standard conformance battery against an encrypting
// driver. Encryption must be fully transparent, so the suite passes unchanged.
type encHarness struct{}

func (encHarness) New(t *testing.T) episodic.Driver {
	t.Helper()
	d := memory.New(memory.Options{})
	t.Cleanup(func() { _ = d.Close() })
	return episodic.NewEncryptedDriver(d, ring(t, "k1"), episodic.EncryptOptions{})
}

func (encHarness) TailSettleTime() time.Duration { return 50 * time.Millisecond }

func TestEncryptedConformance(t *testing.T) {
	episodictest.RunConformance(t, encHarness{})
}

// newPair returns an encrypting driver and the raw driver underneath it, so a
// test can inspect what actually landed in storage.
func newPair(t *testing.T, kr *encryption.Keyring, opts episodic.EncryptOptions) (episodic.Driver, episodic.Driver) {
	t.Helper()
	raw := memory.New(memory.Options{})
	t.Cleanup(func() { _ = raw.Close() })
	return episodic.NewEncryptedDriver(raw, kr, opts), raw
}

// collect drains a Range into a payload slice.
func collect(t *testing.T, d episodic.Driver, ns string, opts episodic.RangeOptions) []string {
	t.Helper()
	var out []string
	err := d.Range(context.Background(), ns, opts, func(ev episodic.Event) error {
		out = append(out, string(ev.Payload))
		return nil
	})
	require.NoError(t, err)
	return out
}

func TestPayloadIsCiphertextAtRest(t *testing.T) {
	ctx := context.Background()
	enc, raw := newPair(t, ring(t, "k1"), episodic.EncryptOptions{})

	secret := []byte(`{"ssn":"078-05-1120"}`)
	ev, err := enc.Append(ctx, "ns", episodic.AppendOptions{
		Type:      "message",
		Payload:   secret,
		Role:      "user",
		SessionID: "s1",
	})
	require.NoError(t, err)
	assert.Equal(t, secret, ev.Payload, "Append must return plaintext")

	stored := collect(t, raw, "ns", episodic.RangeOptions{})
	require.Len(t, stored, 1)
	assert.NotContains(t, stored[0], "078-05-1120")
	assert.True(t, encryption.IsEnvelope([]byte(stored[0])), "stored payload is not an envelope")

	// Fields the backend filters and orders on stay plaintext.
	var got episodic.Event
	require.NoError(t, raw.Range(ctx, "ns", episodic.RangeOptions{}, func(e episodic.Event) error {
		got = e
		return nil
	}))
	assert.Equal(t, "message", got.Type)
	assert.Equal(t, "user", got.Role)
	assert.Equal(t, "s1", got.SessionID)

	assert.Equal(t, []string{string(secret)}, collect(t, enc, "ns", episodic.RangeOptions{}))
}

func TestAppendDoesNotMutateCallerSlice(t *testing.T) {
	ctx := context.Background()
	enc, _ := newPair(t, ring(t, "k1"), episodic.EncryptOptions{})

	caller := []byte("original")
	_, err := enc.Append(ctx, "ns", episodic.AppendOptions{Payload: caller})
	require.NoError(t, err)
	assert.Equal(t, []byte("original"), caller)
}

// A deduplicated Append returns the previously stored event, which is sealed —
// it must come back decrypted like any other read.
func TestDedupReturnsDecryptedPayload(t *testing.T) {
	ctx := context.Background()
	enc, _ := newPair(t, ring(t, "k1"), episodic.EncryptOptions{})

	first, err := enc.Append(ctx, "ns", episodic.AppendOptions{
		Payload:  []byte("once"),
		DedupKey: "idem-1",
	})
	require.NoError(t, err)

	second, err := enc.Append(ctx, "ns", episodic.AppendOptions{
		Payload:  []byte("once"),
		DedupKey: "idem-1",
	})
	require.NoError(t, err)

	assert.Equal(t, first.ID, second.ID, "dedup must return the stored event")
	assert.Equal(t, first.Cursor, second.Cursor)
	assert.Equal(t, []byte("once"), second.Payload)
}

func TestTailDecrypts(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	enc, _ := newPair(t, ring(t, "k1"), episodic.EncryptOptions{})

	_, err := enc.Append(ctx, "ns", episodic.AppendOptions{Payload: []byte("historical")})
	require.NoError(t, err)

	got := make(chan string, 1)
	errc := make(chan error, 1)
	go func() {
		errc <- enc.Tail(ctx, "ns", episodic.TailOptions{IncludeHistorical: true}, func(ev episodic.Event) error {
			got <- string(ev.Payload)
			return nil
		})
	}()

	select {
	case p := <-got:
		assert.Equal(t, "historical", p)
	case err := <-errc:
		t.Fatalf("Tail returned early: %v", err)
	case <-ctx.Done():
		t.Fatal("timed out waiting for a tailed event")
	}
	cancel()
	<-errc
}

func TestNilAndEmptyPayloads(t *testing.T) {
	ctx := context.Background()
	enc, _ := newPair(t, ring(t, "k1"), episodic.EncryptOptions{})

	ev, err := enc.Append(ctx, "ns", episodic.AppendOptions{Payload: nil})
	require.NoError(t, err)
	assert.Empty(t, ev.Payload)

	ev, err = enc.Append(ctx, "ns", episodic.AppendOptions{Payload: []byte{}})
	require.NoError(t, err)
	assert.Empty(t, ev.Payload)

	for _, p := range collect(t, enc, "ns", episodic.RangeOptions{}) {
		assert.Empty(t, p)
	}
}

// AAD binds a payload to its namespace, so ciphertext moved between namespaces
// by someone with raw substrate access will not open.
func TestRelocatedPayloadFailsToOpen(t *testing.T) {
	ctx := context.Background()
	enc, raw := newPair(t, ring(t, "k1"), episodic.EncryptOptions{})

	_, err := enc.Append(ctx, "ns-a", episodic.AppendOptions{Payload: []byte("secret")})
	require.NoError(t, err)
	sealed := collect(t, raw, "ns-a", episodic.RangeOptions{})
	require.Len(t, sealed, 1)

	_, err = raw.Append(ctx, "ns-b", episodic.AppendOptions{Payload: []byte(sealed[0])})
	require.NoError(t, err)

	err = enc.Range(ctx, "ns-b", episodic.RangeOptions{}, func(episodic.Event) error { return nil })
	require.ErrorIs(t, err, encryption.ErrDecrypt)
}

// Domain separation: a kv value sealed under the same keyring and namespace must
// not open as an episodic payload.
func TestCrossBlockCiphertextFailsToOpen(t *testing.T) {
	ctx := context.Background()
	kr := ring(t, "k1")
	enc, raw := newPair(t, kr, episodic.EncryptOptions{})

	// Seal with the kv block's associated data for the same namespace.
	kvSealed, err := kr.Encrypt([]byte("kv secret"), kvStyleAAD("ns", "somekey"))
	require.NoError(t, err)

	_, err = raw.Append(ctx, "ns", episodic.AppendOptions{Payload: kvSealed})
	require.NoError(t, err)

	err = enc.Range(ctx, "ns", episodic.RangeOptions{}, func(episodic.Event) error { return nil })
	require.ErrorIs(t, err, encryption.ErrDecrypt)
}

// kvStyleAAD mirrors internal/kv's unexported valueAAD so this test can forge a
// ciphertext from the other block without exporting either construction.
func kvStyleAAD(namespace, key string) []byte {
	const block = "kv"
	var aad []byte
	appendPart := func(s string) {
		aad = append(aad, byte(len(s))) // all parts here are < 128 bytes, so one varint byte
		aad = append(aad, s...)
	}
	appendPart(block)
	appendPart(namespace)
	appendPart(key)
	return aad
}

func TestKeyRotation(t *testing.T) {
	ctx := context.Background()
	raw := memory.New(memory.Options{})
	t.Cleanup(func() { _ = raw.Close() })

	oldRing := ring(t, "k1")
	before := episodic.NewEncryptedDriver(raw, oldRing, episodic.EncryptOptions{})
	_, err := before.Append(ctx, "ns", episodic.AppendOptions{Payload: []byte("written before")})
	require.NoError(t, err)

	rotated, err := encryption.NewKeyring([]encryption.KeySpec{
		{ID: "k2", Secret: bytes.Repeat([]byte{9}, 32)},
		{ID: "k1", Secret: bytes.Repeat([]byte{1}, 32)},
	})
	require.NoError(t, err)
	after := episodic.NewEncryptedDriver(raw, rotated, episodic.EncryptOptions{})

	assert.Equal(t, []string{"written before"}, collect(t, after, "ns", episodic.RangeOptions{}))

	_, err = after.Append(ctx, "ns", episodic.AppendOptions{Payload: []byte("written after")})
	require.NoError(t, err)

	// The retired-key-only ring cannot open what the new active key sealed.
	err = before.Range(ctx, "ns", episodic.RangeOptions{}, func(episodic.Event) error { return nil })
	require.ErrorIs(t, err, encryption.ErrDecrypt)
}

func TestAllowPlaintextReads(t *testing.T) {
	ctx := context.Background()
	strict, raw := newPair(t, ring(t, "k1"), episodic.EncryptOptions{})

	_, err := raw.Append(ctx, "ns", episodic.AppendOptions{Payload: []byte("pre-existing")})
	require.NoError(t, err)

	err = strict.Range(ctx, "ns", episodic.RangeOptions{}, func(episodic.Event) error { return nil })
	require.ErrorIs(t, err, encryption.ErrDecrypt, "strict mode must reject unsealed payloads")

	lenient := episodic.NewEncryptedDriver(raw, ring(t, "k1"), episodic.EncryptOptions{AllowPlaintextReads: true})
	assert.Equal(t, []string{"pre-existing"}, collect(t, lenient, "ns", episodic.RangeOptions{}))
}

func TestSizeCapabilityIsForwarded(t *testing.T) {
	ctx := context.Background()
	enc, _ := newPair(t, ring(t, "k1"), episodic.EncryptOptions{})

	sizer, ok := enc.(interface {
		Size(context.Context, string) (int64, error)
	})
	require.True(t, ok, "encrypted driver must still satisfy the namespaceSizer capability")

	for i := 0; i < 3; i++ {
		_, err := enc.Append(ctx, "ns", episodic.AppendOptions{Payload: []byte("v")})
		require.NoError(t, err)
	}
	n, err := sizer.Size(ctx, "ns")
	require.NoError(t, err)
	assert.Equal(t, int64(3), n)
}

func TestCloseDelegates(t *testing.T) {
	raw := memory.New(memory.Options{})
	enc := episodic.NewEncryptedDriver(raw, ring(t, "k1"), episodic.EncryptOptions{})
	require.NoError(t, enc.Close())
}
