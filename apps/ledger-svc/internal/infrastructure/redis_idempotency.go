// Package infrastructure contains adapters for non-DB external systems:
// Redis (idempotency cache), Kafka (event producer), and telemetry.
//
// As with internal/repository, driver-specific errors do not leak — adapters
// translate to domain sentinel errors or wrapped errors at the boundary.
package infrastructure

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/phuongdpham/fintech/apps/ledger-svc/internal/domain"
)

// IdempotencyTerminalTTL is the wall-clock window during which Redis
// answers "duplicate" once the request has terminated (COMPLETED or
// FAILED). 24h absorbs client retries that span a typical incident
// without bloating Redis.
//
// IdempotencyStartedTTL is the much shorter window applied while the
// request is in flight (STARTED). The split exists to unstick a known
// failure mode: if SetState(FAILED) itself fails to reach Redis after
// a hard repo error, the slot would otherwise stay STARTED for 24h and
// every retry would return ErrTransferInFlight. Five minutes bounds
// that lockout to a single circuit-breaker window. PostgreSQL's
// composite UNIQUE on (tenant_id, idempotency_key) remains the durable
// barrier past either TTL.
const (
	IdempotencyTerminalTTL = 24 * time.Hour
	IdempotencyStartedTTL  = 5 * time.Minute
)

// RedisIdempotencyStore implements domain.IdempotencyStore on top of
// go-redis/v9. Uses SETNX (SET ... NX ... EX) for the atomic acquire,
// then GET to surface the current state on collision.
type RedisIdempotencyStore struct {
	client      redis.UniversalClient
	prefix      string
	startedTTL  time.Duration
	terminalTTL time.Duration
}

func NewRedisIdempotencyStore(client redis.UniversalClient) *RedisIdempotencyStore {
	return &RedisIdempotencyStore{
		client:      client,
		prefix:      "ledger:idem:",
		startedTTL:  IdempotencyStartedTTL,
		terminalTTL: IdempotencyTerminalTTL,
	}
}

// WithPrefix returns a copy of s using the given key prefix. Useful for
// multi-tenant deployments sharing one Redis cluster.
func (s *RedisIdempotencyStore) WithPrefix(prefix string) *RedisIdempotencyStore {
	cp := *s
	cp.prefix = prefix
	return &cp
}

// WithTTLs returns a copy of s using the given STARTED and terminal
// TTLs. Tests use small values to keep eviction deterministic.
func (s *RedisIdempotencyStore) WithTTLs(startedTTL, terminalTTL time.Duration) *RedisIdempotencyStore {
	cp := *s
	cp.startedTTL = startedTTL
	cp.terminalTTL = terminalTTL
	return &cp
}

func (s *RedisIdempotencyStore) key(k string) string { return s.prefix + k }

// recordSep separates the state and fingerprint inside the Redis value.
// State strings are fixed-vocabulary (STARTED|COMPLETED|FAILED) and the
// fingerprint is hex (no ':'), so a single split is unambiguous.
const recordSep = ":"

func encodeRecord(rec domain.IdempotencyRecord) string {
	return string(rec.State) + recordSep + rec.Fingerprint
}

func decodeRecord(raw string) domain.IdempotencyRecord {
	state, fp, ok := strings.Cut(raw, recordSep)
	if !ok {
		// Legacy value (state-only). Treat fingerprint as empty so the
		// usecase falls through to PG, which remains the source of truth.
		return domain.IdempotencyRecord{State: domain.IdempotencyState(raw)}
	}
	return domain.IdempotencyRecord{
		State:       domain.IdempotencyState(state),
		Fingerprint: fp,
	}
}

// Acquire attempts an atomic SETNX with the given fingerprint.
//
// Contract:
//
//	(true,  zero,      nil)  caller owns the slot; proceed
//	(false, existing,  nil)  duplicate; caller compares fingerprints and
//	                         either replays (match) or rejects with
//	                         ErrRequestFingerprintMismatch (no match)
//	(false, zero,      nil)  race: SETNX failed but TTL expired before GET.
//	                         Conservative reading is "duplicate". Caller
//	                         falls through to Postgres.
//	(false, zero,      err)  Redis fault. Caller fails open to PG; the
//	                         composite UNIQUE plus the durable
//	                         request_fingerprint column catch duplicates
//	                         and mismatched bodies respectively.
func (s *RedisIdempotencyStore) Acquire(ctx context.Context, key, fingerprint string) (bool, domain.IdempotencyRecord, error) {
	if key == "" {
		return false, domain.IdempotencyRecord{}, fmt.Errorf("redis idempotency: key is required")
	}
	value := encodeRecord(domain.IdempotencyRecord{
		State:       domain.IdempotencyStarted,
		Fingerprint: fingerprint,
	})
	ok, err := s.client.SetNX(ctx, s.key(key), value, s.startedTTL).Result()
	if err != nil {
		return false, domain.IdempotencyRecord{}, fmt.Errorf("redis idempotency: SETNX: %w", err)
	}
	if ok {
		return true, domain.IdempotencyRecord{}, nil
	}
	cur, err := s.client.Get(ctx, s.key(key)).Result()
	if errors.Is(err, redis.Nil) {
		return false, domain.IdempotencyRecord{}, nil
	}
	if err != nil {
		return false, domain.IdempotencyRecord{}, fmt.Errorf("redis idempotency: GET: %w", err)
	}
	return false, decodeRecord(cur), nil
}

// SetState writes the new state preserving the fingerprint. STARTED reuses
// the short startedTTL; terminal states (COMPLETED, FAILED) refresh to the
// long terminalTTL, keeping the duplicate-detection window open for the
// full 24h on settled flows.
func (s *RedisIdempotencyStore) SetState(ctx context.Context, key, fingerprint string, state domain.IdempotencyState) error {
	if key == "" {
		return fmt.Errorf("redis idempotency: key is required")
	}
	var ttl time.Duration
	switch state {
	case domain.IdempotencyStarted:
		ttl = s.startedTTL
	case domain.IdempotencyCompleted, domain.IdempotencyFailed:
		ttl = s.terminalTTL
	default:
		return fmt.Errorf("redis idempotency: invalid state %q", state)
	}
	value := encodeRecord(domain.IdempotencyRecord{State: state, Fingerprint: fingerprint})
	if err := s.client.Set(ctx, s.key(key), value, ttl).Err(); err != nil {
		return fmt.Errorf("redis idempotency: SET state: %w", err)
	}
	return nil
}
