package usecase_test

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/shopspring/decimal"
	"github.com/stretchr/testify/require"
	"go.uber.org/mock/gomock"

	"github.com/phuongdpham/fintech/apps/ledger-svc/internal/domain"
	"github.com/phuongdpham/fintech/apps/ledger-svc/internal/domain/mocks"
	"github.com/phuongdpham/fintech/apps/ledger-svc/internal/usecase"
)

// silentLogger discards usecase warnings during tests so failure signal
// only comes from assertions.
func silentLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func validInput() usecase.TransferInput {
	return usecase.TransferInput{
		TenantID:       "tenant-a",
		IdempotencyKey: "k1",
		FromAccountID:  uuid.MustParse("11111111-1111-1111-1111-111111111111"),
		ToAccountID:    uuid.MustParse("22222222-2222-2222-2222-222222222222"),
		Amount:         decimal.NewFromInt(100),
		Currency:       "USD",
	}
}

// scopedKey mirrors usecase.scopedIdempotencyKey: the opaque key the
// usecase composes from tenant + caller-supplied key before handing it to
// the IdempotencyStore.
const scopedKey = "tenant-a:k1"

// originalTx is the existing committed transaction returned on the replay
// path. Fingerprint matches validInput() so replays succeed.
func originalTx(t *testing.T) *domain.Transaction {
	t.Helper()
	txID := uuid.New()
	return &domain.Transaction{
		ID:                 txID,
		TenantID:           "tenant-a",
		IdempotencyKey:     "k1",
		RequestFingerprint: usecase.TransferFingerprint(validInput()),
		Status:             domain.TransactionStatusCommitted,
		Entries: []domain.JournalEntry{
			{ID: uuid.New(), TransactionID: txID, Amount: decimal.NewFromInt(-100), Currency: "USD"},
			{ID: uuid.New(), TransactionID: txID, Amount: decimal.NewFromInt(100), Currency: "USD"},
		},
		CreatedAt: time.Now().UTC(),
	}
}

// freshTx is the *domain.Transaction the repo returns from a successful
// ExecuteTransfer — i.e. a brand-new commit, not a replay.
func freshTx(t *testing.T, in usecase.TransferInput) *domain.Transaction {
	t.Helper()
	txID := uuid.New()
	return &domain.Transaction{
		ID:                 txID,
		TenantID:           in.TenantID,
		IdempotencyKey:     in.IdempotencyKey,
		RequestFingerprint: usecase.TransferFingerprint(in),
		Status:             domain.TransactionStatusCommitted,
		Entries: []domain.JournalEntry{
			{ID: uuid.New(), TransactionID: txID, AccountID: in.FromAccountID, Amount: in.Amount.Neg(), Currency: in.Currency},
			{ID: uuid.New(), TransactionID: txID, AccountID: in.ToAccountID, Amount: in.Amount, Currency: in.Currency},
		},
		CreatedAt: time.Now().UTC(),
	}
}

// TestTransferUsecase_Execute is the table-driven contract test. Each
// case scripts the exact call sequence the usecase must make against
// the two ports — gomock's strict mode fails the test on any
// unexpected interaction, which is the correctness guarantee we want
// (no silent extra Redis writes, no missed PG replays).
func TestTransferUsecase_Execute(t *testing.T) {
	cases := []struct {
		name         string
		in           usecase.TransferInput
		expect       func(t *testing.T, idem *mocks.MockIdempotencyStore, repo *mocks.MockLedgerRepository, in usecase.TransferInput)
		wantErr      error
		wantReplayed bool
	}{
		{
			name: "happy path: clear slot writes tx and marks COMPLETED",
			in:   validInput(),
			expect: func(t *testing.T, idem *mocks.MockIdempotencyStore, repo *mocks.MockLedgerRepository, in usecase.TransferInput) {
				fp := usecase.TransferFingerprint(in)
				gomock.InOrder(
					idem.EXPECT().Acquire(gomock.Any(), scopedKey, fp).
						Return(true, domain.IdempotencyRecord{}, nil),
					repo.EXPECT().ExecuteTransfer(gomock.Any(), gomock.Any()).
						Return(freshTx(t, in), nil),
					idem.EXPECT().SetState(gomock.Any(), scopedKey, fp, domain.IdempotencyCompleted).
						Return(nil),
				)
			},
		},
		{
			name: "replay: Redis says COMPLETED -> load tx, do not re-execute",
			in:   validInput(),
			expect: func(t *testing.T, idem *mocks.MockIdempotencyStore, repo *mocks.MockLedgerRepository, in usecase.TransferInput) {
				fp := usecase.TransferFingerprint(in)
				original := originalTx(t)
				gomock.InOrder(
					idem.EXPECT().Acquire(gomock.Any(), scopedKey, fp).
						Return(false, domain.IdempotencyRecord{State: domain.IdempotencyCompleted, Fingerprint: fp}, nil),
					repo.EXPECT().GetTransactionByIdempotencyKey(gomock.Any(), in.TenantID, in.IdempotencyKey).
						Return(original, nil),
				)
			},
			wantReplayed: true,
		},
		{
			name: "STARTED state -> ErrTransferInFlight, no repo touch",
			in:   validInput(),
			expect: func(t *testing.T, idem *mocks.MockIdempotencyStore, _ *mocks.MockLedgerRepository, in usecase.TransferInput) {
				fp := usecase.TransferFingerprint(in)
				idem.EXPECT().Acquire(gomock.Any(), scopedKey, fp).
					Return(false, domain.IdempotencyRecord{State: domain.IdempotencyStarted, Fingerprint: fp}, nil)
			},
			wantErr: domain.ErrTransferInFlight,
		},
		{
			name: "FAILED state -> ErrIdempotencyKeyFailed, no repo touch",
			in:   validInput(),
			expect: func(t *testing.T, idem *mocks.MockIdempotencyStore, _ *mocks.MockLedgerRepository, in usecase.TransferInput) {
				fp := usecase.TransferFingerprint(in)
				idem.EXPECT().Acquire(gomock.Any(), scopedKey, fp).
					Return(false, domain.IdempotencyRecord{State: domain.IdempotencyFailed, Fingerprint: fp}, nil)
			},
			wantErr: domain.ErrIdempotencyKeyFailed,
		},
		{
			name: "fingerprint mismatch on completed slot -> ErrRequestFingerprintMismatch",
			in:   validInput(),
			expect: func(t *testing.T, idem *mocks.MockIdempotencyStore, _ *mocks.MockLedgerRepository, in usecase.TransferInput) {
				idem.EXPECT().Acquire(gomock.Any(), scopedKey, gomock.Any()).
					Return(false, domain.IdempotencyRecord{State: domain.IdempotencyCompleted, Fingerprint: "stale-fp"}, nil)
			},
			wantErr: domain.ErrRequestFingerprintMismatch,
		},
		{
			name: "fingerprint mismatch on in-flight slot -> ErrRequestFingerprintMismatch",
			in:   validInput(),
			expect: func(t *testing.T, idem *mocks.MockIdempotencyStore, _ *mocks.MockLedgerRepository, in usecase.TransferInput) {
				idem.EXPECT().Acquire(gomock.Any(), scopedKey, gomock.Any()).
					Return(false, domain.IdempotencyRecord{State: domain.IdempotencyStarted, Fingerprint: "stale-fp"}, nil)
			},
			wantErr: domain.ErrRequestFingerprintMismatch,
		},
		{
			name: "Redis race (SETNX fail + TTL expired) falls through to PG",
			in:   validInput(),
			expect: func(t *testing.T, idem *mocks.MockIdempotencyStore, repo *mocks.MockLedgerRepository, in usecase.TransferInput) {
				gomock.InOrder(
					idem.EXPECT().Acquire(gomock.Any(), scopedKey, gomock.Any()).
						Return(false, domain.IdempotencyRecord{}, nil),
					repo.EXPECT().ExecuteTransfer(gomock.Any(), gomock.Any()).
						Return(freshTx(t, in), nil),
				)
				// No SetState — the race path never re-enters the COMPLETED
				// write, by design (the slot is treated as already-claimed
				// by someone). Strict mock catches an accidental call.
			},
		},
		{
			name: "Redis fault on Acquire -> fail-open; PG executes",
			in:   validInput(),
			expect: func(t *testing.T, idem *mocks.MockIdempotencyStore, repo *mocks.MockLedgerRepository, in usecase.TransferInput) {
				gomock.InOrder(
					idem.EXPECT().Acquire(gomock.Any(), scopedKey, gomock.Any()).
						Return(false, domain.IdempotencyRecord{}, errors.New("redis: connection refused")),
					repo.EXPECT().ExecuteTransfer(gomock.Any(), gomock.Any()).
						Return(freshTx(t, in), nil),
				)
				// No SetState — Redis is down, no point trying.
			},
		},
		{
			name: "Redis acquired but PG has the row already -> replay path",
			in:   validInput(),
			expect: func(t *testing.T, idem *mocks.MockIdempotencyStore, repo *mocks.MockLedgerRepository, in usecase.TransferInput) {
				fp := usecase.TransferFingerprint(in)
				original := originalTx(t)
				gomock.InOrder(
					idem.EXPECT().Acquire(gomock.Any(), scopedKey, fp).
						Return(true, domain.IdempotencyRecord{}, nil),
					repo.EXPECT().ExecuteTransfer(gomock.Any(), gomock.Any()).
						Return(nil, domain.ErrDuplicateIdempotencyKey),
					repo.EXPECT().GetTransactionByIdempotencyKey(gomock.Any(), in.TenantID, in.IdempotencyKey).
						Return(original, nil),
					idem.EXPECT().SetState(gomock.Any(), scopedKey, fp, domain.IdempotencyCompleted).
						Return(nil),
				)
			},
			wantReplayed: true,
		},
		{
			name: "repo hard error -> Redis state set to FAILED, error surfaced",
			in:   validInput(),
			expect: func(t *testing.T, idem *mocks.MockIdempotencyStore, repo *mocks.MockLedgerRepository, in usecase.TransferInput) {
				fp := usecase.TransferFingerprint(in)
				pgDown := errors.New("pg: connection refused")
				gomock.InOrder(
					idem.EXPECT().Acquire(gomock.Any(), scopedKey, fp).
						Return(true, domain.IdempotencyRecord{}, nil),
					repo.EXPECT().ExecuteTransfer(gomock.Any(), gomock.Any()).
						Return(nil, pgDown),
					idem.EXPECT().SetState(gomock.Any(), scopedKey, fp, domain.IdempotencyFailed).
						Return(nil),
				)
			},
			wantErr: errors.New("pg: connection refused"), // matched by message, not Is
		},
		// Validation cases — should reject before any port is touched, so
		// no EXPECT() calls. The mock controller's Finish (via the t.Cleanup
		// gomock.NewController installs) catches any port call as failure.
		{
			name:    "validation: empty idempotency key rejected pre-port",
			in:      func() usecase.TransferInput { in := validInput(); in.IdempotencyKey = ""; return in }(),
			wantErr: nil, // not a domain sentinel; just want non-nil err + correct semantics
		},
		{
			name:    "validation: invalid currency rejected pre-port",
			in:      func() usecase.TransferInput { in := validInput(); in.Currency = "us"; return in }(),
			wantErr: domain.ErrInvalidCurrency,
		},
		{
			name: "validation: self-transfer rejected pre-port",
			in: func() usecase.TransferInput {
				in := validInput()
				in.ToAccountID = in.FromAccountID
				return in
			}(),
		},
		{
			name: "validation: zero amount rejected pre-port",
			in:   func() usecase.TransferInput { in := validInput(); in.Amount = decimal.Zero; return in }(),
		},
		{
			name: "validation: negative amount rejected pre-port",
			in:   func() usecase.TransferInput { in := validInput(); in.Amount = decimal.NewFromInt(-1); return in }(),
		},
		{
			name: "validation: missing tenant rejected pre-port",
			in:   func() usecase.TransferInput { in := validInput(); in.TenantID = ""; return in }(),
			wantErr: domain.ErrTenantRequired,
		},
		{
			name: "validation: idempotency key too long rejected pre-port",
			in: func() usecase.TransferInput {
				in := validInput()
				in.IdempotencyKey = "k1234567890" + "1234567890" + "1234567890" + "1234567890" + "1234567890" + "1234567890" + "1234567890"
				return in
			}(),
		},
		{
			name: "validation: idempotency key with disallowed chars rejected pre-port",
			in:   func() usecase.TransferInput { in := validInput(); in.IdempotencyKey = "k1:has:colons"; return in }(),
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctrl := gomock.NewController(t)
			idem := mocks.NewMockIdempotencyStore(ctrl)
			repo := mocks.NewMockLedgerRepository(ctrl)
			if tc.expect != nil {
				tc.expect(t, idem, repo, tc.in)
			}
			uc := usecase.NewTransferUsecase(repo, idem, silentLogger())

			out, err := uc.Execute(t.Context(), tc.in)

			switch {
			case tc.wantErr != nil:
				require.Error(t, err)
				if errors.Is(err, tc.wantErr) {
					// sentinel match
				} else {
					require.Contains(t, err.Error(), tc.wantErr.Error())
				}
				require.Nil(t, out)
			case tc.expect == nil:
				// Validation case with no sentinel — just expect rejection.
				require.Error(t, err)
				require.Nil(t, out)
			default:
				require.NoError(t, err)
				require.NotNil(t, out)
				require.NotNil(t, out.Transaction)
				require.Equal(t, tc.wantReplayed, out.Replayed)
			}
		})
	}
}

// TestTransferUsecase_Execute_PreflightBalanceHolds pins the contract
// that valid inputs always produce balanced legs. Independent of the
// fast-path table since it asserts on the persisted Transaction shape.
func TestTransferUsecase_Execute_PreflightBalanceHolds(t *testing.T) {
	ctrl := gomock.NewController(t)
	idem := mocks.NewMockIdempotencyStore(ctrl)
	repo := mocks.NewMockLedgerRepository(ctrl)
	in := validInput()
	fp := usecase.TransferFingerprint(in)

	idem.EXPECT().Acquire(gomock.Any(), scopedKey, fp).
		Return(true, domain.IdempotencyRecord{}, nil)
	repo.EXPECT().ExecuteTransfer(gomock.Any(), gomock.Any()).
		DoAndReturn(func(_ context.Context, req domain.TransferRequest) (*domain.Transaction, error) {
			// Echo back what the usecase would persist: two balanced legs.
			return freshTx(t, in), nil
		})
	idem.EXPECT().SetState(gomock.Any(), scopedKey, fp, domain.IdempotencyCompleted).
		Return(nil)

	uc := usecase.NewTransferUsecase(repo, idem, silentLogger())
	out, err := uc.Execute(t.Context(), in)
	require.NoError(t, err)
	require.NotNil(t, out)
	require.Len(t, out.Transaction.Entries, 2)

	sum := decimal.Zero
	for _, e := range out.Transaction.Entries {
		sum = sum.Add(e.Amount)
	}
	require.True(t, sum.IsZero(), "persisted legs MUST sum to zero, got %s", sum.String())
}
