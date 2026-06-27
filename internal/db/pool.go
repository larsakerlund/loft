package db

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/larsakerlund/loft/internal/config"
)

// ErrNotConfigured means no Postgres connection is configured (loft.db is unavailable).
var ErrNotConfigured = errors.New("db not configured")

// Pool sizing. MaxConns must stay well under the server's max_connections; lifetimes keep
// token-bearing connections rotating before a short-lived credential expires.
const (
	poolMaxConns    = 20
	poolMaxLifetime = 30 * time.Minute
	poolMaxIdleTime = 5 * time.Minute
)

// NewPool builds the connection pool. A connection string wins (local dev, or any static-password
// deployment); otherwise host + user with a CredentialProvider supplying a fresh password on every
// connection (for example a rotating managed-identity token, no stored secret). Either way the pool is sized and
// given connection lifetimes/idle limits.
func NewPool(ctx context.Context, cfg config.Config) (*pgxpool.Pool, error) {
	connString := cfg.PGConnString
	var beforeConnect func(context.Context, *pgx.ConnConfig) error

	if connString == "" {
		if cfg.PGHost == "" {
			return nil, ErrNotConfigured
		}
		provider, err := newCredentialProvider(cfg)
		if err != nil {
			return nil, err
		}
		connString = fmt.Sprintf("host=%s port=5432 dbname=%s user=%s sslmode=require", cfg.PGHost, cfg.PGDatabase, cfg.PGUser)
		beforeConnect = func(ctx context.Context, cc *pgx.ConnConfig) error {
			pw, err := provider.Password(ctx)
			if err != nil {
				return err
			}
			cc.Password = pw
			return nil
		}
	}

	pcfg, err := pgxpool.ParseConfig(connString)
	if err != nil {
		return nil, err
	}
	pcfg.MaxConns = poolMaxConns
	pcfg.MaxConnLifetime = poolMaxLifetime
	pcfg.MaxConnIdleTime = poolMaxIdleTime
	pcfg.BeforeConnect = beforeConnect
	return pgxpool.NewWithConfig(ctx, pcfg)
}
