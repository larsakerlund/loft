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

// credentials is the saved result of `loft login`: enough to deploy without re-entering anything.
type credentials struct {
	URL         string `json:"url"`
	Issuer      string `json:"issuer"`
	ClientID    string `json:"clientId"`
	Scope       string `json:"scope"`
	AccessToken string `json:"accessToken"`
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
	data, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}
	if err := os.WriteFile(p, data, 0o600); err != nil {
		return err
	}
	return os.Chmod(p, 0o600)
}

// resolveToken finds the bearer for a deploy: LOFT_TOKEN wins (CI), otherwise the saved login. An
// empty token is fine against an auth-off deployment (local dev); loftd answers 401 if it needs one.
func resolveToken(base string) string {
	if t := os.Getenv("LOFT_TOKEN"); t != "" {
		return t
	}
	if c, err := loadCredentials(); err == nil && (base == "" || c.URL == "" || c.URL == strings.TrimRight(base, "/")) {
		return c.AccessToken
	}
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
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String()+"/.well-known/loft", http.NoBody)
	if err != nil {
		return c, err
	}
	resp, err := ctlClient.Do(req)
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
	AccessToken string `json:"access_token"`
	Error       string `json:"error"`
}

// deviceLogin runs the full flow and returns the access token. It prints the verification URL and
// code, then polls until the user approves (or the code expires).
func deviceLogin(ctx context.Context, issuer, clientID, scope string) (string, error) {
	if err := requireSecure(issuer); err != nil {
		return "", err
	}
	meta, err := discover(ctx, issuer)
	if err != nil {
		return "", err
	}
	if meta.DeviceAuthorizationEndpoint == "" {
		return "", fmt.Errorf("provider %s does not advertise a device authorization endpoint", issuer)
	}
	if err := requireSecure(meta.DeviceAuthorizationEndpoint); err != nil {
		return "", err
	}
	if err := requireSecure(meta.TokenEndpoint); err != nil {
		return "", err
	}
	da, err := startDeviceAuth(ctx, meta.DeviceAuthorizationEndpoint, clientID, scope)
	if err != nil {
		return "", err
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
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, wellKnown, http.NoBody)
	if err != nil {
		return m, err
	}
	resp, err := ctlClient.Do(req)
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

func pollToken(ctx context.Context, endpoint, clientID string, da deviceAuth) (string, error) {
	form := url.Values{
		"grant_type":  {"urn:ietf:params:oauth:grant-type:device_code"},
		"device_code": {da.DeviceCode},
		"client_id":   {clientID},
	}
	interval := time.Duration(da.Interval) * time.Second
	for {
		select {
		case <-ctx.Done():
			return "", ctx.Err()
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
			return "", err
		}
		switch {
		case tok.AccessToken != "":
			return tok.AccessToken, nil
		case tok.Error == "authorization_pending":
			// keep polling
		case tok.Error == "slow_down":
			interval += 5 * time.Second
		case tok.Error == "":
			return "", errors.New("device login failed: empty token response")
		default:
			return "", fmt.Errorf("device login failed: %s", tok.Error)
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
