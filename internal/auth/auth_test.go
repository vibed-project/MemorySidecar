package auth

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/vibed-project/mindD/internal/config"
)

func TestCapability_PermitsNamespace(t *testing.T) {
	c := &Capability{Scope: Scope{NamespacePattern: []string{"kv/scratchpad", "kv/tool-*"}}}
	assert.True(t, c.PermitsNamespace("kv", "scratchpad"))
	assert.True(t, c.PermitsNamespace("kv", "tool-cache"))
	assert.True(t, c.PermitsNamespace("kv", "tool-x"))
	assert.False(t, c.PermitsNamespace("kv", "secret"))
	assert.False(t, c.PermitsNamespace("episodic", "scratchpad"))

	wild := &Capability{Scope: Scope{NamespacePattern: []string{"*"}}}
	assert.True(t, wild.PermitsNamespace("kv", "anything"))
}

func TestCapability_PermitsOp(t *testing.T) {
	c := &Capability{Scope: Scope{AllowedOps: []string{"kv.get", "put"}}}
	assert.True(t, c.PermitsOp(OpKVGet))
	assert.True(t, c.PermitsOp(OpKVPut)) // matched by verb-only "put"
	assert.False(t, c.PermitsOp(OpKVDelete))

	wild := &Capability{Scope: Scope{AllowedOps: []string{"*"}}}
	assert.True(t, wild.PermitsOp(OpKVGet))
	assert.True(t, wild.PermitsOp(OpKVDelete))
}

func TestPASETO_RoundTrip(t *testing.T) {
	sec, pub, err := GeneratePASETOKeyPair()
	require.NoError(t, err)

	v, err := NewPASETOVerifier(config.PASETOConfig{PublicKeyHex: pub})
	require.NoError(t, err)

	scope := Scope{
		Tenant:           "acme",
		Agent:            "agent-1",
		NamespacePattern: []string{"kv/scratchpad"},
		AllowedOps:       []string{"kv.get", "kv.put"},
	}
	tok, err := IssuePASETO(sec, scope, 5*time.Minute, "tid-1")
	require.NoError(t, err)

	cap, err := v.Verify(context.Background(), tok)
	require.NoError(t, err)
	assert.Equal(t, "acme", cap.Scope.Tenant)
	assert.Equal(t, []string{"kv/scratchpad"}, cap.Scope.NamespacePattern)
	assert.Equal(t, []string{"kv.get", "kv.put"}, cap.Scope.AllowedOps)
	assert.Equal(t, "agent-1", cap.Subject)
}

func TestPASETO_Expired(t *testing.T) {
	sec, pub, err := GeneratePASETOKeyPair()
	require.NoError(t, err)
	v, _ := NewPASETOVerifier(config.PASETOConfig{PublicKeyHex: pub})

	tok, err := IssuePASETO(sec,
		Scope{Tenant: "acme", NamespacePattern: []string{"kv/*"}, AllowedOps: []string{"*"}},
		-time.Minute, "")
	require.NoError(t, err)

	_, err = v.Verify(context.Background(), tok)
	require.Error(t, err)
}

func TestPASETO_MultiKey_AcceptsEither(t *testing.T) {
	secOld, pubOld, err := GeneratePASETOKeyPair()
	require.NoError(t, err)
	secNew, pubNew, err := GeneratePASETOKeyPair()
	require.NoError(t, err)

	v, err := NewPASETOVerifier(config.PASETOConfig{
		PublicKeyHexes: []string{pubNew, pubOld}, // new preferred, old still trusted
	})
	require.NoError(t, err)

	tokOld, err := IssuePASETO(secOld, Scope{
		Tenant: "acme", NamespacePattern: []string{"kv/*"}, AllowedOps: []string{"*"},
	}, time.Minute, "")
	require.NoError(t, err)
	tokNew, err := IssuePASETO(secNew, Scope{
		Tenant: "acme", NamespacePattern: []string{"kv/*"}, AllowedOps: []string{"*"},
	}, time.Minute, "")
	require.NoError(t, err)

	got, err := v.Verify(context.Background(), tokOld)
	require.NoError(t, err)
	assert.Equal(t, "acme", got.Scope.Tenant)

	got, err = v.Verify(context.Background(), tokNew)
	require.NoError(t, err)
	assert.Equal(t, "acme", got.Scope.Tenant)
}

func TestPASETO_MultiKey_RejectsUntrusted(t *testing.T) {
	_, pub1, _ := GeneratePASETOKeyPair()
	_, pub2, _ := GeneratePASETOKeyPair()
	secRogue, _, _ := GeneratePASETOKeyPair()
	v, _ := NewPASETOVerifier(config.PASETOConfig{PublicKeyHexes: []string{pub1, pub2}})

	tok, err := IssuePASETO(secRogue, Scope{
		Tenant: "acme", NamespacePattern: []string{"kv/*"}, AllowedOps: []string{"*"},
	}, time.Minute, "")
	require.NoError(t, err)

	_, err = v.Verify(context.Background(), tok)
	require.Error(t, err)
}

func TestPASETO_AllKeys_MergesSingularAndList(t *testing.T) {
	cfg := config.PASETOConfig{
		PublicKeyHex:   "aaa",
		PublicKeyHexes: []string{"bbb", "aaa"}, // dup of singular should not double up
	}
	keys := cfg.AllKeys()
	assert.Equal(t, []string{"bbb", "aaa"}, keys)
}

func TestPASETO_WrongSigner(t *testing.T) {
	_, pub, _ := GeneratePASETOKeyPair()
	otherSec, _, _ := GeneratePASETOKeyPair()
	v, _ := NewPASETOVerifier(config.PASETOConfig{PublicKeyHex: pub})

	tok, err := IssuePASETO(otherSec,
		Scope{Tenant: "acme", NamespacePattern: []string{"kv/*"}, AllowedOps: []string{"*"}},
		time.Minute, "")
	require.NoError(t, err)

	_, err = v.Verify(context.Background(), tok)
	require.Error(t, err)
}

func TestJWT_HS256_RoundTrip(t *testing.T) {
	secret := []byte("test-secret-not-for-production-use-32bytes")
	t.Setenv("JWT_SECRET_FOR_TESTS", string(secret))

	v, err := NewJWTVerifier(config.JWTConfig{Alg: "HS256", SecretEnv: "JWT_SECRET_FOR_TESTS"})
	require.NoError(t, err)

	tok, err := IssueJWT(secret, Scope{
		Tenant:           "acme",
		Agent:            "a1",
		NamespacePattern: []string{"kv/scratchpad"},
		AllowedOps:       []string{"kv.put"},
	}, time.Minute, "tid-1")
	require.NoError(t, err)

	cap, err := v.Verify(context.Background(), tok)
	require.NoError(t, err)
	assert.Equal(t, "acme", cap.Scope.Tenant)
	assert.Equal(t, []string{"kv/scratchpad"}, cap.Scope.NamespacePattern)
}

func TestContext_WithCapability(t *testing.T) {
	c := &Capability{Scope: Scope{Tenant: "acme"}}
	ctx := WithCapability(context.Background(), c)
	got, ok := FromContext(ctx)
	require.True(t, ok)
	assert.Same(t, c, got)
}
