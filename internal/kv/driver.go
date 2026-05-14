package kv

import (
	"context"
	"errors"
	"time"
)

// ErrNotFound is returned by drivers when a key is absent.
var ErrNotFound = errors.New("kv: not found")

// ErrVersionMismatch is returned when an if_version CAS check fails.
var ErrVersionMismatch = errors.New("kv: version mismatch")

// Record is the canonical in-memory representation of a stored item.
type Record struct {
	Key         string
	Value       []byte
	ContentType string
	Metadata    map[string]string
	Version     uint64
	CreatedAt   time.Time
	ExpiresAt   time.Time // zero = no expiry
}

// PutOptions carries the writable fields of a Put call.
type PutOptions struct {
	Value       []byte
	ContentType string
	Metadata    map[string]string
	TTL         time.Duration // 0 = no expiry
	IfVersion   *uint64       // nil = unconditional write
}

// DeleteOptions carries fields for a conditional delete.
type DeleteOptions struct {
	IfVersion *uint64
}

// ScanOptions narrows a Scan call.
type ScanOptions struct {
	KeyPrefix     string
	Limit         uint32 // 0 = unbounded
	IncludeValues bool
}

// Driver is the contract every KV backend implements.
// Drivers must be safe for concurrent use.
type Driver interface {
	Get(ctx context.Context, namespace, key string) (Record, error)
	Put(ctx context.Context, namespace, key string, opts PutOptions) (Record, error)
	Delete(ctx context.Context, namespace, key string, opts DeleteOptions) (existed bool, err error)
	Scan(ctx context.Context, namespace string, opts ScanOptions, yield func(Record) error) error
	Close() error
}
