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
	// Access instrumentation (U5). Only maintained when the namespace opts in
	// via AccessPolicy; otherwise both stay zero and reads never write.
	LastAccessed time.Time
	AccessCount  uint64
}

// AccessPolicy is a namespace's opt-in cache-tier behaviour (U5). The zero
// value is fully disabled, so a namespace that doesn't configure it pays no
// per-read cost. Supported by the in-memory driver only.
type AccessPolicy struct {
	// Track records LastAccessed/AccessCount on each Get.
	Track bool
	// SlideTTL, when > 0, extends a TTL'd record's expiry to now+SlideTTL on
	// each Get (read-through residency). Records without a TTL are unaffected.
	SlideTTL time.Duration
	// Capacity, when > 0, caps live keys per namespace; on Put over capacity the
	// coldest keys are evicted by heat.
	Capacity int
	// HeatHalfLife tunes the eviction heat decay (heat = access_count *
	// 2^(-age/half_life)); 0 uses a default.
	HeatHalfLife time.Duration
}

// Enabled reports whether the policy does anything, so callers can skip the
// write path entirely when it's off.
func (p AccessPolicy) Enabled() bool {
	return p.Track || p.SlideTTL > 0 || p.Capacity > 0
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
