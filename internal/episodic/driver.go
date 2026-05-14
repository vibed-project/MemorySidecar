package episodic

import (
	"context"
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
}

// AppendOptions carries the writable fields of an Append call.
type AppendOptions struct {
	Type     string
	Payload  []byte
	Metadata map[string]string
}

// RangeOptions narrows a Range scan.
type RangeOptions struct {
	AfterCursor  uint64 // exclusive lower bound; 0 = from start
	BeforeCursor uint64 // exclusive upper bound; 0 = no upper
	Limit        uint32 // 0 = unbounded
	Reverse      bool
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
	Close() error
}
