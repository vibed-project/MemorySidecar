// Package kvtest provides a driver-agnostic conformance suite that every
// kv.Driver implementation must pass. New backends get coverage for free by
// implementing Harness and calling RunConformance.
package kvtest

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"memsidecar/internal/kv"
)

// Harness adapts a concrete driver to the conformance suite.
//
// New must return a freshly isolated driver; the suite calls it once per
// subtest. NewWithClock returns a driver wired to the given fake clock — for
// drivers that decide expiry server-side (e.g. Postgres via NOW()) and cannot
// honor an injected clock, return (nil, false) and the suite falls back to a
// real-time sleep with a short TTL.
type Harness interface {
	New(t *testing.T) kv.Driver
	NewWithClock(t *testing.T, clock *FakeClock) (kv.Driver, bool)
}

// FakeClock is a monotonic clock controllable from tests.
type FakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func NewFakeClock(start time.Time) *FakeClock { return &FakeClock{t: start} }

func (c *FakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *FakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

// RunConformance runs the full conformance battery against the driver
// produced by h. Each case runs as a t.Run subtest so callers can target
// individual behaviors with -run.
func RunConformance(t *testing.T, h Harness) {
	t.Helper()
	cases := []struct {
		name string
		fn   func(*testing.T, Harness)
	}{
		{"PutGetDelete", testPutGetDelete},
		{"Versioning", testVersioning},
		{"CAS_Put", testCASPut},
		{"CAS_Delete", testCASDelete},
		{"TTL", testTTL},
		{"Scan_PrefixAndLimit", testScanPrefixAndLimit},
		{"Scan_ExpiredFiltered", testScanExpiredFiltered},
		{"Scan_YieldError", testScanYieldError},
		{"Scan_StartAfterCursor", testScanStartAfterCursor},
		{"MultiGet", testMultiGet},
		{"ConcurrentPut", testConcurrentPut},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) { tc.fn(t, h) })
	}
}

func testPutGetDelete(t *testing.T, h Harness) {
	d := h.New(t)
	ctx := context.Background()

	_, err := d.Get(ctx, "ns", "missing")
	require.ErrorIs(t, err, kv.ErrNotFound)

	rec, err := d.Put(ctx, "ns", "k", kv.PutOptions{Value: []byte("v"), ContentType: "text/plain"})
	require.NoError(t, err)
	assert.Equal(t, uint64(1), rec.Version)

	got, err := d.Get(ctx, "ns", "k")
	require.NoError(t, err)
	assert.Equal(t, []byte("v"), got.Value)
	assert.Equal(t, "text/plain", got.ContentType)

	existed, err := d.Delete(ctx, "ns", "k", kv.DeleteOptions{})
	require.NoError(t, err)
	assert.True(t, existed)

	_, err = d.Get(ctx, "ns", "k")
	require.ErrorIs(t, err, kv.ErrNotFound)
}

func testVersioning(t *testing.T, h Harness) {
	d := h.New(t)
	ctx := context.Background()
	r, err := d.Put(ctx, "ns", "k", kv.PutOptions{Value: []byte("a")})
	require.NoError(t, err)
	assert.Equal(t, uint64(1), r.Version)
	r, err = d.Put(ctx, "ns", "k", kv.PutOptions{Value: []byte("b")})
	require.NoError(t, err)
	assert.Equal(t, uint64(2), r.Version)
	got, err := d.Get(ctx, "ns", "k")
	require.NoError(t, err)
	assert.Equal(t, []byte("b"), got.Value)
}

func testCASPut(t *testing.T, h Harness) {
	d := h.New(t)
	ctx := context.Background()

	zero := uint64(0)
	_, err := d.Put(ctx, "ns", "k", kv.PutOptions{Value: []byte("a"), IfVersion: &zero})
	require.NoError(t, err)

	wrong := uint64(99)
	_, err = d.Put(ctx, "ns", "k", kv.PutOptions{Value: []byte("b"), IfVersion: &wrong})
	require.ErrorIs(t, err, kv.ErrVersionMismatch)

	one := uint64(1)
	rec, err := d.Put(ctx, "ns", "k", kv.PutOptions{Value: []byte("c"), IfVersion: &one})
	require.NoError(t, err)
	assert.Equal(t, uint64(2), rec.Version)
}

func testCASDelete(t *testing.T, h Harness) {
	d := h.New(t)
	ctx := context.Background()
	_, err := d.Put(ctx, "ns", "k", kv.PutOptions{Value: []byte("a")})
	require.NoError(t, err)

	wrong := uint64(99)
	_, err = d.Delete(ctx, "ns", "k", kv.DeleteOptions{IfVersion: &wrong})
	require.ErrorIs(t, err, kv.ErrVersionMismatch)

	one := uint64(1)
	existed, err := d.Delete(ctx, "ns", "k", kv.DeleteOptions{IfVersion: &one})
	require.NoError(t, err)
	assert.True(t, existed)
}

// testTTL uses the fake clock when the driver supports it; otherwise falls
// back to a real-second sleep with a short TTL.
func testTTL(t *testing.T, h Harness) {
	ctx := context.Background()
	if clock := NewFakeClock(time.Now()); true {
		if d, ok := h.NewWithClock(t, clock); ok {
			_, err := d.Put(ctx, "ns", "k", kv.PutOptions{Value: []byte("x"), TTL: time.Minute})
			require.NoError(t, err)

			clock.Advance(30 * time.Second)
			_, err = d.Get(ctx, "ns", "k")
			require.NoError(t, err)

			clock.Advance(31 * time.Second)
			_, err = d.Get(ctx, "ns", "k")
			require.ErrorIs(t, err, kv.ErrNotFound)
			return
		}
	}

	d := h.New(t)
	_, err := d.Put(ctx, "ns", "k", kv.PutOptions{Value: []byte("x"), TTL: time.Second})
	require.NoError(t, err)
	time.Sleep(2 * time.Second)
	_, err = d.Get(ctx, "ns", "k")
	require.ErrorIs(t, err, kv.ErrNotFound)
}

func testScanPrefixAndLimit(t *testing.T, h Harness) {
	d := h.New(t)
	ctx := context.Background()
	for _, k := range []string{"a/1", "a/2", "a/3", "b/1"} {
		_, err := d.Put(ctx, "ns", k, kv.PutOptions{Value: []byte(k)})
		require.NoError(t, err)
	}
	var got []string
	err := d.Scan(ctx, "ns", kv.ScanOptions{KeyPrefix: "a/", Limit: 2, IncludeValues: true},
		func(r kv.Record) error { got = append(got, r.Key); return nil })
	require.NoError(t, err)
	assert.Equal(t, []string{"a/1", "a/2"}, got)
}

func testScanStartAfterCursor(t *testing.T, h Harness) {
	d := h.New(t)
	ctx := context.Background()
	for _, k := range []string{"a/1", "a/2", "a/3", "a/4"} {
		_, err := d.Put(ctx, "ns", k, kv.PutOptions{Value: []byte(k)})
		require.NoError(t, err)
	}

	page := func(opts kv.ScanOptions) []string {
		var got []string
		require.NoError(t, d.Scan(ctx, "ns", opts, func(r kv.Record) error {
			got = append(got, r.Key)
			return nil
		}))
		return got
	}

	// StartAfter is an exclusive lower bound; paging with the last key resumes.
	first := page(kv.ScanOptions{KeyPrefix: "a/", Limit: 2})
	assert.Equal(t, []string{"a/1", "a/2"}, first)
	second := page(kv.ScanOptions{KeyPrefix: "a/", Limit: 2, StartAfter: first[len(first)-1]})
	assert.Equal(t, []string{"a/3", "a/4"}, second)
	// The last page's cursor yields nothing further.
	assert.Empty(t, page(kv.ScanOptions{KeyPrefix: "a/", Limit: 2, StartAfter: second[len(second)-1]}))
}

func testMultiGet(t *testing.T, h Harness) {
	d := h.New(t)
	ctx := context.Background()
	for _, k := range []string{"a", "b", "c"} {
		_, err := d.Put(ctx, "ns", k, kv.PutOptions{Value: []byte(k), ContentType: "text/plain"})
		require.NoError(t, err)
	}

	// Found keys returned (ordered by key); missing keys omitted; repeats deduped.
	recs, err := d.MultiGet(ctx, "ns", []string{"c", "missing", "a", "a"})
	require.NoError(t, err)
	keys := make([]string, len(recs))
	for i, r := range recs {
		keys[i] = r.Key
	}
	assert.Equal(t, []string{"a", "c"}, keys)
	assert.Equal(t, []byte("a"), recs[0].Value)
	assert.Equal(t, "text/plain", recs[0].ContentType)

	// Empty request → empty result.
	empty, err := d.MultiGet(ctx, "ns", nil)
	require.NoError(t, err)
	assert.Empty(t, empty)

	// All-missing → empty.
	none, err := d.MultiGet(ctx, "ns", []string{"x", "y"})
	require.NoError(t, err)
	assert.Empty(t, none)
}

func testScanExpiredFiltered(t *testing.T, h Harness) {
	ctx := context.Background()
	clock := NewFakeClock(time.Now())
	d, ok := h.NewWithClock(t, clock)
	if !ok {
		t.Skip("driver does not support fake-clock injection")
	}

	_, err := d.Put(ctx, "ns", "live", kv.PutOptions{Value: []byte("v"), TTL: time.Hour})
	require.NoError(t, err)
	_, err = d.Put(ctx, "ns", "dead", kv.PutOptions{Value: []byte("v"), TTL: time.Second})
	require.NoError(t, err)

	clock.Advance(2 * time.Second)
	var got []string
	err = d.Scan(ctx, "ns", kv.ScanOptions{}, func(r kv.Record) error {
		got = append(got, r.Key)
		return nil
	})
	require.NoError(t, err)
	assert.Equal(t, []string{"live"}, got)
}

func testScanYieldError(t *testing.T, h Harness) {
	d := h.New(t)
	ctx := context.Background()
	for _, k := range []string{"a", "b", "c"} {
		_, err := d.Put(ctx, "ns", k, kv.PutOptions{Value: []byte(k)})
		require.NoError(t, err)
	}
	bomb := errors.New("boom")
	err := d.Scan(ctx, "ns", kv.ScanOptions{},
		func(r kv.Record) error { return bomb })
	require.ErrorIs(t, err, bomb)
}

func testConcurrentPut(t *testing.T, h Harness) {
	d := h.New(t)
	ctx := context.Background()
	const (
		writers   = 50
		perWriter = 100
	)
	var wg sync.WaitGroup
	for i := 0; i < writers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < perWriter; j++ {
				_, _ = d.Put(ctx, "ns", "shared", kv.PutOptions{Value: []byte("x")})
			}
		}()
	}
	wg.Wait()
	got, err := d.Get(ctx, "ns", "shared")
	require.NoError(t, err)
	assert.Equal(t, uint64(writers*perWriter), got.Version)
}
