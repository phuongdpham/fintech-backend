package grpc_test

import (
	"context"
	"io"
	"log/slog"
	"net"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	"go.uber.org/mock/gomock"
	gogrpc "google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"google.golang.org/grpc/test/bufconn"

	"github.com/phuongdpham/fintech/apps/ledger-svc/internal/domain"
	"github.com/phuongdpham/fintech/apps/ledger-svc/internal/domain/mocks"
	transportgrpc "github.com/phuongdpham/fintech/apps/ledger-svc/internal/transport/grpc"
	"github.com/phuongdpham/fintech/apps/ledger-svc/internal/transport/grpc/interceptors"
	"github.com/phuongdpham/fintech/apps/ledger-svc/internal/usecase"
	pb "github.com/phuongdpham/fintech/libs/go/proto-gen/fintech/ledger/v1"
)

const (
	testTenant      = "tenant-a"
	testIdemKey     = "k1"
	testScopedKey   = "tenant-a:k1"
	testFromAccount = "11111111-1111-1111-1111-111111111111"
	testToAccount   = "22222222-2222-2222-2222-222222222222"
)

// injectTestClaims is the test-only shim that mimics the production
// interceptor chain (EdgeIdentity + RequestID). Both are required for
// audit.EnvelopeFromContext to succeed downstream — short-circuiting
// claims without a request_id leaves the envelope incomplete and every
// usecase call hard-fails.
func injectTestClaims(ctx context.Context, req any, _ *gogrpc.UnaryServerInfo, handler gogrpc.UnaryHandler) (any, error) {
	ctx = interceptors.WithClaims(ctx, &interceptors.Claims{Subject: "test-user", Tenant: testTenant})
	ctx = interceptors.WithRequestID(ctx, "test-request-id")
	return handler(ctx, req)
}

const bufSize = 1024 * 1024

type harness struct {
	client pb.LedgerServiceClient
	idem   *mocks.MockIdempotencyStore
	repo   *mocks.MockLedgerRepository
	stop   func()
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	ctrl := gomock.NewController(t)
	idem := mocks.NewMockIdempotencyStore(ctrl)
	repo := mocks.NewMockLedgerRepository(ctrl)

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	uc := usecase.NewTransferUsecase(repo, idem, logger)
	handler := transportgrpc.NewLedgerHandler(uc, repo, logger)

	lis := bufconn.Listen(bufSize)
	srv := gogrpc.NewServer(gogrpc.UnaryInterceptor(injectTestClaims))
	pb.RegisterLedgerServiceServer(srv, handler)
	go func() { _ = srv.Serve(lis) }()

	conn, err := gogrpc.NewClient("passthrough://bufnet",
		gogrpc.WithContextDialer(func(_ context.Context, _ string) (net.Conn, error) {
			return lis.Dial()
		}),
		gogrpc.WithTransportCredentials(insecure.NewCredentials()),
	)
	require.NoError(t, err)
	return &harness{
		client: pb.NewLedgerServiceClient(conn),
		idem:   idem,
		repo:   repo,
		stop: func() {
			_ = conn.Close()
			srv.Stop()
			_ = lis.Close()
		},
	}
}

func validTransferReq() *pb.TransferRequest {
	return &pb.TransferRequest{
		IdempotencyKey: testIdemKey,
		FromAccountId:  testFromAccount,
		ToAccountId:    testToAccount,
		Amount:         "100.0000",
		Currency:       "USD",
	}
}

// validTransferFingerprint is the canonical fingerprint of validTransferReq()
// the production handler computes after parsing the proto.
func validTransferFingerprint() string {
	return usecase.TransferFingerprint(usecase.TransferInput{
		TenantID:       testTenant,
		IdempotencyKey: testIdemKey,
		FromAccountID:  uuid.MustParse(testFromAccount),
		ToAccountID:    uuid.MustParse(testToAccount),
		Amount:         domain.NewAmountFromInt(100),
		Currency:       "USD",
	})
}

func seededTx(t *testing.T, fp string) *domain.Transaction {
	t.Helper()
	now := time.Now().UTC()
	txID := uuid.New()
	return &domain.Transaction{
		ID:                 txID,
		TenantID:           testTenant,
		IdempotencyKey:     testIdemKey,
		RequestFingerprint: fp,
		Status:             domain.TransactionStatusCommitted,
		Entries: []domain.JournalEntry{
			{ID: uuid.New(), TransactionID: txID, Amount: domain.NewAmountFromInt(-100), Currency: "USD", CreatedAt: now},
			{ID: uuid.New(), TransactionID: txID, Amount: domain.NewAmountFromInt(100), Currency: "USD", CreatedAt: now},
		},
		CreatedAt: now,
	}
}

func TestLedgerHandler_Transfer(t *testing.T) {
	cases := []struct {
		name         string
		mutate       func(*pb.TransferRequest)
		expect       func(t *testing.T, h *harness, fp string)
		wantCode     codes.Code
		wantReplayed bool
	}{
		{
			name: "happy path returns OK with replayed=false",
			expect: func(t *testing.T, h *harness, fp string) {
				gomock.InOrder(
					h.idem.EXPECT().Acquire(gomock.Any(), testScopedKey, fp).
						Return(true, domain.IdempotencyRecord{}, nil),
					h.repo.EXPECT().ExecuteTransfer(gomock.Any(), gomock.Any()).
						Return(seededTx(t, fp), nil),
					h.idem.EXPECT().SetState(gomock.Any(), testScopedKey, fp, domain.IdempotencyCompleted).
						Return(nil),
				)
			},
			wantCode: codes.OK,
		},
		{
			name: "replay path returns OK with replayed=true",
			expect: func(t *testing.T, h *harness, fp string) {
				gomock.InOrder(
					h.idem.EXPECT().Acquire(gomock.Any(), testScopedKey, fp).
						Return(false, domain.IdempotencyRecord{State: domain.IdempotencyCompleted, Fingerprint: fp}, nil),
					h.repo.EXPECT().GetTransactionByIdempotencyKey(gomock.Any(), testTenant, testIdemKey).
						Return(seededTx(t, fp), nil),
				)
			},
			wantCode:     codes.OK,
			wantReplayed: true,
		},
		{
			name: "STARTED state -> Aborted",
			expect: func(t *testing.T, h *harness, fp string) {
				h.idem.EXPECT().Acquire(gomock.Any(), testScopedKey, fp).
					Return(false, domain.IdempotencyRecord{State: domain.IdempotencyStarted, Fingerprint: fp}, nil)
			},
			wantCode: codes.Aborted,
		},
		{
			name: "FAILED state -> AlreadyExists",
			expect: func(t *testing.T, h *harness, fp string) {
				h.idem.EXPECT().Acquire(gomock.Any(), testScopedKey, fp).
					Return(false, domain.IdempotencyRecord{State: domain.IdempotencyFailed, Fingerprint: fp}, nil)
			},
			wantCode: codes.AlreadyExists,
		},
		{
			name: "fingerprint mismatch on completed -> FailedPrecondition",
			expect: func(t *testing.T, h *harness, fp string) {
				h.idem.EXPECT().Acquire(gomock.Any(), testScopedKey, fp).
					Return(false, domain.IdempotencyRecord{State: domain.IdempotencyCompleted, Fingerprint: "stale-fp"}, nil)
			},
			wantCode: codes.FailedPrecondition,
		},
		{
			name:     "invalid from_account_id -> InvalidArgument",
			mutate:   func(r *pb.TransferRequest) { r.FromAccountId = "not-a-uuid" },
			wantCode: codes.InvalidArgument,
		},
		{
			name:     "invalid amount -> InvalidArgument",
			mutate:   func(r *pb.TransferRequest) { r.Amount = "not-a-number" },
			wantCode: codes.InvalidArgument,
		},
		{
			name:     "invalid currency -> InvalidArgument (caught by usecase)",
			mutate:   func(r *pb.TransferRequest) { r.Currency = "us" },
			wantCode: codes.InvalidArgument,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t)
			defer h.stop()

			fp := validTransferFingerprint()
			if tc.expect != nil {
				tc.expect(t, h, fp)
			}
			req := validTransferReq()
			if tc.mutate != nil {
				tc.mutate(req)
			}

			resp, err := h.client.Transfer(t.Context(), req)
			if tc.wantCode == codes.OK {
				require.NoError(t, err)
				require.NotNil(t, resp)
				require.NotNil(t, resp.Transaction)
				require.Equal(t, tc.wantReplayed, resp.Replayed)
				require.Len(t, resp.Transaction.Entries, 2)
				require.Equal(t, "USD", resp.Transaction.Entries[0].Currency)
			} else {
				require.Error(t, err)
				st, ok := status.FromError(err)
				require.True(t, ok, "expected gRPC status error, got %T: %v", err, err)
				require.Equal(t, tc.wantCode, st.Code(), "want %s, got %s: %s", tc.wantCode, st.Code(), st.Message())
			}
		})
	}
}

func TestLedgerHandler_GetTransaction(t *testing.T) {
	h := newHarness(t)
	defer h.stop()

	txID := uuid.New()
	seeded := seededTx(t, validTransferFingerprint())
	seeded.ID = txID

	t.Run("success", func(t *testing.T) {
		h.repo.EXPECT().GetTransaction(gomock.Any(), txID).Return(seeded, nil)

		resp, err := h.client.GetTransaction(t.Context(), &pb.GetTransactionRequest{Id: txID.String()})
		require.NoError(t, err)
		require.NotNil(t, resp.Transaction)
		require.Equal(t, txID.String(), resp.Transaction.Id)
		require.Equal(t, "COMMITTED", resp.Transaction.Status)
	})

	t.Run("invalid uuid -> InvalidArgument", func(t *testing.T) {
		_, err := h.client.GetTransaction(t.Context(), &pb.GetTransactionRequest{Id: "bad"})
		require.Error(t, err)
		st, _ := status.FromError(err)
		require.Equal(t, codes.InvalidArgument, st.Code())
	})

	t.Run("missing tx -> NotFound", func(t *testing.T) {
		missingID := uuid.New()
		h.repo.EXPECT().GetTransaction(gomock.Any(), missingID).
			Return(nil, domain.ErrTransactionNotFound)

		_, err := h.client.GetTransaction(t.Context(), &pb.GetTransactionRequest{Id: missingID.String()})
		require.Error(t, err)
		st, _ := status.FromError(err)
		require.Equal(t, codes.NotFound, st.Code())
	})
}

func TestLedgerHandler_Transfer_TrailerOnReplay(t *testing.T) {
	h := newHarness(t)
	defer h.stop()

	fp := validTransferFingerprint()
	gomock.InOrder(
		h.idem.EXPECT().Acquire(gomock.Any(), testScopedKey, fp).
			Return(false, domain.IdempotencyRecord{State: domain.IdempotencyCompleted, Fingerprint: fp}, nil),
		h.repo.EXPECT().GetTransactionByIdempotencyKey(gomock.Any(), testTenant, testIdemKey).
			Return(seededTx(t, fp), nil),
	)

	var trailer metadata.MD
	resp, err := h.client.Transfer(t.Context(), validTransferReq(), gogrpc.Trailer(&trailer))
	require.NoError(t, err)
	require.True(t, resp.Replayed)
	require.Equal(t, []string{"true"}, trailer.Get(transportgrpc.MetadataKeyReplayed),
		"trailer must signal replay")
}
