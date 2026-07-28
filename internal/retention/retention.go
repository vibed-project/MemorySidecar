// Package retention runs scheduled, config-driven retention/GC over the
// episodic event log. It's the composition-root counterpart to the per-record
// TTL sweepers baked into the kv drivers: a background scheduler that
// periodically prunes accumulating data by policy (max age and/or newest-N),
// driving the episodic Expire primitive.
//
// It lives above the service layer and operates on raw storage namespaces, so
// with tenant isolation on it discovers and prunes every tenant's namespace via
// the optional NamespaceLister driver capability.
package retention

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"memsidecar/internal/auth"
	"memsidecar/internal/config"
	"memsidecar/internal/episodic"
	"memsidecar/internal/obs"
)

// defaultInterval is the sweep cadence when a policy sets none.
const defaultInterval = 5 * time.Minute

// expireBatch bounds how many events a single Expire call removes, so a sweep
// stays localized and yields between batches (mirrors ExpireOptions.MaxRows).
const expireBatch = 1000

// Policy is a resolved per-namespace episodic retention policy.
type Policy struct {
	Namespace string        // config namespace
	MaxAge    time.Duration // 0 = no age bound
	MaxItems  int           // 0 = no count bound (keep newest N by cursor)
	Action    episodic.ExpireAction
	Interval  time.Duration
}

// NamespaceLister is the optional driver capability that enumerates the storage
// namespaces it currently holds. Retention needs it to find tenant-qualified
// namespaces when tenant isolation is on; a driver without it is pruned only on
// its bare config namespace (and, under isolation, warns that tenant data is
// skipped).
type NamespaceLister interface {
	Namespaces(ctx context.Context) ([]string, error)
}

// Scheduler runs episodic retention policies on background tickers, one
// goroutine per policy.
type Scheduler struct {
	reg             *episodic.Registry
	tenantIsolation bool
	policies        []Policy
	evict           *obs.EvictionCounter
	log             *slog.Logger
	now             func() time.Time

	cancel context.CancelFunc
	wg     sync.WaitGroup
}

// New builds a Scheduler. Start must be called to begin sweeping.
func New(reg *episodic.Registry, tenantIsolation bool, policies []Policy, evict *obs.EvictionCounter, log *slog.Logger) *Scheduler {
	if log == nil {
		log = slog.Default()
	}
	return &Scheduler{
		reg:             reg,
		tenantIsolation: tenantIsolation,
		policies:        policies,
		evict:           evict,
		log:             log,
		now:             time.Now,
	}
}

// Start launches one goroutine per policy. It's a no-op when no policy is
// configured. Call once.
func (s *Scheduler) Start() {
	if len(s.policies) == 0 {
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	s.cancel = cancel
	for _, p := range s.policies {
		s.wg.Add(1)
		go s.loop(ctx, p)
	}
	s.log.Info("retention scheduler started", slog.Int("policies", len(s.policies)))
}

func (s *Scheduler) loop(ctx context.Context, p Policy) {
	defer s.wg.Done()
	t := time.NewTicker(p.Interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			s.sweep(ctx, p)
		}
	}
}

// Close stops all goroutines and waits for the in-flight sweep to finish.
func (s *Scheduler) Close() error {
	if s.cancel != nil {
		s.cancel()
	}
	s.wg.Wait()
	return nil
}

// sweep applies one policy across its target storage namespaces once. Exported
// behavior is tested through this method.
func (s *Scheduler) sweep(ctx context.Context, p Policy) {
	d, ok := s.reg.Resolve(p.Namespace)
	if !ok {
		return
	}
	for _, sns := range s.targets(ctx, d, p.Namespace) {
		var affected uint64
		if p.MaxAge > 0 {
			before := s.now().Add(-p.MaxAge)
			affected += s.expireLoop(ctx, d, sns, episodic.ExpireOptions{
				BeforeTime: before, Action: p.Action, MaxRows: expireBatch,
			})
		}
		if p.MaxItems > 0 {
			if maxCur, ok := s.maxCursor(ctx, d, sns); ok && maxCur > uint64(p.MaxItems) {
				// Keep the newest MaxItems (cursors maxCur-MaxItems+1 .. maxCur);
				// delete everything below that, exclusive upper bound BeforeCursor.
				before := maxCur - uint64(p.MaxItems) + 1
				affected += s.expireLoop(ctx, d, sns, episodic.ExpireOptions{
					BeforeCursor: before, Action: p.Action, MaxRows: expireBatch,
				})
			}
		}
		if affected > 0 {
			// Label the metric by config namespace so it aggregates across tenants.
			s.evict.Add("episodic", p.Namespace, obs.EvictionRetention, int64(affected))
			s.log.Debug("retention swept",
				slog.String("namespace", p.Namespace),
				slog.String("storage_namespace", sns),
				slog.Uint64("affected", affected))
		}
	}
}

// expireLoop drains the retention window in MaxRows-sized batches, returning the
// total affected. It stops on error, context cancellation, or a short batch.
func (s *Scheduler) expireLoop(ctx context.Context, d episodic.Driver, sns string, opts episodic.ExpireOptions) uint64 {
	var total uint64
	for {
		if ctx.Err() != nil {
			return total
		}
		n, err := d.Expire(ctx, sns, opts)
		if err != nil {
			s.log.Warn("retention expire failed",
				slog.String("storage_namespace", sns), slog.String("error", err.Error()))
			return total
		}
		total += n
		if n < uint64(opts.MaxRows) {
			return total // window drained
		}
	}
}

// maxCursor returns the highest cursor in a namespace (a cheap reverse-limit-1
// probe), including tombstoned events so the newest-N window is positional.
func (s *Scheduler) maxCursor(ctx context.Context, d episodic.Driver, sns string) (uint64, bool) {
	var c uint64
	var found bool
	err := d.Range(ctx, sns, episodic.RangeOptions{Reverse: true, Limit: 1, IncludeDeleted: true},
		func(e episodic.Event) error {
			c, found = e.Cursor, true
			return nil
		})
	if err != nil {
		s.log.Warn("retention max-cursor probe failed",
			slog.String("storage_namespace", sns), slog.String("error", err.Error()))
		return 0, false
	}
	return c, found
}

// targets returns the storage namespaces a policy prunes. Without isolation the
// config namespace is the storage namespace; with isolation on it enumerates
// the driver's namespaces and selects those that unqualify to the config name.
func (s *Scheduler) targets(ctx context.Context, d episodic.Driver, configNS string) []string {
	if !s.tenantIsolation {
		return []string{configNS}
	}
	lister, ok := d.(NamespaceLister)
	if !ok {
		s.log.Warn("retention: driver lacks namespace enumeration; tenant-isolated data will not be pruned",
			slog.String("namespace", configNS))
		return nil
	}
	all, err := lister.Namespaces(ctx)
	if err != nil {
		s.log.Warn("retention: namespace enumeration failed",
			slog.String("namespace", configNS), slog.String("error", err.Error()))
		return nil
	}
	var out []string
	for _, sns := range all {
		if auth.UnqualifyNamespace(sns) == configNS {
			out = append(out, sns)
		}
	}
	return out
}

// PoliciesFromConfig extracts the enabled episodic retention policies from cfg.
func PoliciesFromConfig(cfg *config.Config) []Policy {
	var out []Policy
	for _, ns := range cfg.Namespaces {
		if ns.Block != "episodic" || !ns.Retention.Enabled() {
			continue
		}
		out = append(out, Policy{
			Namespace: ns.Name,
			MaxAge:    time.Duration(ns.Retention.MaxAgeSeconds) * time.Second,
			MaxItems:  ns.Retention.MaxItems,
			Action:    action(ns.Retention.Action),
			Interval:  interval(ns.Retention.IntervalSeconds),
		})
	}
	return out
}

func action(s string) episodic.ExpireAction {
	if s == "soft" {
		return episodic.ExpireSoftDelete
	}
	return episodic.ExpireHardDelete // default: reclaim
}

func interval(seconds int) time.Duration {
	if seconds <= 0 {
		return defaultInterval
	}
	return time.Duration(seconds) * time.Second
}
