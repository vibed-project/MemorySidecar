package auth

import (
	"context"
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/golang-jwt/jwt/v5"

	"github.com/vibed-project/mindD/internal/config"
)

type jwtVerifier struct {
	alg    string
	secret []byte
	pubs   []*rsa.PublicKey
}

func NewJWTVerifier(cfg config.JWTConfig) (TokenVerifier, error) {
	v := &jwtVerifier{alg: cfg.Alg}
	switch cfg.Alg {
	case "HS256":
		if cfg.SecretEnv == "" {
			return nil, fmt.Errorf("jwt: secret_env required for HS256")
		}
		s := os.Getenv(cfg.SecretEnv)
		if s == "" {
			return nil, fmt.Errorf("jwt: env var %q is empty", cfg.SecretEnv)
		}
		v.secret = []byte(s)
	case "RS256":
		pems := cfg.AllPublicPEMs()
		if len(pems) == 0 {
			return nil, fmt.Errorf("jwt: public_pem or public_pems required for RS256")
		}
		for i, spec := range pems {
			pubBytes, err := loadPEM(spec)
			if err != nil {
				return nil, fmt.Errorf("jwt: load public_pem[%d]: %w", i, err)
			}
			block, _ := pem.Decode(pubBytes)
			if block == nil {
				return nil, fmt.Errorf("jwt: public_pem[%d] not PEM-encoded", i)
			}
			pubAny, err := x509.ParsePKIXPublicKey(block.Bytes)
			if err != nil {
				return nil, fmt.Errorf("jwt: parse public key %d: %w", i, err)
			}
			rsaPub, ok := pubAny.(*rsa.PublicKey)
			if !ok {
				return nil, fmt.Errorf("jwt: public_pem[%d] is not RSA", i)
			}
			v.pubs = append(v.pubs, rsaPub)
		}
	default:
		return nil, fmt.Errorf("jwt: unsupported alg %q (HS256|RS256)", cfg.Alg)
	}
	return v, nil
}

func loadPEM(spec string) ([]byte, error) {
	if strings.HasPrefix(spec, "-----BEGIN") {
		return []byte(spec), nil
	}
	return os.ReadFile(spec) //nolint:gosec // spec is an operator-configured key-file path
}

type jwtClaims struct {
	Tenant     string   `json:"tenant"`
	Agent      string   `json:"agent,omitempty"`
	Namespaces []string `json:"namespaces"`
	Ops        []string `json:"ops"`
	jwt.RegisteredClaims
}

func (v *jwtVerifier) Verify(_ context.Context, token string) (*Capability, error) {
	parser := jwt.NewParser(
		jwt.WithValidMethods([]string{v.alg}),
		jwt.WithIssuer(tokenIssuer),
		jwt.WithExpirationRequired(),
	)

	var (
		tok     *jwt.Token
		err     error
		claims  = &jwtClaims{}
	)
	switch v.alg {
	case "HS256":
		tok, err = parser.ParseWithClaims(token, claims, func(_ *jwt.Token) (any, error) {
			return v.secret, nil
		})
	case "RS256":
		// Try each rotation key until one verifies.
		var lastErr error
		for _, pub := range v.pubs {
			c := &jwtClaims{}
			t, perr := parser.ParseWithClaims(token, c, func(_ *jwt.Token) (any, error) {
				return pub, nil
			})
			if perr == nil {
				tok, claims = t, c
				err = nil
				break
			}
			lastErr = perr
		}
		if tok == nil {
			err = lastErr
		}
	}
	if err != nil {
		return nil, authErr("jwt: %v", err)
	}
	if !tok.Valid {
		return nil, authErr("jwt: token invalid")
	}
	if claims.Tenant == "" {
		return nil, authErr("jwt: tenant claim missing")
	}

	c := &Capability{
		Subject: claims.Subject,
		Issuer:  claims.Issuer,
		TokenID: claims.ID,
		Scope: Scope{
			Tenant:           claims.Tenant,
			Agent:            claims.Agent,
			NamespacePattern: claims.Namespaces,
			AllowedOps:       claims.Ops,
		},
	}
	if claims.IssuedAt != nil {
		c.IssuedAt = claims.IssuedAt.Time
	}
	if claims.ExpiresAt != nil {
		c.ExpiresAt = claims.ExpiresAt.Time
	}
	return c, nil
}

// IssueJWT mints an HS256 JWT carrying the given scope. Dev-mode only.
func IssueJWT(secret []byte, scope Scope, ttl time.Duration, tokenID string) (string, error) {
	if scope.Tenant == "" {
		return "", fmt.Errorf("jwt: tenant required")
	}
	now := time.Now().UTC()
	claims := &jwtClaims{
		Tenant:     scope.Tenant,
		Agent:      scope.Agent,
		Namespaces: scope.NamespacePattern,
		Ops:        scope.AllowedOps,
		RegisteredClaims: jwt.RegisteredClaims{
			Issuer:    tokenIssuer,
			Subject:   scope.Agent,
			ID:        tokenID,
			IssuedAt:  jwt.NewNumericDate(now),
			NotBefore: jwt.NewNumericDate(now),
			ExpiresAt: jwt.NewNumericDate(now.Add(ttl)),
		},
	}
	t := jwt.NewWithClaims(jwt.SigningMethodHS256, claims)
	return t.SignedString(secret)
}
