package kv_test

import (
	"bytes"
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/vibed-project/mindD/internal/encryption"
	"github.com/vibed-project/mindD/internal/kv"
	"github.com/vibed-project/mindD/internal/kv/drivers/memory"
	"github.com/vibed-project/mindD/internal/kv/kvtest"
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

func (encHarness) New(t *testing.T) kv.Driver {
	t.Helper()
	d := memory.New(memory.Options{SweeperInterval: -1})
	t.Cleanup(func() { _ = d.Close() })
	return kv.NewEncryptedDriver(d, ring(t, "k1"), kv.EncryptOptions{})
}

func (encHarness) NewWithClock(t *testing.T, clock *kvtest.FakeClock) (kv.Driver, bool) {
	t.Helper()
	d := memory.New(memory.Options{SweeperInterval: -1, NowFunc: clock.Now})
	t.Cleanup(func() { _ = d.Close() })
	return kv.NewEncryptedDriver(d, ring(t, "k1"), kv.EncryptOptions{}), true
}

func TestEncryptedConformance(t *testing.T) {
	kvtest.RunConformance(t, encHarness{})
}

// newPair returns an encrypting driver and the raw driver underneath it, so a
// test can inspect what actually landed in storage.
func newPair(t *testing.T, kr *encryption.Keyring, opts kv.EncryptOptions) (kv.Driver, kv.Driver) {
	t.Helper()
	raw := memory.New(memory.Options{SweeperInterval: -1})
	t.Cleanup(func() { _ = raw.Close() })
	return kv.NewEncryptedDriver(raw, kr, opts), raw
}

func TestValueIsCiphertextAtRest(t *testing.T) {
	ctx := context.Background()
	enc, raw := newPair(t, ring(t, "k1"), kv.EncryptOptions{})

	secret := []byte("card number 4111111111111111")
	_, err := enc.Put(ctx, "ns", "k", kv.PutOptions{Value: secret, ContentType: "text/plain"})
	require.NoError(t, err)

	stored, err := raw.Get(ctx, "ns", "k")
	require.NoError(t, err)
	assert.NotContains(t, string(stored.Value), "4111111111111111")
	assert.True(t, encryption.IsEnvelope(stored.Value), "stored value is not an envelope")

	// Metadata the backend needs for querying stays readable.
	assert.Equal(t, "k", stored.Key)
	assert.Equal(t, "text/plain", stored.ContentType)

	got, err := enc.Get(ctx, "ns", "k")
	require.NoError(t, err)
	assert.Equal(t, secret, got.Value)
}

// Put must hand back the plaintext it was given, not the sealed bytes.
func TestPutReturnsPlaintext(t *testing.T) {
	ctx := context.Background()
	enc, _ := newPair(t, ring(t, "k1"), kv.EncryptOptions{})

	rec, err := enc.Put(ctx, "ns", "k", kv.PutOptions{Value: []byte("hello")})
	require.NoError(t, err)
	assert.Equal(t, []byte("hello"), rec.Value)
}

// Put must not mutate the caller's slice into ciphertext.
func TestPutDoesNotMutateCallerSlice(t *testing.T) {
	ctx := context.Background()
	enc, _ := newPair(t, ring(t, "k1"), kv.EncryptOptions{})

	caller := []byte("original")
	_, err := enc.Put(ctx, "ns", "k", kv.PutOptions{Value: caller})
	require.NoError(t, err)
	assert.Equal(t, []byte("original"), caller)
}

func TestMultiGetAndScanDecrypt(t *testing.T) {
	ctx := context.Background()
	enc, _ := newPair(t, ring(t, "k1"), kv.EncryptOptions{})

	want := map[string]string{"a": "alpha", "b": "bravo", "c": "charlie"}
	for k, v := range want {
		_, err := enc.Put(ctx, "ns", k, kv.PutOptions{Value: []byte(v)})
		require.NoError(t, err)
	}

	recs, err := enc.MultiGet(ctx, "ns", []string{"a", "b", "c"})
	require.NoError(t, err)
	require.Len(t, recs, 3)
	for _, r := range recs {
		assert.Equal(t, want[r.Key], string(r.Value), "MultiGet key %q", r.Key)
	}

	got := map[string]string{}
	err = enc.Scan(ctx, "ns", kv.ScanOptions{IncludeValues: true}, func(r kv.Record) error {
		got[r.Key] = string(r.Value)
		return nil
	})
	require.NoError(t, err)
	assert.Equal(t, want, got)
}

// Scan without values must not trip the decrypt path.
func TestScanWithoutValues(t *testing.T) {
	ctx := context.Background()
	enc, _ := newPair(t, ring(t, "k1"), kv.EncryptOptions{})
	_, err := enc.Put(ctx, "ns", "k", kv.PutOptions{Value: []byte("v")})
	require.NoError(t, err)

	n := 0
	err = enc.Scan(ctx, "ns", kv.ScanOptions{IncludeValues: false}, func(r kv.Record) error {
		n++
		assert.Empty(t, r.Value)
		return nil
	})
	require.NoError(t, err)
	assert.Equal(t, 1, n)
}

// A nil value and an empty value must both round-trip; neither was sealed, and
// Decrypt rejects zero-length input.
func TestNilAndEmptyValues(t *testing.T) {
	ctx := context.Background()
	enc, _ := newPair(t, ring(t, "k1"), kv.EncryptOptions{})

	_, err := enc.Put(ctx, "ns", "nil", kv.PutOptions{Value: nil})
	require.NoError(t, err)
	got, err := enc.Get(ctx, "ns", "nil")
	require.NoError(t, err)
	assert.Empty(t, got.Value)

	_, err = enc.Put(ctx, "ns", "empty", kv.PutOptions{Value: []byte{}})
	require.NoError(t, err)
	got, err = enc.Get(ctx, "ns", "empty")
	require.NoError(t, err)
	assert.Empty(t, got.Value)
}

// AAD binding: ciphertext copied to another key or namespace must not open,
// which is the protection against an attacker with raw substrate access
// relocating values.
func TestRelocatedCiphertextFailsToOpen(t *testing.T) {
	ctx := context.Background()
	enc, raw := newPair(t, ring(t, "k1"), kv.EncryptOptions{})

	_, err := enc.Put(ctx, "ns-a", "key1", kv.PutOptions{Value: []byte("secret")})
	require.NoError(t, err)
	stored, err := raw.Get(ctx, "ns-a", "key1")
	require.NoError(t, err)

	// Same key, different namespace.
	_, err = raw.Put(ctx, "ns-b", "key1", kv.PutOptions{Value: stored.Value})
	require.NoError(t, err)
	_, err = enc.Get(ctx, "ns-b", "key1")
	require.ErrorIs(t, err, encryption.ErrDecrypt)

	// Same namespace, different key.
	_, err = raw.Put(ctx, "ns-a", "key2", kv.PutOptions{Value: stored.Value})
	require.NoError(t, err)
	_, err = enc.Get(ctx, "ns-a", "key2")
	require.ErrorIs(t, err, encryption.ErrDecrypt)
}

// The namespace length is varint-prefixed precisely so these two locations
// don't collide into the same associated data.
func TestAmbiguousNamespaceKeySplitIsBound(t *testing.T) {
	ctx := context.Background()
	enc, raw := newPair(t, ring(t, "k1"), kv.EncryptOptions{})

	_, err := enc.Put(ctx, "ns", "akey", kv.PutOptions{Value: []byte("secret")})
	require.NoError(t, err)
	stored, err := raw.Get(ctx, "ns", "akey")
	require.NoError(t, err)

	_, err = raw.Put(ctx, "nsa", "key", kv.PutOptions{Value: stored.Value})
	require.NoError(t, err)
	_, err = enc.Get(ctx, "nsa", "key")
	require.ErrorIs(t, err, encryption.ErrDecrypt)
}

// Rotation: prepend a new active key; values written under the retired key
// still open, and new writes use the new key.
func TestKeyRotation(t *testing.T) {
	ctx := context.Background()
	raw := memory.New(memory.Options{SweeperInterval: -1})
	t.Cleanup(func() { _ = raw.Close() })

	oldRing := ring(t, "k1")
	before := kv.NewEncryptedDriver(raw, oldRing, kv.EncryptOptions{})
	_, err := before.Put(ctx, "ns", "old", kv.PutOptions{Value: []byte("written before")})
	require.NoError(t, err)

	// ring(t, "k2", "k1") derives k2's secret from position, so build the
	// rotated ring explicitly to keep k1's secret identical to oldRing's.
	rotated, err := encryption.NewKeyring([]encryption.KeySpec{
		{ID: "k2", Secret: bytes.Repeat([]byte{9}, 32)},
		{ID: "k1", Secret: bytes.Repeat([]byte{1}, 32)},
	})
	require.NoError(t, err)
	after := kv.NewEncryptedDriver(raw, rotated, kv.EncryptOptions{})

	got, err := after.Get(ctx, "ns", "old")
	require.NoError(t, err)
	assert.Equal(t, []byte("written before"), got.Value)

	_, err = after.Put(ctx, "ns", "new", kv.PutOptions{Value: []byte("written after")})
	require.NoError(t, err)

	// The retired-key-only ring cannot open what the new active key sealed.
	_, err = before.Get(ctx, "ns", "new")
	require.ErrorIs(t, err, encryption.ErrDecrypt)
}

// Migration path: pre-existing plaintext is readable only when explicitly
// allowed, and is re-sealed on the next write.
func TestAllowPlaintextReads(t *testing.T) {
	ctx := context.Background()

	strict, raw := newPair(t, ring(t, "k1"), kv.EncryptOptions{})
	_, err := raw.Put(ctx, "ns", "legacy", kv.PutOptions{Value: []byte("pre-existing")})
	require.NoError(t, err)

	_, err = strict.Get(ctx, "ns", "legacy")
	require.ErrorIs(t, err, encryption.ErrDecrypt, "strict mode must reject unsealed values")

	lenient := kv.NewEncryptedDriver(raw, ring(t, "k1"), kv.EncryptOptions{AllowPlaintextReads: true})
	got, err := lenient.Get(ctx, "ns", "legacy")
	require.NoError(t, err)
	assert.Equal(t, []byte("pre-existing"), got.Value)

	// Rewriting seals it, after which strict mode can read it too.
	_, err = lenient.Put(ctx, "ns", "legacy", kv.PutOptions{Value: got.Value})
	require.NoError(t, err)
	stored, err := raw.Get(ctx, "ns", "legacy")
	require.NoError(t, err)
	assert.True(t, encryption.IsEnvelope(stored.Value))

	got, err = strict.Get(ctx, "ns", "legacy")
	require.NoError(t, err)
	assert.Equal(t, []byte("pre-existing"), got.Value)
}

// Tampering with stored bytes must surface as an error, never as silent
// corruption handed to the caller.
func TestTamperedCiphertextFails(t *testing.T) {
	ctx := context.Background()
	enc, raw := newPair(t, ring(t, "k1"), kv.EncryptOptions{})

	_, err := enc.Put(ctx, "ns", "k", kv.PutOptions{Value: []byte("secret")})
	require.NoError(t, err)
	stored, err := raw.Get(ctx, "ns", "k")
	require.NoError(t, err)

	tampered := bytes.Clone(stored.Value)
	tampered[len(tampered)-1] ^= 0x01
	_, err = raw.Put(ctx, "ns", "k", kv.PutOptions{Value: tampered})
	require.NoError(t, err)

	_, err = enc.Get(ctx, "ns", "k")
	require.ErrorIs(t, err, encryption.ErrDecrypt)
}

// The wrapper must keep exposing the optional Size capability, or encrypted
// namespaces disappear from the item-count gauge and admin introspection.
func TestSizeCapabilityIsForwarded(t *testing.T) {
	ctx := context.Background()
	enc, _ := newPair(t, ring(t, "k1"), kv.EncryptOptions{})

	sizer, ok := enc.(interface {
		Size(context.Context, string) (int64, error)
	})
	require.True(t, ok, "encrypted driver must still satisfy the namespaceSizer capability")

	for _, k := range []string{"a", "b"} {
		_, err := enc.Put(ctx, "ns", k, kv.PutOptions{Value: []byte("v")})
		require.NoError(t, err)
	}
	n, err := sizer.Size(ctx, "ns")
	require.NoError(t, err)
	assert.Equal(t, int64(2), n)
}

// Close must delegate, so the registry's Bind/BindShared ownership still closes
// the underlying driver exactly once.
func TestCloseDelegates(t *testing.T) {
	raw := memory.New(memory.Options{SweeperInterval: -1})
	enc := kv.NewEncryptedDriver(raw, ring(t, "k1"), kv.EncryptOptions{})
	require.NoError(t, enc.Close())
}
