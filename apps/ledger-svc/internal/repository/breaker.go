package repository

import (
	"sync"
	"sync/atomic"
	"time"

	"github.com/prometheus/client_golang/prometheus"
)

// CircuitState describes the breaker phase for a given key.
type CircuitState int32

const (
	CircuitClosed   CircuitState = 0 // requests pass, failures counted
	CircuitHalfOpen CircuitState = 1 // single trial allowed
	CircuitOpen     CircuitState = 2 // requests rejected fast
)

// BreakerConfig governs the trip threshold + cooldown.
//
// FailureRatio: trips Open when (cap-reached failures / total) exceeds
// this in the rolling Window. 0.05 (5%) is the AC default.
//
// MinSamples: ratio is meaningless until N samples have accumulated.
// Below this, the breaker stays Closed regardless of failure rate.
//
// Window: rolling counter window. Anything older is discarded on the
// next sample. Simple restart-window — not a sliding histogram. At
// 10K TPS this still fires within seconds of a real meltdown; the
// imprecision matters less than the overhead of a sliding implementation.
//
// OpenDuration: how long Open stays open before transitioning to
// HalfOpen. The half-open trial then admits exactly one request;
// success closes the circuit, failure re-opens for another OpenDuration.
//
// MetricsState / MetricsOutcomes: optional; nil = skip.
type BreakerConfig struct {
	FailureRatio    float64
	MinSamples      int64
	Window          time.Duration
	OpenDuration    time.Duration
	MetricsState    *prometheus.GaugeVec   // labels: tenant, method
	MetricsOutcomes *prometheus.CounterVec // labels: tenant, method, outcome
}

// DefaultBreakerConfig matches Issue 7's AC numbers.
func DefaultBreakerConfig() BreakerConfig {
	return BreakerConfig{
		FailureRatio: 0.05,
		MinSamples:   20,
		Window:       30 * time.Second,
		OpenDuration: 1 * time.Second,
	}
}

// Breaker tracks per-key (tenant|method) circuit state. Safe for
// concurrent use across goroutines.
type Breaker struct {
	cfg   BreakerConfig
	keys  sync.Map // key string → *breakerEntry
	now   func() time.Time
}

func NewBreaker(cfg BreakerConfig) *Breaker {
	if cfg.Window == 0 {
		cfg = DefaultBreakerConfig()
	}
	return &Breaker{cfg: cfg, now: func() time.Time { return time.Now() }}
}

// Allow reports whether the key's circuit will admit a request now.
// In HalfOpen, the FIRST allow returns true and bumps state to "trial
// outstanding"; subsequent calls return false until the trial reports
// outcome via RecordOutcome.
func (b *Breaker) Allow(key string) bool {
	e := b.entry(key)
	now := b.now()
	state := CircuitState(e.state.Load())
	switch state {
	case CircuitClosed:
		return true
	case CircuitOpen:
		// Time to flip to HalfOpen?
		if now.UnixNano() >= e.openedAtNanos.Load()+int64(b.cfg.OpenDuration) {
			if e.state.CompareAndSwap(int32(CircuitOpen), int32(CircuitHalfOpen)) {
				e.trialIssued.Store(false)
				b.observeState(key, CircuitHalfOpen)
			}
			return b.tryHalfOpenTrial(e)
		}
		return false
	case CircuitHalfOpen:
		return b.tryHalfOpenTrial(e)
	}
	return true
}

// tryHalfOpenTrial is the CAS that exclusively grants the half-open
// trial slot. Concurrent Allow calls all see HalfOpen; only one wins
// the CAS and gets to issue the trial. The rest are rejected to keep
// the trial isolated.
func (b *Breaker) tryHalfOpenTrial(e *breakerEntry) bool {
	return e.trialIssued.CompareAndSwap(false, true)
}

// RecordOutcome reports whether the operation hit the retry cap
// (capReached=true) or completed within the allowed retry budget
// (capReached=false; either success or non-retryable error).
//
// In Closed: increments counters; trips Open if the failure ratio
// crosses the threshold and we have enough samples.
//
// In HalfOpen: capReached=true reverts to Open with a fresh
// OpenDuration; capReached=false closes the circuit.
func (b *Breaker) RecordOutcome(key string, capReached bool) {
	e := b.entry(key)
	now := b.now()
	if b.cfg.MetricsOutcomes != nil {
		outcome := "ok"
		if capReached {
			outcome = "cap_reached"
		}
		tenant, method := splitKey(key)
		b.cfg.MetricsOutcomes.WithLabelValues(tenant, method, outcome).Inc()
	}

	state := CircuitState(e.state.Load())

	if state == CircuitHalfOpen {
		if capReached {
			e.openedAtNanos.Store(now.UnixNano())
			e.state.Store(int32(CircuitOpen))
			e.trialIssued.Store(false)
			b.observeState(key, CircuitOpen)
		} else {
			e.resetCounters(now)
			e.state.Store(int32(CircuitClosed))
			e.trialIssued.Store(false)
			b.observeState(key, CircuitClosed)
		}
		return
	}

	// Closed (or Open during a stale call — ignore Open since the
	// state should have been refreshed by Allow). Update counters.
	e.maybeReset(now, b.cfg.Window)
	e.total.Add(1)
	if capReached {
		e.failures.Add(1)
	}

	total := e.total.Load()
	failures := e.failures.Load()
	if total < b.cfg.MinSamples {
		return
	}
	ratio := float64(failures) / float64(total)
	if ratio >= b.cfg.FailureRatio && state == CircuitClosed {
		// Trip — capture timestamp and flip state.
		e.openedAtNanos.Store(now.UnixNano())
		if e.state.CompareAndSwap(int32(CircuitClosed), int32(CircuitOpen)) {
			b.observeState(key, CircuitOpen)
		}
	}
}

// State returns the current state. Useful in tests; production code
// just calls Allow.
func (b *Breaker) State(key string) CircuitState {
	e := b.entry(key)
	return CircuitState(e.state.Load())
}

func (b *Breaker) entry(key string) *breakerEntry {
	if v, ok := b.keys.Load(key); ok {
		return v.(*breakerEntry)
	}
	e := &breakerEntry{}
	e.windowStartNanos.Store(b.now().UnixNano())
	actual, _ := b.keys.LoadOrStore(key, e)
	return actual.(*breakerEntry)
}

func (b *Breaker) observeState(key string, s CircuitState) {
	if b.cfg.MetricsState == nil {
		return
	}
	tenant, method := splitKey(key)
	b.cfg.MetricsState.WithLabelValues(tenant, method).Set(float64(s))
}

type breakerEntry struct {
	state            atomic.Int32
	windowStartNanos atomic.Int64
	total            atomic.Int64
	failures         atomic.Int64
	openedAtNanos    atomic.Int64
	trialIssued      atomic.Bool
}

// maybeReset rolls the counters when the current window has expired.
// CAS on windowStartNanos ensures only one goroutine resets even
// under concurrent calls.
func (e *breakerEntry) maybeReset(now time.Time, window time.Duration) {
	start := e.windowStartNanos.Load()
	if now.UnixNano()-start < int64(window) {
		return
	}
	if e.windowStartNanos.CompareAndSwap(start, now.UnixNano()) {
		e.total.Store(0)
		e.failures.Store(0)
	}
}

func (e *breakerEntry) resetCounters(now time.Time) {
	e.windowStartNanos.Store(now.UnixNano())
	e.total.Store(0)
	e.failures.Store(0)
}

// BreakerKey assembles the canonical key from tenant + method. Kept
// here so other packages don't reinvent the format and risk drift
// (which would split the per-key state across two slots).
func BreakerKey(tenant, method string) string {
	return tenant + "|" + method
}

func splitKey(key string) (tenant, method string) {
	for i := range len(key) {
		if key[i] == '|' {
			return key[:i], key[i+1:]
		}
	}
	return key, ""
}
