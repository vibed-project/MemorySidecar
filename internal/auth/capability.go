package auth

import (
	"context"
	"fmt"
	"strings"
	"time"
)

// Op identifies an operation on a building block. Form: "<block>.<verb>",
// e.g. "kv.get", "kv.put". Verb-only forms ("get", "put", ...) on a token's
// allowed_ops are interpreted as matching any block.
type Op string

const (
	OpKVGet    Op = "kv.get"
	OpKVPut    Op = "kv.put"
	OpKVDelete Op = "kv.delete"
	OpKVScan   Op = "kv.scan"

	OpEpisodicAppend Op = "episodic.append"
	OpEpisodicRange  Op = "episodic.range"
	OpEpisodicTail   Op = "episodic.tail"
	OpEpisodicExpire Op = "episodic.expire"

	OpSemanticUpsert Op = "semantic.upsert"
	OpSemanticSearch Op = "semantic.search"
	OpSemanticDelete Op = "semantic.delete"
	OpSemanticExpire Op = "semantic.expire"

	OpArtifactPut    Op = "artifact.put"
	OpArtifactGet    Op = "artifact.get"
	OpArtifactStat   Op = "artifact.stat"
	OpArtifactDelete Op = "artifact.delete"
	OpArtifactList   Op = "artifact.list"

	OpLeaseAcquire Op = "lease.acquire"
	OpLeaseRenew   Op = "lease.renew"
	OpLeaseRelease Op = "lease.release"
	OpLeaseInspect Op = "lease.inspect"
	OpLeaseList    Op = "lease.list"

	OpGraphUpsert Op = "graph.upsert"
	OpGraphGet    Op = "graph.get"
	OpGraphQuery  Op = "graph.query" // neighbors + traverse
	OpGraphDelete Op = "graph.delete"

	// OpAdminInspect gates the cross-namespace Admin introspection RPCs. It is
	// not tied to a namespace, so a token grants it via AllowedOps only.
	OpAdminInspect Op = "admin.inspect"
)

// Scope describes what a capability is permitted to do.
type Scope struct {
	Tenant           string   // required
	Agent            string   // optional informational id
	NamespacePattern []string // e.g. ["kv/scratchpad", "kv/tool-*"]
	AllowedOps       []string // e.g. ["kv.get", "put"] — "*" allows all
}

// Capability is the decoded, validated form of a token.
type Capability struct {
	Scope     Scope
	IssuedAt  time.Time
	ExpiresAt time.Time
	Subject   string // typically the agent id
	Issuer    string
	TokenID   string
}

// PermitsNamespace reports whether the capability's namespace patterns cover ns.
// Patterns may use a single trailing "*" for prefix matching, e.g. "kv/tool-*".
// "kv/*" matches every kv namespace.
func (c *Capability) PermitsNamespace(block, name string) bool {
	target := block + "/" + name
	for _, pat := range c.Scope.NamespacePattern {
		if matchNamespace(pat, target) {
			return true
		}
	}
	return false
}

func matchNamespace(pattern, target string) bool {
	if pattern == "*" || pattern == target {
		return true
	}
	if strings.HasSuffix(pattern, "*") {
		prefix := strings.TrimSuffix(pattern, "*")
		return strings.HasPrefix(target, prefix)
	}
	return false
}

// PermitsOp reports whether the capability is allowed to perform op.
// Tokens may list a fully-qualified op ("kv.put") or a verb-only form
// ("put") that matches any block.
func (c *Capability) PermitsOp(op Op) bool {
	full := string(op)
	verb := full
	if i := strings.IndexByte(full, '.'); i >= 0 {
		verb = full[i+1:]
	}
	for _, a := range c.Scope.AllowedOps {
		if a == "*" || a == full || a == verb {
			return true
		}
	}
	return false
}

func (c *Capability) Expired(now time.Time) bool {
	return !c.ExpiresAt.IsZero() && !now.Before(c.ExpiresAt)
}

// tenantSep separates the tenant from the namespace in a qualified storage
// namespace. ASCII unit separator (0x1f): a control char that can't appear in a
// normal tenant or namespace identifier, so qualification is unambiguous.
const tenantSep = "\x1f"

// QualifyNamespace scopes a namespace to a tenant for storage isolation. With
// isolate=false it returns the namespace unchanged — single-tenant behavior,
// byte-for-byte identical to how storage worked before isolation, so existing
// data is unaffected. With isolate=true it prefixes the tenant, so two tenants
// sharing a namespace name address physically separate storage. The isolation
// is enforced here, in one place, rather than in each driver query.
func QualifyNamespace(tenant, namespace string, isolate bool) string {
	if !isolate {
		return namespace
	}
	return tenant + tenantSep + namespace
}

// StorageTenant is the tenant a request's data is stored under. It's the
// capability tenant when isolation is enabled, else "" — so with isolation off
// every request shares the empty-tenant partition (single-tenant behavior).
// Blocks that carry a tenant column (semantic) use this; blocks that qualify
// the namespace string use QualifyNamespace.
func StorageTenant(tenant string, isolate bool) string {
	if !isolate {
		return ""
	}
	return tenant
}

// ErrAuth is the sentinel returned for auth failures.
type ErrAuth struct{ Reason string }

func (e *ErrAuth) Error() string { return "auth: " + e.Reason }

func authErr(format string, a ...any) error {
	return &ErrAuth{Reason: fmt.Sprintf(format, a...)}
}

type ctxKey struct{}

func WithCapability(ctx context.Context, c *Capability) context.Context {
	return context.WithValue(ctx, ctxKey{}, c)
}

func FromContext(ctx context.Context) (*Capability, bool) {
	c, ok := ctx.Value(ctxKey{}).(*Capability)
	return c, ok
}
