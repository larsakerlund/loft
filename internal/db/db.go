// Package db is loft.db: a schemaless, multi-tenant document store on Postgres. Every operation is
// scoped to the calling SITE (from the authenticated host) and Postgres ROW-LEVEL SECURITY enforces
// that boundary in the database itself, so even a buggy query here cannot read another site's rows.
// Collections may opt into owner-only mode, where only a document's creator (stamped server-side
// from the token, never client input) may update or delete it.
package db

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"sync"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/larsakerlund/loft/internal/limit"
	"github.com/larsakerlund/loft/internal/web"
)

// collectionRe bounds collection names: short, predictable identifiers, no slashes, control chars,
// or unbounded length that could bloat keys or muddy logs.
var collectionRe = regexp.MustCompile(`^[A-Za-z0-9._-]{1,128}$`)

func validCollection(s string) bool { return collectionRe.MatchString(s) }

const (
	maxDocsPerSite = 10_000     // storage quota
	maxDocBytes    = 256 * 1024 // per-document size cap
	writesPerMin   = 600        // per-site sustained write rate
	writeBurst     = 60         // per-site burst allowance
)

// Publisher is notified of document changes so realtime subscribers can be fed. Injected so db does
// not depend on the realtime package.
type Publisher func(site, collection string, event any)

// Store is the loft.db service.
type Store struct {
	pool    *pgxpool.Pool
	publish Publisher

	schemaMu   sync.Mutex
	schemaDone bool

	writes *limit.Limiter // per-site write rate limit
}

// New builds a Store. The pool is created here; the schema migration runs on Init (or lazily).
func New(pool *pgxpool.Pool, publish Publisher) *Store {
	return &Store{pool: pool, publish: publish, writes: limit.New(writesPerMin, writeBurst)}
}

// Init warms the connection and runs the schema migration at startup, so the first real request
// isn't slow and misconfiguration surfaces in the boot logs.
func (s *Store) Init(ctx context.Context) error { return s.ensureSchema(ctx) }

// Handler serves /api/db/<collection>[/<id>]. The caller has already been authenticated by the
// web.Auth middleware; mutations are stamped/authorized against the token id, never client input.
func (s *Store) Handler() http.Handler { return http.HandlerFunc(s.serve) }

func (s *Store) serve(w http.ResponseWriter, r *http.Request) {
	user, _ := web.User(r.Context())
	site := web.Site(r)
	rest := strings.TrimPrefix(r.URL.Path, "/api/db/")
	collection, id, _ := strings.Cut(rest, "/")
	collection = strings.TrimSpace(collection)
	if !validCollection(collection) {
		web.Error(w, http.StatusBadRequest, "collection must be 1–128 chars of [A-Za-z0-9._-]")
		return
	}

	switch {
	case id == "" && r.Method == http.MethodPost:
		s.create(w, r, site, collection, user.ID)
	case id == "" && r.Method == http.MethodGet:
		s.list(w, r, site, collection)
	case id != "" && r.Method == http.MethodGet:
		s.get(w, r, site, collection, id)
	case id != "" && r.Method == http.MethodPatch:
		s.update(w, r, site, collection, id, user.ID)
	case id != "" && r.Method == http.MethodDelete:
		s.remove(w, r, site, collection, id, user.ID)
	default:
		web.Error(w, http.StatusMethodNotAllowed, "method not allowed")
	}
}

func (s *Store) create(w http.ResponseWriter, r *http.Request, site, collection, creator string) {
	// Validate/parse the body BEFORE consuming the write-rate counter (a malformed body shouldn't
	// burn a site's write budget).
	doc, ok := readJSONObject(w, r)
	if !ok {
		return
	}
	if !s.allowWrite(site, creator) {
		web.Error(w, http.StatusTooManyRequests, "write rate limit exceeded")
		return
	}
	ownerOnly := r.URL.Query().Get("ownerOnly") == "1"
	_, ownerOnlySpecified := r.URL.Query()["ownerOnly"]
	docBytes, _ := json.Marshal(doc)

	var result map[string]any
	err := s.withSite(r.Context(), site, func(tx pgx.Tx) error {
		var n int
		// Count across the whole SITE (RLS already scopes to it), so the cap is genuinely per-site,
		// not per-collection, which would let a caller multiply storage by spinning up collections.
		if err := tx.QueryRow(r.Context(), "select count(*) from documents").Scan(&n); err != nil {
			return err
		}
		if n >= maxDocsPerSite {
			return errQuota
		}
		// The collection's authorization policy is set by the first create. Upsert idempotently (ON
		// CONFLICT serializes concurrent first-creates at the DB, so neither errors), then read the
		// effective policy back: a later create that EXPLICITLY asks for a different ownerOnly is
		// rejected (409) so an app can't be silently downgraded and a divergence surfaces loudly.
		if _, err := tx.Exec(r.Context(),
			"insert into collections (site, name, owner_only) values (current_setting('loft.site', true), $1, $2) on conflict (site, name) do nothing",
			collection, ownerOnly); err != nil {
			return err
		}
		var stored bool
		if err := tx.QueryRow(r.Context(), "select owner_only from collections where name = $1", collection).Scan(&stored); err != nil {
			return err
		}
		if ownerOnlySpecified && stored != ownerOnly {
			return errPolicyConflict
		}
		var id, gotDoc string
		if err := tx.QueryRow(r.Context(),
			"insert into documents (site, collection, doc, creator) values (current_setting('loft.site', true), $1, $2::jsonb, $3) returning id::text, doc::text",
			collection, string(docBytes), creator).Scan(&id, &gotDoc); err != nil {
			return err
		}
		result = merge(gotDoc, id, &creator)
		return nil
	})
	if err != nil {
		writeErr(w, err)
		return
	}
	s.publish(site, collection, map[string]any{"op": "create", "doc": result})
	web.JSON(w, http.StatusOK, result)
}

func (s *Store) list(w http.ResponseWriter, r *http.Request, site, collection string) {
	lim := 100
	if v, err := strconv.Atoi(r.URL.Query().Get("limit")); err == nil {
		lim = min(max(v, 1), 1000)
	}
	out := []map[string]any{}
	err := s.withSite(r.Context(), site, func(tx pgx.Tx) error {
		rows, err := tx.Query(r.Context(),
			"select id::text, doc::text, creator from documents where collection = $1 order by created_at desc limit $2",
			collection, lim)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var id, doc string
			var creator *string
			if err := rows.Scan(&id, &doc, &creator); err != nil {
				return err
			}
			out = append(out, merge(doc, id, creator))
		}
		return rows.Err()
	})
	if err != nil {
		writeErr(w, err)
		return
	}
	web.JSON(w, http.StatusOK, out)
}

func (s *Store) get(w http.ResponseWriter, r *http.Request, site, collection, id string) {
	if uuid.Validate(id) != nil {
		web.Error(w, http.StatusNotFound, "not found")
		return
	}
	var result map[string]any
	err := s.withSite(r.Context(), site, func(tx pgx.Tx) error {
		var doc string
		var creator *string
		if err := tx.QueryRow(r.Context(),
			"select doc::text, creator from documents where collection = $1 and id = $2::uuid", collection, id).Scan(&doc, &creator); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return errNotFound
			}
			return err
		}
		result = merge(doc, id, creator)
		return nil
	})
	if err != nil {
		writeErr(w, err)
		return
	}
	web.JSON(w, http.StatusOK, result)
}

func (s *Store) update(w http.ResponseWriter, r *http.Request, site, collection, id, userID string) {
	// Cheap deterministic validation BEFORE consuming the write-rate counter, so a malformed id or
	// body can't burn a site's write budget.
	if uuid.Validate(id) != nil {
		web.Error(w, http.StatusNotFound, "not found")
		return
	}
	patch, ok := readJSONObject(w, r)
	if !ok {
		return
	}
	if !s.allowWrite(site, userID) {
		web.Error(w, http.StatusTooManyRequests, "write rate limit exceeded")
		return
	}
	patchBytes, _ := json.Marshal(patch)

	var result map[string]any
	err := s.withSite(r.Context(), site, func(tx pgx.Tx) error {
		if err := ownerCheck(r.Context(), tx, collection, id, userID); err != nil {
			return err
		}
		var doc string
		var creator *string
		if err := tx.QueryRow(r.Context(),
			"update documents set doc = doc || $3::jsonb, updated_at = now() where collection = $1 and id = $2::uuid returning doc::text, creator",
			collection, id, string(patchBytes)).Scan(&doc, &creator); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return errNotFound
			}
			return err
		}
		result = merge(doc, id, creator)
		return nil
	})
	if err != nil {
		writeErr(w, err)
		return
	}
	s.publish(site, collection, map[string]any{"op": "update", "doc": result})
	web.JSON(w, http.StatusOK, result)
}

func (s *Store) remove(w http.ResponseWriter, r *http.Request, site, collection, id, userID string) {
	if uuid.Validate(id) != nil {
		web.Error(w, http.StatusNotFound, "not found")
		return
	}
	err := s.withSite(r.Context(), site, func(tx pgx.Tx) error {
		if err := ownerCheck(r.Context(), tx, collection, id, userID); err != nil {
			return err
		}
		tag, err := tx.Exec(r.Context(), "delete from documents where collection = $1 and id = $2::uuid", collection, id)
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 0 {
			return errNotFound
		}
		return nil
	})
	if err != nil {
		writeErr(w, err)
		return
	}
	s.publish(site, collection, map[string]any{"op": "delete", "id": id})
	web.JSON(w, http.StatusOK, map[string]any{"ok": true})
}

// ownerCheck enforces a collection's owner-only policy: on an owner-only collection, only a
// document's creator may mutate it. Unattributed (legacy) rows stay mutable.
func ownerCheck(ctx context.Context, tx pgx.Tx, collection, id, userID string) error {
	var ownerOnly bool
	err := tx.QueryRow(ctx, "select owner_only from collections where name = $1", collection).Scan(&ownerOnly)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil // no policy ⇒ shared/collaborative
	}
	if err != nil {
		return err
	}
	if !ownerOnly {
		return nil
	}
	var creator *string
	if err := tx.QueryRow(ctx, "select creator from documents where collection = $1 and id = $2::uuid", collection, id).Scan(&creator); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return errNotFound
		}
		return err
	}
	// Fail closed: in an owner-only collection a row must have a creator that matches the caller.
	// An unattributed (NULL-creator) row is not mutable by anyone via the API.
	if creator == nil || *creator != userID {
		return errForbidden
	}
	return nil
}
