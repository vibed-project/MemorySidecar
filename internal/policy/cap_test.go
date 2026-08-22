package policy

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/vibed-project/mindD/internal/auth"
)

func TestCap_TopK(t *testing.T) {
	e, err := NewRuleEngine(Spec{
		Rules: []Rule{{
			Name:   "search-topk-cap",
			Effect: EffectCap,
			Match:  Match{Blocks: []string{"semantic"}, Ops: []string{"search"}},
			Max:    Cap{TopK: 100},
		}},
	})
	require.NoError(t, err)

	// Within the cap → falls through to the default allow.
	within := e.PreRead(context.Background(), HookCtx{
		Capability: cap("acme", ""), Block: "semantic", Op: auth.OpSemanticSearch, TopK: 50,
	})
	assert.True(t, within.Allow)
	assert.False(t, within.Exhausted)

	// Over the cap → rejected as exhausted.
	over := e.PreRead(context.Background(), HookCtx{
		Capability: cap("acme", ""), Block: "semantic", Op: auth.OpSemanticSearch, TopK: 500,
	})
	assert.False(t, over.Allow)
	assert.True(t, over.Exhausted)
	assert.Contains(t, over.Reason, "top_k 500 exceeds cap 100")

	// A non-matching op is unaffected.
	other := e.PreRead(context.Background(), HookCtx{
		Capability: cap("acme", ""), Block: "kv", Op: auth.OpKVScan, Limit: 9999,
	})
	assert.True(t, other.Allow)
}

func TestCap_LimitDimension(t *testing.T) {
	e, err := NewRuleEngine(Spec{
		Rules: []Rule{{
			Name:   "scan-limit-cap",
			Effect: EffectCap,
			Match:  Match{Ops: []string{"scan"}},
			Max:    Cap{Limit: 1000},
		}},
	})
	require.NoError(t, err)
	over := e.PreRead(context.Background(), HookCtx{
		Capability: cap("acme", ""), Block: "kv", Op: auth.OpKVScan, Limit: 5000,
	})
	assert.False(t, over.Allow)
	assert.True(t, over.Exhausted)
	assert.Contains(t, over.Reason, "limit 5000 exceeds cap 1000")
}

func TestCap_RerankCandidateK(t *testing.T) {
	e, err := NewRuleEngine(Spec{
		Rules: []Rule{{
			Name:   "hybrid-candidate-cap",
			Effect: EffectCap,
			Match:  Match{Ops: []string{"semantic.search"}},
			Max:    Cap{RerankCandidateK: 200},
		}},
	})
	require.NoError(t, err)

	within := e.PreRead(context.Background(), HookCtx{
		Capability: cap("acme", ""), Block: "semantic", Op: auth.OpSemanticSearch, RerankCandidateK: 200,
	})
	assert.True(t, within.Allow)

	over := e.PreRead(context.Background(), HookCtx{
		Capability: cap("acme", ""), Block: "semantic", Op: auth.OpSemanticSearch, RerankCandidateK: 500,
	})
	assert.False(t, over.Allow)
	assert.True(t, over.Exhausted)
	assert.Contains(t, over.Reason, "rerank_candidate_k 500 exceeds cap 200")
}

func TestCap_RequiresBound(t *testing.T) {
	_, err := NewRuleEngine(Spec{
		Rules: []Rule{{Name: "empty-cap", Effect: EffectCap}},
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "at least one max")
}
