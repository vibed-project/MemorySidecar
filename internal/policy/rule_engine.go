package policy

import (
	"context"
	"fmt"
	"strings"
	"sync"

	"golang.org/x/time/rate"
)

// RuleEngine evaluates a Spec against each request. Rules are scanned in
// order; the first matching rule's effect determines the outcome.
//
//   - deny       → request rejected with rule.Reason
//   - allow      → request permitted, no further rules consulted
//   - rate_limit → request consumes a token from the rule's bucket; if no
//     token is available, rejected. If a token is taken, the
//     decision falls through to the next rule (so an allow can
//     follow). This lets you chain "rate-limit AND then allow"
//     in a single rule by setting effect: rate_limit only.
//
// If no rule matches, Spec.Default decides (default: allow).
type RuleEngine struct {
	spec    Spec
	mu      sync.Mutex
	buckets map[string]*rate.Limiter
}

// NewRuleEngine validates and compiles the spec.
func NewRuleEngine(spec Spec) (*RuleEngine, error) {
	if err := spec.validate(); err != nil {
		return nil, err
	}
	if spec.Default == "" {
		spec.Default = DefaultAllow
	}
	return &RuleEngine{spec: spec, buckets: map[string]*rate.Limiter{}}, nil
}

func (e *RuleEngine) PreRead(ctx context.Context, h HookCtx) Decision {
	return e.evaluate(ctx, h)
}

func (e *RuleEngine) PreWrite(ctx context.Context, h HookCtx) Decision {
	return e.evaluate(ctx, h)
}

func (e *RuleEngine) evaluate(_ context.Context, h HookCtx) Decision {
	for i := range e.spec.Rules {
		r := &e.spec.Rules[i]
		if !ruleMatches(r, h) {
			continue
		}
		switch r.Effect {
		case EffectAllow:
			return Decision{Allow: true, Reason: r.reasonOr("allowed by " + r.Name)}
		case EffectDeny:
			return Decision{Allow: false, Reason: r.reasonOr("denied by " + r.Name)}
		case EffectRateLimit:
			lim := e.limiterFor(r, h)
			if !lim.Allow() {
				return Decision{Allow: false, Exhausted: true, Reason: r.reasonOr("rate limited by " + r.Name)}
			}
			// Token consumed; fall through to subsequent rules.
		case EffectCap:
			if msg := capExceeded(r, h); msg != "" {
				return Decision{Allow: false, Exhausted: true, Reason: r.reasonOr(msg + " (rule " + r.Name + ")")}
			}
			// Within caps; fall through to subsequent rules.
		}
	}
	if e.spec.Default == DefaultDeny {
		return Decision{Allow: false, Reason: "default deny"}
	}
	return Decision{Allow: true}
}

func (e *RuleEngine) limiterFor(r *Rule, h HookCtx) *rate.Limiter {
	key := bucketKey(r, h)
	e.mu.Lock()
	defer e.mu.Unlock()
	if lim, ok := e.buckets[key]; ok {
		return lim
	}
	burst := r.Bucket.Burst
	if burst <= 0 {
		burst = 1
	}
	lim := rate.NewLimiter(rate.Limit(r.Bucket.RatePerSecond), burst)
	e.buckets[key] = lim
	return lim
}

func bucketKey(r *Rule, h HookCtx) string {
	parts := []string{r.Name}
	tenant := ""
	if h.Capability != nil {
		tenant = h.Capability.Scope.Tenant
	}
	agent := ""
	if h.Capability != nil {
		agent = h.Capability.Scope.Agent
	}
	if r.Bucket.PerTenant {
		parts = append(parts, "t="+tenant)
	}
	if r.Bucket.PerAgent {
		parts = append(parts, "a="+agent)
	}
	if r.Bucket.PerNamespace {
		parts = append(parts, "n="+h.Block+"/"+h.Namespace)
	}
	if r.Bucket.PerOp {
		parts = append(parts, "o="+string(h.Op))
	}
	return strings.Join(parts, "|")
}

// capExceeded returns a non-empty message when the request's magnitude breaches
// any set bound on the rule, otherwise "".
func capExceeded(r *Rule, h HookCtx) string {
	switch {
	case r.Max.TopK > 0 && h.TopK > r.Max.TopK:
		return fmt.Sprintf("top_k %d exceeds cap %d", h.TopK, r.Max.TopK)
	case r.Max.Limit > 0 && h.Limit > r.Max.Limit:
		return fmt.Sprintf("limit %d exceeds cap %d", h.Limit, r.Max.Limit)
	case r.Max.Depth > 0 && h.Depth > r.Max.Depth:
		return fmt.Sprintf("depth %d exceeds cap %d", h.Depth, r.Max.Depth)
	case r.Max.FanOut > 0 && h.FanOut > r.Max.FanOut:
		return fmt.Sprintf("fan_out %d exceeds cap %d", h.FanOut, r.Max.FanOut)
	}
	return ""
}

func ruleMatches(r *Rule, h HookCtx) bool {
	tenant, agent := "", ""
	if h.Capability != nil {
		tenant = h.Capability.Scope.Tenant
		agent = h.Capability.Scope.Agent
	}
	if !anyOrEmpty(r.Match.Tenants, tenant) {
		return false
	}
	if !anyOrEmpty(r.Match.Agents, agent) {
		return false
	}
	if !anyOrEmpty(r.Match.Blocks, h.Block) {
		return false
	}
	if !anyGlobOrEmpty(r.Match.Namespaces, h.Namespace) {
		return false
	}
	if len(r.Match.Ops) > 0 && !matchOp(r.Match.Ops, string(h.Op)) {
		return false
	}
	return true
}

// matchOp accepts either dotted form ("kv.put") or verb-only form ("put").
func matchOp(allowed []string, fullOp string) bool {
	verb := fullOp
	if i := strings.IndexByte(fullOp, '.'); i >= 0 {
		verb = fullOp[i+1:]
	}
	for _, a := range allowed {
		if a == fullOp || a == verb {
			return true
		}
	}
	return false
}

func anyOrEmpty(allowed []string, value string) bool {
	if len(allowed) == 0 {
		return true
	}
	for _, a := range allowed {
		if a == value {
			return true
		}
	}
	return false
}

func anyGlobOrEmpty(patterns []string, value string) bool {
	if len(patterns) == 0 {
		return true
	}
	for _, p := range patterns {
		if globMatch(p, value) {
			return true
		}
	}
	return false
}

// globMatch supports a single trailing "*" prefix wildcard.
func globMatch(pattern, value string) bool {
	if pattern == "*" || pattern == value {
		return true
	}
	if strings.HasSuffix(pattern, "*") {
		return strings.HasPrefix(value, strings.TrimSuffix(pattern, "*"))
	}
	return false
}

func (r *Rule) reasonOr(fallback string) string {
	if r.Reason != "" {
		return r.Reason
	}
	return fallback
}

func (s *Spec) validate() error {
	switch s.Default {
	case "", DefaultAllow, DefaultDeny:
	default:
		return fmt.Errorf("policy: unknown default %q (allow|deny)", s.Default)
	}
	seen := make(map[string]bool, len(s.Rules))
	for i, r := range s.Rules {
		if r.Name == "" {
			return fmt.Errorf("policy: rule %d: name required", i)
		}
		if seen[r.Name] {
			return fmt.Errorf("policy: duplicate rule name %q", r.Name)
		}
		seen[r.Name] = true
		switch r.Effect {
		case EffectAllow, EffectDeny:
		case EffectRateLimit:
			if r.Bucket.RatePerSecond <= 0 {
				return fmt.Errorf("policy: rule %q: bucket.rate_per_second must be > 0", r.Name)
			}
		case EffectCap:
			if r.Max == (Cap{}) {
				return fmt.Errorf("policy: rule %q: cap effect requires at least one max.* bound", r.Name)
			}
		default:
			return fmt.Errorf("policy: rule %q: unknown effect %q (allow|deny|rate_limit|cap)", r.Name, r.Effect)
		}
	}
	return nil
}
