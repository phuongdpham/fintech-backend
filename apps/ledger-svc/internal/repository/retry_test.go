package repository_test

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/stretchr/testify/require"

	"github.com/phuongdpham/fintech/apps/ledger-svc/internal/repository"
)

func newPgError(code string) *pgconn.PgError {
	return &pgconn.PgError{Code: code, Message: "synthetic " + code}
}

func TestIsSerializationFailure(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"nil", nil, false},
		{"plain error", errors.New("boom"), false},
		{"40001 direct", newPgError("40001"), true},
		{"40001 wrapped", fmt.Errorf("at boundary: %w", newPgError("40001")), true},
		{"23505 not retryable", newPgError("23505"), false},
		{"40P01 deadlock not retryable", newPgError("40P01"), false}, // intentionally NOT retried
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			require.Equal(t, tc.want, repository.IsSerializationFailure(tc.err))
		})
	}
}

func TestIsUniqueViolation(t *testing.T) {
	require.True(t, repository.IsUniqueViolation(newPgError("23505")))
	require.True(t, repository.IsUniqueViolation(fmt.Errorf("wrap: %w", newPgError("23505"))))
	require.False(t, repository.IsUniqueViolation(newPgError("40001")))
	require.False(t, repository.IsUniqueViolation(errors.New("plain")))
}

// fastConfig keeps tests sub-millisecond and deterministic.
func fastConfig(maxAttempts int) repository.RetryConfig {
	return repository.RetryConfig{
		MaxAttempts:    maxAttempts,
		InitialBackoff: 100 * time.Microsecond,
		MaxBackoff:     1 * time.Millisecond,
		JitterFraction: 0,
	}
}

func TestWithRetry_RetriesUntilSuccess(t *testing.T) {
	calls := 0
	err := repository.WithRetryOnSerializationFailure(
		context.Background(),
		fastConfig(5),
		func() error {
			calls++
			if calls < 3 {
				return newPgError("40001")
			}
			return nil
		},
	)
	require.NoError(t, err)
	require.Equal(t, 3, calls, "should have stopped retrying after first success")
}

func TestWithRetry_GivesUpAfterMaxAttempts(t *testing.T) {
	calls := 0
	err := repository.WithRetryOnSerializationFailure(
		context.Background(),
		fastConfig(4),
		func() error {
			calls++
			return newPgError("40001")
		},
	)
	require.Error(t, err)
	require.True(t, repository.IsSerializationFailure(err), "final error should still be the 40001")
	require.Equal(t, 4, calls)
}

func TestWithRetry_NonSerializationErrorShortCircuits(t *testing.T) {
	calls := 0
	sentinel := errors.New("not retryable")
	err := repository.WithRetryOnSerializationFailure(
		context.Background(),
		fastConfig(5),
		func() error {
			calls++
			return sentinel
		},
	)
	require.ErrorIs(t, err, sentinel)
	require.Equal(t, 1, calls, "non-40001 errors must not be retried")
}

func TestWithRetry_HonorsContextCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	calls := 0
	cfg := repository.RetryConfig{
		MaxAttempts:    10,
		InitialBackoff: 50 * time.Millisecond,
		MaxBackoff:     50 * time.Millisecond,
		JitterFraction: 0,
	}
	err := repository.WithRetryOnSerializationFailure(ctx, cfg, func() error {
		calls++
		if calls == 1 {
			cancel()
		}
		return newPgError("40001")
	})
	require.ErrorIs(t, err, context.Canceled)
	require.Equal(t, 1, calls, "should bail out on cancellation during backoff")
}

func TestWithRetry_FirstCallNoBackoff(t *testing.T) {
	// Ensures the first attempt isn't gated on a sleep — important for p99
	// latency under no contention.
	start := time.Now()
	_ = repository.WithRetryOnSerializationFailure(
		context.Background(),
		repository.RetryConfig{
			MaxAttempts:    1,
			InitialBackoff: 500 * time.Millisecond,
			MaxBackoff:     500 * time.Millisecond,
		},
		func() error { return nil },
	)
	require.Less(t, time.Since(start), 50*time.Millisecond)
}
