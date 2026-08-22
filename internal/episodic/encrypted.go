package episodic

import (
	"context"
	"encoding/binary"
	"fmt"

	"github.com/vibed-project/mindD/internal/encryption"
)

// EncryptOptions tunes an encrypting driver.
type EncryptOptions struct {
	// AllowPlaintextReads returns payloads that are not well-formed envelopes
	// as-is instead of failing, so a namespace holding pre-existing plaintext
	// can be migrated in place. See config.EncryptionConfig for the caveat.
	AllowPlaintextReads bool
}

// encryptedDriver seals Event.Payload on the way to the backend and opens it on
// the way back. Everything else stays plaintext by necessity: cursors and
// timestamps define the ordering and retention windows, Type/Role/SessionID are
// index-backed Range predicates, DedupKey is a uniqueness constraint, and
// Supersedes is a list of ids the driver tombstones.
//
// It embeds Driver so Expire and Close pass straight through, which also means
// a new Driver method is inherited unencrypted by default — if a future method
// carries payload, override it here.
type encryptedDriver struct {
	Driver
	kr             *encryption.Keyring
	allowPlaintext bool
}

// NewEncryptedDriver wraps d so payloads stored in it are encrypted at rest.
//
// One wrapper is expected per namespace even when several namespaces share a
// backing driver: the wrapper is stateless beyond the keyring, and Close
// delegates, so the registry's Bind/BindShared ownership rules still close the
// underlying driver exactly once.
func NewEncryptedDriver(d Driver, kr *encryption.Keyring, opts EncryptOptions) Driver {
	return &encryptedDriver{Driver: d, kr: kr, allowPlaintext: opts.AllowPlaintextReads}
}

// aadBlock domain-separates episodic ciphertext from other blocks sharing a
// keyring, so a payload lifted out of one block can't be opened as another's.
const aadBlock = "episodic"

// payloadAAD binds a ciphertext to its namespace. Under tenant isolation the
// namespace is already tenant-qualified, so this binds the tenant too.
//
// Unlike kv, the binding stops at the namespace: an event's id and cursor are
// assigned by the driver during Append, so they do not exist yet at the moment
// the payload is sealed. Cross-namespace and cross-block relocation are
// prevented; swapping two payloads *within* one namespace is not detectable
// here, and is left to the append-only log's own integrity.
func payloadAAD(namespace string) []byte {
	aad := make([]byte, 0, 2*binary.MaxVarintLen64+len(aadBlock)+len(namespace))
	aad = binary.AppendUvarint(aad, uint64(len(aadBlock)))
	aad = append(aad, aadBlock...)
	aad = binary.AppendUvarint(aad, uint64(len(namespace)))
	aad = append(aad, namespace...)
	return aad
}

// open decrypts ev.Payload in place.
//
// An empty payload is passed through untouched: Encrypt(nil) stores nil, and
// the Postgres column is NOT NULL so a nil write reads back as a zero-length
// slice. Neither was ever sealed, and Decrypt would reject both.
func (e *encryptedDriver) open(namespace string, ev *Event) error {
	if len(ev.Payload) == 0 {
		return nil
	}
	if e.allowPlaintext && !encryption.IsEnvelope(ev.Payload) {
		return nil
	}
	plaintext, err := e.kr.Decrypt(ev.Payload, payloadAAD(namespace))
	if err != nil {
		return fmt.Errorf("episodic: decrypt event %q: %w", ev.ID, err)
	}
	ev.Payload = plaintext
	return nil
}

func (e *encryptedDriver) Append(ctx context.Context, namespace string, opts AppendOptions) (Event, error) {
	sealed, err := e.kr.Encrypt(opts.Payload, payloadAAD(namespace))
	if err != nil {
		return Event{}, fmt.Errorf("episodic: encrypt payload: %w", err)
	}
	opts.Payload = sealed // opts is a copy; the caller's slice is untouched

	ev, err := e.Driver.Append(ctx, namespace, opts)
	if err != nil {
		return Event{}, err
	}
	// Drivers echo the stored (sealed) payload back, and a deduplicated Append
	// returns the previously stored event — open either so Append and Range agree.
	if err := e.open(namespace, &ev); err != nil {
		return Event{}, err
	}
	return ev, nil
}

func (e *encryptedDriver) Range(ctx context.Context, namespace string, opts RangeOptions, yield func(Event) error) error {
	return e.Driver.Range(ctx, namespace, opts, func(ev Event) error {
		// ev is a copy, so decrypting in place can't disturb the driver.
		if err := e.open(namespace, &ev); err != nil {
			return err
		}
		return yield(ev)
	})
}

func (e *encryptedDriver) Tail(ctx context.Context, namespace string, opts TailOptions, yield func(Event) error) error {
	return e.Driver.Tail(ctx, namespace, opts, func(ev Event) error {
		if err := e.open(namespace, &ev); err != nil {
			return err
		}
		return yield(ev)
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
		return 0, fmt.Errorf("episodic: driver %T does not report size", e.Driver)
	}
	return sizer.Size(ctx, namespace)
}
