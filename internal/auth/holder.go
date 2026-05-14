package auth

import (
	"context"
	"errors"
	"sync/atomic"
)

// VerifierHolder wraps a TokenVerifier in an atomic pointer so the active
// verifier can be swapped at runtime (SIGHUP reload, key rotation, etc.)
// without coordinating with in-flight requests.
//
// VerifierHolder itself satisfies TokenVerifier.
type VerifierHolder struct {
	ptr atomic.Pointer[verifierBox]
}

type verifierBox struct{ v TokenVerifier }

// NewVerifierHolder builds a Holder seeded with v.
func NewVerifierHolder(v TokenVerifier) *VerifierHolder {
	h := &VerifierHolder{}
	h.ptr.Store(&verifierBox{v: v})
	return h
}

// Verifier returns the currently-active verifier.
func (h *VerifierHolder) Verifier() TokenVerifier {
	box := h.ptr.Load()
	if box == nil {
		return nil
	}
	return box.v
}

// Swap atomically replaces the active verifier.
func (h *VerifierHolder) Swap(v TokenVerifier) {
	h.ptr.Store(&verifierBox{v: v})
}

// Verify delegates to the currently-active verifier.
func (h *VerifierHolder) Verify(ctx context.Context, token string) (*Capability, error) {
	v := h.Verifier()
	if v == nil {
		return nil, errors.New("auth: no verifier configured")
	}
	return v.Verify(ctx, token)
}
