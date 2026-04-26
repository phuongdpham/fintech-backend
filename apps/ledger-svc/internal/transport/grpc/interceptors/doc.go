// Package interceptors holds the gRPC unary interceptors composed into
// the server's middleware chain.
//
// # Chain order
//
// Interceptors compose like an onion — the outer-most wraps everything
// below it. The default chain (built by Default in server wiring):
//
//	Recovery   → catches panics in everything below; outer-most so
//	             a panic in any other interceptor still results in a
//	             clean Internal response.
//	RequestID  → ensures every request has a stable correlation id
//	             before logging runs.
//	Logging    → emits start/end events with request_id + status code,
//	             and stashes a per-call slog.Logger in the context so
//	             usecase / handler code can inherit the same fields.
//	Auth       → last before handler. Enforces (or extracts) bearer
//	             token claims, with a per-method public-bypass list.
//
// # Context contract
//
// Downstream code reads request-scoped state via package helpers:
//
//	rid := interceptors.RequestIDFromContext(ctx)
//	log := interceptors.LoggerFromContext(ctx)   // request-id pre-attached
//	cl  := interceptors.ClaimsFromContext(ctx)   // nil on public methods
//
// These are stable across this service's lifetime; downstream call
// sites should depend only on them, not on the interceptors themselves.
package interceptors
