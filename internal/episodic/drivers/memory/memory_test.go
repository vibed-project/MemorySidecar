package memory

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

func newDriver(t *testing.T) *Driver {
	t.Helper()
	d := New(Options{})
	t.Cleanup(func() { _ = d.Close() })
	return d
}

func TestAppend_CursorMonotonic(t *testing.T) {
	d := newDriver(t)
	ctx := context.Background()
	for i := 1; i <= 5; i++ {
		ev, err := d.Append(ctx, "ns", episodic.AppendOptions{Type: "x", Payload: []byte("v")})
		require.NoError(t, err)
		assert.Equal(t, uint64(i), ev.Cursor)
		assert.NotEmpty(t, ev.ID)
	}
}

func TestAppend_NamespacesIndependent(t *testing.T) {
	d := newDriver(t)
	ctx := context.Background()
	a1, _ := d.Append(ctx, "a", episodic.AppendOptions{Type: "x"})
	a2, _ := d.Append(ctx, "a", episodic.AppendOptions{Type: "x"})
	b1, _ := d.Append(ctx, "b", episodic.AppendOptions{Type: "x"})
	assert.Equal(t, uint64(1), a1.Cursor)
	assert.Equal(t, uint64(2), a2.Cursor)
	assert.Equal(t, uint64(1), b1.Cursor)
}

func TestRange(t *testing.T) {
	d := newDriver(t)
	ctx := context.Background()
	for i := 0; i < 5; i++ {
		_, _ = d.Append(ctx, "ns", episodic.AppendOptions{Type: "x", Payload: []byte{byte(i)}})
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

func TestTail_LiveOnly(t *testing.T) {
	d := newDriver(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// One pre-existing event should NOT be replayed (include_historical=false).
	_, _ = d.Append(ctx, "ns", episodic.AppendOptions{Type: "old"})

	got := make(chan uint64, 4)
	done := make(chan error, 1)
	go func() {
		done <- d.Tail(ctx, "ns", episodic.TailOptions{},
			func(e episodic.Event) error { got <- e.Cursor; return nil })
	}()

	// Wait for tail to register before appending.
	time.Sleep(50 * time.Millisecond)
	_, _ = d.Append(ctx, "ns", episodic.AppendOptions{Type: "new1"})
	_, _ = d.Append(ctx, "ns", episodic.AppendOptions{Type: "new2"})

	assert.Equal(t, uint64(2), <-got)
	assert.Equal(t, uint64(3), <-got)
	cancel()
	err := <-done
	assert.ErrorIs(t, err, context.Canceled)
}

func TestTail_WithHistorical(t *testing.T) {
	d := newDriver(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	for i := 0; i < 3; i++ {
		_, _ = d.Append(ctx, "ns", episodic.AppendOptions{Type: "h"})
	}

	got := make(chan uint64, 16)
	done := make(chan error, 1)
	go func() {
		done <- d.Tail(ctx, "ns",
			episodic.TailOptions{IncludeHistorical: true, AfterCursor: 1},
			func(e episodic.Event) error { got <- e.Cursor; return nil })
	}()

	// Receive historical cursors 2, 3 first.
	assert.Equal(t, uint64(2), <-got)
	assert.Equal(t, uint64(3), <-got)

	time.Sleep(50 * time.Millisecond)
	_, _ = d.Append(ctx, "ns", episodic.AppendOptions{Type: "live"})
	assert.Equal(t, uint64(4), <-got)
	cancel()
	<-done
}

func TestTail_SlowSubscriberDetached(t *testing.T) {
	d := newDriver(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Subscriber that never reads its yield channel: simulate by stalling yield.
	release := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		done <- d.Tail(ctx, "ns", episodic.TailOptions{}, func(e episodic.Event) error {
			<-release // block forever until released
			return nil
		})
	}()
	time.Sleep(50 * time.Millisecond) // let tail register

	// Append more than the subscriber buffer can hold to force a drop.
	// One event is consumed-but-blocked inside yield, so buffer-size+1 more
	// fills the channel and the next Append drops the subscriber.
	for i := 0; i < subscriberBufferSize+5; i++ {
		_, _ = d.Append(ctx, "ns", episodic.AppendOptions{Type: "spam"})
	}

	close(release) // let the subscriber unblock from its first event

	select {
	case err := <-done:
		assert.ErrorIs(t, err, ErrSubscriberLagged)
	case <-time.After(2 * time.Second):
		t.Fatal("subscriber was not detached")
	}
}

func TestTail_ContextCancel(t *testing.T) {
	d := newDriver(t)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- d.Tail(ctx, "ns", episodic.TailOptions{}, func(e episodic.Event) error { return nil })
	}()
	time.Sleep(30 * time.Millisecond)
	cancel()
	select {
	case err := <-done:
		assert.ErrorIs(t, err, context.Canceled)
	case <-time.After(time.Second):
		t.Fatal("Tail did not return on context cancellation")
	}
}

func TestConcurrentAppend(t *testing.T) {
	d := newDriver(t)
	ctx := context.Background()
	var wg sync.WaitGroup
	var n int64
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 50; j++ {
				_, err := d.Append(ctx, "ns", episodic.AppendOptions{Type: "x"})
				if err == nil {
					atomic.AddInt64(&n, 1)
				}
			}
		}()
	}
	wg.Wait()
	assert.Equal(t, int64(1000), atomic.LoadInt64(&n))

	// Cursors must be unique and dense.
	seen := make(map[uint64]struct{}, 1000)
	_ = d.Range(ctx, "ns", episodic.RangeOptions{}, func(e episodic.Event) error {
		seen[e.Cursor] = struct{}{}
		return nil
	})
	assert.Len(t, seen, 1000)
}
