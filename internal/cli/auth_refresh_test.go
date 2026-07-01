package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
)

// mockIdP serves OIDC discovery plus a /token handler the test controls. The issuer is 127.0.0.1, so
// requireSecure allows plain HTTP (local-dev host) and the refresh path runs without TLS setup.
func mockIdP(t *testing.T, token http.HandlerFunc) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	var srv *httptest.Server
	mux.HandleFunc("/.well-known/openid-configuration", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]string{
			"issuer":                        srv.URL,
			"token_endpoint":                srv.URL + "/token",
			"device_authorization_endpoint": srv.URL + "/device",
		})
	})
	mux.HandleFunc("/token", token)
	srv = httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

// writeToken is a small helper for a JSON token response.
func writeToken(w http.ResponseWriter, status int, body map[string]any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

func TestRefreshGrantClassifies(t *testing.T) {
	tests := []struct {
		name      string
		status    int
		body      map[string]any
		wantTok   string
		wantInval bool // expect errInvalidGrant
		wantErr   bool // expect any error
	}{
		{name: "success", status: 200, body: map[string]any{"access_token": "at", "refresh_token": "rt"}, wantTok: "at"},
		{name: "invalid_grant is terminal", status: 400, body: map[string]any{"error": "invalid_grant"}, wantInval: true, wantErr: true},
		{name: "other oauth error is transient", status: 400, body: map[string]any{"error": "temporarily_unavailable"}, wantErr: true},
		{name: "empty access token is transient", status: 200, body: map[string]any{"token_type": "Bearer"}, wantErr: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			srv := mockIdP(t, func(w http.ResponseWriter, _ *http.Request) { writeToken(w, tc.status, tc.body) })
			tok, err := refreshGrant(context.Background(), srv.URL+"/token", "cid", "rt")
			if tc.wantErr != (err != nil) {
				t.Fatalf("err = %v, wantErr %v", err, tc.wantErr)
			}
			if tc.wantInval != errors.Is(err, errInvalidGrant) {
				t.Fatalf("errors.Is(errInvalidGrant) = %v, want %v (err %v)", errors.Is(err, errInvalidGrant), tc.wantInval, err)
			}
			if tc.wantTok != "" && tok.AccessToken != tc.wantTok {
				t.Fatalf("access token = %q, want %q", tok.AccessToken, tc.wantTok)
			}
		})
	}
}

func TestRefreshGrantTransportErrorIsTransient(t *testing.T) {
	// A closed server produces a transport error, which must not be mistaken for invalid_grant.
	srv := mockIdP(t, func(http.ResponseWriter, *http.Request) {})
	url := srv.URL + "/token"
	srv.Close()
	_, err := refreshGrant(context.Background(), url, "cid", "rt")
	if err == nil || errors.Is(err, errInvalidGrant) {
		t.Fatalf("want a transient (non invalid_grant) error, got %v", err)
	}
}

func TestRefreshCredentialsRotation(t *testing.T) {
	// The provider rotates: it returns a new refresh token each call. Save-new-if-present must adopt it.
	srv := mockIdP(t, func(w http.ResponseWriter, _ *http.Request) {
		writeToken(w, 200, map[string]any{"access_token": "at2", "refresh_token": "rt2"})
	})
	c := credentials{Issuer: srv.URL, ClientID: "cid", AccessToken: "at1", RefreshToken: "rt1"}
	got, err := refreshCredentials(context.Background(), c)
	if err != nil {
		t.Fatal(err)
	}
	if got.AccessToken != "at2" || got.RefreshToken != "rt2" {
		t.Fatalf("got access %q refresh %q, want at2/rt2", got.AccessToken, got.RefreshToken)
	}
}

func TestRefreshCredentialsKeepsRefreshTokenWhenOmitted(t *testing.T) {
	// A non-rotating provider (Google-style) omits refresh_token on refresh; keep the original.
	srv := mockIdP(t, func(w http.ResponseWriter, _ *http.Request) {
		writeToken(w, 200, map[string]any{"access_token": "at2"})
	})
	c := credentials{Issuer: srv.URL, ClientID: "cid", AccessToken: "at1", RefreshToken: "rt1"}
	got, err := refreshCredentials(context.Background(), c)
	if err != nil {
		t.Fatal(err)
	}
	if got.AccessToken != "at2" || got.RefreshToken != "rt1" {
		t.Fatalf("got access %q refresh %q, want at2/rt1", got.AccessToken, got.RefreshToken)
	}
}

func TestDoRefreshesAndReplaysOnce(t *testing.T) {
	var reqs int32
	var sawSecondBearer string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := atomic.AddInt32(&reqs, 1)
		if n == 1 {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		sawSecondBearer = r.Header.Get("Authorization")
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)

	var refreshes int32
	c := newClient(srv.URL, "old", func(context.Context, string) (string, error) {
		atomic.AddInt32(&refreshes, 1)
		return "new", nil
	})
	resp, err := c.do(context.Background(), func() (*http.Request, error) {
		return c.newRequest(context.Background(), http.MethodGet, "/api/me", http.NoBody)
	})
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if got := atomic.LoadInt32(&reqs); got != 2 {
		t.Fatalf("requests = %d, want 2 (original + one replay)", got)
	}
	if got := atomic.LoadInt32(&refreshes); got != 1 {
		t.Fatalf("refreshes = %d, want 1", got)
	}
	if sawSecondBearer != "Bearer new" {
		t.Fatalf("replay bearer = %q, want %q", sawSecondBearer, "Bearer new")
	}
}

func TestDoTerminalRefreshReturnsOriginal401(t *testing.T) {
	// A dead refresh token (the closure returns an empty token, no error) must surface the original
	// 401 (so errorFor prints "run `loft login`") and must not replay.
	var reqs int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		atomic.AddInt32(&reqs, 1)
		w.WriteHeader(http.StatusUnauthorized)
	}))
	t.Cleanup(srv.Close)

	c := newClient(srv.URL, "old", func(context.Context, string) (string, error) {
		return "", nil
	})
	resp, err := c.do(context.Background(), func() (*http.Request, error) {
		return c.newRequest(context.Background(), http.MethodGet, "/api/me", http.NoBody)
	})
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", resp.StatusCode)
	}
	if got := atomic.LoadInt32(&reqs); got != 1 {
		t.Fatalf("requests = %d, want 1 (no replay on invalid_grant)", got)
	}
}

func TestDoTransientRefreshErrorPropagates(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	t.Cleanup(srv.Close)

	wantErr := errors.New("idp unreachable")
	c := newClient(srv.URL, "old", func(context.Context, string) (string, error) { return "", wantErr })
	resp, err := c.do(context.Background(), func() (*http.Request, error) {
		return c.newRequest(context.Background(), http.MethodGet, "/api/me", http.NoBody)
	})
	if resp != nil {
		_ = resp.Body.Close()
	}
	if !errors.Is(err, wantErr) {
		t.Fatalf("err = %v, want %v", err, wantErr)
	}
}

func TestRefreshFuncClearsTokensOnInvalidGrant(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("LOFT_TOKEN", "")
	srv := mockIdP(t, func(w http.ResponseWriter, _ *http.Request) {
		writeToken(w, 400, map[string]any{"error": "invalid_grant"})
	})
	saved := credentials{URL: "https://loft.example", Issuer: srv.URL, ClientID: "cid", Scope: "openid offline_access", AccessToken: "at1", RefreshToken: "rt1"}
	if err := saveCredentials(saved); err != nil {
		t.Fatal(err)
	}
	token, err := refreshFunc()(context.Background(), "at1")
	if err != nil {
		t.Fatalf("err = %v, want nil (a dead refresh token is handled internally)", err)
	}
	if token != "" {
		t.Fatalf("token = %q, want empty on a dead refresh token", token)
	}
	got, err := loadCredentials()
	if err != nil {
		t.Fatal(err)
	}
	if got.AccessToken != "" || got.RefreshToken != "" {
		t.Fatalf("tokens not cleared: access %q refresh %q", got.AccessToken, got.RefreshToken)
	}
	if got.Issuer != srv.URL || got.URL != "https://loft.example" || got.Scope != "openid offline_access" {
		t.Fatalf("discovery config not preserved: %+v", got)
	}
}

func TestRefreshFuncAdoptsSiblingToken(t *testing.T) {
	// If a sibling already refreshed (disk token differs from the client's starting bearer), adopt it
	// with no network call.
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("LOFT_TOKEN", "")
	var hits int32
	srv := mockIdP(t, func(w http.ResponseWriter, _ *http.Request) {
		atomic.AddInt32(&hits, 1)
		writeToken(w, 200, map[string]any{"access_token": "should-not-be-used"})
	})
	if err := saveCredentials(credentials{Issuer: srv.URL, ClientID: "cid", AccessToken: "sibling-token", RefreshToken: "rt1"}); err != nil {
		t.Fatal(err)
	}
	token, err := refreshFunc()(context.Background(), "stale-token")
	if err != nil {
		t.Fatal(err)
	}
	if token != "sibling-token" {
		t.Fatalf("token = %q, want the sibling's token", token)
	}
	if got := atomic.LoadInt32(&hits); got != 0 {
		t.Fatalf("token endpoint hit %d times, want 0 (adopted without a network call)", got)
	}
}

func TestRefreshFuncSecondRefreshInSameSessionHitsNetwork(t *testing.T) {
	// Regression: the closure compares the disk token against the token that just failed, not a frozen
	// construction-time snapshot. A client that refreshes twice (deploy then overwrite, with the access
	// token expiring at the confirm prompt) must actually refresh the second time, not adopt the token
	// it saved on the first refresh and replay a now-stale bearer.
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("LOFT_TOKEN", "")
	var n int32
	srv := mockIdP(t, func(w http.ResponseWriter, _ *http.Request) {
		i := atomic.AddInt32(&n, 1)
		writeToken(w, 200, map[string]any{"access_token": fmt.Sprintf("at%d", i+1), "refresh_token": fmt.Sprintf("rt%d", i+1)})
	})
	if err := saveCredentials(credentials{Issuer: srv.URL, ClientID: "cid", AccessToken: "at1", RefreshToken: "rt1"}); err != nil {
		t.Fatal(err)
	}
	refresh := refreshFunc()
	first, err := refresh(context.Background(), "at1")
	if err != nil || first != "at2" {
		t.Fatalf("first refresh = %q, %v; want at2", first, err)
	}
	second, err := refresh(context.Background(), first) // the token do() passes on a second 401
	if err != nil || second != "at3" {
		t.Fatalf("second refresh = %q, %v; want at3 (a real network refresh, not an adopt)", second, err)
	}
	if got := atomic.LoadInt32(&n); got != 2 {
		t.Fatalf("token endpoint hits = %d, want 2", got)
	}
}

func TestRefreshFuncTerminalWhenClientIDMissing(t *testing.T) {
	// A record with a refresh token and issuer but no client id cannot refresh. The closure must treat
	// it as terminal (empty token, no network call) so the caller surfaces the clean 401 login hint
	// rather than a "cannot refresh" error.
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("LOFT_TOKEN", "")
	var hits int32
	srv := mockIdP(t, func(w http.ResponseWriter, _ *http.Request) {
		atomic.AddInt32(&hits, 1)
		writeToken(w, 200, map[string]any{"access_token": "nope"})
	})
	if err := saveCredentials(credentials{Issuer: srv.URL, AccessToken: "at1", RefreshToken: "rt1"}); err != nil {
		t.Fatal(err)
	}
	token, err := refreshFunc()(context.Background(), "at1")
	if err != nil {
		t.Fatalf("err = %v, want nil", err)
	}
	if token != "" {
		t.Fatalf("token = %q, want empty (terminal)", token)
	}
	if got := atomic.LoadInt32(&hits); got != 0 {
		t.Fatalf("token endpoint hits = %d, want 0 (no refresh without a client id)", got)
	}
}
