package policy

import (
	"context"
	"sync/atomic"
)

// Holder wraps an Engine in an atomic pointer so the active policy can be
// swapped at runtime (SIGHUP reload, etc) without coordinating with in-flight
// interceptor calls.
//
// Holder itself satisfies Engine, so callers can pass *Holder anywhere an
// Engine is expected — every PreRead/PreWrite call goes through the current
// pointer load.
type Holder struct {
	ptr atomic.Pointer[engineBox]
}

// engineBox is needed because Go's atomic.Pointer doesn't accept interface
// types directly; we wrap the interface in a concrete struct.
type engineBox struct{ e Engine }

// NewHolder builds a Holder seeded with e.
func NewHolder(e Engine) *Holder {
	h := &Holder{}
	h.ptr.Store(&engineBox{e: e})
	return h
}

// Engine returns the currently-active engine.
func (h *Holder) Engine() Engine {
	box := h.ptr.Load()
	if box == nil {
		return NoopEngine{}
	}
	return box.e
}

// Swap atomically replaces the active engine.
func (h *Holder) Swap(e Engine) {
	h.ptr.Store(&engineBox{e: e})
}

func (h *Holder) PreRead(ctx context.Context, hctx HookCtx) Decision {
	return h.Engine().PreRead(ctx, hctx)
}

func (h *Holder) PreWrite(ctx context.Context, hctx HookCtx) Decision {
	return h.Engine().PreWrite(ctx, hctx)
}
