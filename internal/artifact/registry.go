package artifact

import (
	"context"
	"errors"
	"fmt"
	"sync"
)

// Registry resolves a namespace to the Driver backing it.
type Registry struct {
	mu      sync.RWMutex
	byName  map[string]Driver
	drivers []Driver
}

func NewRegistry() *Registry { return &Registry{byName: make(map[string]Driver)} }

// Bind associates a namespace with a driver. The registry takes ownership.
func (r *Registry) Bind(namespace string, d Driver) error {
	if namespace == "" {
		return errors.New("registry: empty namespace")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, dup := r.byName[namespace]; dup {
		return fmt.Errorf("registry: namespace %q already bound", namespace)
	}
	r.byName[namespace] = d
	r.drivers = append(r.drivers, d)
	return nil
}

// BindShared associates a namespace with an already-owned driver.
func (r *Registry) BindShared(namespace string, d Driver) error {
	if namespace == "" {
		return errors.New("registry: empty namespace")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, dup := r.byName[namespace]; dup {
		return fmt.Errorf("registry: namespace %q already bound", namespace)
	}
	r.byName[namespace] = d
	return nil
}

func (r *Registry) Resolve(namespace string) (Driver, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	d, ok := r.byName[namespace]
	return d, ok
}

// namespaceSizer is the optional capability a driver implements to report its
// per-namespace item count cheaply.
type namespaceSizer interface {
	Size(ctx context.Context, namespace string) (int64, error)
}

// NamespaceItems returns the current item count for every bound namespace whose
// driver can report it cheaply. Object-store drivers (fs, s3) don't implement
// Size — counting objects there isn't cheap — so those namespaces are omitted.
// Feeds the memsidecar.namespace.items gauge.
func (r *Registry) NamespaceItems(ctx context.Context) map[string]int64 {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make(map[string]int64, len(r.byName))
	for ns, d := range r.byName {
		if s, ok := d.(namespaceSizer); ok {
			if n, err := s.Size(ctx, ns); err == nil {
				out[ns] = n
			}
		}
	}
	return out
}

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
