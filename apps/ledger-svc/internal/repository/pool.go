// Package repository contains the Postgres persistence adapters that
// implement the domain.* port interfaces.
//
// Conventions:
//   - All public methods take a context.Context and respect cancellation.
//   - All write paths run through InTx so isolation level + retry policy
//     are uniform across the service.
//   - Driver-specific errors do not leak across the package boundary;
//     methods return domain sentinel errors or wrapped errors at most.
package repository

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// PoolConfig is the minimal set of pgxpool knobs we expose. Defaults are
// applied for any zero-value field. Tuned for OLTP at the plan's 5K TPS
// target — adjust MaxConns based on actual Postgres max_connections (300
// per docker-compose.yml) divided by the number of replicas.
type PoolConfig struct {
	DSN             string
	MaxConns        int32
	MinConns        int32
	MaxConnLifetime time.Duration
	MaxConnIdleTime time.Duration
}

func NewPool(ctx context.Context, cfg PoolConfig) (*pgxpool.Pool, error) {
	if cfg.DSN == "" {
		return nil, fmt.Errorf("repository: pool DSN is required")
	}
	pgxCfg, err := pgxpool.ParseConfig(cfg.DSN)
	if err != nil {
		return nil, fmt.Errorf("repository: parse DSN: %w", err)
	}
	if cfg.MaxConns > 0 {
		pgxCfg.MaxConns = cfg.MaxConns
	}
	if cfg.MinConns > 0 {
		pgxCfg.MinConns = cfg.MinConns
	}
	if cfg.MaxConnLifetime > 0 {
		pgxCfg.MaxConnLifetime = cfg.MaxConnLifetime
	}
	if cfg.MaxConnIdleTime > 0 {
		pgxCfg.MaxConnIdleTime = cfg.MaxConnIdleTime
	}
	pool, err := pgxpool.NewWithConfig(ctx, pgxCfg)
	if err != nil {
		return nil, fmt.Errorf("repository: new pool: %w", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("repository: ping: %w", err)
	}
	return pool, nil
}
