package interceptors

import (
	"context"

	"github.com/google/uuid"
	gogrpc "google.golang.org/grpc"
	"google.golang.org/grpc/metadata"
)

// RequestIDMetadataKey is the gRPC metadata header used to propagate the
// request id across the service boundary. Lower-case per the HTTP/2
// header convention; the same key is exposed back to clients via the
// response header so they can correlate logs.
const RequestIDMetadataKey = "x-request-id"

type ctxKeyRequestID struct{}

// RequestIDFromContext returns the per-request id installed by the
// RequestID interceptor. Returns "" if the interceptor wasn't in the
// chain — callers should treat empty as "not available" and not panic.
func RequestIDFromContext(ctx context.Context) string {
	v, _ := ctx.Value(ctxKeyRequestID{}).(string)
	return v
}

// WithRequestID is exposed for tests and edge cases (e.g. a pubsub
// handler outside the gRPC chain that wants to participate in the same
// correlation scheme). Production gRPC code should rely on the
// interceptor; don't call this from handler code.
func WithRequestID(ctx context.Context, id string) context.Context {
	return context.WithValue(ctx, ctxKeyRequestID{}, id)
}

// RequestID extracts an inbound x-request-id metadata value (preferring
// caller-supplied for end-to-end traceability) or generates a fresh
// UUIDv7. v7 is time-ordered, so request ids sort the same way they
// were minted — useful when grepping logs in time-range queries. The
// id is then:
//   1. Stashed in the context for downstream interceptors / handlers.
//   2. Echoed back to the client as a response header.
func RequestID() gogrpc.UnaryServerInterceptor {
	return func(ctx context.Context, req any, info *gogrpc.UnaryServerInfo, handler gogrpc.UnaryHandler) (any, error) {
		id := extractRequestID(ctx)
		if id == "" {
			id = uuid.Must(uuid.NewV7()).String()
		}
		ctx = WithRequestID(ctx, id)

		// Echo to client. Failure to set a header is a server-internal
		// signal; don't fail the RPC over it.
		_ = gogrpc.SetHeader(ctx, metadata.Pairs(RequestIDMetadataKey, id))

		return handler(ctx, req)
	}
}

func extractRequestID(ctx context.Context) string {
	md, ok := metadata.FromIncomingContext(ctx)
	if !ok {
		return ""
	}
	vals := md.Get(RequestIDMetadataKey)
	if len(vals) == 0 {
		return ""
	}
	return vals[0]
}
