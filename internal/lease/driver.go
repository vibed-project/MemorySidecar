package lease

import (
	"context"
	"errors"
	"time"
)

// ErrAlreadyHeld is returned by Acquire when the key is currently held by
// another live holder and either wait_for is 0 or the wait elapsed.
var ErrAlreadyHeld = errors.New("lease: already held")

// ErrNotHeld is returned by Renew/Release when the (namespace, key, holder_id)
// triple does not match a live lease (wrong holder, expired, or absent).
var ErrNotHeld = errors.New("lease: not held by this holder")

// Lease is the canonical representation of a granted lease.
type Lease struct {
	HolderID   string
	Namespace  string
	Key        string
	AcquiredAt time.Time
	ExpiresAt  time.Time
	Metadata   map[string]string
}

// AcquireOptions carries Acquire's per-call parameters.
type AcquireOptions struct {
	TTL      time.Duration
	WaitFor  time.Duration // 0 = fail fast
	Metadata map[string]string
}

// Driver is the contract every lease backend implements. Implementations MUST
// be safe for concurrent use; acquisition MUST be atomic with respect to
// expiry: if a held lease's deadline passes, a concurrent Acquire MAY take
// over the key but only if it does so atomically (no two acquirers can both
// succeed for the same (namespace, key) at the same instant).
type Driver interface {
	Acquire(ctx context.Context, namespace, key string, opts AcquireOptions) (Lease, error)
	Renew(ctx context.Context, namespace, key, holderID string, ttl time.Duration) (Lease, error)
	Release(ctx context.Context, namespace, key, holderID string) (existed bool, err error)
	Inspect(ctx context.Context, namespace, key string) (lease Lease, held bool, err error)
	// List returns every currently-held (unexpired) lease in namespace, ordered
	// by key.
	List(ctx context.Context, namespace string) ([]Lease, error)
	Close() error
}
