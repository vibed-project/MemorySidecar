package encryption

import (
	"bytes"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"strings"
	"testing"
)

func testKey(b byte) []byte { return bytes.Repeat([]byte{b}, 32) }

func newTestRing(t *testing.T, specs ...KeySpec) *Keyring {
	t.Helper()
	kr, err := NewKeyring(specs)
	if err != nil {
		t.Fatalf("NewKeyring: %v", err)
	}
	return kr
}

func TestRoundTrip(t *testing.T) {
	kr := newTestRing(t, KeySpec{ID: "k1", Secret: testKey(1)})
	aad := []byte("ns/key")
	plaintext := []byte("hello world")

	env, err := kr.Encrypt(plaintext, aad)
	if err != nil {
		t.Fatalf("Encrypt: %v", err)
	}
	if bytes.Contains(env, plaintext) {
		t.Fatal("ciphertext contains the plaintext")
	}
	if !IsEnvelope(env) {
		t.Fatal("IsEnvelope false for a real envelope")
	}

	got, err := kr.Decrypt(env, aad)
	if err != nil {
		t.Fatalf("Decrypt: %v", err)
	}
	if !bytes.Equal(got, plaintext) {
		t.Fatalf("round trip: got %q want %q", got, plaintext)
	}
}

// Each Encrypt must use a fresh nonce, or AES-GCM leaks catastrophically.
func TestEncryptIsNonDeterministic(t *testing.T) {
	kr := newTestRing(t, KeySpec{ID: "k1", Secret: testKey(1)})
	a, err := kr.Encrypt([]byte("same"), nil)
	if err != nil {
		t.Fatalf("Encrypt: %v", err)
	}
	b, err := kr.Encrypt([]byte("same"), nil)
	if err != nil {
		t.Fatalf("Encrypt: %v", err)
	}
	if bytes.Equal(a, b) {
		t.Fatal("two seals of the same plaintext are identical — nonce reuse")
	}
}

func TestNilAndEmpty(t *testing.T) {
	kr := newTestRing(t, KeySpec{ID: "k1", Secret: testKey(1)})

	env, err := kr.Encrypt(nil, nil)
	if err != nil {
		t.Fatalf("Encrypt(nil): %v", err)
	}
	if env != nil {
		t.Fatalf("Encrypt(nil) = %v, want nil", env)
	}
	got, err := kr.Decrypt(nil, nil)
	if err != nil || got != nil {
		t.Fatalf("Decrypt(nil) = %v, %v; want nil, nil", got, err)
	}

	// An empty but non-nil plaintext is sealed, and opens back to empty.
	env, err = kr.Encrypt([]byte{}, nil)
	if err != nil {
		t.Fatalf("Encrypt(empty): %v", err)
	}
	if len(env) == 0 {
		t.Fatal("Encrypt(empty) produced no envelope")
	}
	got, err = kr.Decrypt(env, nil)
	if err != nil {
		t.Fatalf("Decrypt(empty envelope): %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("Decrypt(empty envelope) = %q, want empty", got)
	}
}

// AAD binding is what stops a ciphertext being relocated to another
// namespace/key by someone with raw storage access.
func TestAADMismatchFails(t *testing.T) {
	kr := newTestRing(t, KeySpec{ID: "k1", Secret: testKey(1)})
	env, err := kr.Encrypt([]byte("secret"), []byte("ns-a/key1"))
	if err != nil {
		t.Fatalf("Encrypt: %v", err)
	}
	for _, aad := range [][]byte{[]byte("ns-b/key1"), []byte("ns-a/key2"), nil} {
		if _, err := kr.Decrypt(env, aad); !errors.Is(err, ErrDecrypt) {
			t.Fatalf("Decrypt with aad %q = %v, want ErrDecrypt", aad, err)
		}
	}
}

func TestTamperAndTruncateFail(t *testing.T) {
	kr := newTestRing(t, KeySpec{ID: "k1", Secret: testKey(1)})
	aad := []byte("ns/key")
	env, err := kr.Encrypt([]byte("secret payload"), aad)
	if err != nil {
		t.Fatalf("Encrypt: %v", err)
	}

	// Flipping any single byte must break authentication.
	for i := range env {
		bad := bytes.Clone(env)
		bad[i] ^= 0x01
		if _, err := kr.Decrypt(bad, aad); !errors.Is(err, ErrDecrypt) {
			t.Fatalf("byte %d flipped: Decrypt = %v, want ErrDecrypt", i, err)
		}
	}

	for _, n := range []int{0, 1, headerLen - 1, headerLen, len(env) - 1} {
		if _, err := kr.Decrypt(env[:n], aad); !errors.Is(err, ErrDecrypt) {
			t.Fatalf("truncated to %d: Decrypt = %v, want ErrDecrypt", n, err)
		}
	}
}

// Rotation: a new key is prepended, and ciphertext written under the retired
// key must still open because every key stays a decryption candidate.
func TestRotation(t *testing.T) {
	old := newTestRing(t, KeySpec{ID: "k1", Secret: testKey(1)})
	aad := []byte("ns/key")
	legacy, err := old.Encrypt([]byte("written before rotation"), aad)
	if err != nil {
		t.Fatalf("Encrypt: %v", err)
	}

	rotated := newTestRing(t,
		KeySpec{ID: "k2", Secret: testKey(2)},
		KeySpec{ID: "k1", Secret: testKey(1)},
	)
	got, err := rotated.Decrypt(legacy, aad)
	if err != nil {
		t.Fatalf("Decrypt legacy after rotation: %v", err)
	}
	if string(got) != "written before rotation" {
		t.Fatalf("got %q", got)
	}

	// New writes use the new active key, which the old ring cannot open.
	fresh, err := rotated.Encrypt([]byte("after"), aad)
	if err != nil {
		t.Fatalf("Encrypt: %v", err)
	}
	if _, err := old.Decrypt(fresh, aad); !errors.Is(err, ErrDecrypt) {
		t.Fatalf("old ring opened a ciphertext from the new key: %v", err)
	}
}

// Retiring a key must fail loudly, not silently return garbage.
func TestRetiredKeyIsUnopenable(t *testing.T) {
	old := newTestRing(t, KeySpec{ID: "k1", Secret: testKey(1)})
	env, err := old.Encrypt([]byte("orphan"), nil)
	if err != nil {
		t.Fatalf("Encrypt: %v", err)
	}
	only2 := newTestRing(t, KeySpec{ID: "k2", Secret: testKey(2)})
	_, err = only2.Decrypt(env, nil)
	if !errors.Is(err, ErrDecrypt) {
		t.Fatalf("Decrypt = %v, want ErrDecrypt", err)
	}
	if !strings.Contains(err.Error(), "no key for tag") {
		t.Fatalf("error should name the missing tag, got %v", err)
	}
}

// A key id must select the key regardless of its position in the ring.
func TestKeySelectionIsOrderIndependent(t *testing.T) {
	a := newTestRing(t, KeySpec{ID: "k1", Secret: testKey(1)}, KeySpec{ID: "k2", Secret: testKey(2)})
	b := newTestRing(t, KeySpec{ID: "k2", Secret: testKey(2)}, KeySpec{ID: "k1", Secret: testKey(1)})
	env, err := a.Encrypt([]byte("payload"), nil)
	if err != nil {
		t.Fatalf("Encrypt: %v", err)
	}
	got, err := b.Decrypt(env, nil)
	if err != nil {
		t.Fatalf("Decrypt with reordered ring: %v", err)
	}
	if string(got) != "payload" {
		t.Fatalf("got %q", got)
	}
}

func TestNewKeyringRejectsBadSpecs(t *testing.T) {
	tests := []struct {
		name  string
		specs []KeySpec
		want  string
	}{
		{"no keys", nil, "at least one key"},
		{"empty id", []KeySpec{{ID: "", Secret: testKey(1)}}, "empty id"},
		{"short secret", []KeySpec{{ID: "k", Secret: []byte("too short")}}, "must be 32 bytes"},
		{"nil secret", []KeySpec{{ID: "k", Secret: nil}}, "must be 32 bytes"},
		{
			"duplicate id",
			[]KeySpec{{ID: "k", Secret: testKey(1)}, {ID: "k", Secret: testKey(2)}},
			"collides",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := NewKeyring(tt.specs); err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("NewKeyring = %v, want error containing %q", err, tt.want)
			}
		})
	}
}

func TestIsEnvelope(t *testing.T) {
	kr := newTestRing(t, KeySpec{ID: "k1", Secret: testKey(1)})
	env, err := kr.Encrypt([]byte("x"), nil)
	if err != nil {
		t.Fatalf("Encrypt: %v", err)
	}
	if !IsEnvelope(env) {
		t.Fatal("IsEnvelope(envelope) = false")
	}
	for _, b := range [][]byte{
		nil,
		{},
		[]byte("plaintext"),                  // long enough, wrong version byte
		bytes.Repeat([]byte{0}, headerLen-1), // right version, too short
	} {
		if IsEnvelope(b) {
			t.Fatalf("IsEnvelope(%q) = true, want false", b)
		}
	}
}

func TestDecodeSecret(t *testing.T) {
	raw := testKey(7)
	for _, tt := range []struct {
		name string
		in   string
	}{
		{"hex", hex.EncodeToString(raw)},
		{"hex with whitespace", "  " + hex.EncodeToString(raw) + "\n"},
		{"std base64", base64.StdEncoding.EncodeToString(raw)},
		{"raw base64", base64.RawStdEncoding.EncodeToString(raw)},
	} {
		t.Run(tt.name, func(t *testing.T) {
			got, err := DecodeSecret(tt.in)
			if err != nil {
				t.Fatalf("DecodeSecret: %v", err)
			}
			if !bytes.Equal(got, raw) {
				t.Fatalf("got %x want %x", got, raw)
			}
		})
	}

	for _, bad := range []string{"", "nothex", "zz"} {
		if _, err := DecodeSecret(bad); err == nil {
			t.Fatalf("DecodeSecret(%q) succeeded, want error", bad)
		}
	}
}
