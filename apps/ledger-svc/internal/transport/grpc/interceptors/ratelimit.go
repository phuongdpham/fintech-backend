package interceptors

import (
	"context"
	"log/slog"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"golang.org/x/time/rate"
	gogrpc "google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
)

// TierLimit captures the steady-state rate and burst for one tenant
// tier. Use rate.Inf for "unlimited" (e.g. internal traffic).
type TierLimit struct {
	RPS   rate.Limit
	Burst int
}

// RateLimitConfig configures the per-tenant token-bucket interceptor.
//
// TierByTenant maps tenant id → tier label. Unknown tenants get the
// Default tier. Tier definitions live in Tiers; missing tier → Default.
//
// Eviction trims the limiter map when memory pressure is the bigger
// concern than the per-tenant pacing precision. With 100K idle tenants
// each holding ~200 bytes in a sync.Map, that's ~20MB; cheap. The
// eviction TTL is 5 minutes by default — anything older than that is
// presumed dormant.
type RateLimitConfig struct {
	Tiers        map[string]TierLimit
	TierByTenant map[string]string
	Default      TierLimit
	IdleTTL      time.Duration
	// MetricsRejected, when set, increments on every rejection. The
	// counter is labelled (tenant, method, tier) — bound the tenant
	// label cardinality at the source: tier IDs come from a small
	// closed set, tenants are sharded into tiers, and method is
	// closed by the proto.
	MetricsRejected *prometheus.CounterVec
}

// DefaultRateLimitConfig returns a config sized for a small starter
// service: 100 RPS / 200 burst per tenant, 5-minute idle eviction.
// Operators override via env once production traffic shape is known.
func DefaultRateLimitConfig() RateLimitConfig {
	return RateLimitConfig{
		Tiers: map[string]TierLimit{
			"default": {RPS: 100, Burst: 200},
			"premium": {RPS: 1000, Burst: 2000},
			"internal": {RPS: rate.Inf, Burst: 0},
		},
		Default: TierLimit{RPS: 100, Burst: 200},
		IdleTTL: 5 * time.Minute,
	}
}

// rateLimiterEntry tracks per-tenant limiter + last-seen wall clock.
// lastSeenUnix is atomic so the eviction sweeper can read without
// taking the map's lock.
type rateLimiterEntry struct {
	limiter      *rate.Limiter
	tier         string
	lastSeenUnix atomic.Int64
}

// RateLimit returns a server-side unary interceptor that throttles
// per tenant via token bucket. MUST slot AFTER EdgeIdentity in the
// chain — it reads Claims from context.
//
// Public methods (anonymous health checks, reflection) bypass —
// pass them via the EdgeIdentity public-list, not here. RateLimit
// only sees authenticated traffic.
func RateLimit(cfg RateLimitConfig, log *slog.Logger) gogrpc.UnaryServerInterceptor {
	if cfg.IdleTTL == 0 {
		cfg.IdleTTL = 5 * time.Minute
	}
	if cfg.Tiers == nil {
		cfg.Tiers = map[string]TierLimit{}
	}
	limiters := &sync.Map{} // tenant string → *rateLimiterEntry

	// Background sweeper drops idle entries so the map doesn't grow
	// without bound across very long-lived processes. Cheap — runs
	// every IdleTTL/2 with O(N) scan.
	sweepCtx, cancelSweep := context.WithCancel(context.Background())
	go runEvictionSweeper(sweepCtx, limiters, cfg.IdleTTL)
	_ = cancelSweep // currently unused at shutdown; the goroutine exits
	// when the process exits. Wire to a real shutdown if/when this
	// interceptor needs lifecycle management.

	return func(ctx context.Context, req any, info *gogrpc.UnaryServerInfo, handler gogrpc.UnaryHandler) (any, error) {
		claims := ClaimsFromContext(ctx)
		if claims == nil || claims.Tenant == "" {
			// No authenticated tenant — the EdgeIdentity interceptor
			// should have rejected this already; if we got here, the
			// chain order is wrong. Fail closed.
			return nil, status.Error(codes.Unauthenticated, "ratelimit: no tenant in context")
		}
		entry := getOrCreateLimiter(limiters, claims.Tenant, cfg)
		entry.lastSeenUnix.Store(time.Now().Unix())

		if !entry.limiter.Allow() {
			if cfg.MetricsRejected != nil {
				cfg.MetricsRejected.WithLabelValues(
					claims.Tenant, info.FullMethod, entry.tier,
				).Inc()
			}
			// Hint the caller via trailer — gRPC has no Retry-After
			// header, but a small numeric trailer lets clients with
			// retry middleware honor the throttle.
			retryAfterMs := strconv.FormatInt(int64(time.Second/time.Duration(rateOf(entry.limiter))), 10)
			_ = gogrpc.SetTrailer(ctx, metadata.Pairs("retry-after-ms", retryAfterMs))
			log.Debug("rate limited",
				slog.String("tenant", claims.Tenant),
				slog.String("method", info.FullMethod),
				slog.String("tier", entry.tier),
			)
			return nil, status.Errorf(codes.ResourceExhausted,
				"rate limit exceeded for tenant %s on %s", claims.Tenant, info.FullMethod)
		}
		return handler(ctx, req)
	}
}

func getOrCreateLimiter(m *sync.Map, tenant string, cfg RateLimitConfig) *rateLimiterEntry {
	if v, ok := m.Load(tenant); ok {
		return v.(*rateLimiterEntry)
	}
	tier := "default"
	if t, ok := cfg.TierByTenant[tenant]; ok {
		tier = t
	}
	limit := cfg.Default
	if tl, ok := cfg.Tiers[tier]; ok {
		limit = tl
	}
	entry := &rateLimiterEntry{
		limiter: rate.NewLimiter(limit.RPS, limit.Burst),
		tier:    tier,
	}
	entry.lastSeenUnix.Store(time.Now().Unix())
	actual, _ := m.LoadOrStore(tenant, entry)
	return actual.(*rateLimiterEntry)
}

func runEvictionSweeper(ctx context.Context, limiters *sync.Map, ttl time.Duration) {
	t := time.NewTicker(ttl / 2)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-t.C:
			cutoff := now.Add(-ttl).Unix()
			limiters.Range(func(k, v any) bool {
				e := v.(*rateLimiterEntry)
				if e.lastSeenUnix.Load() < cutoff {
					limiters.Delete(k)
				}
				return true
			})
		}
	}
}

// ParseTierMap parses the env-format string TENANT_TIER_MAP=
// "tenant-a:premium,tenant-b:standard" into a map[tenant]tier.
// Empty entries and malformed pairs are silently skipped — boot
// shouldn't fail just because someone fat-fingered a colon. A
// future stricter validator can warn at startup if the map is
// suspiciously empty.
func ParseTierMap(s string) map[string]string {
	out := map[string]string{}
	if s == "" {
		return out
	}
	for pair := range strings.SplitSeq(s, ",") {
		colon := strings.IndexByte(pair, ':')
		if colon <= 0 || colon == len(pair)-1 {
			continue
		}
		out[strings.TrimSpace(pair[:colon])] = strings.TrimSpace(pair[colon+1:])
	}
	return out
}

// rateOf reads the limiter's effective rate (tokens per second) for
// the retry-after hint. Cheap; no allocation.
func rateOf(l *rate.Limiter) rate.Limit {
	return l.Limit()
}
