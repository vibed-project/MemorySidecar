package auth

import (
	"context"
	"encoding/hex"
	"fmt"
	"time"

	"aidanwoods.dev/go-paseto"

	"github.com/vibed-project/mindD/internal/config"
)

const tokenIssuer = "mindd"

type pasetoVerifier struct {
	pubs []paseto.V4AsymmetricPublicKey
}

func NewPASETOVerifier(cfg config.PASETOConfig) (TokenVerifier, error) {
	keys := cfg.AllKeys()
	if len(keys) == 0 {
		return nil, fmt.Errorf("paseto: at least one public key required")
	}
	pubs := make([]paseto.V4AsymmetricPublicKey, 0, len(keys))
	for i, k := range keys {
		pub, err := paseto.NewV4AsymmetricPublicKeyFromHex(k)
		if err != nil {
			return nil, fmt.Errorf("paseto: parse public key %d: %w", i, err)
		}
		pubs = append(pubs, pub)
	}
	return &pasetoVerifier{pubs: pubs}, nil
}

func (v *pasetoVerifier) Verify(_ context.Context, token string) (*Capability, error) {
	parser := paseto.NewParser()
	parser.AddRule(paseto.NotExpired())
	parser.AddRule(paseto.IssuedBy(tokenIssuer))
	var lastErr error
	for _, pub := range v.pubs {
		tok, err := parser.ParseV4Public(pub, token, nil)
		if err == nil {
			return tokenToCapability(tok)
		}
		lastErr = err
	}
	return nil, authErr("paseto: %v", lastErr)
}

func tokenToCapability(tok *paseto.Token) (*Capability, error) {
	var c Capability

	// Standard claims
	if iss, err := tok.GetIssuer(); err == nil {
		c.Issuer = iss
	}
	if sub, err := tok.GetSubject(); err == nil {
		c.Subject = sub
	}
	if jti, err := tok.GetJti(); err == nil {
		c.TokenID = jti
	}
	if iat, err := tok.GetIssuedAt(); err == nil {
		c.IssuedAt = iat
	}
	if exp, err := tok.GetExpiration(); err == nil {
		c.ExpiresAt = exp
	}

	// Custom claims
	var tenant, agent string
	if err := tok.Get("tenant", &tenant); err != nil {
		return nil, authErr("paseto: missing tenant claim: %v", err)
	}
	_ = tok.Get("agent", &agent)

	var ns, ops []string
	if err := tok.Get("namespaces", &ns); err != nil {
		return nil, authErr("paseto: missing namespaces claim: %v", err)
	}
	if err := tok.Get("ops", &ops); err != nil {
		return nil, authErr("paseto: missing ops claim: %v", err)
	}

	c.Scope = Scope{
		Tenant:           tenant,
		Agent:            agent,
		NamespacePattern: ns,
		AllowedOps:       ops,
	}
	return &c, nil
}

// IssuePASETO mints a PASETO v4.public token signed by secretKeyHex carrying
// the given scope. Used by the dev issuer and tests.
func IssuePASETO(secretKeyHex string, scope Scope, ttl time.Duration, tokenID string) (string, error) {
	if secretKeyHex == "" {
		return "", fmt.Errorf("paseto: private_key_hex required")
	}
	sec, err := paseto.NewV4AsymmetricSecretKeyFromHex(secretKeyHex)
	if err != nil {
		return "", fmt.Errorf("paseto: parse secret key: %w", err)
	}
	if scope.Tenant == "" {
		return "", fmt.Errorf("paseto: tenant required")
	}

	now := time.Now().UTC()
	tok := paseto.NewToken()
	tok.SetIssuer(tokenIssuer)
	tok.SetIssuedAt(now)
	tok.SetNotBefore(now)
	tok.SetExpiration(now.Add(ttl))
	if scope.Agent != "" {
		tok.SetSubject(scope.Agent)
	}
	if tokenID != "" {
		tok.SetJti(tokenID)
	}
	if err := tok.Set("tenant", scope.Tenant); err != nil {
		return "", err
	}
	if scope.Agent != "" {
		if err := tok.Set("agent", scope.Agent); err != nil {
			return "", err
		}
	}
	if err := tok.Set("namespaces", scope.NamespacePattern); err != nil {
		return "", err
	}
	if err := tok.Set("ops", scope.AllowedOps); err != nil {
		return "", err
	}
	return tok.V4Sign(sec, nil), nil
}

// GeneratePASETOKeyPair returns a hex-encoded (secret, public) keypair for
// PASETO v4.public. Used only by the dev issuer.
func GeneratePASETOKeyPair() (secretHex, publicHex string, err error) {
	sec := paseto.NewV4AsymmetricSecretKey()
	secBytes := sec.ExportBytes()
	pubBytes := sec.Public().ExportBytes()
	return hex.EncodeToString(secBytes), hex.EncodeToString(pubBytes), nil
}
