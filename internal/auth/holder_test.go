package auth

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type fakeVerifier struct {
	cap *Capability
	err error
}

func (f *fakeVerifier) Verify(_ context.Context, _ string) (*Capability, error) {
	if f.err != nil {
		return nil, f.err
	}
	return f.cap, nil
}

func TestVerifierHolder_InitialAndSwap(t *testing.T) {
	h := NewVerifierHolder(&fakeVerifier{cap: &Capability{Scope: Scope{Tenant: "acme"}}})
	got, err := h.Verify(context.Background(), "tok")
	require.NoError(t, err)
	assert.Equal(t, "acme", got.Scope.Tenant)

	h.Swap(&fakeVerifier{err: errors.New("rotated key, token rejected")})
	_, err = h.Verify(context.Background(), "tok")
	require.ErrorContains(t, err, "rotated key")
}

func TestVerifierHolder_NilUnderlying(t *testing.T) {
	h := &VerifierHolder{}
	_, err := h.Verify(context.Background(), "tok")
	require.ErrorContains(t, err, "no verifier configured")
}

func TestVerifierHolder_SwapConcurrent(t *testing.T) {
	h := NewVerifierHolder(&fakeVerifier{cap: &Capability{Scope: Scope{Tenant: "a"}}})
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(2)
		go func() {
			defer wg.Done()
			for j := 0; j < 100; j++ {
				h.Swap(&fakeVerifier{cap: &Capability{}})
			}
		}()
		go func() {
			defer wg.Done()
			for j := 0; j < 100; j++ {
				_, _ = h.Verify(context.Background(), "tok")
			}
		}()
	}
	wg.Wait()
}
