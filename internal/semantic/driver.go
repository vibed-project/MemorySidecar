package semantic

import (
	"context"
	"errors"
	"time"
)

// ErrNotFound is returned by drivers when a record id is absent.
var ErrNotFound = errors.New("semantic: not found")

// ErrVersionMismatch is returned when an Upsert's if_version CAS check fails
// (ADR-0003, U4).
var ErrVersionMismatch = errors.New("semantic: version mismatch")

// Record is the canonical in-memory representation of a stored vector.
type Record struct {
	// Tenant scopes the record for storage isolation. Set by the service from
	// the caller's capability tenant when tenant isolation is enabled, else "".
	// Part of the record's identity: (tenant, id) is unique, so two tenants may
	// reuse the same id.
	Tenant    string
	ID        string
	Content   string
	Payload   []byte
	Vector    []float32
	Metadata  map[string]string
	CreatedAt time.Time

	// Lifecycle (ADR-0003). Zero values mean "live and open-ended".
	// ValidFrom is when the fact becomes true (application/valid time);
	// drivers default it to now() on write when zero. ValidTo is the exclusive
	// upper bound (zero = open-ended). DeletedAt is a soft-delete tombstone
	// (zero = live).
	ValidFrom time.Time
	ValidTo   time.Time
	DeletedAt time.Time

	// Provenance & revisability (ADR-0003, U2). Supersedes lists ids this
	// record revises; on Upsert drivers set those records' ValidTo to this
	// record's ValidFrom (localized invalidation, self-references ignored).
	// Source is an opaque provenance handle, stored and returned verbatim.
	Supersedes []string
	Source     string

	// Optimistic concurrency (ADR-0003, U4). Version is the monotonic per-id
	// revision counter, set by drivers on Upsert and Search. IfVersion is an
	// optional write precondition: when non-nil, Upsert fails with
	// ErrVersionMismatch unless it equals the current version (0 = must not yet
	// exist). IfVersion is input-only and never stored.
	Version   uint64
	IfVersion *uint64
}

// Hit is a single search result.
type Hit struct {
	Record Record
	Score  float32
}

// SearchOptions narrows a Search call.
type SearchOptions struct {
	// Tenant scopes the search to one tenant's records (storage isolation).
	// Empty when tenant isolation is disabled — matches records stored with the
	// empty tenant, i.e. the whole namespace.
	Tenant      string
	QueryVector []float32
	// QueryText is the raw query for the sparse lane (SPARSE/HYBRID). Empty for
	// pure dense search.
	QueryText string
	// Mode selects the retrieval lanes (Q4). Zero value is ModeDense.
	Mode SearchMode
	// RerankCandidateK is the per-lane candidate depth for SPARSE/HYBRID before
	// fusion. 0 uses DefaultRerankCandidateK.
	RerankCandidateK uint32
	TopK             uint32
	Filter           map[string]string
	IncludePayload   bool
	IncludeVector    bool
	// IDsOnly makes Search return only each hit's record id and score, skipping
	// content, payload, vector, and metadata (and the storage load they cost).
	// It overrides IncludePayload/IncludeVector. See ADR-0002 §8 / plan Q5.
	IDsOnly bool
	// Predicates are structured metadata filters (Q3), ANDed with each other and
	// with Filter. Empty = no additional filtering.
	Predicates []FieldPredicate
	// CreatedAfter / CreatedBefore bound the record creation time (Q2),
	// exclusive on each side; a zero value means unbounded. Applied before
	// ranking — no recency scoring.
	CreatedAfter  time.Time
	CreatedBefore time.Time

	// Lifecycle-aware read (ADR-0003). By default Search returns only records
	// that are live and valid as of AsOf (or now() when AsOf is zero).
	// AsOf evaluates validity at a past instant for point-in-time recall.
	AsOf time.Time
	// IncludeInvalidated bypasses the lifecycle filter and also returns
	// tombstoned, expired, and not-yet-valid records. Metadata filtering still
	// applies.
	IncludeInvalidated bool
}

// DeleteOptions controls a Delete call.
type DeleteOptions struct {
	// Tenant scopes the delete so a tenant can only remove its own record with
	// the given id (storage isolation). Empty = the un-scoped namespace.
	Tenant string
	// Hard physically removes the row. Default (false) is a soft delete that
	// sets DeletedAt=now() and retains the row (ADR-0003).
	Hard bool
}

// ExpireAction selects what Expire does to each matched record (ADR-0003, U3).
type ExpireAction uint8

const (
	// ExpireActionUnspecified is the zero value; drivers must reject it.
	ExpireActionUnspecified ExpireAction = iota
	// ExpireInvalidate sets ValidTo=now() on live records.
	ExpireInvalidate
	// ExpireSoftDelete sets DeletedAt=now(), retaining the row.
	ExpireSoftDelete
	// ExpireHardDelete physically removes the row.
	ExpireHardDelete
)

// ExpireOptions configures a bounded, filter-scoped lifecycle operation.
type ExpireOptions struct {
	// Tenant scopes the operation to one tenant's records (storage isolation).
	// Empty = the un-scoped namespace.
	Tenant string
	// Filter is the exact-match metadata predicate; empty matches every record.
	Filter map[string]string
	Action ExpireAction
	// MaxRows caps the affected set. Must be > 0 so maintenance stays localized.
	MaxRows uint32
}

// Driver is the contract every semantic backend implements. Implementations
// must be safe for concurrent use.
type Driver interface {
	Upsert(ctx context.Context, records []Record) error
	Search(ctx context.Context, opts SearchOptions) ([]Hit, error)
	Delete(ctx context.Context, id string, opts DeleteOptions) (existed bool, err error)
	// Expire applies opts.Action to at most opts.MaxRows records matching
	// opts.Filter, returning the number affected (ADR-0003, U3).
	Expire(ctx context.Context, opts ExpireOptions) (affected uint64, err error)
	Close() error
}

// Dimensions returns the expected vector dimension for the bound driver
// instance. Implementations should embed this so the service can validate
// queries before they hit the driver.
type Dimensioned interface {
	Dimensions() int
}
