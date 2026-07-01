// CLI authentication: the OAuth 2.0 device authorization grant (RFC 8628). `loft login` opens a
// browser-free flow against any OIDC provider, stores the resulting access token, and `loft deploy`
// sends it to loftd as a bearer. No client secret is involved (the CLI is a public client); the token
// lives in the user's config dir at 0600, or comes from LOFT_TOKEN for CI.
package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// ctlClient is the control-plane HTTP client (discovery + token endpoints). Bounded timeout so a
// hung or hostile endpoint cannot stall the CLI forever.
var ctlClient = &http.Client{Timeout: 30 * time.Second}

// credentials is the saved result of `loft login`: enough to deploy without re-entering anything. The
// refresh token renews an expired access token silently, so an expiry does not force a fresh login.
type credentials struct {
	URL          string `json:"url"`
	Issuer       string `json:"issuer"`
	ClientID     string `json:"clientId"`
	Scope        string `json:"scope"`
	AccessToken  string `json:"accessToken"`
	RefreshToken string `json:"refreshToken"`
}

func credentialsPath() (string, error) {
	dir := os.Getenv("XDG_CONFIG_HOME")
	if dir == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		dir = filepath.Join(home, ".config")
	}
	return filepath.Join(dir, "loft", "credentials.json"), nil
}

func loadCredentials() (credentials, error) {
	var c credentials
	p, err := credentialsPath()
	if err != nil {
		return c, err
	}
	data, err := os.ReadFile(p) // #nosec G304 -- fixed path under the user's config dir
	if err != nil {
		return c, err
	}
	if err := json.Unmarshal(data, &c); err != nil {
		return c, err
	}
	return c, nil
}

func saveCredentials(c credentials) error {
	p, err := credentialsPath()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		return err
	}
	data, err := json.MarshalIndent(c, "", "  ") //nolint:gosec // G117: the credentials file holds the OAuth token by design, written 0600 below
	if err != nil {
		return err
	}
	// Write a sibling temp file then rename, so a concurrent reader (a sibling process refreshing under
	// the credentials lock) never observes a half-written file. rename is atomic within a filesystem.
	tmp, err := os.CreateTemp(filepath.Dir(p), ".credentials-*.tmp")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer func() { _ = os.Remove(tmpName) }() // no-op once the rename below succeeds
	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmpName, p)
}

// newSessionClient builds a loftClient for base from the current session, deciding both the bearer
// and refresh eligibility from a single credentials load so that precedence lives in exactly one
// place. LOFT_TOKEN wins (CI) and never refreshes. Otherwise the saved login supplies the bearer, and
// a refresh closure is attached only when the saved record matches this base and carries both a
// refresh token and an issuer. An empty token is fine against an auth-off deployment (local dev);
// loftd answers 401 if it needs one.
func newSessionClient(base string) *loftClient {
	if t := os.Getenv("LOFT_TOKEN"); t != "" {
		return newClient(base, t, nil)
	}
	c, err := loadCredentials()
	if err != nil || (base != "" && c.URL != "" && c.URL != strings.TrimRight(base, "/")) {
		return newClient(base, "", nil) // no saved login, or it is for a different platform
	}
	if c.RefreshToken == "" || c.Issuer == "" || c.ClientID == "" {
		return newClient(base, c.AccessToken, nil)
	}
	return newClient(base, c.AccessToken, refreshFunc())
}

// refreshFunc builds the client's on-401 refresh callback. failingToken is the bearer the request was
// just rejected with; the caller passes the token it actually used, so a client that refreshes more
// than once (deploy then overwrite) compares against the right value each time rather than a stale
// construction-time snapshot. If the stored token already differs from failingToken, a sibling
// process refreshed while this one waited on the lock, so we adopt it without a network call. The
// whole read-refresh-write runs under withCredentialsLock so parallel deploys collapse to one refresh
// instead of racing to spend a rotating refresh token. An empty returned token means re-login is
// required; the caller surfaces the original 401 rather than an OAuth-internal error.
func refreshFunc() func(ctx context.Context, failingToken string) (string, error) {
	return func(ctx context.Context, failingToken string) (string, error) {
		var token string
		err := withCredentialsLock(func() error {
			t, err := refreshLocked(ctx, failingToken)
			token = t
			return err
		})
		return token, err
	}
}

// refreshLocked does the read-refresh-write for one renewal; call it while holding
// withCredentialsLock. It returns the new bearer, or an empty token when re-login is required (a dead
// refresh token, or a record that cannot refresh), so the caller surfaces the original 401 rather
// than an OAuth-internal error. A non-nil error is a transient failure that leaves credentials intact.
func refreshLocked(ctx context.Context, failingToken string) (string, error) {
	c, err := loadCredentials()
	if err != nil {
		return "", err
	}
	// A sibling refreshed while we waited on the lock: adopt its token, skip the network.
	if c.AccessToken != "" && c.AccessToken != failingToken {
		return c.AccessToken, nil
	}
	// The saved record cannot refresh (a sibling cleared it after invalid_grant, or it lacks a refresh
	// token, issuer, or client id): re-login is required.
	if c.RefreshToken == "" || c.Issuer == "" || c.ClientID == "" {
		return "", nil
	}
	refreshed, err := refreshCredentials(ctx, c)
	if err != nil {
		if errors.Is(err, errInvalidGrant) {
			return clearOrAdopt(c), nil
		}
		return "", err
	}
	if err := saveCredentials(refreshed); err != nil {
		return "", err
	}
	return refreshed.AccessToken, nil
}

// clearOrAdopt handles a dead refresh token. It clears the stale secrets, keeping the discovery config
// so a bare `loft login` re-authenticates. First it re-reads: on a platform where the lock is a no-op
// a sibling may have rotated the token since we loaded it, and wiping would destroy that good refresh;
// in that case it adopts the sibling's token instead. An empty return means re-login is required.
func clearOrAdopt(c credentials) string {
	if cur, err := loadCredentials(); err == nil && cur.RefreshToken != c.RefreshToken {
		return cur.AccessToken // the sibling's token, if any; empty stays terminal
	}
	_ = saveCredentials(clearTokens(c))
	return ""
}

// --- platform discovery: configure login from the platform URL alone ---

// cliConfig is the public OAuth config a deployment advertises at /.well-known/loft.
type cliConfig struct {
	Issuer   string `json:"issuer"`
	ClientID string `json:"clientId"`
	Scope    string `json:"scope"`
}

// discoverConfig fetches /.well-known/loft from the platform so `loft login <url>` needs nothing but
// the URL. It is fetched over HTTPS (except for local development) so TLS authenticates that the
// config really came from the named platform, which is what makes trusting the returned issuer safe.
func discoverConfig(ctx context.Context, base string) (cliConfig, error) {
	var c cliConfig
	u, err := url.Parse(strings.TrimRight(base, "/"))
	if err != nil || u.Host == "" {
		return c, fmt.Errorf("invalid platform URL: %q", base)
	}
	if u.Scheme != "https" && !isLocalHost(u.Hostname()) {
		return c, fmt.Errorf("refusing to discover config over plain HTTP from %s (use https)", u.Host)
	}
	//nolint:gosec // G704: the CLI fetches the platform URL the user gives it; a client CLI has no SSRF surface
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String()+"/.well-known/loft", http.NoBody)
	if err != nil {
		return c, err
	}
	resp, err := ctlClient.Do(req) //nolint:gosec // G704: request to the user-provided platform URL, see above
	if err != nil {
		return c, fmt.Errorf("reaching %s: %w", u.Host, err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return c, fmt.Errorf("%s does not advertise CLI config (HTTP %d)", u.Host, resp.StatusCode)
	}
	if err := json.NewDecoder(resp.Body).Decode(&c); err != nil {
		return c, err
	}
	return c, nil
}

// requireSecure rejects a non-HTTPS OAuth endpoint (except for local development), so an access token
// never transits plaintext to a spoofable host.
func requireSecure(rawURL string) error {
	u, err := url.Parse(rawURL)
	if err != nil || u.Host == "" {
		return fmt.Errorf("invalid OAuth endpoint URL: %q", rawURL)
	}
	if u.Scheme != "https" && !isLocalHost(u.Hostname()) {
		return fmt.Errorf("refusing plain-HTTP OAuth endpoint %s (use https)", u.Host)
	}
	return nil
}

// isLocalHost reports whether a host is a loopback / local-dev name, where plain HTTP is acceptable.
func isLocalHost(host string) bool {
	switch host {
	case "localhost", "127.0.0.1", "::1":
		return true
	}
	return strings.HasSuffix(host, ".localhost") ||
		strings.HasSuffix(host, ".localtest.me") ||
		strings.HasSuffix(host, ".local")
}

// --- the device authorization grant ---

type oidcMetadata struct {
	DeviceAuthorizationEndpoint string `json:"device_authorization_endpoint"`
	TokenEndpoint               string `json:"token_endpoint"`
}

type deviceAuth struct {
	DeviceCode              string `json:"device_code"`
	UserCode                string `json:"user_code"`
	VerificationURI         string `json:"verification_uri"`
	VerificationURIComplete string `json:"verification_uri_complete"`
	ExpiresIn               int    `json:"expires_in"`
	Interval                int    `json:"interval"`
}

type tokenResponse struct {
	AccessToken      string `json:"access_token"`
	RefreshToken     string `json:"refresh_token"`
	Error            string `json:"error"`
	ErrorDescription string `json:"error_description"`
}

// errInvalidGrant marks a refresh the provider rejected with the OAuth error invalid_grant: the
// refresh token is expired, revoked, or already spent on a rotating provider. It is terminal, unlike
// a transient transport or 5xx failure, so the caller clears the saved tokens and falls back to
// `loft login` instead of retrying.
var errInvalidGrant = errors.New("refresh token rejected (invalid_grant)")

// refreshGrant runs the RFC 6749 section 6 refresh_token grant and classifies the outcome. It decodes
// the JSON body before inspecting the HTTP status, because a provider returns invalid_grant with HTTP
// 400 and that case must stay terminal rather than being swept into the transient bucket. No client
// secret: loft is a public client. Scope is omitted so the provider reuses the originally granted
// scope, which keeps offline_access alive without re-asserting it and is portable across providers.
func refreshGrant(ctx context.Context, endpoint, clientID, refreshTok string) (tokenResponse, error) {
	form := url.Values{
		"grant_type":    {"refresh_token"},
		"refresh_token": {refreshTok},
		"client_id":     {clientID},
	}
	resp, err := postForm(ctx, endpoint, form)
	if err != nil {
		return tokenResponse{}, err // transient: transport failure, leave credentials untouched
	}
	defer func() { _ = resp.Body.Close() }()
	var tok tokenResponse
	if err := json.NewDecoder(resp.Body).Decode(&tok); err != nil {
		return tokenResponse{}, fmt.Errorf("refresh returned unreadable response (HTTP %d): %w", resp.StatusCode, err)
	}
	switch {
	case tok.Error == "invalid_grant":
		return tok, errInvalidGrant
	case tok.Error != "":
		return tok, fmt.Errorf("refresh failed: %s", oauthError(tok))
	case tok.AccessToken == "":
		// A 2xx-looking body with no access token: treat as transient, so we neither persist an empty
		// token (which would immediately re-401) nor destroy the still-usable refresh token.
		return tokenResponse{}, fmt.Errorf("refresh returned no access token (HTTP %d)", resp.StatusCode)
	}
	return tok, nil
}

// oauthError renders a failed token response, preferring the human-readable description.
func oauthError(tok tokenResponse) string {
	if tok.ErrorDescription != "" {
		return tok.ErrorDescription
	}
	return tok.Error
}

// refreshCredentials trades a record's refresh token for a fresh access token. It re-discovers the
// token endpoint from the saved issuer each time (nothing provider-specific is persisted beyond
// issuer + client id), so the call bypasses oauth2-proxy exactly as the device flow does. Rotation is
// save-new-if-present: the stored refresh token is overwritten only when the provider returns a new
// one, so rotating providers (Entra, Okta) and non-rotating ones (which omit it) both work. It does
// not touch disk; the caller owns persistence and locking.
func refreshCredentials(ctx context.Context, c credentials) (credentials, error) {
	if c.RefreshToken == "" || c.Issuer == "" || c.ClientID == "" {
		return c, errors.New("no refresh token to renew with")
	}
	if err := requireSecure(c.Issuer); err != nil {
		return c, err
	}
	meta, err := discover(ctx, c.Issuer)
	if err != nil {
		return c, err
	}
	if err := requireSecure(meta.TokenEndpoint); err != nil {
		return c, err
	}
	tok, err := refreshGrant(ctx, meta.TokenEndpoint, c.ClientID, c.RefreshToken)
	if err != nil {
		return c, err
	}
	c.AccessToken = tok.AccessToken
	if tok.RefreshToken != "" {
		c.RefreshToken = tok.RefreshToken
	}
	return c, nil
}

// clearTokens blanks the access and refresh tokens but keeps the discovery config (URL, issuer,
// client id, scope), so after an invalid_grant a bare `loft login` still self-configures.
func clearTokens(c credentials) credentials {
	c.AccessToken = ""
	c.RefreshToken = ""
	return c
}

// deviceLogin runs the full flow and returns the token response (access token plus, when the provider
// issues one for the offline_access scope, a refresh token). It prints the verification URL and code,
// then polls until the user approves (or the code expires).
func deviceLogin(ctx context.Context, issuer, clientID, scope string) (tokenResponse, error) {
	if err := requireSecure(issuer); err != nil {
		return tokenResponse{}, err
	}
	meta, err := discover(ctx, issuer)
	if err != nil {
		return tokenResponse{}, err
	}
	if meta.DeviceAuthorizationEndpoint == "" {
		return tokenResponse{}, fmt.Errorf("provider %s does not advertise a device authorization endpoint", issuer)
	}
	if err := requireSecure(meta.DeviceAuthorizationEndpoint); err != nil {
		return tokenResponse{}, err
	}
	if err := requireSecure(meta.TokenEndpoint); err != nil {
		return tokenResponse{}, err
	}
	da, err := startDeviceAuth(ctx, meta.DeviceAuthorizationEndpoint, clientID, scope)
	if err != nil {
		return tokenResponse{}, err
	}
	fmt.Printf("\nTo sign in, visit %s and enter code: %s\n", da.VerificationURI, da.UserCode)
	if da.VerificationURIComplete != "" {
		fmt.Printf("(or open %s)\n", da.VerificationURIComplete)
	}
	fmt.Println("Waiting for approval...")
	return pollToken(ctx, meta.TokenEndpoint, clientID, da)
}

func discover(ctx context.Context, issuer string) (oidcMetadata, error) {
	var m oidcMetadata
	wellKnown := strings.TrimRight(issuer, "/") + "/.well-known/openid-configuration"
	//nolint:gosec // G704: discovery request to the user-provided OIDC issuer; no SSRF surface in a CLI
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, wellKnown, http.NoBody)
	if err != nil {
		return m, err
	}
	resp, err := ctlClient.Do(req) //nolint:gosec // G704: discovery request to the user-provided OIDC issuer, see above
	if err != nil {
		return m, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return m, fmt.Errorf("OIDC discovery at %s returned %d", wellKnown, resp.StatusCode)
	}
	if err := json.NewDecoder(resp.Body).Decode(&m); err != nil {
		return m, err
	}
	return m, nil
}

func startDeviceAuth(ctx context.Context, endpoint, clientID, scope string) (deviceAuth, error) {
	var da deviceAuth
	form := url.Values{"client_id": {clientID}, "scope": {scope}}
	resp, err := postForm(ctx, endpoint, form)
	if err != nil {
		return da, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return da, fmt.Errorf("device authorization returned %d", resp.StatusCode)
	}
	if err := json.NewDecoder(resp.Body).Decode(&da); err != nil {
		return da, err
	}
	if da.Interval <= 0 {
		da.Interval = 5 // RFC 8628 default poll interval
	}
	return da, nil
}

func pollToken(ctx context.Context, endpoint, clientID string, da deviceAuth) (tokenResponse, error) {
	form := url.Values{
		"grant_type":  {"urn:ietf:params:oauth:grant-type:device_code"},
		"device_code": {da.DeviceCode},
		"client_id":   {clientID},
	}
	interval := time.Duration(da.Interval) * time.Second
	for {
		select {
		case <-ctx.Done():
			return tokenResponse{}, ctx.Err()
		case <-time.After(interval):
		}
		tok, err := func() (tokenResponse, error) {
			var t tokenResponse
			resp, err := postForm(ctx, endpoint, form)
			if err != nil {
				return t, err
			}
			defer func() { _ = resp.Body.Close() }()
			if err := json.NewDecoder(resp.Body).Decode(&t); err != nil {
				return t, err
			}
			return t, nil
		}()
		if err != nil {
			return tokenResponse{}, err
		}
		switch {
		case tok.AccessToken != "":
			return tok, nil
		case tok.Error == "authorization_pending":
			// keep polling
		case tok.Error == "slow_down":
			interval += 5 * time.Second
		case tok.Error == "":
			return tokenResponse{}, errors.New("device login failed: empty token response")
		default:
			return tokenResponse{}, fmt.Errorf("device login failed: %s", oauthError(tok))
		}
	}
}

func postForm(ctx context.Context, endpoint string, form url.Values) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	return ctlClient.Do(req)
}
