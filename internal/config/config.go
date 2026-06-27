// Package config loads loftd's runtime configuration from the environment. Every external
// dependency (Postgres, the uploads store, the AI upstream, the identity provider) is wired here so
// the rest of the code takes plain values, not os.Getenv calls scattered around.
package config

import (
	"errors"
	"os"
	"strconv"
)

// Config is loftd's fully-resolved runtime configuration.
type Config struct {
	Listen string // host:port to bind, e.g. 127.0.0.1:8080

	// Postgres (loft.db). A connection string carries a static password (the default, works anywhere);
	// otherwise host+user with a CredentialProvider supplying the password per connection (an Azure
	// managed-identity token is the one provider today).
	PGConnString string
	PGHost       string
	PGDatabase   string
	PGUser       string

	UAMIClientID string // Azure user-assigned managed identity client id (optional), shared by db/uploads/ai

	// Uploads (loft.upload). A local directory (default), or Azure Blob via account (managed identity)
	// or a connection string.
	UploadsAccount    string
	UploadsContainer  string
	UploadsConnString string
	UploadsDir        string

	// Sites (deploy). Where loftd writes a deployed site's files; the ingress serves them. Defaults to
	// a conventional mount path so a shared filesystem (local volume, NFS, EFS, Azure Files) needs no
	// extra wiring.
	SitesDir string

	// AI (loft.ai). Two modes off one endpoint: set AIDeployment for Azure OpenAI / AI Foundry, or
	// AIModel for any OpenAI-compatible endpoint (OpenAI, Groq, vLLM, Ollama, LiteLLM). With neither
	// set, the feature reports "not configured".
	AIEndpoint        string
	AIDeployment      string // Azure deployment name (Azure mode); in the URL path
	AIModel           string // model name (OpenAI-compatible mode); in the request body
	AIKey             string // api-key; empty in Azure mode ⇒ authenticate with the managed identity
	AIAPIVersion      string
	AIReasoningEffort string // sent only when set; for reasoning models that accept it
	AIMaxTokens       int

	// Identity. With an issuer + API audience, the forwarded OIDC ACCESS token (issued for the Loft
	// API, carrying the required scope) is cryptographically validated: signature, issuer, audience,
	// scope, expiry. loftd is a resource server, so it trusts only tokens minted for itself. Any
	// standard OIDC provider works: set OIDCIssuer to its issuer URL, or, as a convenience for Entra,
	// set TenantID and the issuer is derived.
	OIDCIssuer         string // OIDC issuer URL; empty ⇒ derived from TenantID (Entra) if that is set
	TenantID           string // Entra tenant id, a convenience to derive the issuer; ignored if OIDCIssuer is set
	APIAudience        string // the Loft API id (access-token aud); empty ⇒ token validation off
	APIScope           string // required delegated scope on the access token (default access_as_user)
	AuthorizedClientID string // if set, the access token's azp/appid must equal this (the auth proxy's client)
	Dev                bool   // LOCAL DEV ONLY: auth off, identity from DevUser. Refused unless opted in.
	DevUser            string // "email|name|id" used in dev mode; defaults to a local user

	// CLI discovery. Served unauthenticated at /.well-known/loft so `loft login <url>` can configure
	// itself from the platform URL alone. All public values (a public client id is not a secret).
	CLIClientID string // the CLI's public OAuth client id (device flow); empty ⇒ discovery off
	CLIScope    string // the scope `loft login` requests (default "openid offline_access")
}

// Load reads the environment and applies defaults.
func Load() Config {
	return Config{
		Listen:             env("LOFT_LISTEN", "127.0.0.1:8080"),
		PGConnString:       os.Getenv("LOFT_PG_CONNECTION_STRING"),
		PGHost:             os.Getenv("LOFT_PG_HOST"),
		PGDatabase:         env("LOFT_PG_DATABASE", "loft"),
		PGUser:             os.Getenv("LOFT_PG_USER"),
		UAMIClientID:       os.Getenv("LOFT_UAMI_CLIENT_ID"),
		UploadsAccount:     os.Getenv("LOFT_UPLOADS_ACCOUNT"),
		UploadsContainer:   env("LOFT_UPLOADS_CONTAINER", "uploads"),
		UploadsConnString:  os.Getenv("LOFT_UPLOADS_CONNECTION_STRING"),
		UploadsDir:         os.Getenv("LOFT_UPLOADS_DIR"),
		SitesDir:           env("LOFT_SITES_DIR", "/mnt/loft"),
		AIEndpoint:         os.Getenv("LOFT_AI_ENDPOINT"),
		AIDeployment:       os.Getenv("LOFT_AI_DEPLOYMENT"),
		AIModel:            os.Getenv("LOFT_AI_MODEL"),
		AIKey:              os.Getenv("LOFT_AI_KEY"),
		AIAPIVersion:       env("LOFT_AI_API_VERSION", "2025-04-01-preview"),
		AIReasoningEffort:  os.Getenv("LOFT_AI_REASONING_EFFORT"),
		AIMaxTokens:        envInt("LOFT_AI_MAX_TOKENS", 4000),
		OIDCIssuer:         os.Getenv("LOFT_OIDC_ISSUER"),
		TenantID:           os.Getenv("LOFT_TENANT_ID"),
		APIAudience:        os.Getenv("LOFT_API_AUDIENCE"),
		APIScope:           env("LOFT_API_SCOPE", "access_as_user"),
		AuthorizedClientID: os.Getenv("LOFT_AUTHORIZED_CLIENT_ID"),
		Dev:                envBool("LOFT_DEV"),
		DevUser:            os.Getenv("LOFT_DEV_USER"),
		CLIClientID:        os.Getenv("LOFT_CLI_CLIENT_ID"),
		CLIScope:           env("LOFT_CLI_SCOPE", "openid offline_access"),
	}
}

// AIModelName is the model loftd sends upstream: the Azure deployment when in Azure mode, otherwise
// the OpenAI-compatible model name. Empty means AI is unconfigured.
func (c Config) AIModelName() string {
	if c.AIDeployment != "" {
		return c.AIDeployment
	}
	return c.AIModel
}

// OIDCIssuerURL is the issuer loftd validates against and advertises to the CLI: OIDCIssuer when set,
// or the Entra issuer derived from TenantID, or empty when neither is configured.
func (c Config) OIDCIssuerURL() string {
	if c.OIDCIssuer != "" {
		return c.OIDCIssuer
	}
	if c.TenantID != "" {
		return "https://login.microsoftonline.com/" + c.TenantID + "/v2.0"
	}
	return ""
}

// Validate rejects dangerous misconfigurations at startup. In particular, access-token validation
// needs BOTH an issuer (LOFT_OIDC_ISSUER, or LOFT_TENANT_ID for Entra) and the API audience; having
// only one would silently half-configure auth. loftd fails closed at runtime, but we'd rather not
// boot at all.
func (c Config) Validate() error {
	issuerSet := c.OIDCIssuer != "" || c.TenantID != ""
	if issuerSet != (c.APIAudience != "") {
		return errors.New("an OIDC issuer (LOFT_OIDC_ISSUER or LOFT_TENANT_ID) and LOFT_API_AUDIENCE must both be set or both empty")
	}
	// The dev user is an unauthenticated, hard-coded identity. It is for the local loop only and must
	// never be reachable in production, so honoring LOFT_DEV_USER requires an explicit LOFT_DEV=1.
	if c.DevUser != "" && !c.Dev {
		return errors.New("LOFT_DEV_USER is a local-dev escape hatch; set LOFT_DEV=1 to enable it (never in production)")
	}
	if c.Dev && issuerSet {
		return errors.New("LOFT_DEV (auth off) cannot be combined with OIDC validation; configure one or the other")
	}
	return nil
}

func env(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func envInt(key string, def int) int {
	if v, err := strconv.Atoi(os.Getenv(key)); err == nil && v > 0 {
		return v
	}
	return def
}

func envBool(key string) bool {
	switch os.Getenv(key) {
	case "1", "true", "yes":
		return true
	}
	return false
}
