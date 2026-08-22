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

	"github.com/vibed-project/mindD/internal/episodic"
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
		{"RoleSession", testRoleSession},
		{"Range_TimeWindow", testRangeTimeWindow},
		{"Tail_LiveOnly", testTailLiveOnly},
		{"Tail_WithHistorical", testTailWithHistorical},
		{"Tail_ContextCancel", testTailContextCancel},
		{"ConcurrentAppend", testConcurrentAppend},
		{"Dedup_Idempotent", testDedupIdempotent},
		{"Supersedes_Tombstones", testSupersedesTombstones},
		{"Expire_SoftDelete", testExpireSoftDelete},
		{"Expire_HardDelete", testExpireHardDelete},
		{"Expire_MaxRowsBound", testExpireMaxRowsBound},
		{"Expire_Validation", testExpireValidation},
		{"Range_SessionRoleTypeFilter", testRangeSessionRoleTypeFilter},
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

// testRoleSession checks that role/session_id round-trip through Append and
// Range, and that omitting them yields empty strings (additive, R2).
func testRoleSession(t *testing.T, h Harness) {
	d := h.New(t)
	ctx := context.Background()

	ev, err := d.Append(ctx, "ns", episodic.AppendOptions{
		Type: "message", Role: "user", SessionID: "sess-1",
	})
	require.NoError(t, err)
	assert.Equal(t, "user", ev.Role)
	assert.Equal(t, "sess-1", ev.SessionID)

	_, err = d.Append(ctx, "ns", episodic.AppendOptions{Type: "tool_call"})
	require.NoError(t, err)

	var got []episodic.Event
	require.NoError(t, d.Range(ctx, "ns", episodic.RangeOptions{}, func(e episodic.Event) error {
		got = append(got, e)
		return nil
	}))
	require.Len(t, got, 2)
	assert.Equal(t, "user", got[0].Role)
	assert.Equal(t, "sess-1", got[0].SessionID)
	assert.Empty(t, got[1].Role)
	assert.Empty(t, got[1].SessionID)
}

// testRangeTimeWindow checks the exclusive after_time / before_time bounds and
// that they combine (AND) with cursor bounds (R2).
func testRangeTimeWindow(t *testing.T, h Harness) {
	d := h.New(t)
	ctx := context.Background()

	// Space appends out so every event gets a distinct timestamp on both the
	// memory (wall clock) and Postgres (per-tx now()) drivers.
	var ts []time.Time
	for i := 0; i < 3; i++ {
		ev, err := d.Append(ctx, "ns", episodic.AppendOptions{Type: "x"})
		require.NoError(t, err)
		ts = append(ts, ev.Timestamp)
		time.Sleep(3 * time.Millisecond)
	}
	collect := func(opts episodic.RangeOptions) []uint64 {
		var got []uint64
		require.NoError(t, d.Range(ctx, "ns", opts, func(e episodic.Event) error {
			got = append(got, e.Cursor)
			return nil
		}))
		return got
	}

	// after_time is exclusive → drops event 1.
	assert.Equal(t, []uint64{2, 3}, collect(episodic.RangeOptions{AfterTime: ts[0]}))
	// before_time is exclusive → drops event 3.
	assert.Equal(t, []uint64{1, 2}, collect(episodic.RangeOptions{BeforeTime: ts[2]}))
	// The open window (ts[0], ts[2]) keeps only event 2.
	assert.Equal(t, []uint64{2}, collect(episodic.RangeOptions{AfterTime: ts[0], BeforeTime: ts[2]}))
	// Time and cursor bounds AND together.
	assert.Equal(t, []uint64{2}, collect(episodic.RangeOptions{AfterTime: ts[0], BeforeCursor: 3}))
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

// cursorsOf collects the cursors a Range yields, in order.
func cursorsOf(t *testing.T, d episodic.Driver, opts episodic.RangeOptions) []uint64 {
	t.Helper()
	var got []uint64
	require.NoError(t, d.Range(context.Background(), "ns", opts, func(e episodic.Event) error {
		got = append(got, e.Cursor)
		return nil
	}))
	return got
}

// eventsOf collects the events a Range yields, in order.
func eventsOf(t *testing.T, d episodic.Driver, opts episodic.RangeOptions) []episodic.Event {
	t.Helper()
	var got []episodic.Event
	require.NoError(t, d.Range(context.Background(), "ns", opts, func(e episodic.Event) error {
		got = append(got, e)
		return nil
	}))
	return got
}

// testDedupIdempotent checks that a repeated Append with the same dedup_key
// returns the already-stored event (same id/cursor/payload) without writing a
// second event or advancing the cursor, and that keys are scoped per namespace.
func testDedupIdempotent(t *testing.T, h Harness) {
	d := h.New(t)
	ctx := context.Background()

	ev1, err := d.Append(ctx, "ns", episodic.AppendOptions{Type: "x", Payload: []byte("first"), DedupKey: "k1"})
	require.NoError(t, err)
	assert.Equal(t, uint64(1), ev1.Cursor)

	// Replay: same key, different payload → returns the original unchanged.
	ev2, err := d.Append(ctx, "ns", episodic.AppendOptions{Type: "x", Payload: []byte("second"), DedupKey: "k1"})
	require.NoError(t, err)
	assert.Equal(t, ev1.ID, ev2.ID)
	assert.Equal(t, ev1.Cursor, ev2.Cursor)
	assert.Equal(t, []byte("first"), ev2.Payload)

	// A fresh key writes and takes the NEXT cursor — the replay burned none.
	ev3, err := d.Append(ctx, "ns", episodic.AppendOptions{Type: "x", DedupKey: "k2"})
	require.NoError(t, err)
	assert.Equal(t, uint64(2), ev3.Cursor)
	assert.Equal(t, []uint64{1, 2}, cursorsOf(t, d, episodic.RangeOptions{}))

	// Empty dedup_key never dedups.
	_, err = d.Append(ctx, "ns", episodic.AppendOptions{Type: "x"})
	require.NoError(t, err)
	_, err = d.Append(ctx, "ns", episodic.AppendOptions{Type: "x"})
	require.NoError(t, err)
	assert.Len(t, cursorsOf(t, d, episodic.RangeOptions{}), 4)
}

// testSupersedesTombstones checks that appending an event with supersedes
// tombstones the named live events (hidden from a default Range, visible with
// IncludeDeleted), round-trips supersedes/source, and ignores unknown ids.
func testSupersedesTombstones(t *testing.T, h Harness) {
	d := h.New(t)
	ctx := context.Background()

	a, err := d.Append(ctx, "ns", episodic.AppendOptions{Type: "fact", Payload: []byte("v1")})
	require.NoError(t, err)
	b, err := d.Append(ctx, "ns", episodic.AppendOptions{
		Type: "fact", Payload: []byte("v2"), Supersedes: []string{a.ID}, Source: "corr-1",
	})
	require.NoError(t, err)
	assert.Equal(t, []string{a.ID}, b.Supersedes)
	assert.Equal(t, "corr-1", b.Source)

	// Default Range hides the tombstoned event a.
	assert.Equal(t, []uint64{b.Cursor}, cursorsOf(t, d, episodic.RangeOptions{}))

	// IncludeDeleted returns both; a carries a tombstone, b stays live and keeps
	// its provenance on the read path.
	all := eventsOf(t, d, episodic.RangeOptions{IncludeDeleted: true})
	require.Len(t, all, 2)
	assert.Equal(t, a.ID, all[0].ID)
	assert.False(t, all[0].DeletedAt.IsZero(), "superseded event should be tombstoned")
	assert.True(t, all[1].DeletedAt.IsZero(), "superseding event should be live")
	assert.Equal(t, "corr-1", all[1].Source)

	// Superseding an unknown id is a no-op, not an error.
	_, err = d.Append(ctx, "ns", episodic.AppendOptions{Type: "x", Supersedes: []string{"00000000-0000-4000-8000-000000000000"}})
	require.NoError(t, err)
}

// testExpireSoftDelete checks the cursor-windowed soft-delete retention sweep:
// affected count, visibility, and idempotency on re-run.
func testExpireSoftDelete(t *testing.T, h Harness) {
	d := h.New(t)
	ctx := context.Background()
	for i := 0; i < 5; i++ {
		_, err := d.Append(ctx, "ns", episodic.AppendOptions{Type: "x"})
		require.NoError(t, err)
	}

	// Tombstone everything with cursor < 3 (i.e. cursors 1 and 2).
	n, err := d.Expire(ctx, "ns", episodic.ExpireOptions{
		BeforeCursor: 3, Action: episodic.ExpireSoftDelete, MaxRows: 100,
	})
	require.NoError(t, err)
	assert.Equal(t, uint64(2), n)
	assert.Equal(t, []uint64{3, 4, 5}, cursorsOf(t, d, episodic.RangeOptions{}))
	assert.Equal(t, []uint64{1, 2, 3, 4, 5}, cursorsOf(t, d, episodic.RangeOptions{IncludeDeleted: true}))

	// Re-running over the same window affects nothing (already tombstoned).
	n, err = d.Expire(ctx, "ns", episodic.ExpireOptions{
		BeforeCursor: 3, Action: episodic.ExpireSoftDelete, MaxRows: 100,
	})
	require.NoError(t, err)
	assert.Equal(t, uint64(0), n)
}

// testExpireHardDelete checks physical removal and — critically — that cursors
// stay monotonic afterward: a hard delete must not let a later Append reuse a
// cursor value.
func testExpireHardDelete(t *testing.T, h Harness) {
	d := h.New(t)
	ctx := context.Background()
	for i := 0; i < 5; i++ {
		_, err := d.Append(ctx, "ns", episodic.AppendOptions{Type: "x"})
		require.NoError(t, err)
	}

	n, err := d.Expire(ctx, "ns", episodic.ExpireOptions{
		BeforeCursor: 4, Action: episodic.ExpireHardDelete, MaxRows: 100,
	})
	require.NoError(t, err)
	assert.Equal(t, uint64(3), n)
	// Gone even under IncludeDeleted.
	assert.Equal(t, []uint64{4, 5}, cursorsOf(t, d, episodic.RangeOptions{IncludeDeleted: true}))

	// The next append continues from the high-water mark, never reusing 1..3.
	ev, err := d.Append(ctx, "ns", episodic.AppendOptions{Type: "x"})
	require.NoError(t, err)
	assert.Equal(t, uint64(6), ev.Cursor)
}

// testExpireMaxRowsBound checks that max_rows caps the affected set to the
// oldest matches.
func testExpireMaxRowsBound(t *testing.T, h Harness) {
	d := h.New(t)
	ctx := context.Background()
	for i := 0; i < 5; i++ {
		_, err := d.Append(ctx, "ns", episodic.AppendOptions{Type: "x"})
		require.NoError(t, err)
	}

	n, err := d.Expire(ctx, "ns", episodic.ExpireOptions{
		BeforeCursor: 6, Action: episodic.ExpireSoftDelete, MaxRows: 2,
	})
	require.NoError(t, err)
	assert.Equal(t, uint64(2), n)
	// Only the two oldest (1, 2) were tombstoned.
	assert.Equal(t, []uint64{3, 4, 5}, cursorsOf(t, d, episodic.RangeOptions{}))
}

// testExpireValidation checks that Expire rejects an unbounded window and an
// unspecified action at the driver boundary.
func testExpireValidation(t *testing.T, h Harness) {
	d := h.New(t)
	ctx := context.Background()
	_, err := d.Append(ctx, "ns", episodic.AppendOptions{Type: "x"})
	require.NoError(t, err)

	_, err = d.Expire(ctx, "ns", episodic.ExpireOptions{Action: episodic.ExpireSoftDelete, MaxRows: 1})
	require.ErrorIs(t, err, episodic.ErrExpireWindowRequired)

	_, err = d.Expire(ctx, "ns", episodic.ExpireOptions{
		BeforeCursor: 5, Action: episodic.ExpireActionUnspecified, MaxRows: 1,
	})
	require.Error(t, err)
}

// testRangeSessionRoleTypeFilter checks the equality predicates on Range:
// each filters independently, empty means "no filter", and they AND with each
// other and with the cursor/time window.
func testRangeSessionRoleTypeFilter(t *testing.T, h Harness) {
	d := h.New(t)
	ctx := context.Background()

	// cursor: 1     2     3      4     5
	// sess:   s1    s1    s2     s1    s2
	// role:   user  asst  user   tool  user
	// type:   msg   msg   msg    call  obs
	appends := []episodic.AppendOptions{
		{Type: "msg", Role: "user", SessionID: "s1"},
		{Type: "msg", Role: "asst", SessionID: "s1"},
		{Type: "msg", Role: "user", SessionID: "s2"},
		{Type: "call", Role: "tool", SessionID: "s1"},
		{Type: "obs", Role: "user", SessionID: "s2"},
	}
	for _, a := range appends {
		_, err := d.Append(ctx, "ns", a)
		require.NoError(t, err)
	}

	// session_id alone.
	assert.Equal(t, []uint64{1, 2, 4}, cursorsOf(t, d, episodic.RangeOptions{SessionID: "s1"}))
	// role alone.
	assert.Equal(t, []uint64{1, 3, 5}, cursorsOf(t, d, episodic.RangeOptions{Role: "user"}))
	// type alone.
	assert.Equal(t, []uint64{1, 2, 3}, cursorsOf(t, d, episodic.RangeOptions{Type: "msg"}))
	// session AND role.
	assert.Equal(t, []uint64{1}, cursorsOf(t, d, episodic.RangeOptions{SessionID: "s1", Role: "user"}))
	// session AND role AND type.
	assert.Equal(t, []uint64{3}, cursorsOf(t, d, episodic.RangeOptions{SessionID: "s2", Role: "user", Type: "msg"}))
	// Predicate ANDs with the cursor window: session s2 with cursor > 3 → only 5.
	assert.Equal(t, []uint64{5}, cursorsOf(t, d, episodic.RangeOptions{SessionID: "s2", AfterCursor: 3}))
	// Empty predicates disable filtering — full log.
	assert.Equal(t, []uint64{1, 2, 3, 4, 5}, cursorsOf(t, d, episodic.RangeOptions{}))
	// No match → empty.
	assert.Empty(t, cursorsOf(t, d, episodic.RangeOptions{SessionID: "nope"}))
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
