package memory

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"memsidecar/internal/lease"
)

func TestAcquire_Release_Round(t *testing.T) {
	d := New(Options{})
	ctx := context.Background()

	l, err := d.Acquire(ctx, "ns", "k", lease.AcquireOptions{TTL: time.Minute})
	require.NoError(t, err)
	assert.NotEmpty(t, l.HolderID)
	assert.Equal(t, "ns", l.Namespace)
	assert.Equal(t, "k", l.Key)

	cur, held, err := d.Inspect(ctx, "ns", "k")
	require.NoError(t, err)
	assert.True(t, held)
	assert.Equal(t, l.HolderID, cur.HolderID)

	existed, err := d.Release(ctx, "ns", "k", l.HolderID)
	require.NoError(t, err)
	assert.True(t, existed)

	_, held, _ = d.Inspect(ctx, "ns", "k")
	assert.False(t, held)
}

func TestAcquire_ConflictFastFail(t *testing.T) {
	d := New(Options{})
	ctx := context.Background()
	_, err := d.Acquire(ctx, "ns", "k", lease.AcquireOptions{TTL: time.Minute})
	require.NoError(t, err)

	_, err = d.Acquire(ctx, "ns", "k", lease.AcquireOptions{TTL: time.Minute})
	require.ErrorIs(t, err, lease.ErrAlreadyHeld)
}

func TestAcquire_WaitForUnblocksOnRelease(t *testing.T) {
	d := New(Options{PollInterval: 10 * time.Millisecond})
	ctx := context.Background()
	first, err := d.Acquire(ctx, "ns", "k", lease.AcquireOptions{TTL: time.Hour})
	require.NoError(t, err)

	// Background acquire blocks; release after a short delay frees the key.
	got := make(chan lease.Lease, 1)
	errCh := make(chan error, 1)
	go func() {
		l, err := d.Acquire(ctx, "ns", "k", lease.AcquireOptions{
			TTL: time.Minute, WaitFor: 500 * time.Millisecond,
		})
		if err != nil {
			errCh <- err
			return
		}
		got <- l
	}()
	time.Sleep(50 * time.Millisecond)
	_, _ = d.Release(ctx, "ns", "k", first.HolderID)
	select {
	case l := <-got:
		assert.NotEqual(t, first.HolderID, l.HolderID)
	case err := <-errCh:
		t.Fatalf("unexpected error: %v", err)
	case <-time.After(2 * time.Second):
		t.Fatal("Acquire did not unblock after Release")
	}
}

func TestAcquire_WaitForTimeout(t *testing.T) {
	d := New(Options{PollInterval: 10 * time.Millisecond})
	ctx := context.Background()
	_, _ = d.Acquire(ctx, "ns", "k", lease.AcquireOptions{TTL: time.Hour})

	start := time.Now()
	_, err := d.Acquire(ctx, "ns", "k", lease.AcquireOptions{
		TTL: time.Minute, WaitFor: 100 * time.Millisecond,
	})
	elapsed := time.Since(start)
	require.ErrorIs(t, err, lease.ErrAlreadyHeld)
	assert.GreaterOrEqual(t, elapsed.Milliseconds(), int64(90))
}

func TestAcquire_TakesOverExpired(t *testing.T) {
	d := New(Options{})
	ctx := context.Background()
	_, err := d.Acquire(ctx, "ns", "k", lease.AcquireOptions{TTL: 10 * time.Millisecond})
	require.NoError(t, err)
	time.Sleep(30 * time.Millisecond)
	l, err := d.Acquire(ctx, "ns", "k", lease.AcquireOptions{TTL: time.Minute})
	require.NoError(t, err)
	assert.NotEmpty(t, l.HolderID)
}

func TestRenew(t *testing.T) {
	d := New(Options{})
	ctx := context.Background()
	l, err := d.Acquire(ctx, "ns", "k", lease.AcquireOptions{TTL: time.Minute})
	require.NoError(t, err)
	exp1 := l.ExpiresAt

	time.Sleep(10 * time.Millisecond)
	l2, err := d.Renew(ctx, "ns", "k", l.HolderID, 2*time.Minute)
	require.NoError(t, err)
	assert.True(t, l2.ExpiresAt.After(exp1))

	_, err = d.Renew(ctx, "ns", "k", "wrong", time.Minute)
	require.ErrorIs(t, err, lease.ErrNotHeld)
}

func TestRelease_WrongHolder(t *testing.T) {
	d := New(Options{})
	ctx := context.Background()
	_, _ = d.Acquire(ctx, "ns", "k", lease.AcquireOptions{TTL: time.Minute})
	_, err := d.Release(ctx, "ns", "k", "imposter")
	require.ErrorIs(t, err, lease.ErrNotHeld)
}

func TestConcurrentAcquire_AtMostOneWins(t *testing.T) {
	d := New(Options{})
	ctx := context.Background()
	var wg sync.WaitGroup
	var wins int
	var mu sync.Mutex
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := d.Acquire(ctx, "ns", "k", lease.AcquireOptions{TTL: time.Minute})
			if err == nil {
				mu.Lock()
				wins++
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	assert.Equal(t, 1, wins)
}
