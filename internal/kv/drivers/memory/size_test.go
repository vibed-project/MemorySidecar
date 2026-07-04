package memory

import (
	"context"
	"sync"
	"testing"
	"time"

	"memsidecar/internal/kv"
)

func TestSizeCountsKeysPerNamespace(t *testing.T) {
	ctx := context.Background()
	d := New(Options{SweeperInterval: -1})

	if n, _ := d.Size(ctx, "ns"); n != 0 {
		t.Fatalf("empty Size = %d, want 0", n)
	}
	_, _ = d.Put(ctx, "ns", "a", kv.PutOptions{Value: []byte("1")})
	_, _ = d.Put(ctx, "ns", "b", kv.PutOptions{Value: []byte("2")})
	_, _ = d.Put(ctx, "other", "a", kv.PutOptions{Value: []byte("x")})

	if n, _ := d.Size(ctx, "ns"); n != 2 {
		t.Fatalf("Size(ns) = %d, want 2", n)
	}
	if n, _ := d.Size(ctx, "other"); n != 1 {
		t.Fatalf("Size(other) = %d, want 1", n)
	}
	_, _ = d.Delete(ctx, "ns", "a", kv.DeleteOptions{})
	if n, _ := d.Size(ctx, "ns"); n != 1 {
		t.Fatalf("Size(ns) after delete = %d, want 1", n)
	}
}

// evictRecorder captures OnEvict callbacks.
type evictRecorder struct {
	mu  sync.Mutex
	got map[string]int
}

func newEvictRecorder() *evictRecorder { return &evictRecorder{got: map[string]int{}} }

func (e *evictRecorder) hook(ns string, n int) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.got[ns] += n
}

func (e *evictRecorder) count(ns string) int {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.got[ns]
}

func TestEvictionHookOnLazyExpiry(t *testing.T) {
	ctx := context.Background()
	now := time.Unix(1000, 0)
	rec := newEvictRecorder()
	d := New(Options{
		SweeperInterval: -1,
		NowFunc:         func() time.Time { return now },
		OnEvict:         rec.hook,
	})

	_, _ = d.Put(ctx, "ns", "k", kv.PutOptions{Value: []byte("v"), TTL: time.Second})
	now = now.Add(2 * time.Second) // k is now expired

	if _, err := d.Get(ctx, "ns", "k"); err != kv.ErrNotFound {
		t.Fatalf("Get expired key: err = %v, want ErrNotFound", err)
	}
	if got := rec.count("ns"); got != 1 {
		t.Fatalf("lazy-expiry evictions = %d, want 1", got)
	}
}

func TestEvictionHookOnSweep(t *testing.T) {
	ctx := context.Background()
	now := time.Unix(1000, 0)
	rec := newEvictRecorder()
	d := New(Options{
		SweeperInterval: -1, // no background sweeper; we call sweepExpired directly
		NowFunc:         func() time.Time { return now },
		OnEvict:         rec.hook,
	})

	_, _ = d.Put(ctx, "a", "k1", kv.PutOptions{Value: []byte("v"), TTL: time.Second})
	_, _ = d.Put(ctx, "a", "k2", kv.PutOptions{Value: []byte("v"), TTL: time.Second})
	_, _ = d.Put(ctx, "b", "keep", kv.PutOptions{Value: []byte("v")}) // no TTL

	now = now.Add(2 * time.Second)
	d.sweepExpired()

	if got := rec.count("a"); got != 2 {
		t.Fatalf("namespace a sweep evictions = %d, want 2", got)
	}
	if got := rec.count("b"); got != 0 {
		t.Fatalf("namespace b sweep evictions = %d, want 0", got)
	}
	if n, _ := d.Size(ctx, "b"); n != 1 {
		t.Fatalf("namespace b Size = %d, want 1 (non-expiring key kept)", n)
	}
}
