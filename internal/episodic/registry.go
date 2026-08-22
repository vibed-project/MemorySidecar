package episodic

import (
	"context"
	"errors"
	"fmt"
	"sync"
)

// Registry resolves a namespace name to the Driver backing it.
type Registry struct {
	mu      sync.RWMutex
	byName  map[string]Driver
	drivers []Driver
}

func NewRegistry() *Registry {
	return &Registry{byName: make(map[string]Driver)}
}

// Bind associates a namespace name with a driver. The registry takes ownership
// of the driver and will close it when Close is called.
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

// BindShared associates a namespace with an already-owned driver. The driver
// will NOT be closed an additional time.
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

// Resolve returns the driver bound to namespace.
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
// driver can report it cheaply (memory drivers; Postgres via a reltuples
// estimate). Namespaces whose driver has no Size method are omitted. Feeds the
// mindd.namespace.items gauge.
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

// Close closes every driver registered via Bind. Returns the first error.
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
