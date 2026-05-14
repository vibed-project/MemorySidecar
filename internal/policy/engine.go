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
}

// Decision is the result of a policy evaluation.
type Decision struct {
	Allow  bool
	Reason string
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
