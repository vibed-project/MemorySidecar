package semantic

import (
	"context"
	"errors"
	"fmt"
	"sync"

	"github.com/vibed-project/mindD/internal/semantic/embedder"
)

// BoundNamespace is the per-namespace configuration the service needs at
// request time: the driver that stores vectors and the embedder that turns
// query text into a vector.
type BoundNamespace struct {
	Driver   Driver
	Embedder embedder.Embedder
}

// Registry resolves a namespace name to its bound (driver, embedder) pair.
type Registry struct {
	mu      sync.RWMutex
	byName  map[string]BoundNamespace
	drivers []Driver // owned drivers, in registration order
}

func NewRegistry() *Registry {
	return &Registry{byName: make(map[string]BoundNamespace)}
}

// Bind associates a namespace with its driver and embedder. The registry
// takes ownership of the driver.
func (r *Registry) Bind(namespace string, b BoundNamespace) error {
	if namespace == "" {
		return errors.New("registry: empty namespace")
	}
	if b.Driver == nil || b.Embedder == nil {
		return errors.New("registry: driver and embedder required")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, dup := r.byName[namespace]; dup {
		return fmt.Errorf("registry: namespace %q already bound", namespace)
	}
	r.byName[namespace] = b
	r.drivers = append(r.drivers, b.Driver)
	return nil
}

// Resolve returns the binding for namespace.
func (r *Registry) Resolve(namespace string) (BoundNamespace, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	b, ok := r.byName[namespace]
	return b, ok
}

// namespaceSizer is the optional capability a semantic driver implements to
// report its record count cheaply (memory: map length; pgvector: reltuples).
// Semantic drivers are namespace-bound, so Size takes no namespace argument.
type namespaceSizer interface {
	Size(ctx context.Context) (int64, error)
}

// NamespaceItems returns the record count for every bound namespace whose
// driver can report it cheaply. Feeds the mindd.namespace.items gauge.
func (r *Registry) NamespaceItems(ctx context.Context) map[string]int64 {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make(map[string]int64, len(r.byName))
	for ns, b := range r.byName {
		if s, ok := b.Driver.(namespaceSizer); ok {
			if n, err := s.Size(ctx); err == nil {
				out[ns] = n
			}
		}
	}
	return out
}

// Close closes every owned driver. Returns the first error.
func (r *Registry) Close() error {
	r.mu.Lock()
	drivers := r.drivers
	r.drivers = nil
	r.byName = nil
	r.mu.Unlock()
	var first error
	for _, d := range drivers {
		if err := d.Close(); err != nil && first == nil {
			first = err
		}
	}
	return first
}
