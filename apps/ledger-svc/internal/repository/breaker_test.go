package repository_test

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/phuongdpham/fintech/apps/ledger-svc/internal/repository"
)

// TestBreaker_TripOnFailureRatio walks the closed → open transition.
// MinSamples acts as the lower bound on noise; below it the breaker
// stays closed regardless of ratio.
func TestBreaker_TripOnFailureRatio(t *testing.T) {
	cfg := repository.BreakerConfig{
		FailureRatio: 0.5,
		MinSamples:   4,
		Window:       time.Minute,
		OpenDuration: time.Second,
	}
	b := repository.NewBreaker(cfg)
	key := repository.BreakerKey("tenant-a", "ExecuteTransfer")

	// 3 failures, 0 successes — below MinSamples, stays Closed.
	for range 3 {
		require.True(t, b.Allow(key))
		b.RecordOutcome(key, true)
	}
	require.Equal(t, repository.CircuitClosed, b.State(key))

	// 4th failure crosses MinSamples and 100% ratio → Open.
	require.True(t, b.Allow(key))
	b.RecordOutcome(key, true)
	require.Equal(t, repository.CircuitOpen, b.State(key))

	// While Open, Allow returns false.
	require.False(t, b.Allow(key))
}

// TestBreaker_HalfOpenRecovery verifies the recovery path: after
// OpenDuration, exactly ONE trial is admitted; success closes the
// circuit, subsequent allows pass through.
func TestBreaker_HalfOpenRecovery(t *testing.T) {
	cfg := repository.BreakerConfig{
		FailureRatio: 0.5,
		MinSamples:   2,
		Window:       time.Minute,
		OpenDuration: 100 * time.Millisecond,
	}
	b := repository.NewBreaker(cfg)
	key := repository.BreakerKey("tenant-b", "ExecuteTransfer")

	// Trip the breaker.
	for range 2 {
		require.True(t, b.Allow(key))
		b.RecordOutcome(key, true)
	}
	require.Equal(t, repository.CircuitOpen, b.State(key))

	// Wait through OpenDuration; the next Allow should issue exactly
	// one trial and return true. Concurrent Allows during HalfOpen
	// must be rejected (only one trial in flight).
	time.Sleep(120 * time.Millisecond)

	allowed := 0
	rejected := 0
	for range 5 {
		if b.Allow(key) {
			allowed++
		} else {
			rejected++
		}
	}
	require.Equal(t, 1, allowed, "exactly one half-open trial slot")
	require.Equal(t, 4, rejected)

	// Trial reports success → Closed; subsequent calls flow through.
	b.RecordOutcome(key, false)
	require.Equal(t, repository.CircuitClosed, b.State(key))
	require.True(t, b.Allow(key))
}

// TestBreaker_HalfOpenFailureReopens proves the trial outcome matters:
// if the half-open trial reports cap-reached, the circuit goes back
// to Open with a fresh OpenDuration.
func TestBreaker_HalfOpenFailureReopens(t *testing.T) {
	cfg := repository.BreakerConfig{
		FailureRatio: 0.5,
		MinSamples:   2,
		Window:       time.Minute,
		OpenDuration: 50 * time.Millisecond,
	}
	b := repository.NewBreaker(cfg)
	key := repository.BreakerKey("tenant-c", "ExecuteTransfer")

	for range 2 {
		require.True(t, b.Allow(key))
		b.RecordOutcome(key, true)
	}
	time.Sleep(60 * time.Millisecond)

	require.True(t, b.Allow(key))
	b.RecordOutcome(key, true) // trial fails

	require.Equal(t, repository.CircuitOpen, b.State(key))
	require.False(t, b.Allow(key))
}

// TestBreaker_PerKeyIsolation: tripping tenant A's circuit does not
// affect tenant B. The whole point of per-key state.
func TestBreaker_PerKeyIsolation(t *testing.T) {
	cfg := repository.BreakerConfig{
		FailureRatio: 0.5,
		MinSamples:   2,
		Window:       time.Minute,
		OpenDuration: time.Second,
	}
	b := repository.NewBreaker(cfg)

	a := repository.BreakerKey("tenant-a", "ExecuteTransfer")
	otherTenant := repository.BreakerKey("tenant-b", "ExecuteTransfer")

	for range 2 {
		require.True(t, b.Allow(a))
		b.RecordOutcome(a, true)
	}
	require.Equal(t, repository.CircuitOpen, b.State(a))

	require.True(t, b.Allow(otherTenant), "tenant-b should be unaffected by tenant-a's outage")
}
