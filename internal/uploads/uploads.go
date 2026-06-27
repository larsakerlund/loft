// Package uploads is loft.upload: a hosted app POSTs raw file bytes and loftd stores them under the
// calling site's prefix, returning a /uploads/<uuid>/<file> URL (served by the reverse proxy, not loftd). DELETE
// removes a file. The site always comes from the authenticated host and keys are rebuilt from
// validated parts, so one site can never read or delete another's prefix.
package uploads

import (
	"context"
	"errors"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/larsakerlund/loft/internal/config"
	"github.com/larsakerlund/loft/internal/limit"
	"github.com/larsakerlund/loft/internal/web"
)

const maxBytes = 25 * 1024 * 1024 // matches the CLI's per-file limit

// ErrNotConfigured means no upload backend (blob or dir) is configured.
var ErrNotConfigured = errors.New("uploads not configured")

// store is the write/delete backend; keys are always "<site>/<uuid>/<filename>".
type store interface {
	put(ctx context.Context, key, contentType string, body []byte) error
	del(ctx context.Context, key string) error
}

const (
	// Generous enough that legit bursts (e.g. uploading a gallery) sail through; it only exists to
	// stop a runaway flood. The real cost lever for uploads is total stored bytes, not request rate.
	uploadsPerMin  = 120
	uploadBurst    = 30
	maxConcurrent  = 16               // global in-flight uploads (each buffers up to 25MB)
	storeOpTimeout = 60 * time.Second // per blob/dir put/del deadline
)

// Service is the loft.upload HTTP service.
type Service struct {
	store   store
	uploads *limit.Limiter // per-site upload rate limit
	sem     chan struct{}  // global concurrency cap (bounds aggregate upload memory)
}

// New resolves the backend from config: blob (account+MI or connection string) or a local dir.
func New(cfg config.Config) (*Service, error) {
	s, err := newBlobStore(cfg)
	if err != nil {
		return nil, err
	}
	if s == nil {
		if cfg.UploadsDir == "" {
			return nil, ErrNotConfigured
		}
		s = &dirStore{dir: cfg.UploadsDir}
	}
	return &Service{store: s, uploads: limit.New(uploadsPerMin, uploadBurst), sem: make(chan struct{}, maxConcurrent)}, nil
}

// allowUpload is the upload rate limit, keyed per (site, user) so one user can't drain the shared
// budget and 429 their others, mirroring loft.ai's per-(site,user) keying.
func (s *Service) allowUpload(site, userID string) bool { return s.uploads.Allow(site + "|" + userID) }

// Handler serves POST (upload) and DELETE (remove) on /api/upload.
func (s *Service) Handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodPost:
			s.upload(w, r)
		case http.MethodDelete:
			s.remove(w, r)
		default:
			web.Error(w, http.StatusMethodNotAllowed, "method not allowed")
		}
	})
}

func (s *Service) upload(w http.ResponseWriter, r *http.Request) {
	site := web.Site(r)
	user, _ := web.User(r.Context())
	if !s.allowUpload(site, user.ID) {
		web.Error(w, http.StatusTooManyRequests, "upload rate limit — slow down")
		return
	}
	// Global concurrency cap: bounds aggregate memory (each upload buffers up to 25MB). Non-blocking
	// so a flood gets a fast 503 instead of piling up goroutines/heap.
	select {
	case s.sem <- struct{}{}:
		defer func() { <-s.sem }()
	default:
		web.Error(w, http.StatusServiceUnavailable, "server busy, try again")
		return
	}

	filename := safeName(firstHeader(r, "X-Loft-Filename", "file"))
	contentType := firstHeader(r, "Content-Type", "application/octet-stream")

	r.Body = http.MaxBytesReader(w, r.Body, maxBytes)
	body, err := io.ReadAll(r.Body)
	if err != nil {
		web.Error(w, http.StatusRequestEntityTooLarge, "file exceeds 25MB")
		return
	}
	if len(body) == 0 {
		web.Error(w, http.StatusBadRequest, "empty file")
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), storeOpTimeout)
	defer cancel()
	// <site>/<uuid>/<filename>: site-scoped, and the uuid makes every upload a fresh, unguessable URL.
	rel := uuid.NewString() + "/" + filename
	if err := s.store.put(ctx, site+"/"+rel, contentType, body); err != nil {
		log.Printf("loftd upload put failed (site=%q key=%q): %v", site, rel, err) // #nosec G706 -- site/rel are sanitized (SanitizeLabel + uuid/safeName) and %q-escaped
		web.Error(w, http.StatusInternalServerError, "upload failed")
		return
	}
	web.JSON(w, http.StatusOK, map[string]any{"url": "/uploads/" + rel, "name": filename, "size": len(body)})
}

func (s *Service) remove(w http.ResponseWriter, r *http.Request) {
	site := web.Site(r)
	rel, ok := relFromPath(r.URL.Query().Get("path"))
	if !ok {
		web.Error(w, http.StatusBadRequest, "path: the url from loft.upload() required")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), storeOpTimeout)
	defer cancel()
	if err := s.store.del(ctx, site+"/"+rel); err != nil {
		log.Printf("loftd upload delete failed (site=%q key=%q): %v", site, rel, err) // #nosec G706 -- site/rel are sanitized (SanitizeLabel + uuid/safeName) and %q-escaped
		web.Error(w, http.StatusInternalServerError, "delete failed")
		return
	}
	web.JSON(w, http.StatusOK, map[string]any{"ok": true})
}

var (
	unsafeName = regexp.MustCompile(`[^A-Za-z0-9._-]+`)
	uuidRe     = regexp.MustCompile(`^[0-9a-fA-F-]{36}$`)
)

func safeName(s string) string {
	s = filepath.Base(s)
	s = unsafeName.ReplaceAllString(s, "-")
	s = strings.Trim(s, "-")
	if s == "" {
		return "file"
	}
	return s
}

// relFromPath turns an upload URL ("/uploads/<uuid>/<file>" or a bare "<uuid>/<file>") into a safe
// "<uuid>/<file>" key fragment, or false if it doesn't have that exact shape. Rebuilding from
// validated parts (not trusting the raw string) is what prevents path traversal.
func relFromPath(raw string) (string, bool) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", false
	}
	if u, err := url.Parse(raw); err == nil {
		raw = u.Path
	}
	raw = strings.TrimPrefix(strings.TrimPrefix(raw, "/"), "uploads/")
	parts := strings.Split(strings.Trim(raw, "/"), "/")
	if len(parts) != 2 || !uuidRe.MatchString(parts[0]) {
		return "", false
	}
	return parts[0] + "/" + safeName(parts[1]), true
}

func firstHeader(r *http.Request, key, def string) string {
	if v := r.Header.Get(key); v != "" {
		return v
	}
	return def
}

// --- local directory backend (dev) ---

type dirStore struct{ dir string }

func (d *dirStore) put(_ context.Context, key, _ string, body []byte) error {
	dest, err := d.resolve(key)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil { // #nosec G703 -- dest confined to d.dir by resolve()
		return err
	}
	return os.WriteFile(dest, body, 0o644) // #nosec G703 -- dest confined to d.dir by resolve()
}

func (d *dirStore) del(_ context.Context, key string) error {
	dest, err := d.resolve(key)
	if err != nil {
		return err
	}
	if err := os.Remove(dest); err != nil && !errors.Is(err, os.ErrNotExist) { // #nosec G703 -- dest confined to d.dir by resolve()
		return err // idempotent: a missing file is not an error
	}
	return nil
}

// resolve joins key under the store dir and verifies the result stays inside it: defense in depth
// on top of the validated keys (sanitized site + uuid + safe filename).
func (d *dirStore) resolve(key string) (string, error) {
	dest := filepath.Join(d.dir, filepath.FromSlash(key))
	if dest != d.dir && !strings.HasPrefix(dest, filepath.Clean(d.dir)+string(os.PathSeparator)) {
		return "", errors.New("invalid upload path")
	}
	return dest, nil
}
