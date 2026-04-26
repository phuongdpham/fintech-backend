package usecase

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"github.com/google/uuid"

	"github.com/phuongdpham/fintech/apps/ledger-svc/internal/domain"
)

// TransferUsecase orchestrates a single ledger transfer end-to-end:
//
//	1. Validate input.
//	2. Pre-flight balance assertion (Debit + Credit == 0).
//	3. Atomic Redis SETNX on the idempotency key.
//	4. Repository.ExecuteTransfer — atomic PG tx that writes the
//	   transaction, both journal_entries, AND the outbox event.
//	5. Update Redis state (COMPLETED / FAILED).
//
// Replay path: on duplicate (Redis says COMPLETED, OR PG UNIQUE caught a
// late duplicate) the original transaction is loaded and returned with
// Output.Replayed = true. Caller must accept that response as-is —
// retrying with the same key is a no-op by design.
type TransferUsecase struct {
	ledger domain.LedgerRepository
	idem   domain.IdempotencyStore
	log    *slog.Logger
}

func NewTransferUsecase(ledger domain.LedgerRepository, idem domain.IdempotencyStore, log *slog.Logger) *TransferUsecase {
	if log == nil {
		log = slog.Default()
	}
	return &TransferUsecase{ledger: ledger, idem: idem, log: log}
}

// TransferOutput is what callers receive on a successful Execute.
// Replayed=true means this response was reconstructed from a prior
// committed transaction; the underlying PG row was NOT re-written.
type TransferOutput struct {
	Transaction *domain.Transaction
	Replayed    bool
}

// Execute runs the full transfer flow. See TransferUsecase docstring for
// the step-by-step contract. Returns:
//
//	*TransferOutput, nil   on success (fresh or replayed)
//	nil, ErrTransferInFlight       prior request still STARTED
//	nil, ErrIdempotencyKeyFailed   prior request terminated FAILED
//	nil, ErrUnbalancedTransaction  pre-flight balance check failed
//	nil, <validation err>          input rejected
//	nil, <wrapped err>             upstream (PG / Redis) error not absorbed
func (u *TransferUsecase) Execute(ctx context.Context, in TransferInput) (*TransferOutput, error) {
	if err := validateTransferInput(in); err != nil {
		return nil, err
	}

	// (2) Pre-flight balance assertion — defense in depth before I/O.
	//
	// We construct the *exact* in-memory transaction that the repository
	// will persist (debit on FROM, credit on TO, zero-sum), then call the
	// domain invariant directly. For a 2-leg transfer this is tautological,
	// but the assertion lives here for two reasons:
	//   a. The invariant is a usecase-layer responsibility per the plan.
	//   b. Future Transfer variants (fee splits, FX legs) WILL break the
	//      tautology — keeping the call site unchanged means the invariant
	//      doesn't have to be re-introduced later.
	preflight := buildPreflightTransaction(in)
	if err := preflight.AssertBalanced(); err != nil {
		return nil, err
	}

	// (3) Redis idempotency fast-path.
	acquired, state, err := u.idem.Acquire(ctx, in.IdempotencyKey)
	if err != nil {
		// Fail-open: Redis fault must not block writes. PG's UNIQUE
		// constraint on transactions.idempotency_key is the durable
		// barrier; the outbox is unaffected. Log and proceed.
		u.log.WarnContext(ctx, "redis idempotency unavailable; falling through to Postgres",
			slog.String("idempotency_key", in.IdempotencyKey),
			slog.Any("err", err))
		return u.executeAndReplayOnDup(ctx, in)
	}

	if !acquired {
		switch state {
		case domain.IdempotencyCompleted:
			return u.replay(ctx, in.IdempotencyKey)
		case domain.IdempotencyStarted:
			return nil, domain.ErrTransferInFlight
		case domain.IdempotencyFailed:
			return nil, domain.ErrIdempotencyKeyFailed
		case "":
			// Race: TTL expired between SETNX and GET inside the store.
			// Conservative: treat as duplicate, defer to PG to confirm.
			return u.executeAndReplayOnDup(ctx, in)
		default:
			return nil, fmt.Errorf("usecase: unknown idempotency state %q", state)
		}
	}

	// (4) Acquired. Run the repository transfer.
	out, err := u.executeAndReplayOnDup(ctx, in)
	if err != nil {
		// (5b) Best-effort failure-state write. Do not surface SetState
		// errors — the original error is what the caller needs to see.
		if setErr := u.idem.SetState(ctx, in.IdempotencyKey, domain.IdempotencyFailed); setErr != nil {
			u.log.WarnContext(ctx, "failed to mark idempotency state FAILED",
				slog.String("idempotency_key", in.IdempotencyKey),
				slog.Any("err", setErr))
		}
		return nil, err
	}

	// (5a) Mark COMPLETED so subsequent retries hit the replay path.
	if setErr := u.idem.SetState(ctx, in.IdempotencyKey, domain.IdempotencyCompleted); setErr != nil {
		u.log.WarnContext(ctx, "failed to mark idempotency state COMPLETED",
			slog.String("idempotency_key", in.IdempotencyKey),
			slog.Any("err", setErr))
		// Intentionally do NOT fail the request. The PG write succeeded;
		// future replays will fall through to PG's UNIQUE constraint and
		// recover via the executeAndReplayOnDup path.
	}
	return out, nil
}

// executeAndReplayOnDup is the path that's safe to call whether or not
// Redis reported a duplicate. On ErrDuplicateIdempotencyKey from PG it
// falls back to a load-by-key replay.
func (u *TransferUsecase) executeAndReplayOnDup(ctx context.Context, in TransferInput) (*TransferOutput, error) {
	payload, err := MarshalTransferCommittedV1(in)
	if err != nil {
		return nil, err
	}
	req := domain.TransferRequest{
		IdempotencyKey: in.IdempotencyKey,
		FromAccountID:  in.FromAccountID,
		ToAccountID:    in.ToAccountID,
		Amount:         in.Amount,
		Currency:       in.Currency,
		OutboxPayload:  payload,
	}
	tx, err := u.ledger.ExecuteTransfer(ctx, req)
	if err == nil {
		return &TransferOutput{Transaction: tx, Replayed: false}, nil
	}
	if errors.Is(err, domain.ErrDuplicateIdempotencyKey) {
		return u.replay(ctx, in.IdempotencyKey)
	}
	return nil, err
}

func (u *TransferUsecase) replay(ctx context.Context, key string) (*TransferOutput, error) {
	tx, err := u.ledger.GetTransactionByIdempotencyKey(ctx, key)
	if err != nil {
		// Inconsistency: idempotency state says COMPLETED (or PG UNIQUE
		// said duplicate) but the row isn't there. Surface as an error
		// rather than silently swallowing — this is paging-territory.
		return nil, fmt.Errorf("usecase: replay lookup failed: %w", err)
	}
	return &TransferOutput{Transaction: tx, Replayed: true}, nil
}

func buildPreflightTransaction(in TransferInput) *domain.Transaction {
	return &domain.Transaction{
		IdempotencyKey: in.IdempotencyKey,
		Status:         domain.TransactionStatusCommitted,
		Entries: []domain.JournalEntry{
			{
				AccountID: in.FromAccountID,
				Amount:    in.Amount.Neg(),
				Currency:  in.Currency,
			},
			{
				AccountID: in.ToAccountID,
				Amount:    in.Amount,
				Currency:  in.Currency,
			},
		},
	}
}

func validateTransferInput(in TransferInput) error {
	if in.IdempotencyKey == "" {
		return fmt.Errorf("usecase: idempotency_key is required")
	}
	if !in.Currency.Valid() {
		return domain.ErrInvalidCurrency
	}
	if in.FromAccountID == uuid.Nil || in.ToAccountID == uuid.Nil {
		return domain.ErrAccountNotFound
	}
	if in.FromAccountID == in.ToAccountID {
		return fmt.Errorf("usecase: from and to account IDs must differ")
	}
	if in.Amount.IsZero() || in.Amount.IsNegative() {
		return fmt.Errorf("usecase: transfer amount must be strictly positive")
	}
	return nil
}
