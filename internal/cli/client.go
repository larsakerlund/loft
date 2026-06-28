// The loftd HTTP client. The CLI bundles the folder
// and uploads it to loftd's /api/deploy, which writes the site through its own storage adapter (the CLI never touches storage directly). Every
// request carries the deploy-client header (so loftd's CSRF gate lets a non-browser call through) and,
// when present, the bearer token from `loft login`.
package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"
)

// errSiteExists is returned by deploy when the site already exists and overwrite was not requested,
// so the caller can confirm with the user and retry.
var errSiteExists = errors.New("site already exists")

// copyInto streams a file's bytes into w.
func copyInto(w io.Writer, path string) error {
	f, err := os.Open(path) // #nosec G304 -- path is a file collected from the deploy dir
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	_, err = io.Copy(w, f)
	return err
}

type loftClient struct {
	base  string
	token string
	http  *http.Client
}

func newClient(base, token string) *loftClient {
	return &loftClient{
		base:  strings.TrimRight(base, "/"),
		token: token,
		http:  &http.Client{Timeout: 5 * time.Minute},
	}
}

func (c *loftClient) newRequest(ctx context.Context, method, path string, body io.Reader) (*http.Request, error) {
	//nolint:gosec // G704: the CLI talks to the platform URL the user configured; a client CLI has no SSRF surface
	req, err := http.NewRequestWithContext(ctx, method, c.base+path, body)
	if err != nil {
		return nil, err
	}
	req.Header.Set("X-Loft-Deploy-Client", "cli")
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}
	return req, nil
}

// errorFor turns a non-2xx response into a useful error: 401 points the user at `loft login`, and
// any other status surfaces loftd's plain-text reason.
func errorFor(resp *http.Response) error {
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	msg := strings.TrimSpace(string(body))
	if resp.StatusCode == http.StatusUnauthorized {
		return fmt.Errorf("not authenticated (run `loft login`): %s", msg)
	}
	if msg == "" {
		msg = resp.Status
	}
	return fmt.Errorf("loftd: %s", msg)
}

type deployResult struct {
	Site  string `json:"site"`
	Files int    `json:"files"`
	Bytes int64  `json:"bytes"`
}

// deploy streams the site's files to /api/deploy as multipart/form-data. The body is built in a pipe
// so a large site is never held whole in memory; the "site" field is written before the files,
// matching the order the server expects.
func (c *loftClient) deploy(ctx context.Context, site string, entries []fileEntry, force bool) (deployResult, error) {
	pr, pw := io.Pipe()
	mw := multipart.NewWriter(pw)
	go func() {
		err := writeMultipart(mw, site, entries, force)
		_ = mw.Close()
		_ = pw.CloseWithError(err)
	}()

	req, err := c.newRequest(ctx, http.MethodPost, "/api/deploy", pr)
	if err != nil {
		return deployResult{}, err
	}
	req.Header.Set("Content-Type", mw.FormDataContentType())

	resp, err := c.http.Do(req)
	if err != nil {
		return deployResult{}, fmt.Errorf("reaching %s: %w", c.base, err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode == http.StatusConflict {
		return deployResult{}, errSiteExists
	}
	if resp.StatusCode/100 != 2 {
		return deployResult{}, errorFor(resp)
	}
	var out deployResult
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return deployResult{}, err
	}
	return out, nil
}

func writeMultipart(mw *multipart.Writer, site string, entries []fileEntry, force bool) error {
	if err := mw.WriteField("site", site); err != nil {
		return err
	}
	if force {
		if err := mw.WriteField("overwrite", "true"); err != nil {
			return err
		}
	}
	for _, e := range entries {
		fw, err := mw.CreateFormFile("files", e.rel)
		if err != nil {
			return err
		}
		if err := copyInto(fw, e.abs); err != nil {
			return err
		}
	}
	return nil
}

// delete removes a deployed site via DELETE /api/deploy?site=<name>.
func (c *loftClient) delete(ctx context.Context, site string) error {
	req, err := c.newRequest(ctx, http.MethodDelete, "/api/deploy?site="+url.QueryEscape(site), http.NoBody)
	if err != nil {
		return err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("reaching %s: %w", c.base, err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode/100 != 2 {
		return errorFor(resp)
	}
	return nil
}

// me returns the signed-in user's email from /api/me.
func (c *loftClient) me(ctx context.Context) (string, error) {
	req, err := c.newRequest(ctx, http.MethodGet, "/api/me", http.NoBody)
	if err != nil {
		return "", err
	}
	resp, err := c.http.Do(req) //nolint:gosec // G704: request to the user-configured platform URL, see newRequest
	if err != nil {
		return "", fmt.Errorf("reaching %s: %w", c.base, err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode/100 != 2 {
		return "", errorFor(resp)
	}
	var u struct {
		Email string `json:"email"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&u); err != nil {
		return "", err
	}
	return u.Email, nil
}
