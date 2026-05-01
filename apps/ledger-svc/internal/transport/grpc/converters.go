package grpc

import (
	"github.com/google/uuid"
	"google.golang.org/genproto/googleapis/rpc/errdetails"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/phuongdpham/fintech/apps/ledger-svc/internal/domain"
	"github.com/phuongdpham/fintech/apps/ledger-svc/internal/usecase"
	pb "github.com/phuongdpham/fintech/libs/go/proto-gen/fintech/ledger/v1"
)

// parseUUID converts a string field to uuid.UUID, returning a gRPC
// InvalidArgument error with field-level detail on parse failure.
// Field-level errors are part of the contract — clients can surface
// them to humans via google.rpc.BadRequest.FieldViolations.
func parseUUID(field, raw string) (uuid.UUID, error) {
	id, err := uuid.Parse(raw)
	if err != nil {
		return uuid.Nil, fieldViolation(field, "must be a valid UUID")
	}
	return id, nil
}

func parseAmount(field, raw string) (domain.Amount, error) {
	a, err := domain.NewAmount(raw)
	if err != nil {
		return domain.Amount{}, fieldViolation(field, "must be a decimal string (e.g. \"100.0000\")")
	}
	return a, nil
}

// transferRequestFromPB validates and converts the pb input to the
// usecase's input shape. Stops at the first error so clients fix one
// thing at a time — easier than batching all violations for a tiny API.
func transferRequestFromPB(req *pb.TransferRequest) (usecase.TransferInput, error) {
	if req == nil {
		return usecase.TransferInput{}, status.Error(codes.InvalidArgument, "request is required")
	}
	from, err := parseUUID("from_account_id", req.GetFromAccountId())
	if err != nil {
		return usecase.TransferInput{}, err
	}
	to, err := parseUUID("to_account_id", req.GetToAccountId())
	if err != nil {
		return usecase.TransferInput{}, err
	}
	amount, err := parseAmount("amount", req.GetAmount())
	if err != nil {
		return usecase.TransferInput{}, err
	}
	return usecase.TransferInput{
		IdempotencyKey: req.GetIdempotencyKey(),
		FromAccountID:  from,
		ToAccountID:    to,
		Amount:         amount,
		Currency:       domain.Currency(req.GetCurrency()),
	}, nil
}

// transactionToPB is the lossless inverse of the proto Transaction.
// Decimals are serialized as strings to preserve NUMERIC(19,4) precision.
func transactionToPB(tx *domain.Transaction) *pb.Transaction {
	if tx == nil {
		return nil
	}
	out := &pb.Transaction{
		Id:             tx.ID.String(),
		IdempotencyKey: tx.IdempotencyKey,
		Status:         string(tx.Status),
		CreatedAt:      timestamppb.New(tx.CreatedAt),
		Entries:        make([]*pb.JournalEntry, len(tx.Entries)),
	}
	for i, e := range tx.Entries {
		out.Entries[i] = &pb.JournalEntry{
			Id:            e.ID.String(),
			TransactionId: e.TransactionID.String(),
			AccountId:     e.AccountID.String(),
			Amount:        e.Amount.String(),
			Currency:      string(e.Currency),
			CreatedAt:     timestamppb.New(e.CreatedAt),
		}
	}
	return out
}

// fieldViolation builds a status.Error carrying a structured BadRequest
// detail. Clients that consume gRPC properly (e.g. our future BFF) can
// surface field-level messages to UIs without parsing strings.
func fieldViolation(field, description string) error {
	st := status.New(codes.InvalidArgument, "invalid request: "+field+": "+description)
	det, err := st.WithDetails(&errdetails.BadRequest{
		FieldViolations: []*errdetails.BadRequest_FieldViolation{
			{Field: field, Description: description},
		},
	})
	if err != nil {
		return st.Err()
	}
	return det.Err()
}
