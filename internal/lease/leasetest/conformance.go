// Package leasetest provides a driver-agnostic conformance suite that every
// lease.Driver implementation must pass.
package leasetest

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"memsidecar/internal/lease"
)

// Harness adapts a concrete driver to the conformance suite.
//
// New must return a freshly isolated driver. The driver should be configured
// with a short poll interval (≤50ms) so WaitFor-based tests complete quickly;
// drivers without polling can ignore that.
type Harness interface {
	New(t *testing.T) lease.Driver
}

// RunConformance runs the full conformance battery against the driver
// produced by h.
func RunConformance(t *testing.T, h Harness) {
	t.Helper()
	cases := []struct {
		name string
		fn   func(*testing.T, Harness)
	}{
		{"AcquireRelease_Round", testAcquireReleaseRound},
		{"Acquire_ConflictFastFail", testAcquireConflictFastFail},
		{"Acquire_WaitForUnblocksOnRelease", testAcquireWaitForUnblocks},
		{"Acquire_WaitForTimeout", testAcquireWaitForTimeout},
		{"Acquire_TakesOverExpired", testAcquireTakesOverExpired},
		{"Renew", testRenew},
		{"Release_WrongHolder", testReleaseWrongHolder},
		{"ConcurrentAcquire_AtMostOneWins", testConcurrentAcquireAtMostOneWins},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) { tc.fn(t, h) })
	}
}

func testAcquireReleaseRound(t *testing.T, h Harness) {
	d := h.New(t)
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

	_, held, err = d.Inspect(ctx, "ns", "k")
	require.NoError(t, err)
	assert.False(t, held)
}

func testAcquireConflictFastFail(t *testing.T, h Harness) {
	d := h.New(t)
	ctx := context.Background()
	_, err := d.Acquire(ctx, "ns", "k", lease.AcquireOptions{TTL: time.Minute})
	require.NoError(t, err)

	_, err = d.Acquire(ctx, "ns", "k", lease.AcquireOptions{TTL: time.Minute})
	require.ErrorIs(t, err, lease.ErrAlreadyHeld)
}

func testAcquireWaitForUnblocks(t *testing.T, h Harness) {
	d := h.New(t)
	ctx := context.Background()
	first, err := d.Acquire(ctx, "ns", "k", lease.AcquireOptions{TTL: time.Hour})
	require.NoError(t, err)

	got := make(chan lease.Lease, 1)
	errCh := make(chan error, 1)
	go func() {
		l, err := d.Acquire(ctx, "ns", "k", lease.AcquireOptions{
			TTL: time.Minute, WaitFor: 2 * time.Second,
		})
		if err != nil {
			errCh <- err
			return
		}
		got <- l
	}()
	time.Sleep(100 * time.Millisecond)
	_, _ = d.Release(ctx, "ns", "k", first.HolderID)

	select {
	case l := <-got:
		assert.NotEqual(t, first.HolderID, l.HolderID)
	case err := <-errCh:
		t.Fatalf("unexpected error: %v", err)
	case <-time.After(3 * time.Second):
		t.Fatal("Acquire did not unblock after Release")
	}
}

func testAcquireWaitForTimeout(t *testing.T, h Harness) {
	d := h.New(t)
	ctx := context.Background()
	_, err := d.Acquire(ctx, "ns", "k", lease.AcquireOptions{TTL: time.Hour})
	require.NoError(t, err)

	start := time.Now()
	_, err = d.Acquire(ctx, "ns", "k", lease.AcquireOptions{
		TTL: time.Minute, WaitFor: 200 * time.Millisecond,
	})
	elapsed := time.Since(start)
	require.ErrorIs(t, err, lease.ErrAlreadyHeld)
	assert.GreaterOrEqual(t, elapsed, 180*time.Millisecond,
		"WaitFor should have waited close to the requested duration")
}

func testAcquireTakesOverExpired(t *testing.T, h Harness) {
	d := h.New(t)
	ctx := context.Background()
	_, err := d.Acquire(ctx, "ns", "k", lease.AcquireOptions{TTL: 500 * time.Millisecond})
	require.NoError(t, err)

	time.Sleep(700 * time.Millisecond)
	l, err := d.Acquire(ctx, "ns", "k", lease.AcquireOptions{TTL: time.Minute})
	require.NoError(t, err)
	assert.NotEmpty(t, l.HolderID)
}

func testRenew(t *testing.T, h Harness) {
	d := h.New(t)
	ctx := context.Background()
	l, err := d.Acquire(ctx, "ns", "k", lease.AcquireOptions{TTL: time.Minute})
	require.NoError(t, err)
	exp1 := l.ExpiresAt

	time.Sleep(50 * time.Millisecond)
	l2, err := d.Renew(ctx, "ns", "k", l.HolderID, 2*time.Minute)
	require.NoError(t, err)
	assert.True(t, l2.ExpiresAt.After(exp1))

	_, err = d.Renew(ctx, "ns", "k", "wrong", time.Minute)
	require.ErrorIs(t, err, lease.ErrNotHeld)
}

func testReleaseWrongHolder(t *testing.T, h Harness) {
	d := h.New(t)
	ctx := context.Background()
	_, err := d.Acquire(ctx, "ns", "k", lease.AcquireOptions{TTL: time.Minute})
	require.NoError(t, err)
	_, err = d.Release(ctx, "ns", "k", "imposter")
	require.ErrorIs(t, err, lease.ErrNotHeld)
}

func testConcurrentAcquireAtMostOneWins(t *testing.T, h Harness) {
	d := h.New(t)
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
