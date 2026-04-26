package interceptors

import (
	"context"
	"log/slog"
	"time"

	gogrpc "google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/peer"
	"google.golang.org/grpc/status"
)

type ctxKeyLogger struct{}

// LoggerFromContext returns the request-scoped logger pre-populated
// with rpc.method + request_id. Falls back to slog.Default() when the
// context didn't pass through the Logging interceptor — handler code
// can call this unconditionally without a nil check.
func LoggerFromContext(ctx context.Context) *slog.Logger {
	if l, ok := ctx.Value(ctxKeyLogger{}).(*slog.Logger); ok && l != nil {
		return l
	}
	return slog.Default()
}

// WithLogger overrides the request-scoped logger. Useful for tests and
// for adding extra request-scoped attrs (e.g. the auth subject) from a
// later interceptor.
func WithLogger(ctx context.Context, log *slog.Logger) context.Context {
	return context.WithValue(ctx, ctxKeyLogger{}, log)
}

// Logging emits a start (Debug) and end (Info / Warn) event for every
// RPC, and stashes a per-call slog.Logger into the context so handler
// code inherits request_id + method automatically.
//
// Severity policy:
//   codes.OK                              → Info
//   client errors (InvalidArgument, etc.) → Info  (expected)
//   server errors (Internal, Unknown)     → Error (paging-relevant)
//   transient (Unavailable, DeadlineExceeded) → Warn
func Logging(base *slog.Logger) gogrpc.UnaryServerInterceptor {
	if base == nil {
		base = slog.Default()
	}
	return func(ctx context.Context, req any, info *gogrpc.UnaryServerInfo, handler gogrpc.UnaryHandler) (any, error) {
		start := time.Now()

		callLog := base.With(
			slog.String("rpc.method", info.FullMethod),
			slog.String("request_id", RequestIDFromContext(ctx)),
			slog.String("rpc.peer", peerAddr(ctx)),
		)
		ctx = WithLogger(ctx, callLog)

		callLog.DebugContext(ctx, "rpc start")

		resp, err := handler(ctx, req)

		st, _ := status.FromError(err)
		attrs := []any{
			slog.Duration("rpc.elapsed", time.Since(start)),
			slog.String("rpc.code", st.Code().String()),
		}
		switch logLevelForCode(st.Code()) {
		case slog.LevelError:
			attrs = append(attrs, slog.Any("err", err))
			callLog.ErrorContext(ctx, "rpc complete", attrs...)
		case slog.LevelWarn:
			attrs = append(attrs, slog.Any("err", err))
			callLog.WarnContext(ctx, "rpc complete", attrs...)
		default:
			callLog.InfoContext(ctx, "rpc complete", attrs...)
		}

		return resp, err
	}
}

func logLevelForCode(c codes.Code) slog.Level {
	switch c {
	case codes.OK,
		codes.Canceled,
		codes.InvalidArgument,
		codes.NotFound,
		codes.AlreadyExists,
		codes.FailedPrecondition,
		codes.Aborted,
		codes.OutOfRange,
		codes.PermissionDenied,
		codes.Unauthenticated:
		return slog.LevelInfo
	case codes.Unavailable, codes.DeadlineExceeded, codes.ResourceExhausted:
		return slog.LevelWarn
	default:
		// Internal, Unknown, DataLoss, Unimplemented — paging-relevant.
		return slog.LevelError
	}
}

func peerAddr(ctx context.Context) string {
	if p, ok := peer.FromContext(ctx); ok && p.Addr != nil {
		return p.Addr.String()
	}
	return ""
}
