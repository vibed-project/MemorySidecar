package policy

import (
	"context"

	"memsidecar/internal/auth"
)

// HookCtx carries the contextual data a policy evaluator needs.
type HookCtx struct {
	Capability *auth.Capability
	Block      string // "kv"
	Namespace  string
	Op         auth.Op
	Key        string // for kv ops; empty for scans

	// Request magnitude, used by cap rules (O4 / ADR-0002 §8). Zero means the
	// dimension is not applicable to this request.
	TopK             uint32 // semantic Search result count
	Limit            uint32 // scan/range page size
	Depth            uint32 // graph traversal depth
	FanOut           uint32 // graph traversal fan-out
	RerankCandidateK uint32 // semantic hybrid per-lane candidate depth (Q4)
}

// Decision is the result of a policy evaluation.
type Decision struct {
	Allow  bool
	Reason string
	// Exhausted marks a rejection as a resource/cost limit (a rate limit or a
	// cost cap) rather than a permission failure, so the transport can map it
	// to ResourceExhausted instead of PermissionDenied.
	Exhausted bool
}

// Engine evaluates policy hooks. v0 ships only the NoopEngine that allows
// everything. Future engines (YAML rules, Rego) implement this interface.
type Engine interface {
	PreRead(ctx context.Context, h HookCtx) Decision
	PreWrite(ctx context.Context, h HookCtx) Decision
}

// NoopEngine allows every operation.
type NoopEngine struct{}

func (NoopEngine) PreRead(_ context.Context, _ HookCtx) Decision  { return Decision{Allow: true} }
func (NoopEngine) PreWrite(_ context.Context, _ HookCtx) Decision { return Decision{Allow: true} }
