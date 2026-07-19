package episodic

import (
	"context"
	"errors"
	"time"
)

// Event is the canonical in-memory representation of an episodic record.
type Event struct {
	ID        string
	Cursor    uint64
	Timestamp time.Time
	Type      string
	Payload   []byte
	Metadata  map[string]string
	// Role is the speaker/actor (e.g. "user", "assistant", "tool"); SessionID
	// is a conversation grouping key. Both are first-class (R2) so grouping and
	// cross-session assembly don't rely on a metadata convention.
	Role      string
	SessionID string
	// Supersedes lists ids of earlier events this event revises; Append
	// tombstones each named live event (see AppendOptions). Source is an opaque
	// provenance handle stored and returned verbatim (U2).
	Supersedes []string
	Source     string
	// DeletedAt is the soft-delete tombstone; a zero value means the event is
	// live. A tombstoned event is retained (the log stays append-only) and only
	// returned by Range when IncludeDeleted is set. Set by a superseding Append
	// or by Expire.
	DeletedAt time.Time
}

// AppendOptions carries the writable fields of an Append call.
type AppendOptions struct {
	Type      string
	Payload   []byte
	Metadata  map[string]string
	Role      string
	SessionID string
	// DedupKey is an optional client idempotency key. When non-empty, the first
	// Append for a (namespace, DedupKey) writes the event and any later Append
	// with the same key is a no-op returning the already-stored event (same id
	// and cursor). Empty = no dedup.
	DedupKey string
	// Supersedes names earlier live events in the same namespace to tombstone
	// (DeletedAt = now) in the same transaction as this write; self-references
	// and already-tombstoned ids are ignored. Source is stored on the event.
	Supersedes []string
	Source     string
}

// RangeOptions narrows a Range scan.
type RangeOptions struct {
	AfterCursor  uint64 // exclusive lower bound; 0 = from start
	BeforeCursor uint64 // exclusive upper bound; 0 = no upper
	Limit        uint32 // 0 = unbounded
	Reverse      bool
	// Optional event-timestamp window, combined (AND) with the cursor bounds.
	// AfterTime is an exclusive lower bound, BeforeTime an exclusive upper
	// bound; a zero value means unbounded on that side.
	AfterTime  time.Time
	BeforeTime time.Time
	// IncludeDeleted also returns tombstoned events. Default (false) returns
	// only live events (DeletedAt zero).
	IncludeDeleted bool
	// Equality predicates on first-class fields, ANDed with the window and with
	// each other. An empty string disables that predicate. Together they turn
	// "reconstruct session X" into a bounded, index-backed scan.
	SessionID string
	Role      string
	Type      string
}

// ExpireAction selects what Expire does to each matched event.
type ExpireAction uint8

const (
	// ExpireActionUnspecified is the zero value; drivers must reject it.
	ExpireActionUnspecified ExpireAction = iota
	// ExpireSoftDelete sets DeletedAt = now on live events, retaining the row.
	ExpireSoftDelete
	// ExpireHardDelete physically removes the row.
	ExpireHardDelete
)

// ExpireOptions bounds a retention sweep. The window is a strict upper bound
// (BeforeCursor and/or BeforeTime — at least one must be set), matched
// oldest-first and capped at MaxRows so the operation stays localized.
type ExpireOptions struct {
	BeforeCursor uint64
	BeforeTime   time.Time
	Action       ExpireAction
	MaxRows      uint32 // must be > 0
}

// TailOptions narrows a Tail subscription.
type TailOptions struct {
	AfterCursor       uint64 // exclusive lower bound when IncludeHistorical
	IncludeHistorical bool
}

// Driver is the contract every episodic backend implements.
// Drivers must be safe for concurrent use.
type Driver interface {
	Append(ctx context.Context, namespace string, opts AppendOptions) (Event, error)
	Range(ctx context.Context, namespace string, opts RangeOptions, yield func(Event) error) error
	// Tail blocks, yielding events as they arrive. Returns when ctx is done or
	// yield returns a non-nil error.
	Tail(ctx context.Context, namespace string, opts TailOptions, yield func(Event) error) error
	// Expire applies opts.Action to at most opts.MaxRows events inside the
	// retention window opts describes, returning the number affected (U3).
	Expire(ctx context.Context, namespace string, opts ExpireOptions) (affected uint64, err error)
	Close() error
}

// ErrExpireWindowRequired is returned when an Expire has neither a cursor nor a
// time upper bound, which would match an entire namespace unbounded.
var ErrExpireWindowRequired = errors.New("episodic: expire requires before_cursor or before_time")
