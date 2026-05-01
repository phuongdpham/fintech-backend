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

// ErrAcquireTimeout is the sentinel returned when AcquireConn /
// BeginTxWithAcquire wait past PoolConfig.AcquireTimeout without
// getting a connection. The transport layer maps it to gRPC
// ResourceExhausted so retry middleware can behave correctly. Don't
// compare with errors.Is on context.DeadlineExceeded — the wrapper
// distinguishes "client gave up" from "we ran out of pool slots."
var ErrAcquireTimeout = errors.New("repository: pgxpool acquire timeout")

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
	case errors.Is(err, context.DeadlineExceeded) && cfg.AcquireTimeout > 0 && elapsed >= cfg.AcquireTimeout:
		// Distinguish "we hit AcquireTimeout" from "caller's ctx fired."
		// Only the former is a pool-saturation signal; the latter is
		// the client giving up and propagates as context.DeadlineExceeded.
		metrics.ObserveAcquire(elapsed, "timeout")
		return nil, fmt.Errorf("%w (after %s)", ErrAcquireTimeout, cfg.AcquireTimeout)
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
// observed) then begins a transaction on it.
//
// Returns a wrapper tx that releases the underlying *pgxpool.Conn to
// the pool on Commit or Rollback. This wrapper is necessary because
// *pgxpool.Conn.BeginTx returns the raw pgx.Tx (from the inner
// *pgx.Conn), NOT a *pgxpool.Tx — so the raw tx's Commit/Rollback do
// not know about the pool and will not release. Only pool.BeginTx
// returns an auto-releasing tx, but that path doesn't let us observe
// the acquire-wait metric independently. So we wrap.
//
// Caller MUST call tx.Commit or tx.Rollback exactly once.
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
	rawTx, err := conn.BeginTx(ctx, opts)
	if err != nil {
		conn.Release()
		return nil, err
	}
	return &releasingTx{Tx: rawTx, conn: conn}, nil
}

// releasingTx wraps a pgx.Tx returned by *pgxpool.Conn.BeginTx and
// releases the *pgxpool.Conn back to the pool when the tx terminates.
// All other pgx.Tx methods are inherited via embedding.
//
// Why this exists: *pgxpool.Conn.BeginTx is documented as starting a
// tx "from the conn" but the returned Tx has no link back to the
// *pgxpool.Conn — so without this wrapper, Commit/Rollback would
// release the underlying network resource at PG's level but leave the
// pool slot marked acquired forever. That's the leak.
type releasingTx struct {
	pgx.Tx
	conn     *pgxpool.Conn
	released bool
}

func (t *releasingTx) Commit(ctx context.Context) error {
	err := t.Tx.Commit(ctx)
	t.release()
	return err
}

func (t *releasingTx) Rollback(ctx context.Context) error {
	err := t.Tx.Rollback(ctx)
	t.release()
	return err
}

// release is idempotent so a deferred Rollback after a successful
// Commit (the standard pattern) doesn't double-release.
func (t *releasingTx) release() {
	if t.released {
		return
	}
	t.released = true
	t.conn.Release()
}
