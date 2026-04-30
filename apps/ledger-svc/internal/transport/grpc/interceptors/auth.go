package interceptors

import (
	"context"
	"log/slog"

	gogrpc "google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
)

// Claims is the per-request actor identity propagated through context.
// In this architecture the BFF is the trust boundary: it validates the
// caller's JWT, looks up tenancy, and forwards the resolved identity to
// internal services as plain headers. ledger-svc does not validate
// tokens — it trusts the headers because BFF is the only thing that
// reaches it (network policy / mTLS at deploy time enforces that).
//
// Authorization (which scopes a caller has) lives at the BFF too. If
// BFF lets the request through, ledger-svc executes it.
type Claims struct {
	// Subject is the authenticated principal (typically a user UUID or
	// JWT sub claim). Required.
	Subject string
	// Tenant scopes every domain operation. Required.
	Tenant string
	// Session is the BFF-issued session identifier. Empty when the
	// caller is not in a session-bearing flow (e.g. M2M).
	Session string
}

// Header keys for the BFF → ledger-svc identity protocol. Lower-case
// per gRPC metadata convention.
const (
	HeaderTenantID      = "x-tenant-id"
	HeaderActorSubject  = "x-actor-subject"
	HeaderActorSession  = "x-actor-session"
)

type ctxKeyClaims struct{}

// ClaimsFromContext returns the actor claims installed by EdgeIdentity,
// or nil for public methods that bypass the interceptor. Handler code
// MUST nil-check.
func ClaimsFromContext(ctx context.Context) *Claims {
	c, _ := ctx.Value(ctxKeyClaims{}).(*Claims)
	return c
}

// WithClaims is exposed for tests and the rare handler that needs to
// override claims (e.g. impersonation flows).
func WithClaims(ctx context.Context, c *Claims) context.Context {
	return context.WithValue(ctx, ctxKeyClaims{}, c)
}

// EdgeIdentityConfig configures the trusted-edge interceptor.
//
// PublicMethods is the bypass list (full method names like
// "/grpc.health.v1.Health/Check"). Empty map = nothing public.
type EdgeIdentityConfig struct {
	PublicMethods map[string]struct{}
}

// DefaultPublicMethods covers the standard infra RPCs that should never
// require an actor identity: health checks (k8s probes / load balancers)
// and gRPC reflection (developer tooling like grpcurl).
func DefaultPublicMethods() map[string]struct{} {
	return map[string]struct{}{
		"/grpc.health.v1.Health/Check":                                   {},
		"/grpc.health.v1.Health/Watch":                                   {},
		"/grpc.reflection.v1.ServerReflection/ServerReflectionInfo":      {},
		"/grpc.reflection.v1alpha.ServerReflection/ServerReflectionInfo": {},
	}
}

// EdgeIdentity is the unary interceptor that extracts the BFF-supplied
// actor headers and installs them on context. Missing required headers
// produce Unauthenticated; an empty tenant or subject is treated the
// same as a missing header to defend against BFF bugs.
func EdgeIdentity(cfg EdgeIdentityConfig) gogrpc.UnaryServerInterceptor {
	public := cfg.PublicMethods
	if public == nil {
		public = map[string]struct{}{}
	}
	return func(ctx context.Context, req any, info *gogrpc.UnaryServerInfo, handler gogrpc.UnaryHandler) (any, error) {
		if _, ok := public[info.FullMethod]; ok {
			return handler(ctx, req)
		}

		md, ok := metadata.FromIncomingContext(ctx)
		if !ok {
			return nil, status.Error(codes.Unauthenticated, "missing edge identity headers")
		}
		tenant := firstHeader(md, HeaderTenantID)
		subject := firstHeader(md, HeaderActorSubject)
		if tenant == "" || subject == "" {
			return nil, status.Error(codes.Unauthenticated, "missing edge identity headers")
		}
		c := &Claims{
			Subject: subject,
			Tenant:  tenant,
			Session: firstHeader(md, HeaderActorSession),
		}
		ctx = WithClaims(ctx, c)

		// Enrich the per-call logger so subsequent log lines include the
		// actor + tenant. Cheap; pays back the first time someone greps.
		ctx = WithLogger(ctx, LoggerFromContext(ctx).With(
			slog.String("auth.sub", c.Subject),
			slog.String("auth.tenant", c.Tenant),
		))

		return handler(ctx, req)
	}
}

func firstHeader(md metadata.MD, key string) string {
	v := md.Get(key)
	if len(v) == 0 {
		return ""
	}
	return v[0]
}
