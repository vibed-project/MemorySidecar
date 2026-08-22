package policy

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/vibed-project/mindD/internal/auth"
)

func cap(tenant string, ns string) *auth.Capability {
	return &auth.Capability{Scope: auth.Scope{Tenant: tenant, Agent: "a1"}}
}

func TestDefault_Allow_NoRules(t *testing.T) {
	e, err := NewRuleEngine(Spec{})
	require.NoError(t, err)
	d := e.PreRead(context.Background(), HookCtx{Capability: cap("acme", "")})
	assert.True(t, d.Allow)
}

func TestDefault_Deny_NoRules(t *testing.T) {
	e, _ := NewRuleEngine(Spec{Default: DefaultDeny})
	d := e.PreRead(context.Background(), HookCtx{Capability: cap("acme", "")})
	assert.False(t, d.Allow)
	assert.Equal(t, "default deny", d.Reason)
}

func TestDeny_ByTenant(t *testing.T) {
	e, _ := NewRuleEngine(Spec{
		Rules: []Rule{{
			Name:   "no-other-tenant",
			Effect: EffectDeny,
			Reason: "tenant blocked",
			Match:  Match{Tenants: []string{"other"}},
		}},
	})
	deny := e.PreWrite(context.Background(), HookCtx{Capability: cap("other", ""), Block: "kv", Op: auth.OpKVPut})
	assert.False(t, deny.Allow)
	assert.Equal(t, "tenant blocked", deny.Reason)

	allow := e.PreWrite(context.Background(), HookCtx{Capability: cap("acme", ""), Block: "kv", Op: auth.OpKVPut})
	assert.True(t, allow.Allow)
}

func TestDeny_ByOpVerb(t *testing.T) {
	e, _ := NewRuleEngine(Spec{
		Rules: []Rule{{Name: "no-writes", Effect: EffectDeny, Match: Match{Ops: []string{"put", "delete", "append"}}}},
	})
	deny := e.PreWrite(context.Background(), HookCtx{Capability: cap("acme", ""), Block: "kv", Op: auth.OpKVPut})
	assert.False(t, deny.Allow)
	allow := e.PreRead(context.Background(), HookCtx{Capability: cap("acme", ""), Block: "kv", Op: auth.OpKVGet})
	assert.True(t, allow.Allow)
}

func TestDeny_ByNamespaceGlob(t *testing.T) {
	e, _ := NewRuleEngine(Spec{
		Rules: []Rule{{
			Name: "no-secrets", Effect: EffectDeny,
			Match: Match{Namespaces: []string{"secret-*"}},
		}},
	})
	deny := e.PreRead(context.Background(), HookCtx{
		Capability: cap("acme", ""), Block: "kv", Namespace: "secret-keys", Op: auth.OpKVGet,
	})
	assert.False(t, deny.Allow)
	allow := e.PreRead(context.Background(), HookCtx{
		Capability: cap("acme", ""), Block: "kv", Namespace: "scratchpad", Op: auth.OpKVGet,
	})
	assert.True(t, allow.Allow)
}

func TestAllow_StopsRuleScan(t *testing.T) {
	// An earlier allow short-circuits later deny.
	e, _ := NewRuleEngine(Spec{
		Rules: []Rule{
			{Name: "allow-acme", Effect: EffectAllow, Match: Match{Tenants: []string{"acme"}}},
			{Name: "global-deny", Effect: EffectDeny},
		},
	})
	d := e.PreRead(context.Background(), HookCtx{Capability: cap("acme", ""), Block: "kv", Op: auth.OpKVGet})
	assert.True(t, d.Allow)
}

func TestRateLimit_DropsExtras(t *testing.T) {
	e, _ := NewRuleEngine(Spec{
		Rules: []Rule{{
			Name:   "tight",
			Effect: EffectRateLimit,
			Match:  Match{Ops: []string{"semantic.search"}},
			Bucket: Bucket{RatePerSecond: 0.01, Burst: 2, PerTenant: true},
		}},
	})
	h := HookCtx{Capability: cap("acme", ""), Block: "semantic", Op: auth.OpSemanticSearch}
	// Burst=2 → first two pass, third is denied.
	assert.True(t, e.PreRead(context.Background(), h).Allow)
	assert.True(t, e.PreRead(context.Background(), h).Allow)
	third := e.PreRead(context.Background(), h)
	assert.False(t, third.Allow)
	assert.Contains(t, third.Reason, "rate limited by tight")
}

func TestRateLimit_SeparateBucketsPerTenant(t *testing.T) {
	e, _ := NewRuleEngine(Spec{
		Rules: []Rule{{
			Name:   "per-tenant",
			Effect: EffectRateLimit,
			Match:  Match{Ops: []string{"semantic.search"}},
			Bucket: Bucket{RatePerSecond: 0.01, Burst: 1, PerTenant: true},
		}},
	})
	acme := HookCtx{Capability: cap("acme", ""), Op: auth.OpSemanticSearch}
	beta := HookCtx{Capability: cap("beta", ""), Op: auth.OpSemanticSearch}
	assert.True(t, e.PreRead(context.Background(), acme).Allow)
	assert.True(t, e.PreRead(context.Background(), beta).Allow) // separate bucket
	assert.False(t, e.PreRead(context.Background(), acme).Allow) // acme exhausted
}

func TestRateLimit_FallsThroughOnSuccess(t *testing.T) {
	// A rate_limit that passes should NOT short-circuit a later deny.
	e, _ := NewRuleEngine(Spec{
		Rules: []Rule{
			{Name: "rl", Effect: EffectRateLimit, Bucket: Bucket{RatePerSecond: 1000, Burst: 100}},
			{Name: "deny-all", Effect: EffectDeny},
		},
	})
	d := e.PreRead(context.Background(), HookCtx{Capability: cap("acme", ""), Op: auth.OpKVGet})
	assert.False(t, d.Allow)
	assert.Contains(t, d.Reason, "deny-all")
}

func TestValidate_DuplicateRuleName(t *testing.T) {
	_, err := NewRuleEngine(Spec{Rules: []Rule{
		{Name: "x", Effect: EffectAllow},
		{Name: "x", Effect: EffectDeny},
	}})
	require.ErrorContains(t, err, "duplicate rule name")
}

func TestValidate_UnknownEffect(t *testing.T) {
	_, err := NewRuleEngine(Spec{Rules: []Rule{{Name: "x", Effect: "redact"}}})
	require.ErrorContains(t, err, "unknown effect")
}

func TestValidate_RateLimitNeedsRate(t *testing.T) {
	_, err := NewRuleEngine(Spec{Rules: []Rule{{Name: "x", Effect: EffectRateLimit}}})
	require.ErrorContains(t, err, "rate_per_second must be > 0")
}
