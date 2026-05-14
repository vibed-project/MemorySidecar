package lease

import (
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
