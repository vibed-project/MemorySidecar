package auth

import (
	"context"
	"fmt"

	"memsidecar/internal/config"
)

// TokenVerifier decodes and validates a bearer token, returning the capability.
type TokenVerifier interface {
	Verify(ctx context.Context, token string) (*Capability, error)
}

// NewVerifier builds the verifier configured in cfg.
func NewVerifier(cfg config.AuthConfig) (TokenVerifier, error) {
	switch cfg.Verifier {
	case "paseto":
		return NewPASETOVerifier(cfg.PASETO)
	case "jwt":
		return NewJWTVerifier(cfg.JWT)
	default:
		return nil, fmt.Errorf("unknown verifier %q", cfg.Verifier)
	}
}
