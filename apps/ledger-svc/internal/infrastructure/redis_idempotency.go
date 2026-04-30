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

// Acquire attempts an atomic SETNX with TTL.
//
// Contract:
//
//	(true,  "",        nil)  caller owns the slot; proceed
//	(false, currState, nil)  duplicate; honor the existing in-flight or
//	                         completed request (caller looks up Postgres
//	                         to return the original response)
//	(false, "",        nil)  race: SETNX failed but TTL expired before GET.
//	                         Conservative reading is "duplicate". Caller
//	                         falls through to Postgres which is the source
//	                         of truth.
//	(false, "",        err)  Redis fault. Caller decides: fail-closed (reject
//	                         the request) or fail-open (let Postgres' UNIQUE
//	                         constraint catch duplicates). The plan's
//	                         architecture favors fail-open here because PG
//	                         remains durable, but the policy lives in the
//	                         usecase, not here.
func (s *RedisIdempotencyStore) Acquire(ctx context.Context, key string) (bool, domain.IdempotencyState, error) {
	if key == "" {
		return false, "", fmt.Errorf("redis idempotency: key is required")
	}
	ok, err := s.client.SetNX(ctx, s.key(key), string(domain.IdempotencyStarted), s.startedTTL).Result()
	if err != nil {
		return false, "", fmt.Errorf("redis idempotency: SETNX: %w", err)
	}
	if ok {
		return true, "", nil
	}
	cur, err := s.client.Get(ctx, s.key(key)).Result()
	if errors.Is(err, redis.Nil) {
		// TTL expired between SETNX and GET. Stay conservative: report
		// duplicate-with-no-state and let the caller resolve via Postgres.
		return false, "", nil
	}
	if err != nil {
		return false, "", fmt.Errorf("redis idempotency: GET: %w", err)
	}
	state := domain.IdempotencyState(cur)
	return false, state, nil
}

// SetState writes the new state. STARTED reuses the short startedTTL;
// terminal states (COMPLETED, FAILED) refresh to the long terminalTTL,
// keeping the duplicate-detection window open for the full 24h on
// settled flows.
func (s *RedisIdempotencyStore) SetState(ctx context.Context, key string, state domain.IdempotencyState) error {
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
	if err := s.client.Set(ctx, s.key(key), string(state), ttl).Err(); err != nil {
		return fmt.Errorf("redis idempotency: SET state: %w", err)
	}
	return nil
}
