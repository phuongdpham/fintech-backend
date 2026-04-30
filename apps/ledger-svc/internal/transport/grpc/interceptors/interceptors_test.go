package interceptors_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	gogrpc "google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"

	"github.com/phuongdpham/fintech/apps/ledger-svc/internal/transport/grpc/interceptors"
)

// ---------------------------------------------------------------------------
// Helpers — interceptor tests just call the function with a fake handler.
// No bufconn needed; we exercise the unit, not the wire.
// ---------------------------------------------------------------------------

func mkInfo(method string) *gogrpc.UnaryServerInfo {
	return &gogrpc.UnaryServerInfo{FullMethod: method}
}

func okHandler(_ context.Context, _ any) (any, error) { return "ok", nil }

// captureCtx returns a handler that records the ctx it was invoked with,
// so we can assert that interceptors injected the expected values.
func captureCtx(out *context.Context) gogrpc.UnaryHandler {
	return func(ctx context.Context, _ any) (any, error) {
		*out = ctx
		return "ok", nil
	}
}

// captureLogs builds a slog.Logger backed by an in-memory buffer plus a
// helper that parses the JSON lines back out. Tests assert on log
// content (level, fields) rather than trusting a single line of stderr.
func captureLogs() (*slog.Logger, *bytes.Buffer) {
	buf := &bytes.Buffer{}
	return slog.New(slog.NewJSONHandler(buf, &slog.HandlerOptions{Level: slog.LevelDebug})), buf
}

func parseJSONLines(t *testing.T, buf *bytes.Buffer) []map[string]any {
	t.Helper()
	out := []map[string]any{}
	for _, line := range strings.Split(strings.TrimSpace(buf.String()), "\n") {
		if line == "" {
			continue
		}
		var m map[string]any
		require.NoError(t, json.Unmarshal([]byte(line), &m))
		out = append(out, m)
	}
	return out
}

// ---------------------------------------------------------------------------
// RequestID
// ---------------------------------------------------------------------------

func TestRequestID(t *testing.T) {
	cases := []struct {
		name        string
		incoming    metadata.MD
		wantInbound bool // true if the inbound id must be reused verbatim
	}{
		{"no inbound -> generates uuid", nil, false},
		{"inbound is reused", metadata.Pairs(interceptors.RequestIDMetadataKey, "rid-from-caller"), true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			if tc.incoming != nil {
				ctx = metadata.NewIncomingContext(ctx, tc.incoming)
			}

			var captured context.Context
			_, err := interceptors.RequestID()(ctx, nil, mkInfo("/svc/Method"), captureCtx(&captured))
			require.NoError(t, err)

			rid := interceptors.RequestIDFromContext(captured)
			require.NotEmpty(t, rid)
			if tc.wantInbound {
				require.Equal(t, "rid-from-caller", rid)
			} else {
				// Generated should look like a UUID (36 chars with dashes).
				require.Len(t, rid, 36)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// Logging
// ---------------------------------------------------------------------------

func TestLogging_LevelMapping(t *testing.T) {
	cases := []struct {
		name     string
		err      error
		wantLast string // expected last-line "level" value
	}{
		{"OK -> INFO", nil, "INFO"},
		{"NotFound -> INFO (expected client outcome)", status.Error(codes.NotFound, "x"), "INFO"},
		{"Unavailable -> WARN", status.Error(codes.Unavailable, "x"), "WARN"},
		{"Internal -> ERROR", status.Error(codes.Internal, "x"), "ERROR"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			log, buf := captureLogs()
			handler := func(_ context.Context, _ any) (any, error) {
				return nil, tc.err
			}
			// Ensure request_id flows in too.
			ctx := interceptors.WithRequestID(context.Background(), "rid-test")
			_, _ = interceptors.Logging(log)(ctx, nil, mkInfo("/svc/Foo"), handler)

			lines := parseJSONLines(t, buf)
			require.NotEmpty(t, lines)
			last := lines[len(lines)-1]
			require.Equal(t, tc.wantLast, last["level"])
			require.Equal(t, "rid-test", last["request_id"])
			require.Equal(t, "/svc/Foo", last["rpc.method"])
		})
	}
}

func TestLogging_InjectsLoggerIntoContext(t *testing.T) {
	log, _ := captureLogs()
	var captured context.Context
	_, err := interceptors.Logging(log)(context.Background(), nil, mkInfo("/svc/M"), captureCtx(&captured))
	require.NoError(t, err)

	got := interceptors.LoggerFromContext(captured)
	require.NotNil(t, got)
	// The injected logger should NOT be the slog.Default fallback.
	require.NotSame(t, slog.Default(), got)
}

// ---------------------------------------------------------------------------
// Recovery
// ---------------------------------------------------------------------------

func TestRecovery(t *testing.T) {
	cases := []struct {
		name      string
		handler   gogrpc.UnaryHandler
		wantCode  codes.Code
		wantPanic bool // expected to log the panic
	}{
		{
			name:     "no panic passes through",
			handler:  okHandler,
			wantCode: codes.OK,
		},
		{
			name: "panic with string",
			handler: func(_ context.Context, _ any) (any, error) {
				panic("boom")
			},
			wantCode:  codes.Internal,
			wantPanic: true,
		},
		{
			name: "panic with error",
			handler: func(_ context.Context, _ any) (any, error) {
				panic(errors.New("kaboom"))
			},
			wantCode:  codes.Internal,
			wantPanic: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			log, buf := captureLogs()
			resp, err := interceptors.Recovery(log)(context.Background(), nil, mkInfo("/svc/M"), tc.handler)

			if tc.wantCode == codes.OK {
				require.NoError(t, err)
				require.NotNil(t, resp)
				return
			}
			require.Error(t, err)
			st, _ := status.FromError(err)
			require.Equal(t, tc.wantCode, st.Code())
			require.Nil(t, resp)
			if tc.wantPanic {
				lines := parseJSONLines(t, buf)
				require.NotEmpty(t, lines)
				require.Equal(t, "ERROR", lines[len(lines)-1]["level"])
				require.Contains(t, lines[len(lines)-1]["msg"], "panic")
			}
		})
	}
}

// ---------------------------------------------------------------------------
// EdgeIdentity
// ---------------------------------------------------------------------------

func TestEdgeIdentity(t *testing.T) {
	publicSet := map[string]struct{}{
		"/grpc.health.v1.Health/Check": {},
	}

	cases := []struct {
		name       string
		cfg        interceptors.EdgeIdentityConfig
		method     string
		incomingMD metadata.MD
		wantCode   codes.Code
		wantClaims *interceptors.Claims
	}{
		{
			name:     "public method bypasses identity check",
			cfg:      interceptors.EdgeIdentityConfig{PublicMethods: publicSet},
			method:   "/grpc.health.v1.Health/Check",
			wantCode: codes.OK,
		},
		{
			name:     "missing all headers -> Unauthenticated",
			cfg:      interceptors.EdgeIdentityConfig{PublicMethods: publicSet},
			method:   "/svc/M",
			wantCode: codes.Unauthenticated,
		},
		{
			name:   "missing tenant -> Unauthenticated",
			cfg:    interceptors.EdgeIdentityConfig{PublicMethods: publicSet},
			method: "/svc/M",
			incomingMD: metadata.Pairs(
				interceptors.HeaderActorSubject, "alice",
			),
			wantCode: codes.Unauthenticated,
		},
		{
			name:   "missing subject -> Unauthenticated",
			cfg:    interceptors.EdgeIdentityConfig{PublicMethods: publicSet},
			method: "/svc/M",
			incomingMD: metadata.Pairs(
				interceptors.HeaderTenantID, "tenant-a",
			),
			wantCode: codes.Unauthenticated,
		},
		{
			name:   "empty tenant string -> Unauthenticated",
			cfg:    interceptors.EdgeIdentityConfig{PublicMethods: publicSet},
			method: "/svc/M",
			incomingMD: metadata.Pairs(
				interceptors.HeaderTenantID, "",
				interceptors.HeaderActorSubject, "alice",
			),
			wantCode: codes.Unauthenticated,
		},
		{
			name:   "tenant + subject present -> OK with claims",
			cfg:    interceptors.EdgeIdentityConfig{PublicMethods: publicSet},
			method: "/svc/M",
			incomingMD: metadata.Pairs(
				interceptors.HeaderTenantID, "tenant-a",
				interceptors.HeaderActorSubject, "alice",
			),
			wantCode:   codes.OK,
			wantClaims: &interceptors.Claims{Subject: "alice", Tenant: "tenant-a"},
		},
		{
			name:   "tenant + subject + session -> session populated",
			cfg:    interceptors.EdgeIdentityConfig{PublicMethods: publicSet},
			method: "/svc/M",
			incomingMD: metadata.Pairs(
				interceptors.HeaderTenantID, "tenant-a",
				interceptors.HeaderActorSubject, "alice",
				interceptors.HeaderActorSession, "sess-42",
			),
			wantCode: codes.OK,
			wantClaims: &interceptors.Claims{
				Subject: "alice", Tenant: "tenant-a", Session: "sess-42",
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			if tc.incomingMD != nil {
				ctx = metadata.NewIncomingContext(ctx, tc.incomingMD)
			}
			var captured context.Context
			handler := captureCtx(&captured)

			_, err := interceptors.EdgeIdentity(tc.cfg)(ctx, nil, mkInfo(tc.method), handler)

			if tc.wantCode == codes.OK {
				require.NoError(t, err)
				gotClaims := interceptors.ClaimsFromContext(captured)
				if tc.wantClaims == nil {
					require.Nil(t, gotClaims)
				} else {
					require.NotNil(t, gotClaims)
					require.Equal(t, tc.wantClaims.Subject, gotClaims.Subject)
					require.Equal(t, tc.wantClaims.Tenant, gotClaims.Tenant)
					require.Equal(t, tc.wantClaims.Session, gotClaims.Session)
				}
			} else {
				require.Error(t, err)
				st, _ := status.FromError(err)
				require.Equal(t, tc.wantCode, st.Code())
			}
		})
	}
}
