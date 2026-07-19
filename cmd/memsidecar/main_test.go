package main

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"memsidecar/internal/auth"
	"memsidecar/internal/config"
	"memsidecar/internal/policy"
)

// A cap rule's max.* bounds must survive the whole path: YAML → koanf → config
// mirror → buildPolicyEngine → engine. Regression guard for the plumbing gap
// where PolicyRuleConfig had no `max` field (koanf silently dropped it) and the
// converter never copied it, so every `effect: cap` rule reached the engine
// with an empty Cap and the server refused to boot.
func TestBuildPolicyEngine_CapBoundFromYAML(t *testing.T) {
	yaml := `
server:
  grpc:
    tcp: "127.0.0.1:7777"
auth:
  verifier: paseto
  paseto:
    public_key_hex: "61a2d0067233cf8d6570c76f37573cbc31d33032ab256fe0c8032c0987d0fbf9"
policy:
  default: allow
  rules:
    - name: cap-search-topk
      effect: cap
      reason: "top_k over 200 is not allowed"
      match:
        op: ["semantic.search"]
      max:
        top_k: 200
backends:
  - name: mem
    driver: memory
namespaces:
  - block: kv
    name: scratch
    backend: mem
`
	path := filepath.Join(t.TempDir(), "config.yaml")
	require.NoError(t, os.WriteFile(path, []byte(yaml), 0o600))

	cfg, err := config.Load(path)
	require.NoError(t, err)

	// Parse gap: the max block must reach the config mirror.
	require.Len(t, cfg.Policy.Rules, 1)
	assert.Equal(t, uint32(200), cfg.Policy.Rules[0].Max.TopK)

	// Convert + engine gap: the engine must build (no "requires at least one
	// max.* bound" error) and enforce the bound.
	eng, err := buildPolicyEngine(cfg.Policy)
	require.NoError(t, err)

	c := &auth.Capability{Scope: auth.Scope{Tenant: "acme", Agent: "a1"}}
	search := func(topK uint32) policy.Decision {
		return eng.PreRead(context.Background(), policy.HookCtx{
			Capability: c, Block: "semantic", Op: auth.OpSemanticSearch, TopK: topK,
		})
	}

	over := search(500)
	assert.False(t, over.Allow, "top_k over the cap should be rejected")
	assert.True(t, over.Exhausted, "a cap rejection is a resource limit, not a permission failure")
	// A configured reason takes precedence over the engine's auto message.
	assert.Contains(t, over.Reason, "top_k over 200 is not allowed")

	// Pin the bound exactly at 200: 200 passes ("exceeds" is strict), 201 fails.
	assert.True(t, search(200).Allow, "top_k == cap should pass")
	assert.False(t, search(201).Allow, "top_k just over the cap should be rejected")
}
