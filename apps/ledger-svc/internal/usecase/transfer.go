package usecase

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"

	"github.com/google/uuid"

	"github.com/phuongdpham/fintech/apps/ledger-svc/internal/audit"
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

	// (3) Redis idempotency fast-path. Key is tenant-scoped so two tenants
	// can legally use the same caller-supplied string without colliding.
	// Fingerprint binds the slot to this exact request body; a later
	// request with the same key but a different body is rejected.
	scopedKey := scopedIdempotencyKey(in.TenantID, in.IdempotencyKey)
	fingerprint := TransferFingerprint(in)
	acquired, current, err := u.idem.Acquire(ctx, scopedKey, fingerprint)
	if err != nil {
		// Fail-open: Redis fault must not block writes. PG's UNIQUE
		// constraint on (tenant_id, idempotency_key) is the durable
		// barrier; the outbox is unaffected. Log and proceed.
		u.log.WarnContext(ctx, "redis idempotency unavailable; falling through to Postgres",
			slog.String("idempotency_key", in.IdempotencyKey),
			slog.String("tenant_id", in.TenantID),
			slog.Any("err", err))
		return u.executeAndReplayOnDup(ctx, in, fingerprint)
	}

	if !acquired {
		// Fingerprint mismatch is checked before state — even an in-flight
		// request with a different body is wrong, not "still working".
		if current.Fingerprint != "" && current.Fingerprint != fingerprint {
			return nil, domain.ErrRequestFingerprintMismatch
		}
		switch current.State {
		case domain.IdempotencyCompleted:
			return u.replay(ctx, in.TenantID, in.IdempotencyKey, fingerprint)
		case domain.IdempotencyStarted:
			return nil, domain.ErrTransferInFlight
		case domain.IdempotencyFailed:
			return nil, domain.ErrIdempotencyKeyFailed
		case "":
			// Race: TTL expired between SETNX and GET inside the store.
			// Conservative: treat as duplicate, defer to PG to confirm.
			return u.executeAndReplayOnDup(ctx, in, fingerprint)
		default:
			return nil, fmt.Errorf("usecase: unknown idempotency state %q", current.State)
		}
	}

	// (4) Acquired. Run the repository transfer.
	out, err := u.executeAndReplayOnDup(ctx, in, fingerprint)
	if err != nil {
		// (5b) Best-effort failure-state write. Do not surface SetState
		// errors — the original error is what the caller needs to see.
		if setErr := u.idem.SetState(ctx, scopedKey, fingerprint, domain.IdempotencyFailed); setErr != nil {
			u.log.WarnContext(ctx, "failed to mark idempotency state FAILED",
				slog.String("idempotency_key", in.IdempotencyKey),
				slog.String("tenant_id", in.TenantID),
				slog.Any("err", setErr))
		}
		return nil, err
	}

	// (5a) Mark COMPLETED so subsequent retries hit the replay path.
	if setErr := u.idem.SetState(ctx, scopedKey, fingerprint, domain.IdempotencyCompleted); setErr != nil {
		u.log.WarnContext(ctx, "failed to mark idempotency state COMPLETED",
			slog.String("idempotency_key", in.IdempotencyKey),
			slog.String("tenant_id", in.TenantID),
			slog.Any("err", setErr))
		// Intentionally do NOT fail the request. The PG write succeeded;
		// future replays will fall through to PG's UNIQUE constraint and
		// recover via the executeAndReplayOnDup path.
	}
	return out, nil
}

// TransferFingerprint produces the canonical SHA-256 (hex) of the request
// body. Pipe-delimited fields are stable across language runtimes — no
// JSON-canonical ordering trap. Amount.String() preserves NUMERIC(19,4)
// without binary representation drift. SHA-256 is hardware-accelerated
// (SHA-NI) on every modern x86_64 / arm64 server CPU; for ~100 byte input
// this is well under a microsecond.
//
// Exported so test packages can pre-seed fakes with the same canonical
// hash the production path computes. Production callers don't need it
// directly — Execute calls it internally.
func TransferFingerprint(in TransferInput) string {
	canonical := in.TenantID + "|" +
		in.FromAccountID.String() + "|" +
		in.ToAccountID.String() + "|" +
		in.Amount.String() + "|" +
		string(in.Currency) + "|" +
		in.IdempotencyKey
	sum := sha256.Sum256([]byte(canonical))
	return hex.EncodeToString(sum[:])
}

// scopedIdempotencyKey builds the opaque key handed to the Redis store.
// Format: "<tenant>:<key>". The store stays oblivious to tenants; tenant
// isolation lives at the call site so there's exactly one place to audit.
func scopedIdempotencyKey(tenantID, key string) string {
	return tenantID + ":" + key
}

// executeAndReplayOnDup is the path that's safe to call whether or not
// Redis reported a duplicate. On ErrDuplicateIdempotencyKey from PG it
// falls back to a load-by-key replay, which also catches durable
// fingerprint mismatches the Redis fast-path missed.
func (u *TransferUsecase) executeAndReplayOnDup(ctx context.Context, in TransferInput, fingerprint string) (*TransferOutput, error) {
	payload, err := MarshalTransferCommittedV1(in)
	if err != nil {
		return nil, err
	}
	envelope, err := audit.EnvelopeFromContext(ctx)
	if err != nil {
		// Wiring bug: by the time we reach the usecase, EdgeIdentity
		// has populated Claims and RequestID. An empty envelope means
		// a misconfigured interceptor chain — fail loud, not silent.
		return nil, fmt.Errorf("usecase: %w", err)
	}
	req := domain.TransferRequest{
		TenantID:           in.TenantID,
		IdempotencyKey:     in.IdempotencyKey,
		RequestFingerprint: fingerprint,
		FromAccountID:      in.FromAccountID,
		ToAccountID:        in.ToAccountID,
		Amount:             in.Amount,
		Currency:           in.Currency,
		OutboxPayload:      payload,
		AuditEnvelope:      envelope,
	}
	tx, err := u.ledger.ExecuteTransfer(ctx, req)
	if err == nil {
		return &TransferOutput{Transaction: tx, Replayed: false}, nil
	}
	if errors.Is(err, domain.ErrDuplicateIdempotencyKey) {
		return u.replay(ctx, in.TenantID, in.IdempotencyKey, fingerprint)
	}
	return nil, err
}

func (u *TransferUsecase) replay(ctx context.Context, tenantID, key, fingerprint string) (*TransferOutput, error) {
	tx, err := u.ledger.GetTransactionByIdempotencyKey(ctx, tenantID, key)
	if err != nil {
		// Inconsistency: idempotency state says COMPLETED (or PG UNIQUE
		// said duplicate) but the row isn't there. Surface as an error
		// rather than silently swallowing — this is paging-territory.
		return nil, fmt.Errorf("usecase: replay lookup failed: %w", err)
	}
	// Durable fingerprint check — catches mismatches the Redis fast-path
	// missed (Redis was down, or the slot expired between Acquire and
	// the duplicate retry). Same key + different body must NOT replay.
	if tx.RequestFingerprint != "" && tx.RequestFingerprint != fingerprint {
		return nil, domain.ErrRequestFingerprintMismatch
	}
	return &TransferOutput{Transaction: tx, Replayed: true}, nil
}

func buildPreflightTransaction(in TransferInput) *domain.Transaction {
	return &domain.Transaction{
		TenantID:       in.TenantID,
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

// idempotencyKeyMaxLen caps caller-supplied keys at 64 bytes. The plan's
// canonical shape is a UUID (36 chars) or short opaque token; 64 leaves
// headroom without exposing Redis / PG to a megabyte-key DoS.
const idempotencyKeyMaxLen = 64

// idempotencyKeyAllowed restricts the charset to alphanumerics, hyphen,
// and underscore. Two reasons:
//   - We use ':' as the tenant/key separator in scopedIdempotencyKey;
//     a key containing ':' would break that contract.
//   - Excluding control chars and whitespace removes a class of log
//     poisoning and Redis-protocol oddities. UUIDs and URL-safe base64
//     fit this set, which covers every realistic caller shape.
func idempotencyKeyAllowed(s string) bool {
	for i := range len(s) {
		c := s[i]
		switch {
		case c >= '0' && c <= '9':
		case c >= 'a' && c <= 'z':
		case c >= 'A' && c <= 'Z':
		case c == '-' || c == '_':
		default:
			return false
		}
	}
	return true
}

func validateTransferInput(in TransferInput) error {
	if in.TenantID == "" {
		return domain.ErrTenantRequired
	}
	if in.IdempotencyKey == "" {
		return fmt.Errorf("usecase: idempotency_key is required")
	}
	if len(in.IdempotencyKey) > idempotencyKeyMaxLen {
		return fmt.Errorf("usecase: idempotency_key exceeds %d bytes", idempotencyKeyMaxLen)
	}
	if !idempotencyKeyAllowed(in.IdempotencyKey) {
		return fmt.Errorf("usecase: idempotency_key must match [A-Za-z0-9_-]+")
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
