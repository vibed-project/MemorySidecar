package memory

import (
	"context"
	"errors"
	"math"
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
	onEvict         func(namespace, cause string, n int)
	access          map[string]kv.AccessPolicy // namespace -> opt-in cache-tier policy
}

// Options configures a Driver.
type Options struct {
	SweeperInterval time.Duration // 0 disables; default 30s
	NowFunc         func() time.Time
	// OnEvict, if set, is called (best-effort, outside the driver lock) when
	// keys are removed. cause is "ttl" (expiry) or "capacity" (heat eviction).
	// Never called with n <= 0.
	OnEvict func(namespace, cause string, n int)
	// AccessPolicies configures per-namespace cache-tier behaviour (U5). A
	// namespace with no entry (or a zero policy) tracks nothing and its reads
	// never write.
	AccessPolicies map[string]kv.AccessPolicy
}

// New builds a Driver and starts the background sweeper if SweeperInterval > 0.
func New(opts Options) *Driver {
	d := &Driver{
		items:           make(map[string]map[string]*kv.Record),
		now:             time.Now,
		sweeperInterval: opts.SweeperInterval,
		stopSweep:       make(chan struct{}),
		onEvict:         opts.OnEvict,
		access:          opts.AccessPolicies,
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
		d.evicted(ns, "ttl", n)
	}
}

// evicted reports n evictions in namespace to the optional OnEvict hook.
func (d *Driver) evicted(namespace, cause string, n int) {
	if d.onEvict != nil && n > 0 {
		d.onEvict(namespace, cause, n)
	}
}

// policyFor returns the namespace's access policy (zero value if unset).
func (d *Driver) policyFor(namespace string) kv.AccessPolicy {
	return d.access[namespace]
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
			d.evicted(namespace, "ttl", 1)
		}
		return kv.Record{}, kv.ErrNotFound
	}

	// Access-tier read path (U5): only taken when the namespace opts in, so a
	// pure-read workload without a policy never acquires the write lock.
	if pol := d.policyFor(namespace); pol.Enabled() {
		now := d.now()
		d.mu.Lock()
		if cur, still := d.items[namespace][key]; still && cur == r {
			if pol.Track {
				cur.AccessCount++
				cur.LastAccessed = now
			}
			if pol.SlideTTL > 0 && !cur.ExpiresAt.IsZero() {
				cur.ExpiresAt = now.Add(pol.SlideTTL)
			}
			snapshot := *cur
			d.mu.Unlock()
			return snapshot, nil
		}
		d.mu.Unlock()
	}
	return *r, nil
}

func (d *Driver) Put(_ context.Context, namespace, key string, opts kv.PutOptions) (kv.Record, error) {
	var evictedN int
	d.mu.Lock()
	// Registered before the unlock defer, so it runs *after* the mutex is
	// released — the OnEvict hook is always called outside the lock.
	defer func() {
		if evictedN > 0 {
			d.evicted(namespace, "capacity", evictedN)
		}
	}()
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

	// Heat-based capacity eviction (U5): when over capacity, drop the coldest
	// keys (never the one just written).
	if pol := d.policyFor(namespace); pol.Capacity > 0 {
		evictedN = evictColdest(bucket, key, pol, now)
	}
	return *r, nil
}

const defaultHeatHalfLife = time.Hour

// heatOf scores a record's residency value: access_count decayed by the time
// since it was last accessed (or created, if never accessed). Higher = hotter.
func heatOf(r *kv.Record, pol kv.AccessPolicy, now time.Time) float64 {
	half := pol.HeatHalfLife
	if half <= 0 {
		half = defaultHeatHalfLife
	}
	last := r.LastAccessed
	if last.IsZero() {
		last = r.CreatedAt
	}
	age := now.Sub(last)
	if age < 0 {
		age = 0
	}
	return float64(r.AccessCount) * math.Exp2(-age.Seconds()/half.Seconds())
}

// evictColdest removes the lowest-heat keys until bucket is at pol.Capacity,
// never evicting protected (the just-written key). Returns the count removed.
func evictColdest(bucket map[string]*kv.Record, protected string, pol kv.AccessPolicy, now time.Time) int {
	over := len(bucket) - pol.Capacity
	if over <= 0 {
		return 0
	}
	type kh struct {
		key  string
		heat float64
	}
	cands := make([]kh, 0, len(bucket))
	for k, r := range bucket {
		if k == protected {
			continue
		}
		cands = append(cands, kh{k, heatOf(r, pol, now)})
	}
	sort.Slice(cands, func(i, j int) bool {
		if cands[i].heat != cands[j].heat {
			return cands[i].heat < cands[j].heat
		}
		return cands[i].key < cands[j].key // deterministic on ties
	})
	n := 0
	for i := 0; i < over && i < len(cands); i++ {
		delete(bucket, cands[i].key)
		n++
	}
	return n
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

func (d *Driver) MultiGet(ctx context.Context, namespace string, keys []string) ([]kv.Record, error) {
	// Reuse Get so TTL expiry and access instrumentation behave as N Gets.
	seen := make(map[string]struct{}, len(keys))
	found := make([]kv.Record, 0, len(keys))
	for _, k := range keys {
		if _, dup := seen[k]; dup {
			continue
		}
		seen[k] = struct{}{}
		r, err := d.Get(ctx, namespace, k)
		if errors.Is(err, kv.ErrNotFound) {
			continue
		}
		if err != nil {
			return nil, err
		}
		found = append(found, r)
	}
	sort.Slice(found, func(i, j int) bool { return found[i].Key < found[j].Key })
	return found, nil
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
		if opts.StartAfter != "" && k <= opts.StartAfter {
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
