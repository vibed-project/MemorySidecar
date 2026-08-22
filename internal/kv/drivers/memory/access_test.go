package memory

import (
	"context"
	"testing"
	"time"

	"github.com/vibed-project/mindD/internal/kv"
)

// With no policy, a read must not mutate the stored record (zero write
// amplification on the pure-read path).
func TestAccess_DisabledNoWriteOnRead(t *testing.T) {
	ctx := context.Background()
	d := New(Options{SweeperInterval: -1})
	_, _ = d.Put(ctx, "ns", "k", kv.PutOptions{Value: []byte("v")})

	for i := 0; i < 3; i++ {
		if _, err := d.Get(ctx, "ns", "k"); err != nil {
			t.Fatalf("Get: %v", err)
		}
	}
	r := d.items["ns"]["k"]
	if r.AccessCount != 0 || !r.LastAccessed.IsZero() {
		t.Fatalf("read mutated record with policy off: count=%d lastAccessed=%v", r.AccessCount, r.LastAccessed)
	}
}

func TestAccess_TrackAdvances(t *testing.T) {
	ctx := context.Background()
	now := time.Unix(1000, 0)
	d := New(Options{
		SweeperInterval: -1,
		NowFunc:         func() time.Time { return now },
		AccessPolicies:  map[string]kv.AccessPolicy{"ns": {Track: true}},
	})
	_, _ = d.Put(ctx, "ns", "k", kv.PutOptions{Value: []byte("v")})

	rec, err := d.Get(ctx, "ns", "k")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if rec.AccessCount != 1 || !rec.LastAccessed.Equal(now) {
		t.Fatalf("after 1 Get: count=%d lastAccessed=%v", rec.AccessCount, rec.LastAccessed)
	}
	_, _ = d.Get(ctx, "ns", "k")
	if got := d.items["ns"]["k"].AccessCount; got != 2 {
		t.Fatalf("after 2 Gets: count=%d want 2", got)
	}
}

func TestAccess_SlideTTL(t *testing.T) {
	ctx := context.Background()
	now := time.Unix(1000, 0)
	d := New(Options{
		SweeperInterval: -1,
		NowFunc:         func() time.Time { return now },
		AccessPolicies:  map[string]kv.AccessPolicy{"ns": {SlideTTL: 60 * time.Second}},
	})
	_, _ = d.Put(ctx, "ns", "ttl", kv.PutOptions{Value: []byte("v"), TTL: 30 * time.Second})
	_, _ = d.Put(ctx, "ns", "perm", kv.PutOptions{Value: []byte("v")}) // no TTL

	now = now.Add(20 * time.Second) // key still live (expires at 1030)
	rec, err := d.Get(ctx, "ns", "ttl")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	want := now.Add(60 * time.Second)
	if !rec.ExpiresAt.Equal(want) {
		t.Fatalf("slide: ExpiresAt=%v want %v", rec.ExpiresAt, want)
	}
	if !d.items["ns"]["ttl"].ExpiresAt.Equal(want) {
		t.Fatalf("slide not persisted")
	}
	// A key with no TTL must not gain one from a slide.
	perm, _ := d.Get(ctx, "ns", "perm")
	if !perm.ExpiresAt.IsZero() {
		t.Fatalf("non-TTL key gained expiry from slide: %v", perm.ExpiresAt)
	}
}

func TestAccess_HeatEviction(t *testing.T) {
	ctx := context.Background()
	now := time.Unix(1_000_000, 0)
	rec := newEvictRecorder()
	d := New(Options{
		SweeperInterval: -1,
		NowFunc:         func() time.Time { return now },
		OnEvict:         rec.hook,
		AccessPolicies:  map[string]kv.AccessPolicy{"ns": {Track: true, Capacity: 2, HeatHalfLife: time.Hour}},
	})
	_, _ = d.Put(ctx, "ns", "hot", kv.PutOptions{Value: []byte("h")})
	_, _ = d.Put(ctx, "ns", "cold", kv.PutOptions{Value: []byte("c")})
	for i := 0; i < 5; i++ {
		_, _ = d.Get(ctx, "ns", "hot") // warm "hot" up; "cold" stays untouched
	}

	// Third key pushes over capacity 2 → the coldest non-fresh key is evicted.
	_, _ = d.Put(ctx, "ns", "fresh", kv.PutOptions{Value: []byte("f")})

	if _, ok := d.items["ns"]["cold"]; ok {
		t.Fatalf("cold key should have been evicted")
	}
	if _, ok := d.items["ns"]["hot"]; !ok {
		t.Fatalf("hot key should survive")
	}
	if _, ok := d.items["ns"]["fresh"]; !ok {
		t.Fatalf("just-written key should survive")
	}
	if got := rec.countCause("ns", "capacity"); got != 1 {
		t.Fatalf("capacity evictions = %d, want 1", got)
	}
}
