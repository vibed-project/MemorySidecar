// Package episodictest provides a driver-agnostic conformance suite that
// every episodic.Driver implementation must pass.
package episodictest

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"memsidecar/internal/episodic"
)

// Harness adapts a concrete driver to the conformance suite.
//
// New must return a freshly isolated driver; the suite calls it once per
// subtest. TailSettleTime is how long the suite waits after starting a Tail
// before producing events — push-based drivers (memory) can use a few ms,
// poll-based drivers (Postgres) need to exceed their poll interval.
type Harness interface {
	New(t *testing.T) episodic.Driver
	TailSettleTime() time.Duration
}

// RunConformance runs the full conformance battery against the driver
// produced by h.
func RunConformance(t *testing.T, h Harness) {
	t.Helper()
	cases := []struct {
		name string
		fn   func(*testing.T, Harness)
	}{
		{"Append_CursorMonotonic", testAppendCursorMonotonic},
		{"Append_NamespacesIndependent", testAppendNamespacesIndependent},
		{"Range", testRange},
		{"Tail_LiveOnly", testTailLiveOnly},
		{"Tail_WithHistorical", testTailWithHistorical},
		{"Tail_ContextCancel", testTailContextCancel},
		{"ConcurrentAppend", testConcurrentAppend},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) { tc.fn(t, h) })
	}
}

func testAppendCursorMonotonic(t *testing.T, h Harness) {
	d := h.New(t)
	ctx := context.Background()
	for i := 1; i <= 5; i++ {
		ev, err := d.Append(ctx, "ns", episodic.AppendOptions{Type: "x", Payload: []byte("v")})
		require.NoError(t, err)
		assert.Equal(t, uint64(i), ev.Cursor)
		assert.NotEmpty(t, ev.ID)
	}
}

func testAppendNamespacesIndependent(t *testing.T, h Harness) {
	d := h.New(t)
	ctx := context.Background()
	a1, err := d.Append(ctx, "a", episodic.AppendOptions{Type: "x"})
	require.NoError(t, err)
	a2, err := d.Append(ctx, "a", episodic.AppendOptions{Type: "x"})
	require.NoError(t, err)
	b1, err := d.Append(ctx, "b", episodic.AppendOptions{Type: "x"})
	require.NoError(t, err)
	assert.Equal(t, uint64(1), a1.Cursor)
	assert.Equal(t, uint64(2), a2.Cursor)
	assert.Equal(t, uint64(1), b1.Cursor)
}

func testRange(t *testing.T, h Harness) {
	d := h.New(t)
	ctx := context.Background()
	for i := 0; i < 5; i++ {
		_, err := d.Append(ctx, "ns", episodic.AppendOptions{Type: "x", Payload: []byte{byte(i)}})
		require.NoError(t, err)
	}
	collect := func(opts episodic.RangeOptions) []uint64 {
		var got []uint64
		err := d.Range(ctx, "ns", opts, func(e episodic.Event) error {
			got = append(got, e.Cursor)
			return nil
		})
		require.NoError(t, err)
		return got
	}
	assert.Equal(t, []uint64{1, 2, 3, 4, 5}, collect(episodic.RangeOptions{}))
	assert.Equal(t, []uint64{3, 4, 5}, collect(episodic.RangeOptions{AfterCursor: 2}))
	assert.Equal(t, []uint64{2, 3}, collect(episodic.RangeOptions{AfterCursor: 1, BeforeCursor: 4}))
	assert.Equal(t, []uint64{1, 2}, collect(episodic.RangeOptions{Limit: 2}))
	assert.Equal(t, []uint64{5, 4, 3, 2, 1}, collect(episodic.RangeOptions{Reverse: true}))
}

func testTailLiveOnly(t *testing.T, h Harness) {
	d := h.New(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Pre-existing event must NOT be replayed when IncludeHistorical is false.
	_, err := d.Append(ctx, "ns", episodic.AppendOptions{Type: "old"})
	require.NoError(t, err)

	got := make(chan uint64, 4)
	done := make(chan error, 1)
	go func() {
		done <- d.Tail(ctx, "ns", episodic.TailOptions{},
			func(e episodic.Event) error { got <- e.Cursor; return nil })
	}()

	time.Sleep(h.TailSettleTime())
	_, err = d.Append(ctx, "ns", episodic.AppendOptions{Type: "new1"})
	require.NoError(t, err)
	_, err = d.Append(ctx, "ns", episodic.AppendOptions{Type: "new2"})
	require.NoError(t, err)

	assert.Equal(t, uint64(2), receive(t, got, h))
	assert.Equal(t, uint64(3), receive(t, got, h))

	cancel()
	<-done
}

func testTailWithHistorical(t *testing.T, h Harness) {
	d := h.New(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	for i := 0; i < 3; i++ {
		_, err := d.Append(ctx, "ns", episodic.AppendOptions{Type: "h"})
		require.NoError(t, err)
	}

	got := make(chan uint64, 16)
	done := make(chan error, 1)
	go func() {
		done <- d.Tail(ctx, "ns",
			episodic.TailOptions{IncludeHistorical: true, AfterCursor: 1},
			func(e episodic.Event) error { got <- e.Cursor; return nil })
	}()

	assert.Equal(t, uint64(2), receive(t, got, h))
	assert.Equal(t, uint64(3), receive(t, got, h))

	time.Sleep(h.TailSettleTime())
	_, err := d.Append(ctx, "ns", episodic.AppendOptions{Type: "live"})
	require.NoError(t, err)
	assert.Equal(t, uint64(4), receive(t, got, h))

	cancel()
	<-done
}

func testTailContextCancel(t *testing.T, h Harness) {
	d := h.New(t)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- d.Tail(ctx, "ns", episodic.TailOptions{}, func(e episodic.Event) error { return nil })
	}()
	time.Sleep(h.TailSettleTime())
	cancel()
	select {
	case err := <-done:
		assert.ErrorIs(t, err, context.Canceled)
	case <-time.After(2 * time.Second):
		t.Fatal("Tail did not return on context cancellation")
	}
}

func testConcurrentAppend(t *testing.T, h Harness) {
	d := h.New(t)
	ctx := context.Background()
	const (
		writers   = 20
		perWriter = 50
		total     = writers * perWriter
	)
	var wg sync.WaitGroup
	var n int64
	for i := 0; i < writers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < perWriter; j++ {
				_, err := d.Append(ctx, "ns", episodic.AppendOptions{Type: "x"})
				if err == nil {
					atomic.AddInt64(&n, 1)
				}
			}
		}()
	}
	wg.Wait()
	assert.Equal(t, int64(total), atomic.LoadInt64(&n))

	seen := make(map[uint64]struct{}, total)
	err := d.Range(ctx, "ns", episodic.RangeOptions{}, func(e episodic.Event) error {
		seen[e.Cursor] = struct{}{}
		return nil
	})
	require.NoError(t, err)
	assert.Len(t, seen, total)
}

func receive(t *testing.T, ch <-chan uint64, h Harness) uint64 {
	t.Helper()
	// Allow up to 10× the settle time, with a 2s floor, before declaring loss.
	budget := 10 * h.TailSettleTime()
	if budget < 2*time.Second {
		budget = 2 * time.Second
	}
	select {
	case v := <-ch:
		return v
	case <-time.After(budget):
		t.Fatalf("timed out waiting for tail event after %s", budget)
		return 0
	}
}
