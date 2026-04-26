package interceptors

import (
	"context"
	"errors"
	"log/slog"
	"strings"

	gogrpc "google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
)

// Claims is the auth payload propagated through the request context.
// Kept transport-agnostic so non-gRPC entrypoints (cron, queue workers)
// could populate it via WithClaims and reuse downstream authorization
// code.
type Claims struct {
	Subject string
	Email   string
	Tenant  string
	Scopes  []string
	// Raw is the original token string for downstream propagation
	// (e.g. forwarding to another service). Empty if not relevant.
	Raw string
}

// HasScope reports whether the claims include the named scope.
// Linear scan because scope sets are small (< 20).
func (c *Claims) HasScope(s string) bool {
	if c == nil {
		return false
	}
	for _, x := range c.Scopes {
		if x == s {
			return true
		}
	}
	return false
}

type ctxKeyClaims struct{}

// ClaimsFromContext returns the request's auth claims, or nil if the
// request didn't go through Auth (e.g. public method) or auth was
// disabled in this deployment. Handler code MUST nil-check.
func ClaimsFromContext(ctx context.Context) *Claims {
	c, _ := ctx.Value(ctxKeyClaims{}).(*Claims)
	return c
}

// WithClaims is exported for tests and the (rare) handler that needs to
// override claims (e.g. impersonation).
func WithClaims(ctx context.Context, c *Claims) context.Context {
	return context.WithValue(ctx, ctxKeyClaims{}, c)
}

// Verifier validates a bearer token and extracts claims. Implementations
// will typically front a JWKS endpoint (JWT) or an OAuth2 introspection
// endpoint. The interface is small so swapping is a one-line change.
//
// Verifier MUST be safe for concurrent use.
type Verifier interface {
	Verify(ctx context.Context, token string) (*Claims, error)
}

// ErrInvalidToken is the canonical error a Verifier returns when the
// supplied token is syntactically valid but rejected (bad signature,
// expired, wrong issuer, etc.). The interceptor maps this to
// codes.Unauthenticated; other Verifier errors map to codes.Internal
// because they likely indicate a JWKS / network problem.
var ErrInvalidToken = errors.New("auth: invalid token")

// AuthConfig configures the Auth interceptor.
//
// Required:
//   true  — every non-public method requires a valid token; missing or
//           bad token → Unauthenticated.
//   false — token presence is optional; if present and verifiable, claims
//           are populated; if absent or invalid, the request still
//           proceeds with nil claims. Use only in dev / behind a trusted
//           edge proxy that already authenticates.
//
// PublicMethods is the bypass list (full method names like
// "/grpc.health.v1.Health/Check"). Empty map = nothing public.
type AuthConfig struct {
	Verifier      Verifier
	Required      bool
	PublicMethods map[string]struct{}
}

// DefaultPublicMethods covers the standard infra RPCs that should never
// require auth: health checks (k8s probes / load balancers) and gRPC
// reflection (developer tooling like grpcurl).
func DefaultPublicMethods() map[string]struct{} {
	return map[string]struct{}{
		"/grpc.health.v1.Health/Check":                                 {},
		"/grpc.health.v1.Health/Watch":                                 {},
		"/grpc.reflection.v1.ServerReflection/ServerReflectionInfo":    {},
		"/grpc.reflection.v1alpha.ServerReflection/ServerReflectionInfo": {},
	}
}

// Auth is the unary interceptor that gates non-public methods on a valid
// bearer token. See AuthConfig for the required-vs-best-effort policy.
func Auth(cfg AuthConfig) gogrpc.UnaryServerInterceptor {
	public := cfg.PublicMethods
	if public == nil {
		public = map[string]struct{}{}
	}
	return func(ctx context.Context, req any, info *gogrpc.UnaryServerInfo, handler gogrpc.UnaryHandler) (any, error) {
		if _, ok := public[info.FullMethod]; ok {
			return handler(ctx, req)
		}

		token, ok := bearerFromContext(ctx)
		if !ok {
			if cfg.Required {
				return nil, status.Error(codes.Unauthenticated, "missing bearer token")
			}
			return handler(ctx, req)
		}

		if cfg.Verifier == nil {
			if cfg.Required {
				return nil, status.Error(codes.Internal, "auth required but no verifier configured")
			}
			return handler(ctx, req)
		}

		claims, err := cfg.Verifier.Verify(ctx, token)
		if err != nil {
			if errors.Is(err, ErrInvalidToken) {
				return nil, status.Error(codes.Unauthenticated, "invalid token")
			}
			// Unexpected verifier error (network, JWKS down). Fail closed
			// when Required, fail open otherwise.
			if cfg.Required {
				LoggerFromContext(ctx).ErrorContext(ctx, "auth verifier error",
					slog.String("rpc.method", info.FullMethod),
					slog.Any("err", err))
				return nil, status.Error(codes.Internal, "auth verifier unavailable")
			}
			return handler(ctx, req)
		}
		ctx = WithClaims(ctx, claims)

		// Enrich the per-call logger so subsequent log lines include subject.
		if claims != nil && claims.Subject != "" {
			ctx = WithLogger(ctx, LoggerFromContext(ctx).With("auth.sub", claims.Subject))
		}

		return handler(ctx, req)
	}
}

func bearerFromContext(ctx context.Context) (string, bool) {
	md, ok := metadata.FromIncomingContext(ctx)
	if !ok {
		return "", false
	}
	for _, h := range md.Get("authorization") {
		const prefix = "Bearer "
		if len(h) > len(prefix) && strings.EqualFold(h[:len(prefix)], prefix) {
			return strings.TrimSpace(h[len(prefix):]), true
		}
	}
	return "", false
}

// ---------------------------------------------------------------------------
// Verifier implementations
// ---------------------------------------------------------------------------

// DevTokenVerifier accepts a single hardcoded bearer token for local
// development and CI. NEVER use in production — the token is compared as
// a plain string (no signature, no expiry).
type DevTokenVerifier struct {
	Token  string
	Claims Claims
}

func (v *DevTokenVerifier) Verify(_ context.Context, token string) (*Claims, error) {
	if v == nil || v.Token == "" || token != v.Token {
		return nil, ErrInvalidToken
	}
	c := v.Claims
	c.Raw = token
	return &c, nil
}

// rejectAllVerifier is used as the default when AuthConfig.Required=true
// but no Verifier is supplied — fail-closed in production wiring.
type rejectAllVerifier struct{}

func (rejectAllVerifier) Verify(context.Context, string) (*Claims, error) {
	return nil, ErrInvalidToken
}

// RejectAllVerifier returns a Verifier that rejects every token. Used as
// a deliberate "auth required but real verifier not yet wired" placeholder.
func RejectAllVerifier() Verifier { return rejectAllVerifier{} }
