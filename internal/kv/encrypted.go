package kv

import (
	"context"
	"encoding/binary"
	"fmt"

	"github.com/vibed-project/mindD/internal/encryption"
)

// EncryptOptions tunes an encrypting driver.
type EncryptOptions struct {
	// AllowPlaintextReads returns values that are not well-formed envelopes
	// as-is instead of failing, so a namespace holding pre-existing plaintext
	// can be migrated in place. See config.EncryptionConfig for the caveat.
	AllowPlaintextReads bool
}

// encryptedDriver seals Record.Value on the way to the backend and opens it on
// the way back. Only the value is touched: keys, metadata, versions and expiry
// stay plaintext because the backend orders, filters and sweeps on them.
//
// It embeds Driver so unrelated methods (Delete, Close) pass straight through,
// which also means a new Driver method is inherited unencrypted by default —
// if a future method carries payload, override it here.
type encryptedDriver struct {
	Driver
	kr             *encryption.Keyring
	allowPlaintext bool
}

// NewEncryptedDriver wraps d so values stored in it are encrypted at rest.
//
// One wrapper is expected per namespace even when several namespaces share a
// backing driver: the wrapper is stateless beyond the keyring, and Close
// delegates, so the registry's Bind/BindShared ownership rules still close the
// underlying driver exactly once.
func NewEncryptedDriver(d Driver, kr *encryption.Keyring, opts EncryptOptions) Driver {
	return &encryptedDriver{Driver: d, kr: kr, allowPlaintext: opts.AllowPlaintextReads}
}

// aadBlock domain-separates kv ciphertext from other blocks sharing a keyring,
// so a value lifted out of one block can't be opened as another's.
const aadBlock = "kv"

// valueAAD binds a ciphertext to its exact location. Each variable-length part
// is varint-prefixed so ("ns", "akey") and ("nsa", "key") can't produce the
// same associated data — without that, a value could be relocated between
// namespaces whose names happen to concatenate alike. Under tenant isolation
// the namespace is already tenant-qualified, so this binds the tenant too.
func valueAAD(namespace, key string) []byte {
	aad := make([]byte, 0, 3*binary.MaxVarintLen64+len(aadBlock)+len(namespace)+len(key))
	aad = binary.AppendUvarint(aad, uint64(len(aadBlock)))
	aad = append(aad, aadBlock...)
	aad = binary.AppendUvarint(aad, uint64(len(namespace)))
	aad = append(aad, namespace...)
	aad = binary.AppendUvarint(aad, uint64(len(key)))
	aad = append(aad, key...)
	return aad
}

func (e *encryptedDriver) seal(namespace, key string, plaintext []byte) ([]byte, error) {
	sealed, err := e.kr.Encrypt(plaintext, valueAAD(namespace, key))
	if err != nil {
		return nil, fmt.Errorf("kv: encrypt %q: %w", key, err)
	}
	return sealed, nil
}

// open decrypts rec.Value in place, using key rather than rec.Key so callers
// that know the requested key don't depend on the driver echoing it back.
//
// An empty value is passed through untouched: Encrypt(nil) stores nil, and the
// Postgres column is NOT NULL so a nil write reads back as a zero-length slice.
// Neither was ever sealed, and Decrypt would reject both.
func (e *encryptedDriver) open(namespace, key string, rec *Record) error {
	if len(rec.Value) == 0 {
		return nil
	}
	if e.allowPlaintext && !encryption.IsEnvelope(rec.Value) {
		return nil
	}
	plaintext, err := e.kr.Decrypt(rec.Value, valueAAD(namespace, key))
	if err != nil {
		return fmt.Errorf("kv: decrypt %q: %w", key, err)
	}
	rec.Value = plaintext
	return nil
}

func (e *encryptedDriver) Get(ctx context.Context, namespace, key string) (Record, error) {
	rec, err := e.Driver.Get(ctx, namespace, key)
	if err != nil {
		return Record{}, err
	}
	if err := e.open(namespace, key, &rec); err != nil {
		return Record{}, err
	}
	return rec, nil
}

func (e *encryptedDriver) MultiGet(ctx context.Context, namespace string, keys []string) ([]Record, error) {
	recs, err := e.Driver.MultiGet(ctx, namespace, keys)
	if err != nil {
		return nil, err
	}
	for i := range recs {
		if err := e.open(namespace, recs[i].Key, &recs[i]); err != nil {
			return nil, err
		}
	}
	return recs, nil
}

func (e *encryptedDriver) Put(ctx context.Context, namespace, key string, opts PutOptions) (Record, error) {
	sealed, err := e.seal(namespace, key, opts.Value)
	if err != nil {
		return Record{}, err
	}
	opts.Value = sealed // opts is a copy; the caller's slice is untouched

	rec, err := e.Driver.Put(ctx, namespace, key, opts)
	if err != nil {
		return Record{}, err
	}
	// Drivers echo the stored (sealed) value back; open it so Put and Get agree.
	if err := e.open(namespace, key, &rec); err != nil {
		return Record{}, err
	}
	return rec, nil
}

func (e *encryptedDriver) Scan(ctx context.Context, namespace string, opts ScanOptions, yield func(Record) error) error {
	return e.Driver.Scan(ctx, namespace, opts, func(rec Record) error {
		// rec is a copy, so decrypting in place can't disturb the driver.
		if err := e.open(namespace, rec.Key, &rec); err != nil {
			return err
		}
		return yield(rec)
	})
}

// Size forwards the optional namespaceSizer capability. Without this the
// wrapper would fail the type assertion in Registry.NamespaceItems and
// encrypted namespaces would silently vanish from the item-count gauge and
// from admin introspection. Reporting an error for a driver that can't size
// itself matches what the registry already expects from such drivers.
func (e *encryptedDriver) Size(ctx context.Context, namespace string) (int64, error) {
	sizer, ok := e.Driver.(namespaceSizer)
	if !ok {
		return 0, fmt.Errorf("kv: driver %T does not report size", e.Driver)
	}
	return sizer.Size(ctx, namespace)
}
