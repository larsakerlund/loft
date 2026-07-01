// Package deploy publishes a site: a multipart POST of a site's files that loftd writes under the
// chosen subdomain to be served as static files. The write is staged in a temp dir then mirrored over
// the old site, so a site is never served half-deployed.
//
// Both the browser console and the `loft` CLI deploy here. Three gates keep this from becoming a way
// for one hosted site to publish over others (the flat-trust model authorizes every user for
// every site, so the only boundary that matters here is WHERE the call originates):
//   - origin: the request must come from the apex (X-Loft-Site = _apex, set by the proxy from the
//     validated hostname and never client-settable), never a hosted site.
//   - fetch metadata: Sec-Fetch-Site must be same-origin, none, or absent. Browsers always send this
//     and page script cannot forge or strip it, so a deploy driven from any other origin (same-site
//     subdomain or cross-site) is refused regardless of CORS. This is the load-bearing defense against
//     a hosted site emulating the CLI.
//   - browser CSRF: a browser deploy must additionally be same-origin (the Origin host equals the
//     request host). The CLI is not a browser and carries no ambient cookie or fetch metadata; it
//     proves intent with X-Loft-Deploy-Client, which a cross-origin page cannot set without a CORS
//     preflight loftd never grants.
package deploy

import (
	"errors"
	"io"
	"log"
	"mime"
	"mime/multipart"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"strings"

	"github.com/google/uuid"

	"github.com/larsakerlund/loft/internal/config"
	"github.com/larsakerlund/loft/internal/limit"
	"github.com/larsakerlund/loft/internal/web"
)

const (
	maxFileBytes  = 25 * 1024 * 1024  // per file, matches the upload/CLI limit
	maxTotalBytes = 100 * 1024 * 1024 // per deploy, across all files
	maxFiles      = 2000              // per deploy
	deploysPerMin = 30
	deployBurst   = 10
)

// Service is the loft deploy HTTP service.
type Service struct {
	dir     string
	deploys *limit.Limiter
}

// New builds the service. The sites dir always has a value (config defaults it to the mount path),
// and deploy is gated to the root site regardless, so unlike db/uploads there is no
// "not configured" state to fall back to.
func New(cfg config.Config) *Service {
	return &Service{dir: cfg.SitesDir, deploys: limit.New(deploysPerMin, deployBurst)}
}

// Handler serves POST /api/deploy (publish a site) and DELETE /api/deploy?site=<name> (remove one).
func (s *Service) Handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost && r.Method != http.MethodDelete {
			web.Error(w, http.StatusMethodNotAllowed, "method not allowed")
			return
		}
		if msg, ok := deployAllowed(r); !ok {
			web.Error(w, http.StatusForbidden, msg)
			return
		}
		user, _ := web.User(r.Context())
		if !s.deploys.Allow(user.ID) {
			web.Error(w, http.StatusTooManyRequests, "deploy rate limit — slow down")
			return
		}
		if r.Method == http.MethodDelete {
			s.remove(w, r)
			return
		}
		s.deploy(w, r)
	})
}

// deployAllowed enforces the origin + CSRF gates (see the package doc). Returns the user-facing
// reason when refused.
func deployAllowed(r *http.Request) (string, bool) {
	if web.Site(r) != "_apex" {
		return "deploy is only available from the console or the CLI", false
	}
	// Fetch-metadata backstop. Browsers always send Sec-Fetch-Site and page script can neither forge
	// nor remove it, so a deploy driven from a hosted site is same-site or cross-site and is refused
	// here. Only a same-origin browser request (the console), a top-level navigation, or a non-browser
	// client (the CLI, which sends no Sec-Fetch-Site) gets through. This does not depend on loftd
	// withholding CORS headers, so it holds even if a proxy later adds permissive CORS.
	switch r.Header.Get("Sec-Fetch-Site") {
	case "", "none", "same-origin":
	default: // same-site, cross-site
		return "cross-origin deploy refused", false
	}
	// The console is same-origin; the CLI proves intent with X-Loft-Deploy-Client, which a cross-origin
	// page cannot set without a CORS preflight loftd never grants.
	if !sameOrigin(r) && r.Header.Get("X-Loft-Deploy-Client") == "" {
		return "cross-origin deploy refused", false
	}
	return "", true
}

// remove deletes a deployed site. The name comes from the query, sanitized the same way the served
// host is, so it can only ever name a directory directly under the sites root.
func (s *Service) remove(w http.ResponseWriter, r *http.Request) {
	site := web.SanitizeLabel(strings.TrimSpace(r.URL.Query().Get("site")))
	if site == "" || site == "_apex" {
		web.Error(w, http.StatusBadRequest, "a site name is required")
		return
	}
	dest := filepath.Join(s.dir, site)
	if _, err := os.Stat(dest); err != nil { //nolint:gosec // G703: site is SanitizeLabel'd before Join, so dest stays under the sites root
		web.Error(w, http.StatusNotFound, "no such site")
		return
	}
	if err := os.RemoveAll(dest); err != nil { //nolint:gosec // G703: site is SanitizeLabel'd before Join, so dest stays under the sites root
		log.Printf("loftd deploy delete failed (site=%q): %v", site, err) //nolint:gosec // G706: site is SanitizeLabel'd and %q-escaped
		web.Error(w, http.StatusInternalServerError, "delete failed")
		return
	}
	web.JSON(w, http.StatusOK, map[string]any{"site": site, "deleted": true})
}

func (s *Service) deploy(w http.ResponseWriter, r *http.Request) {
	mr, err := r.MultipartReader()
	if err != nil {
		web.Error(w, http.StatusBadRequest, "expected a multipart form")
		return
	}

	// Stage into a temp dir first (a fresh dir, so the upload itself never touches the live site),
	// then mirror it onto the site directory. We mirror in place rather than rename the live
	// directory aside, because on a network filesystem (notably an SMB share) renaming a directory whose files the reverse proxy holds
	// open fails with "permission denied"; overwriting the files in place does not.
	staging := filepath.Join(s.dir, ".staging-"+uuid.NewString())
	if err := os.MkdirAll(staging, 0o755); err != nil { // #nosec G703 -- staging is under s.dir
		web.Error(w, http.StatusInternalServerError, "deploy failed")
		return
	}
	defer func() { _ = os.RemoveAll(staging) }() // #nosec G703 -- temp dir, removed on every path

	res, uerr := stage(mr, staging)
	if uerr != nil {
		web.Error(w, uerr.status, uerr.msg)
		return
	}
	dest := filepath.Join(s.dir, res.site)
	// Overwriting an existing site is a deliberate act (like the CLI): refuse unless the caller
	// confirmed, so a deploy can't clobber someone else's live site by reusing its name.
	if !res.overwrite {
		if _, err := os.Stat(dest); err == nil { // #nosec G706 -- site is SanitizeLabel'd
			web.Error(w, http.StatusConflict, "site already exists")
			return
		}
	}
	if err := mirror(staging, dest); err != nil {
		log.Printf("loftd deploy mirror failed (site=%q): %v", res.site, err) // #nosec G706 -- site is SanitizeLabel'd and %q-escaped
		web.Error(w, http.StatusInternalServerError, "deploy failed")
		return
	}
	web.JSON(w, http.StatusOK, map[string]any{"site": res.site, "files": res.files, "bytes": res.bytes})
}

type staged struct {
	site      string
	overwrite bool
	files     int
	bytes     int64
}

type userError struct {
	status int
	msg    string
}

// stage drains the multipart body into the staging dir and reports the target site and totals, or a
// user-facing error. The site field must arrive before the files (the console sends it first).
func stage(mr *multipart.Reader, staging string) (staged, *userError) {
	var out staged
	for {
		part, err := mr.NextPart()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return out, &userError{http.StatusBadRequest, "malformed upload"}
		}
		switch part.FormName() {
		case "site":
			out.site = web.SanitizeLabel(strings.TrimSpace(readField(part)))
		case "overwrite":
			out.overwrite = strings.TrimSpace(readField(part)) == "true"
		case "files":
			if uerr := stageFile(staging, part, &out); uerr != nil {
				return out, uerr
			}
		}
	}
	if out.site == "" || out.site == "_apex" {
		return out, &userError{http.StatusBadRequest, "a site name is required"}
	}
	if out.files == 0 {
		return out, &userError{http.StatusBadRequest, "no files to deploy"}
	}
	return out, nil
}

// stageFile writes one "files" part and updates the running totals.
func stageFile(staging string, part *multipart.Part, out *staged) *userError {
	if out.site == "" || out.site == "_apex" {
		return &userError{http.StatusBadRequest, "site name must come before the files"}
	}
	rel := cleanRelPath(partPath(part))
	if rel == "" {
		return nil // a part with no usable path (e.g. an empty dir entry)
	}
	out.files++
	if out.files > maxFiles {
		return &userError{http.StatusRequestEntityTooLarge, "too many files in one deploy"}
	}
	n, err := writeFile(staging, rel, part, maxTotalBytes-out.bytes)
	if err != nil {
		return &userError{http.StatusRequestEntityTooLarge, err.Error()}
	}
	out.bytes += n
	return nil
}

// mirror makes dest match staging by writing each staged file in place (overwriting), then deleting
// anything in dest that staging doesn't have. Unlike a rename-swap this never moves the live
// directory, so it works on network filesystems where the reverse proxy holds open handles on the served files. It is
// not atomic (a request mid-deploy can see a mix), an acceptable trade for a static-site push and
// the same approach the CLI uses against the share.
func mirror(staging, dest string) error {
	if err := os.MkdirAll(dest, 0o755); err != nil { // #nosec G703 -- dest under the sites root
		return err
	}
	staged := map[string]struct{}{}
	walkErr := filepath.WalkDir(staging, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(staging, p)
		if rel == "." {
			return nil
		}
		staged[rel] = struct{}{}
		return writeEntry(p, filepath.Join(dest, rel), d.IsDir())
	})
	if walkErr != nil {
		return walkErr
	}
	prune(dest, staged)
	return nil
}

// writeEntry mirrors one staged node (src) onto target. A prior deploy may have left the opposite
// kind of node in place — a regular file where we now want a directory, or vice versa — and the OS
// cannot convert one into the other (MkdirAll → ENOTDIR, os.Create → EISDIR), so we drop the
// conflicting node before creating the replacement.
func writeEntry(src, target string, isDir bool) error {
	if fi, lerr := os.Lstat(target); lerr == nil && fi.IsDir() != isDir {
		if err := os.RemoveAll(target); err != nil { // #nosec G703 G304 -- target under dest
			return err
		}
	}
	if isDir {
		return os.MkdirAll(target, 0o755) // #nosec G703 -- target under dest
	}
	return copyFile(src, target)
}

// prune removes everything under dest that the new deploy no longer includes — files and now-unused
// directories alike, so a folder a later deploy drops does not linger on the shared mount.
// Best-effort: a node the reverse proxy still holds open can't be removed on some network
// filesystems, which is harmless (it just lingers) and not worth failing the deploy on.
func prune(dest string, staged map[string]struct{}) {
	_ = filepath.WalkDir(dest, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return nil //nolint:nilerr // best-effort cleanup: skip entries we can't read
		}
		rel, _ := filepath.Rel(dest, p)
		if rel == "." {
			return nil
		}
		if _, ok := staged[rel]; ok {
			return nil // part of the new deploy — keep it (and descend into kept directories)
		}
		_ = os.RemoveAll(p) // #nosec G122 G304 -- p under the validated site dir, no symlinks in a deployed tree
		if d.IsDir() {
			return filepath.SkipDir // whole subtree is gone; no need to descend
		}
		return nil
	})
}

// copyFile writes src over dst (truncating an existing file), creating parent dirs as needed.
func copyFile(src, dst string) error {
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil { // #nosec G703 -- dst under dest
		return err
	}
	in, err := os.Open(src) // #nosec G304 -- src is a staged file under the sites root
	if err != nil {
		return err
	}
	defer func() { _ = in.Close() }()
	out, err := os.Create(dst) // #nosec G304 -- dst within the validated site dir
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		_ = out.Close()
		return err
	}
	return out.Close()
}

// writeFile streams one part to staging/rel, capped at the per-file limit and the remaining budget.
func writeFile(staging, rel string, part *multipart.Part, remaining int64) (int64, error) {
	dest := filepath.Join(staging, filepath.FromSlash(rel))
	if !within(staging, dest) {
		return 0, errors.New("invalid file path")
	}
	if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil { // #nosec G703 -- dest within staging
		return 0, errors.New("deploy failed")
	}
	f, err := os.Create(dest) // #nosec G304 G703 -- dest within staging (cleanRelPath + within)
	if err != nil {
		return 0, errors.New("deploy failed")
	}
	defer func() { _ = f.Close() }()

	lim := int64(maxFileBytes)
	if remaining < lim {
		lim = remaining
	}
	// LimitReader+1 so hitting the limit is detectable rather than a silent truncation.
	n, err := io.Copy(f, io.LimitReader(part, lim+1))
	if err != nil {
		return 0, errors.New("deploy failed")
	}
	if n > lim {
		return 0, errors.New("deploy exceeds the size limit")
	}
	return n, nil
}

// partPath is the upload's relative path WITHIN the site, read from the raw Content-Disposition
// filename. We deliberately do NOT use part.FileName(): it applies filepath.Base per RFC 7578 §4.2,
// collapsing every nested file (assets/app.js -> app.js) into the site root and breaking any build
// with subdirectories. Honoring the directory path is the point here — directory uploads are how both
// the CLI and the browser FormData idiom send a site. A parse error yields "", which stageFile skips;
// cleanRelPath still strips ".." so this stays traversal-safe.
func partPath(part *multipart.Part) string {
	_, params, _ := mime.ParseMediaType(part.Header.Get("Content-Disposition"))
	return params["filename"]
}

// cleanRelPath reduces a part filename to a safe forward-slash relative path, dropping any "."/".."
// and leading slashes. Empty if nothing usable remains. Rebuilding from cleaned parts (not trusting
// the raw name) is what prevents traversal out of the staging dir.
func cleanRelPath(name string) string {
	name = strings.ReplaceAll(name, "\\", "/")
	out := make([]string, 0, 8)
	for _, seg := range strings.Split(name, "/") {
		seg = strings.TrimSpace(seg)
		if seg == "" || seg == "." || seg == ".." {
			continue
		}
		out = append(out, seg)
	}
	return path.Join(out...)
}

// within reports whether dest stays inside root (defense in depth on top of cleanRelPath).
func within(root, dest string) bool {
	rel, err := filepath.Rel(root, dest)
	if err != nil {
		return false
	}
	return rel != ".." && !strings.HasPrefix(rel, ".."+string(os.PathSeparator))
}

// sameOrigin reports whether the request's Origin host matches its Host. A browser always sends
// Origin on a cross-origin POST, so a mismatch (or a parse failure) means the call did not come from
// a page on our own host.
func sameOrigin(r *http.Request) bool {
	origin := r.Header.Get("Origin")
	if origin == "" {
		return false
	}
	u, err := url.Parse(origin)
	if err != nil {
		return false
	}
	return u.Host == r.Host
}

func readField(part *multipart.Part) string {
	b, _ := io.ReadAll(io.LimitReader(part, 256))
	return string(b)
}
