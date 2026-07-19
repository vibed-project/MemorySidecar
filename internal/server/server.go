package server

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"sync"

	"go.opentelemetry.io/contrib/instrumentation/google.golang.org/grpc/otelgrpc"
	"go.opentelemetry.io/otel/metric"
	"google.golang.org/grpc"
	"google.golang.org/grpc/health"
	healthpb "google.golang.org/grpc/health/grpc_health_v1"
	"google.golang.org/grpc/reflection"

	adminv1 "memsidecar/gen/memsidecar/admin/v1"
	artifactv1 "memsidecar/gen/memsidecar/artifact/v1"
	episodicv1 "memsidecar/gen/memsidecar/episodic/v1"
	graphv1 "memsidecar/gen/memsidecar/graph/v1"
	kvv1 "memsidecar/gen/memsidecar/kv/v1"
	leasev1 "memsidecar/gen/memsidecar/lease/v1"
	semanticv1 "memsidecar/gen/memsidecar/semantic/v1"
	"memsidecar/internal/admin"
	"memsidecar/internal/artifact"
	"memsidecar/internal/auth"
	"memsidecar/internal/config"
	"memsidecar/internal/episodic"
	"memsidecar/internal/graph"
	"memsidecar/internal/interceptor"
	"memsidecar/internal/kv"
	"memsidecar/internal/lease"
	"memsidecar/internal/policy"
	"memsidecar/internal/semantic"
)

// Deps is everything Server needs that lives outside its lifetime.
type Deps struct {
	Logger        *slog.Logger
	Verifier      auth.TokenVerifier
	Policy        policy.Engine
	MeterProvider metric.MeterProvider
	KV            *kv.Registry
	Episodic      *episodic.Registry
	Semantic      *semantic.Registry
	Artifact      *artifact.Registry
	Lease         *lease.Registry
	Graph         *graph.Registry
	// Admin is the optional cross-namespace introspection service. When nil the
	// Admin RPC is not served.
	Admin *admin.Service
}

// Server owns the gRPC server and its listeners.
type Server struct {
	cfg       config.ServerConfig
	log       *slog.Logger
	grpcSrv   *grpc.Server
	listeners []net.Listener
	udsPath   string
}

func New(cfg config.ServerConfig, deps Deps) (*Server, error) {
	if deps.Logger == nil {
		return nil, errors.New("server: Logger required")
	}
	if deps.Verifier == nil {
		return nil, errors.New("server: Verifier required")
	}
	if deps.KV == nil {
		return nil, errors.New("server: KV registry required")
	}
	if deps.Policy == nil {
		deps.Policy = policy.NoopEngine{}
	}

	// Interceptor chain order: recovery → observability → auth → policy → service.
	unaryChain := []grpc.UnaryServerInterceptor{
		interceptor.RecoveryUnary(deps.Logger),
		interceptor.ObservabilityUnary(deps.Logger, deps.MeterProvider),
		interceptor.AuthUnary(deps.Verifier),
		interceptor.PolicyUnary(deps.Policy),
	}
	streamChain := []grpc.StreamServerInterceptor{
		interceptor.RecoveryStream(deps.Logger),
		interceptor.ObservabilityStream(deps.Logger, deps.MeterProvider),
		interceptor.AuthStream(deps.Verifier),
		interceptor.PolicyStream(deps.Policy),
	}

	statsOpts := []otelgrpc.Option{}
	if deps.MeterProvider != nil {
		statsOpts = append(statsOpts, otelgrpc.WithMeterProvider(deps.MeterProvider))
	}
	statsHandler := otelgrpc.NewServerHandler(statsOpts...)
	grpcSrv := grpc.NewServer(
		grpc.ChainUnaryInterceptor(unaryChain...),
		grpc.ChainStreamInterceptor(streamChain...),
		grpc.StatsHandler(statsHandler),
	)

	kvv1.RegisterKVServer(grpcSrv, kv.NewService(deps.KV))
	if deps.Episodic != nil {
		episodicv1.RegisterEpisodicServer(grpcSrv, episodic.NewService(deps.Episodic))
	}
	if deps.Semantic != nil {
		semanticv1.RegisterSemanticServer(grpcSrv, semantic.NewService(deps.Semantic))
	}
	if deps.Artifact != nil {
		artifactv1.RegisterArtifactServer(grpcSrv, artifact.NewService(deps.Artifact))
	}
	if deps.Lease != nil {
		leasev1.RegisterLeaseServer(grpcSrv, lease.NewService(deps.Lease))
	}
	if deps.Graph != nil {
		graphv1.RegisterGraphServer(grpcSrv, graph.NewService(deps.Graph))
	}
	if deps.Admin != nil {
		adminv1.RegisterAdminServer(grpcSrv, deps.Admin)
	}

	hs := health.NewServer()
	hs.SetServingStatus("memsidecar.kv.v1.KV", healthpb.HealthCheckResponse_SERVING)
	if deps.Episodic != nil {
		hs.SetServingStatus("memsidecar.episodic.v1.Episodic", healthpb.HealthCheckResponse_SERVING)
	}
	if deps.Semantic != nil {
		hs.SetServingStatus("memsidecar.semantic.v1.Semantic", healthpb.HealthCheckResponse_SERVING)
	}
	if deps.Artifact != nil {
		hs.SetServingStatus("memsidecar.artifact.v1.Artifact", healthpb.HealthCheckResponse_SERVING)
	}
	if deps.Lease != nil {
		hs.SetServingStatus("memsidecar.lease.v1.Lease", healthpb.HealthCheckResponse_SERVING)
	}
	if deps.Graph != nil {
		hs.SetServingStatus("memsidecar.graph.v1.Graph", healthpb.HealthCheckResponse_SERVING)
	}
	if deps.Admin != nil {
		hs.SetServingStatus("memsidecar.admin.v1.Admin", healthpb.HealthCheckResponse_SERVING)
	}
	hs.SetServingStatus("", healthpb.HealthCheckResponse_SERVING)
	healthpb.RegisterHealthServer(grpcSrv, hs)

	reflection.Register(grpcSrv)

	return &Server{cfg: cfg, log: deps.Logger, grpcSrv: grpcSrv}, nil
}

// Start opens listeners and begins serving in background goroutines. Returns
// the first listener error encountered via the returned error channel.
func (s *Server) Start() (<-chan error, error) {
	errs := make(chan error, 2)
	var serveWG sync.WaitGroup

	if s.cfg.GRPC.TCP != "" {
		lis, err := net.Listen("tcp", s.cfg.GRPC.TCP)
		if err != nil {
			return nil, fmt.Errorf("listen tcp %s: %w", s.cfg.GRPC.TCP, err)
		}
		tlsMode := "plaintext"
		if s.cfg.GRPC.TLS != nil && s.cfg.GRPC.TLS.CertFile != "" {
			tlsCfg, err := buildServerTLS(s.cfg.GRPC.TLS)
			if err != nil {
				_ = lis.Close()
				return nil, fmt.Errorf("tcp tls: %w", err)
			}
			lis = tls.NewListener(lis, tlsCfg)
			tlsMode = "tls"
			if s.cfg.GRPC.TLS.ClientCAFile != "" {
				tlsMode = "mtls"
			}
		}
		s.listeners = append(s.listeners, lis)
		s.log.Info(
			"listening",
			slog.String("transport", "tcp"),
			slog.String("addr", s.cfg.GRPC.TCP),
			slog.String("security", tlsMode),
		)
		serveWG.Add(1)
		go func() {
			defer serveWG.Done()
			if err := s.grpcSrv.Serve(lis); err != nil {
				errs <- fmt.Errorf("tcp serve: %w", err)
			}
		}()
	}

	if s.cfg.GRPC.UDS != "" {
		path := s.cfg.GRPC.UDS
		if err := prepareUDS(path); err != nil {
			return nil, fmt.Errorf("prepare uds %s: %w", path, err)
		}
		lis, err := net.Listen("unix", path)
		if err != nil {
			return nil, fmt.Errorf("listen unix %s: %w", path, err)
		}
		if err := os.Chmod(path, 0o660); err != nil { //nolint:gosec // UDS needs group access; 0660 is intentional
			s.log.Warn("chmod uds", slog.String("path", path), slog.String("err", err.Error()))
		}
		s.listeners = append(s.listeners, lis)
		s.udsPath = path
		s.log.Info("listening", slog.String("transport", "unix"), slog.String("addr", path))
		serveWG.Add(1)
		go func() {
			defer serveWG.Done()
			if err := s.grpcSrv.Serve(lis); err != nil {
				errs <- fmt.Errorf("uds serve: %w", err)
			}
		}()
	}

	if len(s.listeners) == 0 {
		return nil, errors.New("server: no listeners configured")
	}

	go func() { serveWG.Wait(); close(errs) }()
	return errs, nil
}

// ServeListener serves on the given listener in a background goroutine. It
// exists so tests can plug in an in-memory transport (e.g. bufconn) without
// going through the network. Shutdown still tears the server down cleanly.
func (s *Server) ServeListener(lis net.Listener) <-chan error {
	errs := make(chan error, 1)
	s.listeners = append(s.listeners, lis)
	go func() {
		defer close(errs)
		if err := s.grpcSrv.Serve(lis); err != nil {
			errs <- err
		}
	}()
	return errs
}

// Shutdown gracefully stops the gRPC server. If ctx is cancelled before
// shutdown completes, the server is force-stopped.
func (s *Server) Shutdown(ctx context.Context) error {
	done := make(chan struct{})
	go func() {
		s.grpcSrv.GracefulStop()
		close(done)
	}()
	select {
	case <-done:
	case <-ctx.Done():
		s.grpcSrv.Stop()
	}
	if s.udsPath != "" {
		_ = os.Remove(s.udsPath)
	}
	return nil
}

// buildServerTLS loads the server cert+key and optionally the client CA
// bundle, returning a *tls.Config suitable for tls.NewListener.
//
// When ClientCAFile is set, ClientAuth is RequireAndVerifyClientCert if
// RequireClientCert is true, otherwise VerifyClientCertIfGiven.
func buildServerTLS(cfg *config.TLSConfig) (*tls.Config, error) {
	if cfg.CertFile == "" || cfg.KeyFile == "" {
		return nil, errors.New("tls: cert_file and key_file required")
	}
	cert, err := tls.LoadX509KeyPair(cfg.CertFile, cfg.KeyFile)
	if err != nil {
		return nil, fmt.Errorf("load keypair: %w", err)
	}
	out := &tls.Config{
		Certificates: []tls.Certificate{cert},
		MinVersion:   tls.VersionTLS13,
		// gRPC requires h2 to be negotiated via ALPN. Without this, recent
		// grpc-go versions fail the handshake with "missing selected ALPN
		// property" (see grpc/grpc-go#434).
		NextProtos: []string{"h2"},
	}
	if cfg.ClientCAFile != "" {
		caPEM, err := os.ReadFile(cfg.ClientCAFile)
		if err != nil {
			return nil, fmt.Errorf("read client_ca_file: %w", err)
		}
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(caPEM) {
			return nil, fmt.Errorf("client_ca_file: no PEM blocks parsed")
		}
		out.ClientCAs = pool
		if cfg.RequireClientCert {
			out.ClientAuth = tls.RequireAndVerifyClientCert
		} else {
			out.ClientAuth = tls.VerifyClientCertIfGiven
		}
	}
	return out, nil
}

func prepareUDS(path string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil { //nolint:gosec // parent dir for the UDS socket; 0755 is intentional
		return err
	}
	if _, err := os.Stat(path); err == nil {
		if err := os.Remove(path); err != nil {
			return err
		}
	}
	return nil
}
