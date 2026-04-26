package grpc

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"time"

	"go.opentelemetry.io/contrib/instrumentation/google.golang.org/grpc/otelgrpc"
	gogrpc "google.golang.org/grpc"
	"google.golang.org/grpc/health"
	healthpb "google.golang.org/grpc/health/grpc_health_v1"
	"google.golang.org/grpc/keepalive"
	"google.golang.org/grpc/reflection"

	"github.com/phuongdpham/fintech/apps/ledger-svc/internal/transport/grpc/interceptors"
	pb "github.com/phuongdpham/fintech/libs/go/proto-gen/fintech/ledger/v1"
)

// ServerConfig is the surface ops tunes. Defaults match expected
// k8s networking — keepalive aggressive enough to detect dead peers
// inside the typical 60s dataplane idle timeout.
type ServerConfig struct {
	Addr           string        // host:port the gRPC server binds
	MaxRecvMsgSize int           // default 4MiB; override for large bulk RPCs
	KeepaliveTime  time.Duration // server-side ping every N (default 30s)
	Auth           interceptors.AuthConfig
}

func defaultServerConfig() ServerConfig {
	return ServerConfig{
		Addr:           ":9090",
		MaxRecvMsgSize: 4 * 1024 * 1024,
		KeepaliveTime:  30 * time.Second,
	}
}

// Server is the long-running gRPC listener. Lifecycle:
//
//	s := New(...)
//	go func() { _ = s.Serve(ctx) }()
//	... wait for shutdown signal ...
//	s.GracefulStop(shutdownCtx)
type Server struct {
	cfg       ServerConfig
	grpc      *gogrpc.Server
	health    *health.Server
	listener  net.Listener
	log       *slog.Logger
}

// New builds the gRPC server, registers the LedgerService + standard
// health-check service, and binds the listener so it's ready to Serve.
//
// We bind here (rather than in Serve) so the caller learns about port
// conflicts at boot, not at the start of Serve in a goroutine.
func New(cfg ServerConfig, handler pb.LedgerServiceServer, log *slog.Logger) (*Server, error) {
	def := defaultServerConfig()
	if cfg.Addr == "" {
		cfg.Addr = def.Addr
	}
	if cfg.MaxRecvMsgSize == 0 {
		cfg.MaxRecvMsgSize = def.MaxRecvMsgSize
	}
	if cfg.KeepaliveTime == 0 {
		cfg.KeepaliveTime = def.KeepaliveTime
	}
	if log == nil {
		log = slog.Default()
	}

	lis, err := net.Listen("tcp", cfg.Addr)
	if err != nil {
		return nil, fmt.Errorf("grpc: listen %s: %w", cfg.Addr, err)
	}

	// Interceptor chain — order matters. Recovery is outer-most so a panic
	// in any interceptor below still produces a clean Internal status.
	// RequestID then Logging give downstream interceptors + the handler a
	// stable correlation id and a per-call slog.Logger via context. Auth
	// runs last before the handler so it can log via the per-call logger.
	if cfg.Auth.PublicMethods == nil {
		cfg.Auth.PublicMethods = interceptors.DefaultPublicMethods()
	}
	chain := gogrpc.ChainUnaryInterceptor(
		interceptors.Recovery(log),
		interceptors.RequestID(),
		interceptors.Logging(log),
		interceptors.Auth(cfg.Auth),
	)

	srv := gogrpc.NewServer(
		chain,
		// otelgrpc as a StatsHandler is the OTel-recommended path post-v0.50:
		// it sees headers + metadata that pure interceptors miss and works
		// for streaming RPCs without a parallel chain. Lives outside the
		// ChainUnaryInterceptor stack on purpose. Uses the global
		// TracerProvider (configured by internal/observability.Init).
		gogrpc.StatsHandler(otelgrpc.NewServerHandler()),
		gogrpc.MaxRecvMsgSize(cfg.MaxRecvMsgSize),
		gogrpc.KeepaliveParams(keepalive.ServerParameters{
			Time:    cfg.KeepaliveTime,
			Timeout: 10 * time.Second,
		}),
		gogrpc.KeepaliveEnforcementPolicy(keepalive.EnforcementPolicy{
			MinTime:             10 * time.Second,
			PermitWithoutStream: true,
		}),
	)
	pb.RegisterLedgerServiceServer(srv, handler)

	hs := health.NewServer()
	healthpb.RegisterHealthServer(srv, hs)
	hs.SetServingStatus("", healthpb.HealthCheckResponse_SERVING)
	hs.SetServingStatus(pb.LedgerService_ServiceDesc.ServiceName, healthpb.HealthCheckResponse_SERVING)

	// Reflection enables grpcurl / clients to introspect; SAFE in private
	// service-to-service mesh, REMOVE if exposing publicly without auth.
	reflection.Register(srv)

	return &Server{
		cfg:      cfg,
		grpc:     srv,
		health:   hs,
		listener: lis,
		log:      log,
	}, nil
}

// Addr returns the actually-bound address (useful for tests when cfg.Addr
// was ":0" — we resolve to a real port at Listen time).
func (s *Server) Addr() string { return s.listener.Addr().String() }

// SetNotServing flips the health-check serving status. Wire to readiness
// probes during shutdown so the load balancer drains before stop.
func (s *Server) SetNotServing() {
	s.health.Shutdown()
}

// Serve blocks until the listener fails or GracefulStop is invoked.
func (s *Server) Serve() error {
	s.log.Info("grpc server serving", slog.String("addr", s.cfg.Addr))
	return s.grpc.Serve(s.listener)
}

// GracefulStop drains in-flight RPCs, honoring the deadline of ctx; if
// ctx is cancelled before drain completes it falls through to a hard
// Stop. Idiomatic two-phase shutdown.
func (s *Server) GracefulStop(ctx context.Context) {
	done := make(chan struct{})
	go func() {
		s.grpc.GracefulStop()
		close(done)
	}()
	select {
	case <-done:
	case <-ctx.Done():
		s.log.Warn("grpc graceful stop timed out; forcing", slog.Any("err", ctx.Err()))
		s.grpc.Stop()
		<-done
	}
}
