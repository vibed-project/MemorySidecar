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
}

// Options configures a Driver.
type Options struct {
	SweeperInterval time.Duration // 0 disables; default 30s
	NowFunc         func() time.Time
}

// New builds a Driver and starts the background sweeper if SweeperInterval > 0.
func New(opts Options) *Driver {
	d := &Driver{
		items:           make(map[string]map[string]*kv.Record),
		now:             time.Now,
		sweeperInterval: opts.SweeperInterval,
		stopSweep:       make(chan struct{}),
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
	defer d.mu.Unlock()
	for ns, m := range d.items {
		for k, r := range m {
			if !r.ExpiresAt.IsZero() && !r.ExpiresAt.After(now) {
				delete(m, k)
			}
		}
		if len(m) == 0 {
			delete(d.items, ns)
		}
	}
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
		if cur, still := d.items[namespace][key]; still && cur == r {
			delete(d.items[namespace], key)
		}
		d.mu.Unlock()
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
