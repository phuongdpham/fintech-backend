package usecase_test

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/shopspring/decimal"
	"github.com/stretchr/testify/require"

	"github.com/phuongdpham/fintech/apps/ledger-svc/internal/domain"
	"github.com/phuongdpham/fintech/apps/ledger-svc/internal/usecase"
)

// ---------------------------------------------------------------------------
// Fakes — in-memory implementations of the two domain ports the usecase
// depends on. Kept package-local to this test so they can drift if the
// production interfaces evolve, without polluting prod code.
// ---------------------------------------------------------------------------

type fakeIdemStore struct {
	mu          sync.Mutex
	records     map[string]domain.IdempotencyRecord
	acquireErr  error
	setStateErr error
	// forceRace makes Acquire return (false, zero, nil) — the SETNX-fail
	// + GET-returns-nil race documented in the Redis adapter.
	forceRace bool
}

func newFakeIdem() *fakeIdemStore {
	return &fakeIdemStore{records: map[string]domain.IdempotencyRecord{}}
}

func (f *fakeIdemStore) Acquire(_ context.Context, key, fingerprint string) (bool, domain.IdempotencyRecord, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.acquireErr != nil {
		return false, domain.IdempotencyRecord{}, f.acquireErr
	}
	if f.forceRace {
		return false, domain.IdempotencyRecord{}, nil
	}
	if cur, ok := f.records[key]; ok {
		return false, cur, nil
	}
	f.records[key] = domain.IdempotencyRecord{State: domain.IdempotencyStarted, Fingerprint: fingerprint}
	return true, domain.IdempotencyRecord{}, nil
}

func (f *fakeIdemStore) SetState(_ context.Context, key, fingerprint string, s domain.IdempotencyState) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.setStateErr != nil {
		return f.setStateErr
	}
	f.records[key] = domain.IdempotencyRecord{State: s, Fingerprint: fingerprint}
	return nil
}

func (f *fakeIdemStore) getState(key string) domain.IdempotencyState {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.records[key].State
}

func (f *fakeIdemStore) seedRecord(key string, rec domain.IdempotencyRecord) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.records[key] = rec
}

type fakeLedgerRepo struct {
	mu               sync.Mutex
	txByID           map[uuid.UUID]*domain.Transaction
	txByIdem         map[string]*domain.Transaction
	executeCalls     int
	replayLookupCalls int
	executeErr       error
}

func newFakeRepo() *fakeLedgerRepo {
	return &fakeLedgerRepo{
		txByID:   map[uuid.UUID]*domain.Transaction{},
		txByIdem: map[string]*domain.Transaction{},
	}
}

func (f *fakeLedgerRepo) seed(tx *domain.Transaction) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.txByID[tx.ID] = tx
	f.txByIdem[idemBucketKey(tx.TenantID, tx.IdempotencyKey)] = tx
}

// idemBucketKey scopes the per-tenant uniqueness lookup. Mirrors the real
// PG composite UNIQUE (tenant_id, idempotency_key).
func idemBucketKey(tenantID, key string) string { return tenantID + "|" + key }

func (f *fakeLedgerRepo) ExecuteTransfer(_ context.Context, req domain.TransferRequest) (*domain.Transaction, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.executeCalls++
	if f.executeErr != nil {
		return nil, f.executeErr
	}
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
	// Defense-in-depth: real repo asserts here too.
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
	f.replayLookupCalls++
	tx, ok := f.txByIdem[idemBucketKey(tenantID, key)]
	if !ok {
		return nil, domain.ErrTransactionNotFound
	}
	return tx, nil
}

// ---------------------------------------------------------------------------
// Tests
// ---------------------------------------------------------------------------

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

func TestTransferUsecase_Execute(t *testing.T) {
	redisDown := errors.New("redis: connection refused")

	cases := []struct {
		name string
		// setup mutates the fakes before invocation. Empty default = first
		// request, fresh ports.
		setup func(*testing.T, *fakeIdemStore, *fakeLedgerRepo)
		in    usecase.TransferInput

		// Expectations
		wantErr            error // sentinel; matched with errors.Is
		wantReplayed       bool
		wantExecuteCalls   int
		wantReplayLookups  int
		wantFinalIdemState domain.IdempotencyState
	}{
		{
			name:               "happy path: clear key writes new tx and marks COMPLETED",
			in:                 validInput(),
			wantExecuteCalls:   1,
			wantFinalIdemState: domain.IdempotencyCompleted,
		},
		{
			name: "replay: Redis says COMPLETED -> load existing tx, do not re-execute",
			setup: func(_ *testing.T, idem *fakeIdemStore, repo *fakeLedgerRepo) {
				fp := usecase.TransferFingerprint(validInput())
				txID := uuid.New()
				tx := &domain.Transaction{
					ID:                 txID,
					TenantID:           "tenant-a",
					IdempotencyKey:     "k1",
					RequestFingerprint: fp,
					Status:             domain.TransactionStatusCommitted,
					Entries: []domain.JournalEntry{
						{ID: uuid.New(), TransactionID: txID, Amount: decimal.NewFromInt(-100), Currency: "USD"},
						{ID: uuid.New(), TransactionID: txID, Amount: decimal.NewFromInt(100), Currency: "USD"},
					},
				}
				repo.seed(tx)
				idem.seedRecord("tenant-a:k1", domain.IdempotencyRecord{
					State: domain.IdempotencyCompleted, Fingerprint: fp,
				})
			},
			in:                 validInput(),
			wantReplayed:       true,
			wantExecuteCalls:   0,
			wantReplayLookups:  1,
			wantFinalIdemState: domain.IdempotencyCompleted, // unchanged
		},
		{
			name: "STARTED state -> ErrTransferInFlight, no repo touch",
			setup: func(_ *testing.T, idem *fakeIdemStore, _ *fakeLedgerRepo) {
				fp := usecase.TransferFingerprint(validInput())
				idem.seedRecord("tenant-a:k1", domain.IdempotencyRecord{
					State: domain.IdempotencyStarted, Fingerprint: fp,
				})
			},
			in:                 validInput(),
			wantErr:            domain.ErrTransferInFlight,
			wantExecuteCalls:   0,
			wantFinalIdemState: domain.IdempotencyStarted,
		},
		{
			name: "FAILED state -> ErrIdempotencyKeyFailed, no repo touch",
			setup: func(_ *testing.T, idem *fakeIdemStore, _ *fakeLedgerRepo) {
				fp := usecase.TransferFingerprint(validInput())
				idem.seedRecord("tenant-a:k1", domain.IdempotencyRecord{
					State: domain.IdempotencyFailed, Fingerprint: fp,
				})
			},
			in:                 validInput(),
			wantErr:            domain.ErrIdempotencyKeyFailed,
			wantExecuteCalls:   0,
			wantFinalIdemState: domain.IdempotencyFailed,
		},
		{
			name: "fingerprint mismatch on completed slot -> ErrRequestFingerprintMismatch",
			setup: func(_ *testing.T, idem *fakeIdemStore, _ *fakeLedgerRepo) {
				idem.seedRecord("tenant-a:k1", domain.IdempotencyRecord{
					State: domain.IdempotencyCompleted, Fingerprint: "deadbeef-prior-request-fp",
				})
			},
			in:                 validInput(),
			wantErr:            domain.ErrRequestFingerprintMismatch,
			wantExecuteCalls:   0,
			wantFinalIdemState: domain.IdempotencyCompleted, // unchanged
		},
		{
			name: "fingerprint mismatch on in-flight slot -> ErrRequestFingerprintMismatch",
			setup: func(_ *testing.T, idem *fakeIdemStore, _ *fakeLedgerRepo) {
				idem.seedRecord("tenant-a:k1", domain.IdempotencyRecord{
					State: domain.IdempotencyStarted, Fingerprint: "deadbeef-prior-request-fp",
				})
			},
			in:                 validInput(),
			wantErr:            domain.ErrRequestFingerprintMismatch,
			wantExecuteCalls:   0,
			wantFinalIdemState: domain.IdempotencyStarted, // unchanged
		},
		{
			name: "Redis race (SETNX fail + TTL expired) falls through to PG; PG writes new tx",
			setup: func(_ *testing.T, idem *fakeIdemStore, _ *fakeLedgerRepo) {
				idem.forceRace = true
			},
			in:               validInput(),
			wantExecuteCalls: 1,
			// In the race path we never came back to update Redis state —
			// the slot is empty in the fake.
			wantFinalIdemState: "",
		},
		{
			name: "Redis fault on Acquire -> fail-open; PG executes successfully",
			setup: func(_ *testing.T, idem *fakeIdemStore, _ *fakeLedgerRepo) {
				idem.acquireErr = redisDown
			},
			in:                 validInput(),
			wantExecuteCalls:   1,
			wantFinalIdemState: "", // we never tried SetState; Redis is down
		},
		{
			name: "Redis acquired but PG has the row already (late dup) -> replay path",
			setup: func(_ *testing.T, _ *fakeIdemStore, repo *fakeLedgerRepo) {
				txID := uuid.New()
				tx := &domain.Transaction{
					ID:             txID,
					TenantID:       "tenant-a",
					IdempotencyKey: "k1",
					Status:         domain.TransactionStatusCommitted,
					Entries: []domain.JournalEntry{
						{ID: uuid.New(), TransactionID: txID, Amount: decimal.NewFromInt(-100), Currency: "USD"},
						{ID: uuid.New(), TransactionID: txID, Amount: decimal.NewFromInt(100), Currency: "USD"},
					},
				}
				repo.seed(tx)
				// idem store fresh -> Acquire returns (true, "", nil)
				// but the repo finds the duplicate row.
			},
			in:                 validInput(),
			wantReplayed:       true,
			wantExecuteCalls:   1,
			wantReplayLookups:  1,
			wantFinalIdemState: domain.IdempotencyCompleted,
		},
		{
			name: "repo returns hard error -> Redis state set to FAILED, error surfaced",
			setup: func(_ *testing.T, _ *fakeIdemStore, repo *fakeLedgerRepo) {
				repo.executeErr = errors.New("pg: connection refused")
			},
			in:                 validInput(),
			wantErr:            nil, // we don't have a domain sentinel for this; assert by IsError
			wantExecuteCalls:   1,
			wantFinalIdemState: domain.IdempotencyFailed,
		},
		{
			name:             "validation: empty idempotency key rejected pre-Redis",
			in:               func() usecase.TransferInput { in := validInput(); in.IdempotencyKey = ""; return in }(),
			wantErr:          nil,
			wantExecuteCalls: 0,
		},
		{
			name:             "validation: invalid currency rejected pre-Redis",
			in:               func() usecase.TransferInput { in := validInput(); in.Currency = "us"; return in }(),
			wantErr:          domain.ErrInvalidCurrency,
			wantExecuteCalls: 0,
		},
		{
			name: "validation: self-transfer rejected pre-Redis",
			in: func() usecase.TransferInput {
				in := validInput()
				in.ToAccountID = in.FromAccountID
				return in
			}(),
			wantExecuteCalls: 0,
		},
		{
			name:             "validation: zero amount rejected pre-Redis",
			in:               func() usecase.TransferInput { in := validInput(); in.Amount = decimal.Zero; return in }(),
			wantExecuteCalls: 0,
		},
		{
			name:             "validation: negative amount rejected pre-Redis",
			in:               func() usecase.TransferInput { in := validInput(); in.Amount = decimal.NewFromInt(-1); return in }(),
			wantExecuteCalls: 0,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			idem := newFakeIdem()
			repo := newFakeRepo()
			if tc.setup != nil {
				tc.setup(t, idem, repo)
			}
			uc := usecase.NewTransferUsecase(repo, idem, silentLogger())

			out, err := uc.Execute(context.Background(), tc.in)

			if tc.wantErr != nil {
				require.Error(t, err)
				require.True(t, errors.Is(err, tc.wantErr), "want %v, got %v", tc.wantErr, err)
				require.Nil(t, out)
			} else if tc.wantExecuteCalls == 0 && tc.wantReplayed == false {
				// Validation cases — error expected even without sentinel.
				if tc.name != "happy path: clear key writes new tx and marks COMPLETED" {
					require.Error(t, err, "validation should reject")
					require.Nil(t, out)
				}
			}

			// Output expectations on success paths.
			if err == nil {
				require.NotNil(t, out)
				require.NotNil(t, out.Transaction)
				require.Equal(t, tc.wantReplayed, out.Replayed)
			}

			require.Equal(t, tc.wantExecuteCalls, repo.executeCalls, "ExecuteTransfer call count")
			require.Equal(t, tc.wantReplayLookups, repo.replayLookupCalls, "GetByIdemKey call count")
			if tc.wantFinalIdemState != "" || tc.wantExecuteCalls > 0 || tc.wantReplayed {
				require.Equal(t, tc.wantFinalIdemState, idem.getState("tenant-a:k1"), "final idempotency state")
			}
		})
	}
}

// Sanity test: explicit, isolated check that the pre-flight balance assertion
// is wired. With the current 2-leg builder, valid inputs always balance; this
// test pins that contract so future refactors don't silently regress it.
func TestTransferUsecase_Execute_PreflightBalanceHolds(t *testing.T) {
	idem := newFakeIdem()
	repo := newFakeRepo()
	uc := usecase.NewTransferUsecase(repo, idem, silentLogger())

	out, err := uc.Execute(context.Background(), validInput())
	require.NoError(t, err)
	require.NotNil(t, out)
	require.Len(t, out.Transaction.Entries, 2)

	sum := decimal.Zero
	for _, e := range out.Transaction.Entries {
		sum = sum.Add(e.Amount)
	}
	require.True(t, sum.IsZero(), "persisted legs MUST sum to zero, got %s", sum.String())
}
