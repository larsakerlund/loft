// Package server wires loftd's HTTP surface: one mux with the central auth gate on every /api/*
// route (so unmatched paths still 404), the realtime upgrade endpoints, and boot-time warmups.
// loftd serves ONLY /api/*; the reverse proxy serves the static sites and uploaded files.
package server

import (
	"context"
	"errors"
	"log"
	"net/http"

	"github.com/larsakerlund/loft/internal/ai"
	"github.com/larsakerlund/loft/internal/config"
	"github.com/larsakerlund/loft/internal/db"
	"github.com/larsakerlund/loft/internal/deploy"
	"github.com/larsakerlund/loft/internal/identity"
	"github.com/larsakerlund/loft/internal/realtime"
	"github.com/larsakerlund/loft/internal/uploads"
	"github.com/larsakerlund/loft/internal/web"
)

// Server is loftd's assembled HTTP application.
type Server struct {
	handler http.Handler
	warm    []func(context.Context) error
	hub     *realtime.Hub
}

// New builds the server from config, resolving every backend. Features whose dependencies aren't
// configured degrade to a clear status (501/500) rather than failing startup.
func New(ctx context.Context, cfg config.Config) (*Server, error) {
	resolver := identity.NewResolver(cfg)
	hub := realtime.NewHub()
	mux := http.NewServeMux()
	auth := func(h http.Handler) http.Handler { return web.Auth(resolver, h) }
	s := &Server{hub: hub}

	// loft.db
	switch pool, err := db.NewPool(ctx, cfg); {
	case errors.Is(err, db.ErrNotConfigured):
		mux.Handle("/api/db/", auth(stub(http.StatusNotImplemented, "db not configured")))
	case err != nil:
		return nil, err
	default:
		store := db.New(pool, hub.PublishDb)
		s.warm = append(s.warm, store.Init)
		mux.Handle("/api/db/subscribe", auth(hub.SubscribeHandler()))
		mux.Handle("/api/db/", auth(store.Handler()))
	}

	// loft.upload
	switch up, err := uploads.New(cfg); {
	case errors.Is(err, uploads.ErrNotConfigured):
		mux.Handle("/api/upload", auth(stub(http.StatusInternalServerError, "uploads not configured")))
	case err != nil:
		return nil, err
	default:
		mux.Handle("/api/upload", auth(up.Handler()))
	}

	// loft deploy: POST publishes a site, DELETE removes one. Gated to the apex origin (the root site or
	// the CLI) inside the handler, which also rejects other methods.
	mux.Handle("/api/deploy", auth(deploy.New(cfg).Handler()))

	// loft.ai (the handler itself reports 501 when the endpoint isn't configured)
	aiSvc := ai.New(cfg)
	s.warm = append(s.warm, aiSvc.Init)
	mux.Handle("POST /api/ai/chat", auth(aiSvc.Handler()))

	// loft.socket + identity
	mux.Handle("/api/socket", auth(hub.SocketHandler()))
	mux.Handle("GET /api/me", auth(http.HandlerFunc(meHandler)))

	// CLI discovery: public (no auth), so `loft login <url>` configures itself from the URL alone.
	// Everything returned is public OAuth client config; a public client id is not a secret.
	mux.Handle("GET /.well-known/loft", cliConfigHandler(cfg))

	s.handler = mux
	return s, nil
}

// Handler is the assembled http.Handler.
func (s *Server) Handler() http.Handler { return s.handler }

// Close releases long-lived resources (terminates live WebSockets) on graceful shutdown.
func (s *Server) Close() { s.hub.Close() }

// Warm runs boot-time warmups (Postgres token + schema migration, AI token) so the first real
// request isn't slow and misconfiguration surfaces in the logs. Failures are logged, not fatal.
func (s *Server) Warm(ctx context.Context) {
	for _, w := range s.warm {
		if err := w(ctx); err != nil {
			log.Printf("loftd warmup: %v", err)
		}
	}
}

func meHandler(w http.ResponseWriter, r *http.Request) {
	user, _ := web.User(r.Context())
	web.JSON(w, http.StatusOK, user)
}

// cliConfigHandler serves the public OAuth configuration the CLI needs to log in: the OIDC issuer,
// the CLI's public client id, and the scope to request.
func cliConfigHandler(cfg config.Config) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		web.JSON(w, http.StatusOK, map[string]any{
			"issuer":   cfg.OIDCIssuerURL(),
			"clientId": cfg.CLIClientID,
			"scope":    cfg.CLIScope,
		})
	})
}

func stub(status int, msg string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { web.Error(w, status, msg) })
}
