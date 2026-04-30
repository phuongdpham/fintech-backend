package grpc_test

import (
	"context"
	"io"
	"log/slog"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/shopspring/decimal"
	"github.com/stretchr/testify/require"
	gogrpc "google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"google.golang.org/grpc/test/bufconn"

	"github.com/phuongdpham/fintech/apps/ledger-svc/internal/domain"
	transportgrpc "github.com/phuongdpham/fintech/apps/ledger-svc/internal/transport/grpc"
	"github.com/phuongdpham/fintech/apps/ledger-svc/internal/transport/grpc/interceptors"
	"github.com/phuongdpham/fintech/apps/ledger-svc/internal/usecase"
	pb "github.com/phuongdpham/fintech/libs/go/proto-gen/fintech/ledger/v1"
)

// testTenant is the synthetic tenant the test harness's auth shim attaches
// to every request. Mirrors what the real Auth interceptor would do once
// configured with a Verifier returning Claims.Tenant.
const testTenant = "tenant-a"

// injectTestClaims is the test-only auth shim. It pre-populates a Claims
// record with Tenant=testTenant on every incoming request so the handler's
// tenant-required check sees a valid tenant. Real deployments use
// interceptors.Auth + a Verifier; this stays out of the test loop because
// the suite is exercising handler-and-below behavior, not auth.
func injectTestClaims(ctx context.Context, req any, _ *gogrpc.UnaryServerInfo, handler gogrpc.UnaryHandler) (any, error) {
	ctx = interceptors.WithClaims(ctx, &interceptors.Claims{Subject: "test-user", Tenant: testTenant})
	return handler(ctx, req)
}

// ---------------------------------------------------------------------------
// In-memory fakes — match the contract of the production ports closely
// enough that the handler is exercised end-to-end without Postgres or
// Redis. Mirror of the usecase test's fakes, kept local so the two
// suites can drift independently.
// ---------------------------------------------------------------------------

type fakeIdemStore struct {
	mu      sync.Mutex
	records map[string]domain.IdempotencyRecord
}

func newFakeIdem() *fakeIdemStore {
	return &fakeIdemStore{records: map[string]domain.IdempotencyRecord{}}
}
func (f *fakeIdemStore) Acquire(_ context.Context, key, fingerprint string) (bool, domain.IdempotencyRecord, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if cur, ok := f.records[key]; ok {
		return false, cur, nil
	}
	f.records[key] = domain.IdempotencyRecord{State: domain.IdempotencyStarted, Fingerprint: fingerprint}
	return true, domain.IdempotencyRecord{}, nil
}
func (f *fakeIdemStore) SetState(_ context.Context, key, fingerprint string, s domain.IdempotencyState) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.records[key] = domain.IdempotencyRecord{State: s, Fingerprint: fingerprint}
	return nil
}

type fakeLedgerRepo struct {
	mu       sync.Mutex
	txByID   map[uuid.UUID]*domain.Transaction
	txByIdem map[string]*domain.Transaction
}

func newFakeRepo() *fakeLedgerRepo {
	return &fakeLedgerRepo{
		txByID:   map[uuid.UUID]*domain.Transaction{},
		txByIdem: map[string]*domain.Transaction{},
	}
}
// idemBucketKey scopes the per-tenant uniqueness lookup. Mirrors the real
// PG composite UNIQUE (tenant_id, idempotency_key).
func idemBucketKey(tenantID, key string) string { return tenantID + "|" + key }

func (f *fakeLedgerRepo) seed(tx *domain.Transaction) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.txByID[tx.ID] = tx
	f.txByIdem[idemBucketKey(tx.TenantID, tx.IdempotencyKey)] = tx
}
func (f *fakeLedgerRepo) ExecuteTransfer(_ context.Context, req domain.TransferRequest) (*domain.Transaction, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	bucket := idemBucketKey(req.TenantID, req.IdempotencyKey)
	if _, exists := f.txByIdem[bucket]; exists {
		return nil, domain.ErrDuplicateIdempotencyKey
	}
	now := time.Now().UTC()
	txID := uuid.New()
	tx := &domain.Transaction{
		ID:                 txID,
		TenantID:           req.TenantID,
		IdempotencyKey:     req.IdempotencyKey,
		RequestFingerprint: req.RequestFingerprint,
		Status:             domain.TransactionStatusCommitted,
		Entries: []domain.JournalEntry{
			{ID: uuid.New(), TransactionID: txID, AccountID: req.FromAccountID, Amount: req.Amount.Neg(), Currency: req.Currency, CreatedAt: now},
			{ID: uuid.New(), TransactionID: txID, AccountID: req.ToAccountID, Amount: req.Amount, Currency: req.Currency, CreatedAt: now},
		},
		CreatedAt: now,
	}
	if err := tx.AssertBalanced(); err != nil {
		return nil, err
	}
	f.txByID[txID] = tx
	f.txByIdem[bucket] = tx
	return tx, nil
}
func (f *fakeLedgerRepo) GetTransaction(_ context.Context, id uuid.UUID) (*domain.Transaction, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	tx, ok := f.txByID[id]
	if !ok {
		return nil, domain.ErrTransactionNotFound
	}
	return tx, nil
}
func (f *fakeLedgerRepo) GetTransactionByIdempotencyKey(_ context.Context, tenantID, key string) (*domain.Transaction, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	tx, ok := f.txByIdem[idemBucketKey(tenantID, key)]
	if !ok {
		return nil, domain.ErrTransactionNotFound
	}
	return tx, nil
}

// ---------------------------------------------------------------------------
// Test harness — bufconn-backed gRPC server + client.
// ---------------------------------------------------------------------------

const bufSize = 1024 * 1024

type harness struct {
	client pb.LedgerServiceClient
	idem   *fakeIdemStore
	repo   *fakeLedgerRepo
	stop   func()
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	idem := newFakeIdem()
	repo := newFakeRepo()
	uc := usecase.NewTransferUsecase(repo, idem, slog.New(slog.NewTextHandler(io.Discard, nil)))
	handler := transportgrpc.NewLedgerHandler(uc, repo, slog.New(slog.NewTextHandler(io.Discard, nil)))

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
		IdempotencyKey: "k1",
		FromAccountId:  "11111111-1111-1111-1111-111111111111",
		ToAccountId:    "22222222-2222-2222-2222-222222222222",
		Amount:         "100.0000",
		Currency:       "USD",
	}
}

// validTransferFingerprint returns the canonical fingerprint of the
// validTransferReq() body as the production handler would compute it
// (after parsing the proto). Test setups use this to seed the fake
// idempotency store with a fingerprint that matches the request the
// test will send — otherwise the mismatch path fires.
func validTransferFingerprint() string {
	return usecase.TransferFingerprint(usecase.TransferInput{
		TenantID:       testTenant,
		IdempotencyKey: "k1",
		FromAccountID:  uuid.MustParse("11111111-1111-1111-1111-111111111111"),
		ToAccountID:    uuid.MustParse("22222222-2222-2222-2222-222222222222"),
		Amount:         decimal.NewFromFloat(100.0),
		Currency:       "USD",
	})
}

// ---------------------------------------------------------------------------
// Tests
// ---------------------------------------------------------------------------

func TestLedgerHandler_Transfer(t *testing.T) {
	cases := []struct {
		name     string
		setup    func(*harness)
		mutate   func(*pb.TransferRequest)
		wantCode codes.Code
		wantReplayed bool
	}{
		{
			name:     "happy path returns OK with replayed=false",
			wantCode: codes.OK,
		},
		{
			name: "replay path returns OK with replayed=true",
			setup: func(h *harness) {
				fp := validTransferFingerprint()
				now := time.Now().UTC()
				txID := uuid.New()
				h.repo.seed(&domain.Transaction{
					ID:                 txID,
					TenantID:           testTenant,
					IdempotencyKey:     "k1",
					RequestFingerprint: fp,
					Status:             domain.TransactionStatusCommitted,
					Entries: []domain.JournalEntry{
						{ID: uuid.New(), TransactionID: txID, Amount: decimal.NewFromInt(-100), Currency: "USD", CreatedAt: now},
						{ID: uuid.New(), TransactionID: txID, Amount: decimal.NewFromInt(100), Currency: "USD", CreatedAt: now},
					},
					CreatedAt: now,
				})
				h.idem.records[testTenant+":k1"] = domain.IdempotencyRecord{
					State: domain.IdempotencyCompleted, Fingerprint: fp,
				}
			},
			wantCode:     codes.OK,
			wantReplayed: true,
		},
		{
			name: "STARTED state -> Aborted",
			setup: func(h *harness) {
				h.idem.records[testTenant+":k1"] = domain.IdempotencyRecord{
					State: domain.IdempotencyStarted, Fingerprint: validTransferFingerprint(),
				}
			},
			wantCode: codes.Aborted,
		},
		{
			name: "FAILED state -> AlreadyExists",
			setup: func(h *harness) {
				h.idem.records[testTenant+":k1"] = domain.IdempotencyRecord{
					State: domain.IdempotencyFailed, Fingerprint: validTransferFingerprint(),
				}
			},
			wantCode: codes.AlreadyExists,
		},
		{
			name: "fingerprint mismatch on completed -> FailedPrecondition",
			setup: func(h *harness) {
				h.idem.records[testTenant+":k1"] = domain.IdempotencyRecord{
					State: domain.IdempotencyCompleted, Fingerprint: "stale-prior-request-fp",
				}
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

			if tc.setup != nil {
				tc.setup(h)
			}
			req := validTransferReq()
			if tc.mutate != nil {
				tc.mutate(req)
			}

			resp, err := h.client.Transfer(context.Background(), req)
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

	now := time.Now().UTC()
	txID := uuid.New()
	h.repo.seed(&domain.Transaction{
		ID:             txID,
		TenantID:       testTenant,
		IdempotencyKey: "seeded",
		Status:         domain.TransactionStatusCommitted,
		Entries: []domain.JournalEntry{
			{ID: uuid.New(), TransactionID: txID, Amount: decimal.NewFromInt(-50), Currency: "USD", CreatedAt: now},
			{ID: uuid.New(), TransactionID: txID, Amount: decimal.NewFromInt(50), Currency: "USD", CreatedAt: now},
		},
		CreatedAt: now,
	})

	t.Run("success", func(t *testing.T) {
		resp, err := h.client.GetTransaction(context.Background(), &pb.GetTransactionRequest{Id: txID.String()})
		require.NoError(t, err)
		require.NotNil(t, resp.Transaction)
		require.Equal(t, txID.String(), resp.Transaction.Id)
		require.Equal(t, "COMMITTED", resp.Transaction.Status)
	})

	t.Run("invalid uuid -> InvalidArgument", func(t *testing.T) {
		_, err := h.client.GetTransaction(context.Background(), &pb.GetTransactionRequest{Id: "bad"})
		require.Error(t, err)
		st, _ := status.FromError(err)
		require.Equal(t, codes.InvalidArgument, st.Code())
	})

	t.Run("missing tx -> NotFound", func(t *testing.T) {
		_, err := h.client.GetTransaction(context.Background(), &pb.GetTransactionRequest{Id: uuid.New().String()})
		require.Error(t, err)
		st, _ := status.FromError(err)
		require.Equal(t, codes.NotFound, st.Code())
	})
}

func TestLedgerHandler_Transfer_TrailerOnReplay(t *testing.T) {
	h := newHarness(t)
	defer h.stop()

	fp := validTransferFingerprint()
	now := time.Now().UTC()
	txID := uuid.New()
	h.repo.seed(&domain.Transaction{
		ID:                 txID,
		TenantID:           testTenant,
		IdempotencyKey:     "k1",
		RequestFingerprint: fp,
		Status:             domain.TransactionStatusCommitted,
		Entries: []domain.JournalEntry{
			{ID: uuid.New(), TransactionID: txID, Amount: decimal.NewFromInt(-100), Currency: "USD", CreatedAt: now},
			{ID: uuid.New(), TransactionID: txID, Amount: decimal.NewFromInt(100), Currency: "USD", CreatedAt: now},
		},
		CreatedAt: now,
	})
	h.idem.records[testTenant+":k1"] = domain.IdempotencyRecord{
		State: domain.IdempotencyCompleted, Fingerprint: fp,
	}

	var trailer metadata.MD
	resp, err := h.client.Transfer(context.Background(), validTransferReq(), gogrpc.Trailer(&trailer))
	require.NoError(t, err)
	require.True(t, resp.Replayed)
	require.Equal(t, []string{"true"}, trailer.Get(transportgrpc.MetadataKeyReplayed),
		"trailer must signal replay")
}
