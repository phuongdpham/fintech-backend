package grpc

import (
	"context"
	"log/slog"

	gogrpc "google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"

	"github.com/phuongdpham/fintech/apps/ledger-svc/internal/domain"
	"github.com/phuongdpham/fintech/apps/ledger-svc/internal/transport/grpc/interceptors"
	"github.com/phuongdpham/fintech/apps/ledger-svc/internal/usecase"
	pb "github.com/phuongdpham/fintech/libs/go/proto-gen/fintech/ledger/v1"
)

// LedgerHandler implements pb.LedgerServiceServer.
//
// It is intentionally thin: parse → call usecase / repo → serialize.
// Any business logic here is a smell — push it down into usecase or
// domain.
type LedgerHandler struct {
	pb.UnimplementedLedgerServiceServer
	transfer *usecase.TransferUsecase
	ledger   domain.LedgerRepository
	log      *slog.Logger
}

func NewLedgerHandler(transfer *usecase.TransferUsecase, ledger domain.LedgerRepository, log *slog.Logger) *LedgerHandler {
	if log == nil {
		log = slog.Default()
	}
	return &LedgerHandler{transfer: transfer, ledger: ledger, log: log}
}

// MetadataKeyReplayed is the gRPC trailer set when the response is the
// replay of a previously-committed transaction. Clients can dedup or
// log differently based on this signal.
const MetadataKeyReplayed = "x-idempotent-replayed"

func (h *LedgerHandler) Transfer(ctx context.Context, req *pb.TransferRequest) (*pb.TransferResponse, error) {
	in, err := transferRequestFromPB(req)
	if err != nil {
		// transferRequestFromPB already returns a properly-formed
		// status.Error; pass through verbatim.
		return nil, err
	}

	// Tenant comes from authenticated claims, never from the request body.
	// This is what isolates one tenant's idempotency namespace from another.
	tenant := tenantFromClaims(ctx)
	if tenant == "" {
		return nil, status.Error(codes.Unauthenticated, "missing or empty tenant claim")
	}
	in.TenantID = tenant

	out, err := h.transfer.Execute(ctx, in)
	if err != nil {
		return nil, asGRPCError(h.log, err)
	}

	if out.Replayed {
		// Trailer (not header) — written at end of unary response, after
		// the handler returns. SetTrailer is the canonical way to do
		// this on a unary RPC.
		_ = grpcSetTrailer(ctx, MetadataKeyReplayed, "true")
	}

	return &pb.TransferResponse{
		Transaction: transactionToPB(out.Transaction),
		Replayed:    out.Replayed,
	}, nil
}

func (h *LedgerHandler) GetTransaction(ctx context.Context, req *pb.GetTransactionRequest) (*pb.GetTransactionResponse, error) {
	if req == nil {
		return nil, asGRPCError(h.log, domain.ErrTransactionNotFound)
	}
	id, err := parseUUID("id", req.GetId())
	if err != nil {
		return nil, err
	}
	tx, err := h.ledger.GetTransaction(ctx, id)
	if err != nil {
		return nil, asGRPCError(h.log, err)
	}
	return &pb.GetTransactionResponse{Transaction: transactionToPB(tx)}, nil
}

// grpcSetTrailer is a thin wrapper so the handler isn't littered with
// metadata.Pairs/SetTrailer noise. Returns the underlying error for
// optional handling — failure to set a trailer is a server-side bug,
// not a client-facing one.
func grpcSetTrailer(ctx context.Context, key, value string) error {
	return gogrpc.SetTrailer(ctx, metadata.Pairs(key, value))
}

// tenantFromClaims pulls the authenticated tenant from request context.
// Returns "" when the auth interceptor didn't populate claims (anonymous
// or unauthenticated request) — the caller is responsible for rejecting
// those with codes.Unauthenticated.
func tenantFromClaims(ctx context.Context) string {
	c := interceptors.ClaimsFromContext(ctx)
	if c == nil {
		return ""
	}
	return c.Tenant
}
