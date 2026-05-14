package artifact

import (
	"context"
	"errors"
	"io"
	"time"
)

// ErrNotFound is returned when an artifact id is absent from a namespace.
var ErrNotFound = errors.New("artifact: not found")

// Meta captures everything we know about a stored artifact except its bytes.
type Meta struct {
	ID          string
	Size        uint64
	ContentType string
	SHA256      string
	Metadata    map[string]string
	CreatedAt   time.Time
}

// PutHeader is the per-call metadata passed to Driver.Put.
type PutHeader struct {
	ID          string // empty → server assigns
	ContentType string
	Metadata    map[string]string
	// SHA256 and Size are computed by the service layer (TeeReader → hash)
	// and passed to the driver so it can persist them in metadata. Drivers
	// MUST trust these values rather than recompute.
	SHA256 string
	Size   uint64
}

// GetOptions carries optional read parameters.
type GetOptions struct {
	Offset uint64
	Length uint64 // 0 = until end
}

// Driver is the contract every artifact backend implements. Implementations
// MUST be safe for concurrent use.
type Driver interface {
	// Put consumes r until EOF and persists the bytes under (namespace, header.ID).
	// If header.ID is empty, the driver assigns one (UUID v4).
	// The driver must return Meta with the assigned id and CreatedAt set.
	Put(ctx context.Context, namespace string, header PutHeader, r io.Reader) (Meta, error)

	// Open returns metadata and a reader for the artifact's bytes. The reader
	// honours GetOptions.Offset/Length. The caller is responsible for closing
	// the returned io.ReadCloser.
	Open(ctx context.Context, namespace, id string, opts GetOptions) (Meta, io.ReadCloser, error)

	Stat(ctx context.Context, namespace, id string) (Meta, error)
	Delete(ctx context.Context, namespace, id string) (existed bool, err error)
	Close() error
}
