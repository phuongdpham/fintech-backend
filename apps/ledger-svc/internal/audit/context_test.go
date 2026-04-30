package audit_test

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/phuongdpham/fintech/apps/ledger-svc/internal/audit"
	"github.com/phuongdpham/fintech/apps/ledger-svc/internal/transport/grpc/interceptors"
)

func TestEnvelopeFromContext_HappyPath(t *testing.T) {
	ctx := context.Background()
	ctx = interceptors.WithClaims(ctx, &interceptors.Claims{
		Subject: "alice",
		Tenant:  "tenant-a",
		Session: "sess-1",
	})
	ctx = interceptors.WithRequestID(ctx, "req-001")

	env, err := audit.EnvelopeFromContext(ctx)
	require.NoError(t, err)
	require.Equal(t, "tenant-a", env.TenantID)
	require.Equal(t, "alice", env.ActorSubject)
	require.Equal(t, "sess-1", env.ActorSession)
	require.Equal(t, "req-001", env.RequestID)
	require.False(t, env.OccurredAt.IsZero(), "OccurredAt must be set")
}

func TestEnvelopeFromContext_RejectsIncomplete(t *testing.T) {
	cases := []struct {
		name  string
		build func() context.Context
	}{
		{
			name:  "no claims at all",
			build: func() context.Context { return interceptors.WithRequestID(context.Background(), "req-001") },
		},
		{
			name: "claims but empty tenant",
			build: func() context.Context {
				ctx := interceptors.WithClaims(context.Background(), &interceptors.Claims{Subject: "alice"})
				return interceptors.WithRequestID(ctx, "req-001")
			},
		},
		{
			name: "claims but empty subject",
			build: func() context.Context {
				ctx := interceptors.WithClaims(context.Background(), &interceptors.Claims{Tenant: "tenant-a"})
				return interceptors.WithRequestID(ctx, "req-001")
			},
		},
		{
			name: "no request id",
			build: func() context.Context {
				return interceptors.WithClaims(context.Background(), &interceptors.Claims{
					Subject: "alice", Tenant: "tenant-a",
				})
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := audit.EnvelopeFromContext(tc.build())
			require.ErrorIs(t, err, audit.ErrEnvelopeIncomplete)
		})
	}
}
