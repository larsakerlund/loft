// Command loftd is the Loft runtime API. An authenticating reverse proxy serves the static sites and
// forwards /api/* here with a validated OIDC access token; loftd re-validates the bearer itself, so
// the API is closed even to a caller inside the network.
package main

import (
	"context"
	"errors"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/larsakerlund/loft/internal/config"
	"github.com/larsakerlund/loft/internal/server"
)

// shutdownGrace bounds how long in-flight requests have to finish on SIGTERM before loftd exits.
const shutdownGrace = 15 * time.Second

func main() {
	if err := run(); err != nil {
		log.Fatalf("loftd: %v", err)
	}
}

func run() error {
	cfg := config.Load()
	if err := cfg.Validate(); err != nil {
		return err
	}
	if cfg.Dev {
		log.Println("loftd: DEV MODE — auth is OFF, identity is a hard-coded local user. Never expose this.")
	}

	// Root context cancelled on SIGTERM/SIGINT. Orchestrators send SIGTERM on
	// redeploy, so this lets in-flight /api/db writes and uploads finish instead of being severed.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	srv, err := server.New(ctx, cfg)
	if err != nil {
		return err
	}

	// Warm Postgres (token + schema) and the AI token in the background; tied to ctx so a shutdown
	// during startup cancels the warmups too.
	go srv.Warm(ctx)

	httpServer := &http.Server{
		Addr:              cfg.Listen,
		Handler:           srv.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
		// Deliberately NO WriteTimeout: loft.ai streams SSE and loft.socket holds WebSockets open, and
		// a write deadline would sever them. IdleTimeout only bounds idle keep-alive reuse, which is safe.
		IdleTimeout: 120 * time.Second,
	}

	serveErr := make(chan error, 1)
	go func() {
		log.Printf("loftd listening on %s", cfg.Listen)
		if err := httpServer.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			serveErr <- err
		}
	}()

	select {
	case err := <-serveErr:
		return err
	case <-ctx.Done():
	}

	stop() // restore default signal handling so a second signal force-quits
	log.Println("loftd shutting down")
	srv.Close() // terminate live WebSockets (hijacked conns Shutdown won't wait for)
	shutdownCtx, cancel := context.WithTimeout(context.Background(), shutdownGrace)
	defer cancel()
	return httpServer.Shutdown(shutdownCtx)
}
