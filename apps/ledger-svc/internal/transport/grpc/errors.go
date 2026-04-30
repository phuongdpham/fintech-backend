// Package grpc is the ledger service's gRPC transport adapter.
//
// Layering rule: handlers translate between protobuf wire types and the
// usecase layer. Domain errors map to gRPC status codes here, in one
// place — adding a domain sentinel without updating the table falls
// through to Internal, which forces an explicit decision.
package grpc

import (
	"errors"
	"log/slog"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/phuongdpham/fintech/apps/ledger-svc/internal/domain"
)

// errorMapping is the exhaustive translation table from domain sentinels
// to gRPC codes. Mirrors the HTTP transport's intent but with gRPC's
// richer code surface (FailedPrecondition vs Conflict semantics).
var errorMapping = []struct {
	target error
	code   codes.Code
}{
	{domain.ErrInvalidCurrency, codes.InvalidArgument},
	{domain.ErrInvalidAccountStatus, codes.InvalidArgument},
	{domain.ErrInvalidTxStatus, codes.InvalidArgument},
	{domain.ErrInvalidOutboxStatus, codes.InvalidArgument},
	{domain.ErrUnbalancedTransaction, codes.InvalidArgument},
	{domain.ErrCurrencyMismatch, codes.InvalidArgument},
	{domain.ErrAccountClosed, codes.FailedPrecondition},
	{domain.ErrAccountFrozen, codes.FailedPrecondition},
	{domain.ErrAccountNotFound, codes.NotFound},
	{domain.ErrTransactionNotFound, codes.NotFound},
	{domain.ErrTransferInFlight, codes.Aborted},
	{domain.ErrIdempotencyKeyFailed, codes.AlreadyExists},
	{domain.ErrDuplicateIdempotencyKey, codes.AlreadyExists},
	{domain.ErrTenantRequired, codes.Unauthenticated},
	{domain.ErrAccountTenantMismatch, codes.PermissionDenied},
}

// asGRPCError translates a domain or wrapped error to a gRPC status error.
// Unknown errors yield codes.Internal with a generic message; the
// underlying detail is logged for operators but not exposed to clients
// (no PII / inference attack surface).
func asGRPCError(log *slog.Logger, err error) error {
	if err == nil {
		return nil
	}
	for _, m := range errorMapping {
		if errors.Is(err, m.target) {
			return status.Error(m.code, err.Error())
		}
	}
	log.Error("unhandled error in gRPC transport", slog.Any("err", err))
	return status.Error(codes.Internal, "internal error")
}
