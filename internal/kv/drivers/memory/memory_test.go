package memory

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

func newDriver(t *testing.T) *Driver {
	t.Helper()
	d := New(Options{SweeperInterval: -1}) // disable sweeper for determinism
	t.Cleanup(func() { _ = d.Close() })
	return d
}

func TestPutGetDelete(t *testing.T) {
	d := newDriver(t)
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

func TestVersioning(t *testing.T) {
	d := newDriver(t)
	ctx := context.Background()
	r, _ := d.Put(ctx, "ns", "k", kv.PutOptions{Value: []byte("a")})
	assert.Equal(t, uint64(1), r.Version)
	r, _ = d.Put(ctx, "ns", "k", kv.PutOptions{Value: []byte("b")})
	assert.Equal(t, uint64(2), r.Version)
	// createdAt preserved across updates
	got, _ := d.Get(ctx, "ns", "k")
	assert.Equal(t, []byte("b"), got.Value)
}

func TestCAS_Put(t *testing.T) {
	d := newDriver(t)
	ctx := context.Background()

	// expect-no-row write
	zero := uint64(0)
	_, err := d.Put(ctx, "ns", "k", kv.PutOptions{Value: []byte("a"), IfVersion: &zero})
	require.NoError(t, err)

	// wrong version → mismatch
	wrong := uint64(99)
	_, err = d.Put(ctx, "ns", "k", kv.PutOptions{Value: []byte("b"), IfVersion: &wrong})
	require.ErrorIs(t, err, kv.ErrVersionMismatch)

	// correct version → ok
	one := uint64(1)
	rec, err := d.Put(ctx, "ns", "k", kv.PutOptions{Value: []byte("c"), IfVersion: &one})
	require.NoError(t, err)
	assert.Equal(t, uint64(2), rec.Version)
}

func TestCAS_Delete(t *testing.T) {
	d := newDriver(t)
	ctx := context.Background()
	_, _ = d.Put(ctx, "ns", "k", kv.PutOptions{Value: []byte("a")})

	wrong := uint64(99)
	_, err := d.Delete(ctx, "ns", "k", kv.DeleteOptions{IfVersion: &wrong})
	require.ErrorIs(t, err, kv.ErrVersionMismatch)

	one := uint64(1)
	existed, err := d.Delete(ctx, "ns", "k", kv.DeleteOptions{IfVersion: &one})
	require.NoError(t, err)
	assert.True(t, existed)
}

func TestTTL_LazyExpiry(t *testing.T) {
	now := time.Now()
	clock := &fakeClock{t: now}
	d := New(Options{SweeperInterval: -1, NowFunc: clock.Now})
	t.Cleanup(func() { _ = d.Close() })

	ctx := context.Background()
	_, err := d.Put(ctx, "ns", "k", kv.PutOptions{Value: []byte("x"), TTL: time.Minute})
	require.NoError(t, err)

	clock.advance(30 * time.Second)
	_, err = d.Get(ctx, "ns", "k")
	require.NoError(t, err)

	clock.advance(31 * time.Second)
	_, err = d.Get(ctx, "ns", "k")
	require.ErrorIs(t, err, kv.ErrNotFound)
}

func TestScan_PrefixAndLimit(t *testing.T) {
	d := newDriver(t)
	ctx := context.Background()
	for _, k := range []string{"a/1", "a/2", "a/3", "b/1"} {
		_, _ = d.Put(ctx, "ns", k, kv.PutOptions{Value: []byte(k)})
	}
	var got []string
	err := d.Scan(ctx, "ns", kv.ScanOptions{KeyPrefix: "a/", Limit: 2, IncludeValues: true},
		func(r kv.Record) error { got = append(got, r.Key); return nil })
	require.NoError(t, err)
	assert.Equal(t, []string{"a/1", "a/2"}, got)
}

func TestScan_ExpiredFiltered(t *testing.T) {
	now := time.Now()
	clock := &fakeClock{t: now}
	d := New(Options{SweeperInterval: -1, NowFunc: clock.Now})
	t.Cleanup(func() { _ = d.Close() })

	ctx := context.Background()
	_, _ = d.Put(ctx, "ns", "live", kv.PutOptions{Value: []byte("v"), TTL: time.Hour})
	_, _ = d.Put(ctx, "ns", "dead", kv.PutOptions{Value: []byte("v"), TTL: time.Second})

	clock.advance(2 * time.Second)
	var got []string
	_ = d.Scan(ctx, "ns", kv.ScanOptions{}, func(r kv.Record) error { got = append(got, r.Key); return nil })
	assert.Equal(t, []string{"live"}, got)
}

func TestScan_YieldError(t *testing.T) {
	d := newDriver(t)
	ctx := context.Background()
	for _, k := range []string{"a", "b", "c"} {
		_, _ = d.Put(ctx, "ns", k, kv.PutOptions{Value: []byte(k)})
	}
	bomb := errors.New("boom")
	err := d.Scan(ctx, "ns", kv.ScanOptions{},
		func(r kv.Record) error { return bomb })
	require.ErrorIs(t, err, bomb)
}

func TestConcurrentPut(t *testing.T) {
	d := newDriver(t)
	ctx := context.Background()
	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 100; j++ {
				_, _ = d.Put(ctx, "ns", "shared", kv.PutOptions{Value: []byte("x")})
			}
		}()
	}
	wg.Wait()
	got, err := d.Get(ctx, "ns", "shared")
	require.NoError(t, err)
	assert.Equal(t, uint64(50*100), got.Version)
}

type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *fakeClock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}
