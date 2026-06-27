// Package identity resolves the signed-in user from a request's headers. A reverse proxy forwards
// the OIDC ACCESS token (issued for the Loft API, carrying the required scope) as a Bearer; loftd is
// a resource server and does not blindly trust it: when configured with an issuer + API audience,
// the token is cryptographically validated against the provider (signature, issuer, audience, scope,
// expiry), so loftd accepts only tokens minted for itself. Identity is keyed on the immutable subject
// (the provider's `oid`, falling back to the standard `sub`): email and display name are mutable and
// must never be used as an identity key. Any standard OIDC provider works; for Entra the issuer can be derived from a tenant id.
package identity

import (
	"context"
	"log"
	"net/http"
	"strings"
	"sync"

	"github.com/coreos/go-oidc/v3/oidc"

	"github.com/larsakerlund/loft/internal/config"
)

// User is the signed-in user, exposed to apps via /api/me.
type User struct {
	Email string `json:"email"` // mutable: display/contact only
	Name  string `json:"name"`  // mutable display name
	ID    string `json:"id"`    // stable, immutable subject (oid/sub): the identity key
}

// Resolver turns request headers into a User. Safe for concurrent use.
type Resolver struct {
	cfg      config.Config
	issuer   string
	mu       sync.Mutex
	verifier *oidc.IDTokenVerifier // lazily created on first validated token; retried until it succeeds
}

// NewResolver builds a Resolver from config. The issuer is taken from OIDCIssuer when set, or derived
// from the Entra tenant as a convenience otherwise.
func NewResolver(cfg config.Config) *Resolver {
	return &Resolver{cfg: cfg, issuer: cfg.OIDCIssuerURL()}
}

// UserFromHeaders resolves identity in order of trust:
//  1. a forwarded OIDC access token (Authorization: Bearer), cryptographically VALIDATED (a
//     present-but-invalid token is rejected outright);
//  2. LOFT_DEV_USER (local, auth off, gated by LOFT_DEV).
//
// There is no header/proxy-trust fallback: a deployment authenticates with bearer tokens (the proxy
// forwards the validated access token), so loftd never derives identity from spoofable request
// headers. The bool is false when no identity can be established.
func (r *Resolver) UserFromHeaders(ctx context.Context, h http.Header) (User, bool) {
	if raw, ok := bearer(h.Get("Authorization")); ok {
		return r.validate(ctx, raw)
	}
	// Local-dev escape hatch, gated by LOFT_DEV (Config.Validate refuses DevUser without it, so this
	// hard-coded identity can never run in production). "email|Display Name|id"; defaults to a local
	// user so LOFT_DEV=1 alone is enough.
	if r.cfg.Dev {
		dev := r.cfg.DevUser
		if dev == "" {
			dev = "dev@local|Dev User"
		}
		parts := strings.Split(dev, "|")
		email := parts[0]
		name := email
		if len(parts) > 1 && parts[1] != "" {
			name = parts[1]
		}
		id := email
		if len(parts) > 2 && parts[2] != "" {
			id = parts[2]
		}
		return User{Email: email, Name: name, ID: id}, true
	}
	return User{}, false
}

// validate verifies an OIDC access token issued for the Loft API. Returns false (rejecting the
// request) on any problem, including a missing required scope.
func (r *Resolver) validate(ctx context.Context, raw string) (User, bool) {
	if r.issuer == "" || r.cfg.APIAudience == "" {
		return User{}, false // not configured to validate ⇒ don't trust the token
	}
	verifier, err := r.getVerifier(ctx)
	if err != nil {
		log.Printf("loftd: oidc provider init failed: %v", err)
		return User{}, false
	}
	tok, err := verifier.Verify(ctx, raw)
	if err != nil {
		log.Printf("loftd: access-token verify failed: %v", err)
		return User{}, false
	}
	var c struct {
		Email             string `json:"email"`
		PreferredUsername string `json:"preferred_username"`
		Name              string `json:"name"`
		Oid               string `json:"oid"`
		Sub               string `json:"sub"`
		Scp               string `json:"scp"`   // Entra: space-delimited delegated scopes
		Scope             string `json:"scope"` // standard OIDC: space-delimited scopes
		Azp               string `json:"azp"`   // authorized party (v2): the calling client app id
		Appid             string `json:"appid"` // same, on v1-style tokens
	}
	if err := tok.Claims(&c); err != nil {
		return User{}, false
	}
	// Require the delegated scope, so a token for the API obtained without it is rejected. The scope
	// claim is `scp` on Entra, `scope` on standard OIDC providers; accept either.
	if !hasScope(c.Scp+" "+c.Scope, r.cfg.APIScope) {
		log.Printf("loftd: access token missing required scope %q", r.cfg.APIScope)
		return User{}, false
	}
	// Pin the calling client: audience alone lets any tenant app that obtains the scope call us.
	// When configured, the token's azp/appid must equal the configured authorized client id.
	if r.cfg.AuthorizedClientID != "" && firstNonEmpty(c.Azp, c.Appid) != r.cfg.AuthorizedClientID {
		log.Printf("loftd: access token from unauthorized client party %q", firstNonEmpty(c.Azp, c.Appid))
		return User{}, false
	}
	email := firstNonEmpty(c.Email, c.PreferredUsername)
	if email == "" {
		return User{}, false
	}
	// oid is the immutable tenant-wide id; sub (stable per app) is the fallback.
	return User{Email: email, Name: firstNonEmpty(c.Name, email), ID: firstNonEmpty(c.Oid, c.Sub, email)}, true
}

func hasScope(scp, want string) bool {
	if want == "" {
		return true
	}
	for _, s := range strings.Fields(scp) {
		if s == want {
			return true
		}
	}
	return false
}

func (r *Resolver) getVerifier(ctx context.Context) (*oidc.IDTokenVerifier, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.verifier != nil {
		return r.verifier, nil
	}
	provider, err := oidc.NewProvider(ctx, r.issuer)
	if err != nil {
		return nil, err
	}
	// The token is an access token, not an ID token, but the verifier's checks we rely on apply to
	// both: signature against the issuer's JWKS, issuer, audience (the Loft API app), and expiry.
	// go-oidc does not require a nonce unless asked, so it is fine for access tokens.
	r.verifier = provider.Verifier(&oidc.Config{ClientID: r.cfg.APIAudience})
	return r.verifier, nil
}

func bearer(authz string) (string, bool) {
	const p = "bearer "
	if len(authz) > len(p) && strings.EqualFold(authz[:len(p)], p) {
		return strings.TrimSpace(authz[len(p):]), true
	}
	return "", false
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}
