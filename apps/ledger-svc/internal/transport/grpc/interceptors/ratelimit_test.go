package interceptors_test

import (
	"context"
	"io"
	"log/slog"
	"testing"

	"github.com/stretchr/testify/require"
	"golang.org/x/time/rate"
	gogrpc "google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/phuongdpham/fintech/apps/ledger-svc/internal/transport/grpc/interceptors"
)

func silentRLLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelDebug}))
}

func passHandler(_ context.Context, req any) (any, error) { return req, nil }

func ctxWithTenant(tenant string) context.Context {
	return interceptors.WithClaims(context.Background(), &interceptors.Claims{
		Subject: "tester",
		Tenant:  tenant,
	})
}

// TestRateLimit_AllowUnderLimit confirms that requests within the
// per-tenant budget pass through unmodified.
func TestRateLimit_AllowUnderLimit(t *testing.T) {
	cfg := interceptors.RateLimitConfig{
		Tiers:   map[string]interceptors.TierLimit{"default": {RPS: 1000, Burst: 1000}},
		Default: interceptors.TierLimit{RPS: 1000, Burst: 1000},
	}
	intc := interceptors.RateLimit(cfg, silentRLLogger())
	info := &gogrpc.UnaryServerInfo{FullMethod: "/x/M"}

	for range 10 {
		out, err := intc(ctxWithTenant("tenant-a"), "req", info, passHandler)
		require.NoError(t, err)
		require.Equal(t, "req", out)
	}
}

// TestRateLimit_RejectOverBurst confirms RESOURCE_EXHAUSTED once the
// bucket is empty. With Burst=1, the second call within the window is
// rejected.
func TestRateLimit_RejectOverBurst(t *testing.T) {
	cfg := interceptors.RateLimitConfig{
		Tiers:   map[string]interceptors.TierLimit{"default": {RPS: 1, Burst: 1}},
		Default: interceptors.TierLimit{RPS: 1, Burst: 1},
	}
	intc := interceptors.RateLimit(cfg, silentRLLogger())
	info := &gogrpc.UnaryServerInfo{FullMethod: "/x/M"}
	ctx := ctxWithTenant("tenant-a")

	_, err := intc(ctx, "req", info, passHandler)
	require.NoError(t, err)

	_, err = intc(ctx, "req", info, passHandler)
	require.Error(t, err)
	st, ok := status.FromError(err)
	require.True(t, ok)
	require.Equal(t, codes.ResourceExhausted, st.Code())
}

// TestRateLimit_TenantIsolation is the load-bearing test for the
// noisy-neighbor guarantee: tenant A draining its bucket leaves
// tenant B's bucket completely untouched.
func TestRateLimit_TenantIsolation(t *testing.T) {
	cfg := interceptors.RateLimitConfig{
		Tiers:   map[string]interceptors.TierLimit{"default": {RPS: 1, Burst: 1}},
		Default: interceptors.TierLimit{RPS: 1, Burst: 1},
	}
	intc := interceptors.RateLimit(cfg, silentRLLogger())
	info := &gogrpc.UnaryServerInfo{FullMethod: "/x/M"}

	// Tenant A drains its bucket.
	_, err := intc(ctxWithTenant("tenant-a"), "req", info, passHandler)
	require.NoError(t, err)
	_, err = intc(ctxWithTenant("tenant-a"), "req", info, passHandler)
	require.Error(t, err, "tenant-a should be limited after draining")

	// Tenant B should still have a full bucket.
	_, err = intc(ctxWithTenant("tenant-b"), "req", info, passHandler)
	require.NoError(t, err, "tenant-b's bucket must not be affected by tenant-a")
}

// TestRateLimit_TierResolution verifies that explicit tier mapping
// overrides the default. Premium tier with Inf rate never rejects.
func TestRateLimit_TierResolution(t *testing.T) {
	cfg := interceptors.RateLimitConfig{
		Tiers: map[string]interceptors.TierLimit{
			"default": {RPS: 1, Burst: 1},
			"premium": {RPS: rate.Inf, Burst: 0},
		},
		TierByTenant: map[string]string{"vip-tenant": "premium"},
		Default:      interceptors.TierLimit{RPS: 1, Burst: 1},
	}
	intc := interceptors.RateLimit(cfg, silentRLLogger())
	info := &gogrpc.UnaryServerInfo{FullMethod: "/x/M"}
	ctx := ctxWithTenant("vip-tenant")

	for range 100 {
		_, err := intc(ctx, "req", info, passHandler)
		require.NoError(t, err, "premium tier should never reject")
	}
}

// TestRateLimit_RejectsWithoutTenant proves the chain-order safety
// net: if EdgeIdentity didn't run (or didn't set claims), RateLimit
// fails closed with Unauthenticated.
func TestRateLimit_RejectsWithoutTenant(t *testing.T) {
	cfg := interceptors.DefaultRateLimitConfig()
	intc := interceptors.RateLimit(cfg, silentRLLogger())
	info := &gogrpc.UnaryServerInfo{FullMethod: "/x/M"}

	_, err := intc(context.Background(), "req", info, passHandler)
	require.Error(t, err)
	st, _ := status.FromError(err)
	require.Equal(t, codes.Unauthenticated, st.Code())
}

func TestParseTierMap(t *testing.T) {
	cases := []struct {
		in   string
		want map[string]string
	}{
		{"", map[string]string{}},
		{"a:premium", map[string]string{"a": "premium"}},
		{"a:premium,b:standard", map[string]string{"a": "premium", "b": "standard"}},
		{" a : premium , b : standard ", map[string]string{"a": "premium", "b": "standard"}},
		{"malformed,a:premium", map[string]string{"a": "premium"}},
		{":nokey,a:premium", map[string]string{"a": "premium"}},
	}
	for _, tc := range cases {
		t.Run(tc.in, func(t *testing.T) {
			got := interceptors.ParseTierMap(tc.in)
			require.Equal(t, tc.want, got)
		})
	}
}
