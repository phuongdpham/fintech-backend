package usecase

import (
	"fmt"

	"github.com/google/uuid"
	"google.golang.org/protobuf/proto"

	"github.com/phuongdpham/fintech/apps/ledger-svc/internal/domain"
	pb "github.com/phuongdpham/fintech/libs/go/proto-gen/fintech/ledger/v1"
)

// SchemaTransferCommittedV1 names the proto message we emit as the
// outbox payload. Stored alongside the bytes in outbox_events.event_schema
// so consumers can route without parsing the payload first.
const SchemaTransferCommittedV1 = "fintech.ledger.v1.TransferCommittedV1"

// MarshalTransferCommittedV1 produces the proto-serialized bytes
// written to the outbox row's BYTEA column. The worker forwards
// these bytes to Kafka without re-encoding — one marshal, zero parse.
//
// Pre-issue-10 this was JSON; the swap eliminates ~1 MB/s of JSON
// marshal at 10K TPS while keeping the wire shape consumers expect
// (the proto-gen TransferCommittedV1 fields are name-aligned with the
// previous JSON).
func MarshalTransferCommittedV1(in TransferInput) ([]byte, error) {
	evt := &pb.TransferCommittedV1{
		TenantId:       in.TenantID,
		IdempotencyKey: in.IdempotencyKey,
		FromAccountId:  in.FromAccountID.String(),
		ToAccountId:    in.ToAccountID.String(),
		Amount:         in.Amount.String(),
		Currency:       string(in.Currency),
	}
	b, err := proto.Marshal(evt)
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
