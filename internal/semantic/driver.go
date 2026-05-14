package semantic

import (
	"context"
	"errors"
	"time"
)

// ErrNotFound is returned by drivers when a record id is absent.
var ErrNotFound = errors.New("semantic: not found")

// Record is the canonical in-memory representation of a stored vector.
type Record struct {
	ID        string
	Content   string
	Payload   []byte
	Vector    []float32
	Metadata  map[string]string
	CreatedAt time.Time
}

// Hit is a single search result.
type Hit struct {
	Record Record
	Score  float32
}

// SearchOptions narrows a Search call.
type SearchOptions struct {
	QueryVector    []float32
	TopK           uint32
	Filter         map[string]string
	IncludePayload bool
	IncludeVector  bool
}

// Driver is the contract every semantic backend implements. Implementations
// must be safe for concurrent use.
type Driver interface {
	Upsert(ctx context.Context, records []Record) error
	Search(ctx context.Context, opts SearchOptions) ([]Hit, error)
	Delete(ctx context.Context, id string) (existed bool, err error)
	Close() error
}

// Dimensions returns the expected vector dimension for the bound driver
// instance. Implementations should embed this so the service can validate
// queries before they hit the driver.
type Dimensioned interface {
	Dimensions() int
}
