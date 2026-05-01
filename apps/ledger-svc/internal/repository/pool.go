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
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/prometheus/client_golang/prometheus"
)

// PoolConfig is the minimal set of pgxpool knobs we expose. Defaults
// are applied for any zero-value field.
//
// Sizing rule of thumb for the OLTP write path:
//
//	MaxConns ≥ TPS × p99_tx_seconds × safety_factor(2)
//
// At 10K TPS with 5ms p99 tx, that's 100 active connections; default
// 120 leaves headroom. PG's max_connections must accommodate the sum
// across replicas + outbox workers.
type PoolConfig struct {
	DSN             string
	MaxConns        int32
	MinConns        int32
	MaxConnLifetime time.Duration
	MaxConnIdleTime time.Duration

	// AcquireTimeout caps the per-Acquire wait. When the pool is full
	// and no connection becomes available within this window, the
	// caller's request fails fast instead of stretching p99 latency
	// silently. Sized so it's strictly less than the typical client
	// deadline — failing here is preferable to consuming the deadline
	// in a queue.
	AcquireTimeout time.Duration
}

// NewPool builds a *pgxpool.Pool with the configured sizing. Caller is
// responsible for closing it on shutdown.
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

// AcquireMetrics is the subset of observability.Metrics this package
// needs. Defining it here keeps the import graph one-way — repository
// has no dependency on the broader observability package.
type AcquireMetrics interface {
	ObserveAcquire(latency time.Duration, outcome string)
}

// PromAcquireMetrics adapts a *prometheus.HistogramVec into the
// AcquireMetrics interface. Lives next to the consumer; keeps Prometheus
// types out of the public package surface.
type PromAcquireMetrics struct {
	Wait *prometheus.HistogramVec
}

func (p PromAcquireMetrics) ObserveAcquire(latency time.Duration, outcome string) {
	if p.Wait == nil {
		return
	}
	p.Wait.WithLabelValues(outcome).Observe(latency.Seconds())
}

// nopAcquireMetrics is the zero-value fallback when no metrics adapter
// is wired. Lets tests construct repos without touching Prometheus.
type nopAcquireMetrics struct{}

func (nopAcquireMetrics) ObserveAcquire(time.Duration, string) {}

// AcquireConn wraps pool.Acquire with a per-call latency histogram and
// a configurable acquire timeout. Outcomes:
//
//	ok       — connection returned
//	timeout  — AcquireTimeout elapsed before a connection was free
//	err      — driver error (network, auth, etc.)
//
// The timeout is enforced via context.WithTimeout layered on top of the
// caller's ctx. Caller's deadline still wins if it's shorter.
func AcquireConn(
	ctx context.Context,
	pool *pgxpool.Pool,
	cfg PoolConfig,
	metrics AcquireMetrics,
) (*pgxpool.Conn, error) {
	if metrics == nil {
		metrics = nopAcquireMetrics{}
	}
	acquireCtx := ctx
	var cancel context.CancelFunc
	if cfg.AcquireTimeout > 0 {
		acquireCtx, cancel = context.WithTimeout(ctx, cfg.AcquireTimeout)
		defer cancel()
	}
	start := time.Now()
	conn, err := pool.Acquire(acquireCtx)
	elapsed := time.Since(start)
	switch {
	case err == nil:
		metrics.ObserveAcquire(elapsed, "ok")
		return conn, nil
	case errors.Is(err, context.DeadlineExceeded) && cfg.AcquireTimeout > 0:
		metrics.ObserveAcquire(elapsed, "timeout")
		return nil, fmt.Errorf("repository: acquire timeout after %s: %w", cfg.AcquireTimeout, err)
	default:
		metrics.ObserveAcquire(elapsed, "err")
		return nil, err
	}
}

// ExportPoolStats periodically scrapes pool.Stat() into the supplied
// gauges. Returns a stop func; safe to call after ctx is done.
//
// Why a periodic exporter instead of a Prometheus Collector that reads
// pool.Stat() on demand: a Collector would couple the metrics package to
// pgxpool. Reading every 2s into pre-registered gauges keeps the import
// graph one-way (repository → metrics) and the cost negligible.
func ExportPoolStats(ctx context.Context, pool *pgxpool.Pool, acquired, idle prometheus.Gauge) func() {
	stopCtx, cancel := context.WithCancel(ctx)
	go func() {
		t := time.NewTicker(2 * time.Second)
		defer t.Stop()
		for {
			select {
			case <-stopCtx.Done():
				return
			case <-t.C:
				s := pool.Stat()
				acquired.Set(float64(s.AcquiredConns()))
				idle.Set(float64(s.IdleConns()))
			}
		}
	}()
	return cancel
}

// BeginTxWithAcquire is the metric-aware equivalent of pool.BeginTx —
// it acquires a connection through AcquireConn (so wait time gets
// observed) then begins a transaction on it. Caller MUST call tx.Commit
// or tx.Rollback; releasing the conn is automatic on tx termination.
func BeginTxWithAcquire(
	ctx context.Context,
	pool *pgxpool.Pool,
	cfg PoolConfig,
	metrics AcquireMetrics,
	opts pgx.TxOptions,
) (pgx.Tx, error) {
	conn, err := AcquireConn(ctx, pool, cfg, metrics)
	if err != nil {
		return nil, err
	}
	tx, err := conn.BeginTx(ctx, opts)
	if err != nil {
		conn.Release()
		return nil, err
	}
	return tx, nil
}
