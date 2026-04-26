package interceptors

import (
	"context"
	"log/slog"
	"runtime/debug"

	gogrpc "google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// Recovery converts panics into a gRPC Internal status error so a single
// bad code path doesn't take down the whole server (gRPC's default
// behavior is to crash the process on a handler panic).
//
// Stack + panic value are logged at Error level so they show up in the
// paging stream. The client only sees the opaque "internal error" — we
// never leak panic detail across the trust boundary.
//
// Place this OUTERMOST in the chain so it also catches panics in the
// other interceptors (logging, auth, etc.).
func Recovery(log *slog.Logger) gogrpc.UnaryServerInterceptor {
	if log == nil {
		log = slog.Default()
	}
	return func(ctx context.Context, req any, info *gogrpc.UnaryServerInfo, handler gogrpc.UnaryHandler) (resp any, err error) {
		defer func() {
			r := recover()
			if r == nil {
				return
			}
			// Use the per-call logger if Logging already ran; otherwise the
			// supplied base logger. Either way we tag with the method.
			callLog := LoggerFromContext(ctx)
			if callLog == slog.Default() {
				callLog = log
			}
			callLog.ErrorContext(ctx, "panic in rpc handler",
				slog.String("rpc.method", info.FullMethod),
				slog.Any("panic", r),
				slog.String("stack", string(debug.Stack())),
			)
			err = status.Error(codes.Internal, "internal error")
			resp = nil
		}()
		return handler(ctx, req)
	}
}
