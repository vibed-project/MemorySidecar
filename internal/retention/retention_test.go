package retention

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"memsidecar/internal/auth"
	"memsidecar/internal/episodic"
	epmem "memsidecar/internal/episodic/drivers/memory"
)

// fixedClock returns a NowFunc pinned at t (for stamping event timestamps).
func fixedClock(t time.Time) func() time.Time { return func() time.Time { return t } }

func liveEvents(t *testing.T, d episodic.Driver, ns string) []episodic.Event {
	t.Helper()
	var out []episodic.Event
	require.NoError(t, d.Range(context.Background(), ns, episodic.RangeOptions{},
		func(e episodic.Event) error { out = append(out, e); return nil }))
	return out
}

func appendN(t *testing.T, d episodic.Driver, ns string, n int) {
	t.Helper()
	for i := 0; i < n; i++ {
		_, err := d.Append(context.Background(), ns, episodic.AppendOptions{Type: "t", Payload: []byte("p")})
		require.NoError(t, err)
	}
}

// setup binds a fresh memory driver whose events are stamped at eventTime, and a
// scheduler whose wall clock is schedTime, carrying the single given policy.
func setup(t *testing.T, isolate bool, eventTime, schedTime time.Time, p Policy) (*Scheduler, episodic.Driver) {
	t.Helper()
	d := epmem.New(epmem.Options{NowFunc: fixedClock(eventTime)})
	reg := episodic.NewRegistry()
	require.NoError(t, reg.Bind(p.Namespace, d))
	t.Cleanup(func() { _ = reg.Close() })
	s := New(reg, isolate, []Policy{p}, nil, nil)
	s.now = fixedClock(schedTime)
	return s, d
}

func TestSweep_MaxAge_HardDelete(t *testing.T) {
	t0 := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	p := Policy{Namespace: "events", MaxAge: time.Hour, Action: episodic.ExpireHardDelete}
	// Events stamped at t0; scheduler runs 2h later, so all are past the 1h window.
	s, d := setup(t, false, t0, t0.Add(2*time.Hour), p)
	appendN(t, d, "events", 4)
	require.Len(t, liveEvents(t, d, "events"), 4)

	s.sweep(context.Background(), p)
	assert.Empty(t, liveEvents(t, d, "events"), "all events older than max_age should be gone")
}

func TestSweep_MaxAge_KeepsFresh(t *testing.T) {
	t0 := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	p := Policy{Namespace: "events", MaxAge: time.Hour, Action: episodic.ExpireHardDelete}
	// Scheduler runs only 30m after the events — inside the 1h window.
	s, d := setup(t, false, t0, t0.Add(30*time.Minute), p)
	appendN(t, d, "events", 3)

	s.sweep(context.Background(), p)
	assert.Len(t, liveEvents(t, d, "events"), 3, "events within max_age must be kept")
}

func TestSweep_MaxItems_KeepsNewest(t *testing.T) {
	t0 := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	p := Policy{Namespace: "events", MaxItems: 2, Action: episodic.ExpireHardDelete}
	s, d := setup(t, false, t0, t0, p)
	appendN(t, d, "events", 5) // cursors 1..5

	s.sweep(context.Background(), p)
	got := liveEvents(t, d, "events")
	require.Len(t, got, 2)
	assert.Equal(t, uint64(4), got[0].Cursor)
	assert.Equal(t, uint64(5), got[1].Cursor)
}

func TestSweep_SoftDelete_Tombstones(t *testing.T) {
	t0 := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	p := Policy{Namespace: "events", MaxAge: time.Hour, Action: episodic.ExpireSoftDelete}
	s, d := setup(t, false, t0, t0.Add(2*time.Hour), p)
	appendN(t, d, "events", 2)

	s.sweep(context.Background(), p)
	assert.Empty(t, liveEvents(t, d, "events"), "soft-deleted events are hidden from a normal Range")

	// But they survive and are returned with include_deleted.
	var withDeleted []episodic.Event
	require.NoError(t, d.Range(context.Background(), "events", episodic.RangeOptions{IncludeDeleted: true},
		func(e episodic.Event) error { withDeleted = append(withDeleted, e); return nil }))
	assert.Len(t, withDeleted, 2)
	assert.False(t, withDeleted[0].DeletedAt.IsZero())
}

// With tenant isolation on, one config namespace fans out to every tenant's
// qualified storage namespace via the driver's NamespaceLister capability.
func TestSweep_TenantIsolation_FansOut(t *testing.T) {
	t0 := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	p := Policy{Namespace: "events", MaxAge: time.Hour, Action: episodic.ExpireHardDelete}
	s, d := setup(t, true, t0, t0.Add(2*time.Hour), p)

	acme := auth.QualifyNamespace("acme", "events", true)
	beta := auth.QualifyNamespace("beta", "events", true)
	other := auth.QualifyNamespace("acme", "other", true) // different config ns; must be untouched
	appendN(t, d, acme, 3)
	appendN(t, d, beta, 2)
	appendN(t, d, other, 1)

	s.sweep(context.Background(), p)
	assert.Empty(t, liveEvents(t, d, acme))
	assert.Empty(t, liveEvents(t, d, beta))
	assert.Len(t, liveEvents(t, d, other), 1, "a namespace outside the policy is not pruned")
}

func TestHelpers(t *testing.T) {
	// Lock the seconds→duration default and the action mapping.
	assert.Equal(t, defaultInterval, interval(0))
	assert.Equal(t, 90*time.Second, interval(90))
	assert.Equal(t, episodic.ExpireHardDelete, action(""))
	assert.Equal(t, episodic.ExpireHardDelete, action("hard"))
	assert.Equal(t, episodic.ExpireSoftDelete, action("soft"))
}
