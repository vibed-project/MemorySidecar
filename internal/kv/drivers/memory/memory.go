package memory

import (
	"context"
	"sort"
	"strings"
	"sync"
	"time"

	"memsidecar/internal/kv"
)

// Driver is an in-memory KV driver. Safe for concurrent use.
type Driver struct {
	mu              sync.RWMutex
	items           map[string]map[string]*kv.Record // namespace -> key -> record
	now             func() time.Time
	sweeperInterval time.Duration
	stopSweep       chan struct{}
	wg              sync.WaitGroup
	onEvict         func(namespace string, n int)
}

// Options configures a Driver.
type Options struct {
	SweeperInterval time.Duration // 0 disables; default 30s
	NowFunc         func() time.Time
	// OnEvict, if set, is called (best-effort, outside the driver lock) when
	// keys are removed because their TTL elapsed — on the background sweep and
	// on lazy expiry during Get. Never called with n <= 0.
	OnEvict func(namespace string, n int)
}

// New builds a Driver and starts the background sweeper if SweeperInterval > 0.
func New(opts Options) *Driver {
	d := &Driver{
		items:           make(map[string]map[string]*kv.Record),
		now:             time.Now,
		sweeperInterval: opts.SweeperInterval,
		stopSweep:       make(chan struct{}),
		onEvict:         opts.OnEvict,
	}
	if opts.NowFunc != nil {
		d.now = opts.NowFunc
	}
	if d.sweeperInterval == 0 {
		d.sweeperInterval = 30 * time.Second
	}
	if d.sweeperInterval > 0 {
		d.wg.Add(1)
		go d.sweepLoop()
	}
	return d
}

func (d *Driver) Close() error {
	select {
	case <-d.stopSweep:
		// already closed
	default:
		close(d.stopSweep)
	}
	d.wg.Wait()
	return nil
}

func (d *Driver) sweepLoop() {
	defer d.wg.Done()
	t := time.NewTicker(d.sweeperInterval)
	defer t.Stop()
	for {
		select {
		case <-d.stopSweep:
			return
		case <-t.C:
			d.sweepExpired()
		}
	}
}

func (d *Driver) sweepExpired() {
	now := d.now()
	d.mu.Lock()
	var evicted map[string]int
	for ns, m := range d.items {
		removed := 0
		for k, r := range m {
			if !r.ExpiresAt.IsZero() && !r.ExpiresAt.After(now) {
				delete(m, k)
				removed++
			}
		}
		if removed > 0 {
			if evicted == nil {
				evicted = make(map[string]int)
			}
			evicted[ns] = removed
		}
		if len(m) == 0 {
			delete(d.items, ns)
		}
	}
	d.mu.Unlock()
	for ns, n := range evicted {
		d.evicted(ns, n)
	}
}

// evicted reports n TTL evictions in namespace to the optional OnEvict hook.
func (d *Driver) evicted(namespace string, n int) {
	if d.onEvict != nil && n > 0 {
		d.onEvict(namespace, n)
	}
}

// Size returns the number of keys held for namespace (its in-memory footprint,
// including any expired-but-not-yet-swept keys still occupying the map).
func (d *Driver) Size(_ context.Context, namespace string) (int64, error) {
	d.mu.RLock()
	defer d.mu.RUnlock()
	return int64(len(d.items[namespace])), nil
}

func (d *Driver) Get(_ context.Context, namespace, key string) (kv.Record, error) {
	d.mu.RLock()
	r, ok := d.items[namespace][key]
	d.mu.RUnlock()
	if !ok {
		return kv.Record{}, kv.ErrNotFound
	}
	if !r.ExpiresAt.IsZero() && !r.ExpiresAt.After(d.now()) {
		// lazy expiry; best-effort delete
		d.mu.Lock()
		removed := false
		if cur, still := d.items[namespace][key]; still && cur == r {
			delete(d.items[namespace], key)
			removed = true
		}
		d.mu.Unlock()
		if removed {
			d.evicted(namespace, 1)
		}
		return kv.Record{}, kv.ErrNotFound
	}
	return *r, nil
}

func (d *Driver) Put(_ context.Context, namespace, key string, opts kv.PutOptions) (kv.Record, error) {
	d.mu.Lock()
	defer d.mu.Unlock()

	now := d.now()
	bucket := d.items[namespace]
	if bucket == nil {
		bucket = make(map[string]*kv.Record)
		d.items[namespace] = bucket
	}
	existing, found := bucket[key]
	if found && !existing.ExpiresAt.IsZero() && !existing.ExpiresAt.After(now) {
		delete(bucket, key)
		existing, found = nil, false
	}

	if opts.IfVersion != nil {
		expected := *opts.IfVersion
		var currentVersion uint64
		if found {
			currentVersion = existing.Version
		}
		if expected != currentVersion {
			return kv.Record{}, kv.ErrVersionMismatch
		}
	}

	var version uint64 = 1
	created := now
	if found {
		version = existing.Version + 1
		created = existing.CreatedAt
	}
	var expires time.Time
	if opts.TTL > 0 {
		expires = now.Add(opts.TTL)
	}
	r := &kv.Record{
		Key:         key,
		Value:       cloneBytes(opts.Value),
		ContentType: opts.ContentType,
		Metadata:    cloneMeta(opts.Metadata),
		Version:     version,
		CreatedAt:   created,
		ExpiresAt:   expires,
	}
	bucket[key] = r
	return *r, nil
}

func (d *Driver) Delete(_ context.Context, namespace, key string, opts kv.DeleteOptions) (bool, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	bucket := d.items[namespace]
	if bucket == nil {
		if opts.IfVersion != nil && *opts.IfVersion != 0 {
			return false, kv.ErrVersionMismatch
		}
		return false, nil
	}
	existing, ok := bucket[key]
	if ok && !existing.ExpiresAt.IsZero() && !existing.ExpiresAt.After(d.now()) {
		delete(bucket, key)
		ok = false
	}
	if opts.IfVersion != nil {
		var current uint64
		if ok {
			current = existing.Version
		}
		if *opts.IfVersion != current {
			return false, kv.ErrVersionMismatch
		}
	}
	if !ok {
		return false, nil
	}
	delete(bucket, key)
	if len(bucket) == 0 {
		delete(d.items, namespace)
	}
	return true, nil
}

func (d *Driver) Scan(_ context.Context, namespace string, opts kv.ScanOptions, yield func(kv.Record) error) error {
	now := d.now()
	d.mu.RLock()
	bucket := d.items[namespace]
	keys := make([]string, 0, len(bucket))
	for k, r := range bucket {
		if !r.ExpiresAt.IsZero() && !r.ExpiresAt.After(now) {
			continue
		}
		if opts.KeyPrefix != "" && !strings.HasPrefix(k, opts.KeyPrefix) {
			continue
		}
		keys = append(keys, k)
	}
	sort.Strings(keys)
	if opts.Limit > 0 && uint32(len(keys)) > opts.Limit {
		keys = keys[:opts.Limit]
	}
	snapshot := make([]kv.Record, 0, len(keys))
	for _, k := range keys {
		r := *bucket[k]
		if !opts.IncludeValues {
			r.Value = nil
		}
		snapshot = append(snapshot, r)
	}
	d.mu.RUnlock()

	for _, r := range snapshot {
		if err := yield(r); err != nil {
			return err
		}
	}
	return nil
}

func cloneBytes(b []byte) []byte {
	if b == nil {
		return nil
	}
	out := make([]byte, len(b))
	copy(out, b)
	return out
}

func cloneMeta(m map[string]string) map[string]string {
	if m == nil {
		return nil
	}
	out := make(map[string]string, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}
