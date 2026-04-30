package repository

import (
	"context"
	"errors"
	"math/rand/v2"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
)

// PostgreSQL SQLSTATE codes we react to.
//
// 40001 (serialization_failure) is what SERIALIZABLE isolation raises when
// the engine can't produce a serial schedule — most often a write-skew or
// read-write conflict. Retrying the whole transaction is the documented
// remediation.
//
// 23505 (unique_violation) is surfaced upward as a domain error
// (idempotency-key collision); never retried.
//
// 23503 (foreign_key_violation) is the schema-level rejection of a journal
// entry pointing at an account in a different tenant (or a tx/leg tenant
// mismatch). Surfaced as ErrAccountTenantMismatch.
const (
	pgSerializationFailureCode = "40001"
	pgUniqueViolationCode      = "23505"
	pgForeignKeyViolationCode  = "23503"
)

// RetryConfig governs the exponential-backoff loop. Defaults aim for low
// p99 latency at modest contention: ~5ms first retry, capped at 200ms,
// 5 attempts (= worst case ~ 5ms+10ms+20ms+40ms ≈ 75ms before giving up,
// pre-jitter). Tune via DI when conflict rates rise.
type RetryConfig struct {
	MaxAttempts    int
	InitialBackoff time.Duration
	MaxBackoff     time.Duration
	JitterFraction float64 // 0.25 = ±25% full-jitter band
}

func DefaultRetryConfig() RetryConfig {
	return RetryConfig{
		MaxAttempts:    5,
		InitialBackoff: 5 * time.Millisecond,
		MaxBackoff:     200 * time.Millisecond,
		JitterFraction: 0.25,
	}
}

// IsSerializationFailure reports whether err originated as a Postgres
// SQLSTATE 40001. Uses errors.As so wrapped errors still match.
func IsSerializationFailure(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == pgSerializationFailureCode
}

// IsUniqueViolation reports whether err originated as a Postgres
// SQLSTATE 23505 (unique constraint violation).
func IsUniqueViolation(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == pgUniqueViolationCode
}

// IsForeignKeyViolation reports whether err originated as a Postgres
// SQLSTATE 23503 (foreign-key violation).
func IsForeignKeyViolation(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == pgForeignKeyViolationCode
}

// WithRetryOnSerializationFailure invokes fn, retrying with exponential
// backoff + jitter when fn returns a SQLSTATE 40001 error. Any other error
// short-circuits and is returned immediately. Honors ctx cancellation
// during the backoff sleep.
//
// fn MUST be idempotent across retries — no externally-visible side effects
// outside the DB transaction it owns.
func WithRetryOnSerializationFailure(ctx context.Context, cfg RetryConfig, fn func() error) error {
	if cfg.MaxAttempts <= 0 {
		cfg = DefaultRetryConfig()
	}
	var err error
	backoff := cfg.InitialBackoff
	for attempt := 0; attempt < cfg.MaxAttempts; attempt++ {
		err = fn()
		if err == nil {
			return nil
		}
		if !IsSerializationFailure(err) {
			return err
		}
		if attempt == cfg.MaxAttempts-1 {
			break
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(jitter(backoff, cfg.JitterFraction)):
		}
		backoff *= 2
		if backoff > cfg.MaxBackoff {
			backoff = cfg.MaxBackoff
		}
	}
	return err
}

func jitter(d time.Duration, frac float64) time.Duration {
	if frac <= 0 {
		return d
	}
	delta := float64(d) * frac
	// Symmetric jitter band: [d-delta, d+delta]
	return d + time.Duration((rand.Float64()*2-1)*delta)
}
