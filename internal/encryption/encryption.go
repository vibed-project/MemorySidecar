// Package encryption provides envelope encryption for values at rest.
//
// A Keyring holds one or more AES-256-GCM keys. The first key is active (used
// for new writes); every key is a decryption candidate, so keys rotate by
// prepending a new one and retiring the old once no ciphertext references it.
//
// Envelope format (all binary):
//
//	version(1) | keyTag(4) | nonce(12) | ciphertext+tag
//
// keyTag is the first 4 bytes of SHA-256(key id), so decryption selects the key
// independent of ring order. Callers pass associated data (AAD) — the storage
// namespace and record key — which is authenticated but not stored, binding a
// ciphertext to its location so it can't be relocated across keys or namespaces
// by anyone with raw storage access.
package encryption

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
)

const (
	envelopeVersion = 1
	keyTagLen       = 4
	nonceLen        = 12 // AES-GCM standard nonce
	headerLen       = 1 + keyTagLen + nonceLen
)

// KeySpec is one configured key: an id (for the tag) and its 32 raw bytes.
type KeySpec struct {
	ID     string
	Secret []byte
}

type key struct {
	tag  [keyTagLen]byte
	aead cipher.AEAD
}

// Keyring encrypts with its active key and decrypts with whichever key a
// ciphertext names. The zero value is unusable; build one with NewKeyring.
type Keyring struct {
	active *key
	byTag  map[[keyTagLen]byte]*key
}

func keyTag(id string) [keyTagLen]byte {
	sum := sha256.Sum256([]byte(id))
	var t [keyTagLen]byte
	copy(t[:], sum[:keyTagLen])
	return t
}

// NewKeyring builds a Keyring from specs; the first is the active encryption
// key. Each secret must be exactly 32 bytes (AES-256). Ids must be unique and,
// after tagging, collision-free.
func NewKeyring(specs []KeySpec) (*Keyring, error) {
	if len(specs) == 0 {
		return nil, errors.New("encryption: at least one key required")
	}
	kr := &Keyring{byTag: make(map[[keyTagLen]byte]*key, len(specs))}
	for i, spec := range specs {
		if spec.ID == "" {
			return nil, fmt.Errorf("encryption: key %d has empty id", i)
		}
		if len(spec.Secret) != 32 {
			return nil, fmt.Errorf("encryption: key %q must be 32 bytes, got %d", spec.ID, len(spec.Secret))
		}
		block, err := aes.NewCipher(spec.Secret)
		if err != nil {
			return nil, fmt.Errorf("encryption: key %q: %w", spec.ID, err)
		}
		aead, err := cipher.NewGCM(block)
		if err != nil {
			return nil, fmt.Errorf("encryption: key %q: %w", spec.ID, err)
		}
		k := &key{tag: keyTag(spec.ID), aead: aead}
		if _, dup := kr.byTag[k.tag]; dup {
			return nil, fmt.Errorf("encryption: key %q tag collides with an earlier key", spec.ID)
		}
		kr.byTag[k.tag] = k
		if i == 0 {
			kr.active = k
		}
	}
	return kr, nil
}

// Encrypt seals plaintext with the active key, authenticating aad. A nil
// plaintext returns nil (nothing to protect); an empty non-nil slice is sealed.
func (kr *Keyring) Encrypt(plaintext, aad []byte) ([]byte, error) {
	if plaintext == nil {
		return nil, nil
	}
	nonce := make([]byte, nonceLen)
	if _, err := rand.Read(nonce); err != nil {
		return nil, fmt.Errorf("encryption: nonce: %w", err)
	}
	out := make([]byte, 0, headerLen+len(plaintext)+kr.active.aead.Overhead())
	out = append(out, envelopeVersion)
	out = append(out, kr.active.tag[:]...)
	out = append(out, nonce...)
	return kr.active.aead.Seal(out, nonce, plaintext, aad), nil
}

// ErrDecrypt is returned when a value can't be opened: unknown key, tampering,
// truncation, or a mismatched aad (wrong namespace/key).
var ErrDecrypt = errors.New("encryption: decrypt failed")

// IsEnvelope reports whether b is shaped like something Encrypt produced. It
// checks the version byte and minimum length only — a true result does not mean
// the value will authenticate, just that it is worth trying.
//
// This exists for the migration path: a namespace that already holds plaintext
// can be switched to encrypted writes, and reads can fall back to returning
// values that were never sealed. Callers that enable that fallback accept that
// an attacker with write access to the substrate can strip an envelope and have
// the plaintext served back.
func IsEnvelope(b []byte) bool {
	return len(b) >= headerLen && b[0] == envelopeVersion
}

// Decrypt opens a value produced by Encrypt with the same aad. A nil input
// returns nil, so decrypting an absent value is a no-op.
func (kr *Keyring) Decrypt(envelope, aad []byte) ([]byte, error) {
	if envelope == nil {
		return nil, nil
	}
	if len(envelope) < headerLen || envelope[0] != envelopeVersion {
		return nil, ErrDecrypt
	}
	var tag [keyTagLen]byte
	copy(tag[:], envelope[1:1+keyTagLen])
	k, ok := kr.byTag[tag]
	if !ok {
		return nil, fmt.Errorf("%w: no key for tag %x", ErrDecrypt, tag)
	}
	nonce := envelope[1+keyTagLen : headerLen]
	pt, err := k.aead.Open(nil, nonce, envelope[headerLen:], aad)
	if err != nil {
		return nil, ErrDecrypt
	}
	return pt, nil
}

// DecodeSecret parses a 32-byte key from hex (64 chars) or standard/raw base64.
// Anything that decodes to a different length is rejected here rather than
// deeper in NewKeyring, so an unset env var (which base64-decodes to zero
// bytes) fails with a message that names the real problem.
func DecodeSecret(s string) ([]byte, error) {
	s = strings.TrimSpace(s)
	if len(s) == 64 {
		if b, err := hex.DecodeString(s); err == nil {
			return b, nil
		}
	}
	for _, enc := range []*base64.Encoding{base64.StdEncoding, base64.RawStdEncoding} {
		if b, err := enc.DecodeString(s); err == nil && len(b) == 32 {
			return b, nil
		}
	}
	return nil, errors.New("encryption: secret must be 32-byte hex or base64")
}
