// Package memory is an in-memory lease driver. Suitable for tests and
// single-process dev; obviously does not provide cross-process coordination.
package memory

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"sort"
	"sync"
	"time"

	"github.com/vibed-project/mindD/internal/lease"
)

// Options configures a Driver.
type Options struct {
	NowFunc func() time.Time
	// PollInterval bounds how often Acquire wakes to check for expiry while
	// blocking on wait_for. Defaults to 50ms.
	PollInterval time.Duration
}

// Driver is an in-memory lease driver. Safe for concurrent use.
type Driver struct {
	mu     sync.Mutex
	leases map[string]map[string]*lease.Lease // namespace -> key -> lease
	cond   *sync.Cond                         // broadcast on Release / expiry observation
	now    func() time.Time
	poll   time.Duration
}

func New(opts Options) *Driver {
	d := &Driver{
		leases: make(map[string]map[string]*lease.Lease),
		now:    time.Now,
		poll:   50 * time.Millisecond,
	}
	d.cond = sync.NewCond(&d.mu)
	if opts.NowFunc != nil {
		d.now = opts.NowFunc
	}
	if opts.PollInterval > 0 {
		d.poll = opts.PollInterval
	}
	return d
}

func (d *Driver) Close() error {
	d.mu.Lock()
	d.cond.Broadcast()
	d.mu.Unlock()
	return nil
}

// Size returns the number of leases currently held in namespace.
func (d *Driver) Size(_ context.Context, namespace string) (int64, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	return int64(len(d.leases[namespace])), nil
}

func (d *Driver) Acquire(ctx context.Context, namespace, key string, opts lease.AcquireOptions) (lease.Lease, error) {
	deadline := d.now().Add(opts.WaitFor)
	for {
		l, ok := d.tryAcquire(namespace, key, opts)
		if ok {
			return l, nil
		}
		if opts.WaitFor <= 0 || !d.now().Before(deadline) {
			return lease.Lease{}, lease.ErrAlreadyHeld
		}
		remain := time.Until(deadline)
		wait := d.poll
		if remain < wait {
			wait = remain
		}
		t := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			t.Stop()
			return lease.Lease{}, ctx.Err()
		case <-t.C:
		}
	}
}

func (d *Driver) tryAcquire(namespace, key string, opts lease.AcquireOptions) (lease.Lease, bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	now := d.now()

	bucket := d.leases[namespace]
	if bucket == nil {
		bucket = make(map[string]*lease.Lease)
		d.leases[namespace] = bucket
	}
	if cur, ok := bucket[key]; ok {
		if cur.ExpiresAt.After(now) {
			return lease.Lease{}, false
		}
		// Expired; fall through and take over.
	}
	l := &lease.Lease{
		HolderID:   randomID(),
		Namespace:  namespace,
		Key:        key,
		AcquiredAt: now.UTC(),
		ExpiresAt:  now.Add(opts.TTL).UTC(),
		Metadata:   cloneMeta(opts.Metadata),
	}
	bucket[key] = l
	return *l, true
}

func (d *Driver) Renew(_ context.Context, namespace, key, holderID string, ttl time.Duration) (lease.Lease, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	now := d.now()
	cur, ok := d.leases[namespace][key]
	if !ok || cur.HolderID != holderID || !cur.ExpiresAt.After(now) {
		return lease.Lease{}, lease.ErrNotHeld
	}
	cur.ExpiresAt = now.Add(ttl).UTC()
	return *cur, nil
}

func (d *Driver) Release(_ context.Context, namespace, key, holderID string) (bool, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	bucket := d.leases[namespace]
	if bucket == nil {
		return false, nil
	}
	cur, ok := bucket[key]
	if !ok {
		return false, nil
	}
	if cur.HolderID != holderID {
		return false, lease.ErrNotHeld
	}
	delete(bucket, key)
	if len(bucket) == 0 {
		delete(d.leases, namespace)
	}
	d.cond.Broadcast()
	return true, nil
}

func (d *Driver) Inspect(_ context.Context, namespace, key string) (lease.Lease, bool, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	cur, ok := d.leases[namespace][key]
	if !ok || !cur.ExpiresAt.After(d.now()) {
		return lease.Lease{}, false, nil
	}
	return *cur, true, nil
}

func (d *Driver) List(_ context.Context, namespace string) ([]lease.Lease, error) {
	d.mu.Lock()
	now := d.now()
	out := make([]lease.Lease, 0, len(d.leases[namespace]))
	for _, l := range d.leases[namespace] {
		if l.ExpiresAt.After(now) {
			out = append(out, *l)
		}
	}
	d.mu.Unlock()
	sort.Slice(out, func(i, j int) bool { return out[i].Key < out[j].Key })
	return out, nil
}

func randomID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return time.Now().UTC().Format("20060102T150405.000000000")
	}
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return hex.EncodeToString(b[0:4]) + "-" + hex.EncodeToString(b[4:6]) + "-" +
		hex.EncodeToString(b[6:8]) + "-" + hex.EncodeToString(b[8:10]) + "-" +
		hex.EncodeToString(b[10:16])
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
