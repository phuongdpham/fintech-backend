package usecase

import (
	"fmt"

	json "github.com/goccy/go-json"
	"github.com/google/uuid"

	"github.com/phuongdpham/fintech/apps/ledger-svc/internal/domain"
)

// TransferCommittedV1 is the schema-versioned outbox payload emitted when
// a Transfer succeeds. It is intentionally minimal — Kafka consumers can
// always join the outbox row's aggregate_id (= the transaction ID) and
// fetch additional detail from the read model.
//
// Versioning: append a new struct (V2 …) when the schema changes. Never
// rename fields or change types in V1; consumers depend on stability.
type TransferCommittedV1 struct {
	Schema         string    `json:"schema"`
	TenantID       string    `json:"tenant_id"`
	IdempotencyKey string    `json:"idempotency_key"`
	FromAccountID  uuid.UUID `json:"from_account_id"`
	ToAccountID    uuid.UUID `json:"to_account_id"`
	// Amount is serialized as a string to preserve NUMERIC(19,4) precision
	// across the JSON boundary; consumers parse with their decimal lib.
	Amount   string `json:"amount"`
	Currency string `json:"currency"`
}

const SchemaTransferCommittedV1 = "fintech.ledger.v1.TransferCommitted"

// MarshalTransferCommittedV1 produces the JSON payload written to the
// outbox row. Pure function — no allocation surprises in the hot path.
func MarshalTransferCommittedV1(in TransferInput) ([]byte, error) {
	evt := TransferCommittedV1{
		Schema:         SchemaTransferCommittedV1,
		TenantID:       in.TenantID,
		IdempotencyKey: in.IdempotencyKey,
		FromAccountID:  in.FromAccountID,
		ToAccountID:    in.ToAccountID,
		Amount:         in.Amount.String(),
		Currency:       string(in.Currency),
	}
	b, err := json.Marshal(evt)
	if err != nil {
		return nil, fmt.Errorf("usecase: marshal TransferCommittedV1: %w", err)
	}
	return b, nil
}

// TransferInput is the public input shape for Transfer.Execute.
// Defined here (not transfer.go) so the codec can refer to it without
// a circular pull.
//
// TenantID is sourced from the authenticated principal (gRPC auth
// interceptor's Claims.Tenant) — never from the request body. The handler
// is responsible for that wiring.
type TransferInput struct {
	TenantID       string
	IdempotencyKey string
	FromAccountID  uuid.UUID
	ToAccountID    uuid.UUID
	Amount         domain.Amount
	Currency       domain.Currency
}
