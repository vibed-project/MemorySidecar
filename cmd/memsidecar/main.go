package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"memsidecar/internal/artifact"
	artfsdrv "memsidecar/internal/artifact/drivers/fs"
	artmemdrv "memsidecar/internal/artifact/drivers/memory"
	arts3drv "memsidecar/internal/artifact/drivers/s3"
	"memsidecar/internal/auth"
	"memsidecar/internal/config"
	"memsidecar/internal/episodic"
	epmemdrv "memsidecar/internal/episodic/drivers/memory"
	eppgdrv "memsidecar/internal/episodic/drivers/postgres"
	"memsidecar/internal/graph"
	graphmemdrv "memsidecar/internal/graph/drivers/memory"
	"memsidecar/internal/kv"
	memdrv "memsidecar/internal/kv/drivers/memory"
	pgdrv "memsidecar/internal/kv/drivers/postgres"
	"memsidecar/internal/lease"
	leasememdrv "memsidecar/internal/lease/drivers/memory"
	leasepgdrv "memsidecar/internal/lease/drivers/postgres"
	"memsidecar/internal/obs"
	"memsidecar/internal/policy"
	"memsidecar/internal/semantic"
	semmemdrv "memsidecar/internal/semantic/drivers/memory"
	sempgdrv "memsidecar/internal/semantic/drivers/postgres"
	"memsidecar/internal/semantic/embedder"
	embollama "memsidecar/internal/semantic/embedder/ollama"
	embopenai "memsidecar/internal/semantic/embedder/openai"
	"memsidecar/internal/server"
	"memsidecar/internal/version"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "memsidecar:", err)
		os.Exit(1)
	}
}

func run() error {
	cfgPath := flag.String("config", "", "path to YAML config file")
	showVersion := flag.Bool("version", false, "print version and exit")
	flag.Parse()

	if *showVersion {
		fmt.Println("memsidecar", version.String())
		return nil
	}
	if *cfgPath == "" {
		return fmt.Errorf("--config required")
	}

	cfg, err := config.Load(*cfgPath)
	if err != nil {
		return err
	}

	logSetup, err := obs.NewLogger(cfg.Observability.Logging)
	if err != nil {
		return err
	}
	log := logSetup.Logger
	slog.SetDefault(log)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	shutdownTracing, err := obs.SetupTracing(ctx, cfg.Observability.Tracing)
	if err != nil {
		return err
	}
	defer func() {
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = shutdownTracing(shutdownCtx)
	}()

	metrics, err := obs.SetupMetrics(cfg.Observability.Metrics)
	if err != nil {
		return fmt.Errorf("metrics: %w", err)
	}
	defer func() {
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = metrics.Shutdown(shutdownCtx)
	}()

	var metricsServer *http.Server
	metricsServerErr := make(chan error, 1)
	if metrics.HTTPHandler != nil {
		mux := http.NewServeMux()
		mux.Handle(metrics.Path, metrics.HTTPHandler)
		metricsServer = &http.Server{
			Addr:              metrics.Addr,
			Handler:           mux,
			ReadHeaderTimeout: 5 * time.Second,
		}
		log.Info(
			"listening",
			slog.String("transport", "http"),
			slog.String("purpose", "metrics"),
			slog.String("addr", metrics.Addr),
			slog.String("path", metrics.Path),
		)
		go func() {
			if err := metricsServer.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
				metricsServerErr <- err
			}
		}()
	}

	verifier, err := auth.NewVerifier(cfg.Auth)
	if err != nil {
		return fmt.Errorf("auth: %w", err)
	}
	verifierHolder := auth.NewVerifierHolder(verifier)

	evict := obs.NewEvictionCounter()

	kvReg, err := buildKVRegistry(cfg, evict)
	if err != nil {
		return fmt.Errorf("kv registry: %w", err)
	}
	defer func() { _ = kvReg.Close() }()

	epReg, err := buildEpisodicRegistry(cfg)
	if err != nil {
		_ = kvReg.Close()
		return fmt.Errorf("episodic registry: %w", err)
	}
	defer func() { _ = epReg.Close() }()

	semReg, err := buildSemanticRegistry(ctx, cfg)
	if err != nil {
		_ = kvReg.Close()
		_ = epReg.Close()
		return fmt.Errorf("semantic registry: %w", err)
	}
	defer func() { _ = semReg.Close() }()

	artReg, err := buildArtifactRegistry(ctx, cfg)
	if err != nil {
		_ = kvReg.Close()
		_ = epReg.Close()
		_ = semReg.Close()
		return fmt.Errorf("artifact registry: %w", err)
	}
	defer func() { _ = artReg.Close() }()

	leaseReg, err := buildLeaseRegistry(ctx, cfg)
	if err != nil {
		_ = kvReg.Close()
		_ = epReg.Close()
		_ = semReg.Close()
		_ = artReg.Close()
		return fmt.Errorf("lease registry: %w", err)
	}
	defer func() { _ = leaseReg.Close() }()

	graphReg, err := buildGraphRegistry(cfg)
	if err != nil {
		_ = kvReg.Close()
		_ = epReg.Close()
		_ = semReg.Close()
		_ = artReg.Close()
		_ = leaseReg.Close()
		return fmt.Errorf("graph registry: %w", err)
	}
	defer func() { _ = graphReg.Close() }()

	// Namespace-growth gauge (memsidecar.namespace.items) over every block's
	// registry. Best-effort: a registration error must not stop startup.
	if err := obs.RegisterNamespaceItemsGauge([]obs.NamespaceItemSource{
		{Block: "kv", Items: kvReg.NamespaceItems},
		{Block: "episodic", Items: epReg.NamespaceItems},
		{Block: "semantic", Items: semReg.NamespaceItems},
		{Block: "artifact", Items: artReg.NamespaceItems},
		{Block: "lease", Items: leaseReg.NamespaceItems},
		{Block: "graph", Items: graphReg.NamespaceItems},
	}); err != nil {
		log.Warn("register namespace.items gauge", slog.String("error", err.Error()))
	}

	polEngine, err := buildPolicyEngine(cfg.Policy)
	if err != nil {
		_ = kvReg.Close()
		_ = epReg.Close()
		_ = semReg.Close()
		return fmt.Errorf("policy: %w", err)
	}
	policyHolder := policy.NewHolder(polEngine)

	srv, err := server.New(cfg.Server, server.Deps{
		Logger:        log,
		Verifier:      verifierHolder,
		Policy:        policyHolder,
		MeterProvider: metrics.MeterProvider,
		KV:            kvReg,
		Episodic:      epReg,
		Semantic:      semReg,
		Artifact:      artReg,
		Lease:         leaseReg,
		Graph:         graphReg,
	})
	if err != nil {
		return err
	}

	serveErrs, err := srv.Start()
	if err != nil {
		return err
	}

	var (
		gatewayServer    *http.Server
		gatewayServerErr = make(chan error, 1)
	)
	if cfg.Server.HTTP.Addr != "" {
		grpcDial := cfg.Server.GRPC.TCP
		if grpcDial == "" {
			grpcDial = cfg.Server.GRPC.UDS
		}
		if grpcDial == "" {
			return fmt.Errorf("server.http.addr set but no grpc listener configured")
		}
		mux, err := server.NewHTTPGateway(ctx, grpcDial)
		if err != nil {
			return fmt.Errorf("http gateway: %w", err)
		}
		gatewayServer = &http.Server{
			Addr:              cfg.Server.HTTP.Addr,
			Handler:           mux,
			ReadHeaderTimeout: 5 * time.Second,
		}
		log.Info(
			"listening",
			slog.String("transport", "http"),
			slog.String("purpose", "gateway"),
			slog.String("addr", cfg.Server.HTTP.Addr),
		)
		go func() {
			if err := gatewayServer.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
				gatewayServerErr <- err
			}
		}()
	}

	// SIGHUP triggers a hot reload of the subsystems that can be swapped
	// safely while serving: policy rules, the auth verifier (e.g. PASETO key
	// rotation), and the log level. Backends, namespaces, listeners, and
	// observability exporters require a full restart.
	hup := make(chan os.Signal, 1)
	signal.Notify(hup, syscall.SIGHUP)
	defer signal.Stop(hup)
	go func() {
		for range hup {
			if err := reloadConfig(*cfgPath, verifierHolder, policyHolder, logSetup.Level, log); err != nil {
				log.Error("reload failed", slog.String("err", err.Error()))
			}
		}
	}()

	log.Info("memsidecar ready",
		slog.String("version", version.Version),
		slog.String("commit", version.Commit))

	select {
	case <-ctx.Done():
		log.Info("shutting down")
	case err := <-serveErrs:
		if err != nil {
			log.Error("serve error", slog.String("err", err.Error()))
			stop()
		}
	case err := <-metricsServerErr:
		if err != nil {
			log.Error("metrics serve error", slog.String("err", err.Error()))
			stop()
		}
	case err := <-gatewayServerErr:
		if err != nil {
			log.Error("gateway serve error", slog.String("err", err.Error()))
			stop()
		}
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), cfg.Server.ShutdownTimeout)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		return err
	}
	if metricsServer != nil {
		_ = metricsServer.Shutdown(shutdownCtx)
	}
	if gatewayServer != nil {
		_ = gatewayServer.Shutdown(shutdownCtx)
	}
	log.Info("stopped")
	return nil
}

// neededBackends returns the names of backends referenced by namespaces of
// the given block.
func neededBackends(cfg *config.Config, block string) map[string]bool {
	out := make(map[string]bool)
	for _, ns := range cfg.Namespaces {
		if ns.Block == block {
			out[ns.Backend] = true
		}
	}
	return out
}

func findBackend(cfg *config.Config, name string) (config.BackendConfig, bool) {
	for _, b := range cfg.Backends {
		if b.Name == name {
			return b, true
		}
	}
	return config.BackendConfig{}, false
}

func buildKVRegistry(cfg *config.Config, evict *obs.EvictionCounter) (*kv.Registry, error) {
	reg := kv.NewRegistry()
	drivers := make(map[string]kv.Driver)
	for name := range neededBackends(cfg, "kv") {
		b, ok := findBackend(cfg, name)
		if !ok {
			return nil, fmt.Errorf("namespace references unknown backend %q", name)
		}
		switch b.Driver {
		case "memory":
			drivers[b.Name] = memdrv.New(memdrv.Options{
				OnEvict:        func(ns, cause string, n int) { evict.Add("kv", ns, cause, int64(n)) },
				AccessPolicies: kvAccessPolicies(cfg, b.Name),
			})
		case "postgres":
			d, err := newKVPostgresDriver(b)
			if err != nil {
				return nil, fmt.Errorf("backend %q: %w", b.Name, err)
			}
			drivers[b.Name] = d
		default:
			return nil, fmt.Errorf("backend %q: unknown driver %q", b.Name, b.Driver)
		}
	}
	used := make(map[string]bool)
	for _, ns := range cfg.Namespaces {
		if ns.Block != "kv" {
			continue
		}
		d := drivers[ns.Backend]
		if used[ns.Backend] {
			if err := reg.BindShared(ns.Name, d); err != nil {
				return nil, err
			}
		} else {
			if err := reg.Bind(ns.Name, d); err != nil {
				return nil, err
			}
			used[ns.Backend] = true
		}
	}
	return reg, nil
}

// kvAccessPolicies gathers the opt-in cache-tier policy (U5) for each kv
// namespace bound to backend. Only namespaces with an enabled policy appear;
// the in-memory driver is the only one that honours it.
func kvAccessPolicies(cfg *config.Config, backend string) map[string]kv.AccessPolicy {
	var out map[string]kv.AccessPolicy
	for _, ns := range cfg.Namespaces {
		if ns.Block != "kv" || ns.Backend != backend {
			continue
		}
		ac := ns.Access
		pol := kv.AccessPolicy{
			Track:        ac.Track,
			SlideTTL:     time.Duration(ac.SlideTTLSeconds) * time.Second,
			Capacity:     ac.Capacity,
			HeatHalfLife: time.Duration(ac.HeatHalfLifeSeconds) * time.Second,
		}
		if !pol.Enabled() {
			continue
		}
		if out == nil {
			out = make(map[string]kv.AccessPolicy)
		}
		out[ns.Name] = pol
	}
	return out
}

func buildEpisodicRegistry(cfg *config.Config) (*episodic.Registry, error) {
	reg := episodic.NewRegistry()
	drivers := make(map[string]episodic.Driver)
	for name := range neededBackends(cfg, "episodic") {
		b, ok := findBackend(cfg, name)
		if !ok {
			return nil, fmt.Errorf("namespace references unknown backend %q", name)
		}
		switch b.Driver {
		case "memory":
			drivers[b.Name] = epmemdrv.New(epmemdrv.Options{})
		case "postgres":
			d, err := newEpisodicPostgresDriver(b)
			if err != nil {
				return nil, fmt.Errorf("backend %q: %w", b.Name, err)
			}
			drivers[b.Name] = d
		default:
			return nil, fmt.Errorf("backend %q: unknown driver %q", b.Name, b.Driver)
		}
	}
	used := make(map[string]bool)
	for _, ns := range cfg.Namespaces {
		if ns.Block != "episodic" {
			continue
		}
		d := drivers[ns.Backend]
		if used[ns.Backend] {
			if err := reg.BindShared(ns.Name, d); err != nil {
				return nil, err
			}
		} else {
			if err := reg.Bind(ns.Name, d); err != nil {
				return nil, err
			}
			used[ns.Backend] = true
		}
	}
	return reg, nil
}

func resolveDSN(b config.BackendConfig) (string, error) {
	dsn := stringOpt(b.Options, "dsn")
	if dsn == "" {
		if env := stringOpt(b.Options, "dsn_env"); env != "" {
			dsn = os.Getenv(env)
		}
	}
	if dsn == "" {
		return "", fmt.Errorf("postgres: dsn (or dsn_env) required")
	}
	return dsn, nil
}

func newKVPostgresDriver(b config.BackendConfig) (*pgdrv.Driver, error) {
	dsn, err := resolveDSN(b)
	if err != nil {
		return nil, err
	}
	maxConns := int32(intOpt(b.Options, "max_conns", 10))
	sweeper := durationOpt(b.Options, "sweeper_interval", 5*time.Minute)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	return pgdrv.New(ctx, pgdrv.Options{
		DSN:             dsn,
		MaxConns:        maxConns,
		SweeperInterval: sweeper,
	})
}

func buildSemanticRegistry(ctx context.Context, cfg *config.Config) (*semantic.Registry, error) {
	reg := semantic.NewRegistry()
	for _, ns := range cfg.Namespaces {
		if ns.Block != "semantic" {
			continue
		}
		b, ok := findBackend(cfg, ns.Backend)
		if !ok {
			return nil, fmt.Errorf("namespace %q references unknown backend %q", ns.Name, ns.Backend)
		}
		emb, err := buildEmbedder(ns.Embedder)
		if err != nil {
			return nil, fmt.Errorf("namespace %q: embedder: %w", ns.Name, err)
		}
		emb = semantic.NewCachingEmbedder(emb, semantic.CacheOptions{
			Namespace: ns.Name,
			Model:     ns.Embedder.Model,
			Capacity:  ns.Embedder.CacheSize,
		})
		var drv semantic.Driver
		switch b.Driver {
		case "memory":
			drv, err = semmemdrv.New(semmemdrv.Options{Dimensions: ns.Embedder.Dimensions})
		case "postgres":
			dsn, dsnErr := resolveDSN(b)
			if dsnErr != nil {
				return nil, fmt.Errorf("backend %q: %w", b.Name, dsnErr)
			}
			drv, err = sempgdrv.New(ctx, sempgdrv.Options{
				DSN:        dsn,
				MaxConns:   int32(intOpt(b.Options, "max_conns", 10)),
				Namespace:  ns.Name,
				Dimensions: ns.Embedder.Dimensions,
				TextSearch: ns.TextSearch,
			})
		default:
			return nil, fmt.Errorf("backend %q: unknown driver %q", b.Name, b.Driver)
		}
		if err != nil {
			return nil, fmt.Errorf("namespace %q: %w", ns.Name, err)
		}
		if err := reg.Bind(ns.Name, semantic.BoundNamespace{Driver: drv, Embedder: emb}); err != nil {
			return nil, err
		}
	}
	return reg, nil
}

// reloadConfig re-reads the YAML config from disk and atomically swaps the
// subsystems that can be hot-reloaded: the auth verifier, the policy engine,
// and the log level. Listener bindings, observability exporters, and the
// driver registries are NOT touched — those still require a full restart.
//
// Returns nil on success even if some non-reloadable fields drifted; the
// caller logs the outcome.
func reloadConfig(
	cfgPath string,
	vh *auth.VerifierHolder,
	ph *policy.Holder,
	level *slog.LevelVar,
	log *slog.Logger,
) error {
	cfg, err := config.Load(cfgPath)
	if err != nil {
		return fmt.Errorf("load: %w", err)
	}

	newVerifier, err := auth.NewVerifier(cfg.Auth)
	if err != nil {
		return fmt.Errorf("auth: %w", err)
	}
	newEngine, err := buildPolicyEngine(cfg.Policy)
	if err != nil {
		return fmt.Errorf("policy: %w", err)
	}
	newLevel, err := obs.ParseLevel(cfg.Observability.Logging.Level)
	if err != nil {
		return fmt.Errorf("logging: %w", err)
	}

	vh.Swap(newVerifier)
	ph.Swap(newEngine)
	level.Set(newLevel)

	log.Info(
		"reloaded",
		slog.String("policy_default", string(cfg.Policy.Default)),
		slog.Int("policy_rules", len(cfg.Policy.Rules)),
		slog.String("verifier", cfg.Auth.Verifier),
		slog.String("log_level", cfg.Observability.Logging.Level),
	)
	return nil
}

func buildLeaseRegistry(ctx context.Context, cfg *config.Config) (*lease.Registry, error) {
	reg := lease.NewRegistry()
	drivers := make(map[string]lease.Driver)
	for name := range neededBackends(cfg, "lease") {
		b, ok := findBackend(cfg, name)
		if !ok {
			return nil, fmt.Errorf("namespace references unknown backend %q", name)
		}
		switch b.Driver {
		case "memory":
			drivers[b.Name] = leasememdrv.New(leasememdrv.Options{})
		case "postgres":
			dsn, err := resolveDSN(b)
			if err != nil {
				return nil, fmt.Errorf("backend %q: %w", b.Name, err)
			}
			d, err := leasepgdrv.New(ctx, leasepgdrv.Options{
				DSN:          dsn,
				MaxConns:     int32(intOpt(b.Options, "max_conns", 10)),
				PollInterval: durationOpt(b.Options, "poll_interval", 100*time.Millisecond),
			})
			if err != nil {
				return nil, fmt.Errorf("backend %q: %w", b.Name, err)
			}
			drivers[b.Name] = d
		default:
			return nil, fmt.Errorf("backend %q: driver %q not supported for lease block", b.Name, b.Driver)
		}
	}
	used := make(map[string]bool)
	for _, ns := range cfg.Namespaces {
		if ns.Block != "lease" {
			continue
		}
		d := drivers[ns.Backend]
		if used[ns.Backend] {
			if err := reg.BindShared(ns.Name, d); err != nil {
				return nil, err
			}
		} else {
			if err := reg.Bind(ns.Name, d); err != nil {
				return nil, err
			}
			used[ns.Backend] = true
		}
	}
	return reg, nil
}

func buildGraphRegistry(cfg *config.Config) (*graph.Registry, error) {
	reg := graph.NewRegistry()
	drivers := make(map[string]graph.Driver)
	for name := range neededBackends(cfg, "graph") {
		b, ok := findBackend(cfg, name)
		if !ok {
			return nil, fmt.Errorf("namespace references unknown backend %q", name)
		}
		switch b.Driver {
		case "memory":
			drivers[b.Name] = graphmemdrv.New(graphmemdrv.Options{})
		default:
			return nil, fmt.Errorf("backend %q: driver %q not supported for graph block (only memory today)", b.Name, b.Driver)
		}
	}
	used := make(map[string]bool)
	for _, ns := range cfg.Namespaces {
		if ns.Block != "graph" {
			continue
		}
		d := drivers[ns.Backend]
		if used[ns.Backend] {
			if err := reg.BindShared(ns.Name, d); err != nil {
				return nil, err
			}
		} else {
			if err := reg.Bind(ns.Name, d); err != nil {
				return nil, err
			}
			used[ns.Backend] = true
		}
	}
	return reg, nil
}

func buildArtifactRegistry(ctx context.Context, cfg *config.Config) (*artifact.Registry, error) {
	reg := artifact.NewRegistry()
	drivers := make(map[string]artifact.Driver)
	for name := range neededBackends(cfg, "artifact") {
		b, ok := findBackend(cfg, name)
		if !ok {
			return nil, fmt.Errorf("namespace references unknown backend %q", name)
		}
		switch b.Driver {
		case "memory":
			drivers[b.Name] = artmemdrv.New(artmemdrv.Options{})
		case "fs":
			d, err := artfsdrv.New(artfsdrv.Options{BaseDir: stringOpt(b.Options, "base_dir")})
			if err != nil {
				return nil, fmt.Errorf("backend %q: %w", b.Name, err)
			}
			drivers[b.Name] = d
		case "s3":
			d, err := newArtifactS3Driver(ctx, b)
			if err != nil {
				return nil, fmt.Errorf("backend %q: %w", b.Name, err)
			}
			drivers[b.Name] = d
		default:
			return nil, fmt.Errorf("backend %q: driver %q not supported for artifact block", b.Name, b.Driver)
		}
	}
	used := make(map[string]bool)
	for _, ns := range cfg.Namespaces {
		if ns.Block != "artifact" {
			continue
		}
		d := drivers[ns.Backend]
		if used[ns.Backend] {
			if err := reg.BindShared(ns.Name, d); err != nil {
				return nil, err
			}
		} else {
			if err := reg.Bind(ns.Name, d); err != nil {
				return nil, err
			}
			used[ns.Backend] = true
		}
	}
	return reg, nil
}

func newArtifactS3Driver(ctx context.Context, b config.BackendConfig) (*arts3drv.Driver, error) {
	access := stringOpt(b.Options, "access_key")
	if access == "" {
		access = os.Getenv(stringOpt(b.Options, "access_key_env"))
	}
	secret := stringOpt(b.Options, "secret_key")
	if secret == "" {
		secret = os.Getenv(stringOpt(b.Options, "secret_key_env"))
	}
	return arts3drv.New(ctx, arts3drv.Options{
		Endpoint:  stringOpt(b.Options, "endpoint"),
		UseSSL:    boolOpt(b.Options, "use_ssl", false),
		Region:    stringOpt(b.Options, "region"),
		Bucket:    stringOpt(b.Options, "bucket"),
		Prefix:    stringOpt(b.Options, "prefix"),
		AccessKey: access,
		SecretKey: secret,
	})
}

func boolOpt(opts map[string]interface{}, key string, def bool) bool {
	if v, ok := opts[key].(bool); ok {
		return v
	}
	return def
}

func buildPolicyEngine(cfg config.PolicyConfig) (policy.Engine, error) {
	// Empty policy section → NoopEngine (back-compat with configs predating
	// the policy slice).
	if cfg.Default == "" && len(cfg.Rules) == 0 {
		return policy.NoopEngine{}, nil
	}

	spec := policy.Spec{Default: policy.Default(cfg.Default)}
	for _, r := range cfg.Rules {
		spec.Rules = append(spec.Rules, policy.Rule{
			Name:   r.Name,
			Effect: policy.Effect(r.Effect),
			Reason: r.Reason,
			Match: policy.Match{
				Tenants:    r.Match.Tenant,
				Agents:     r.Match.Agent,
				Blocks:     r.Match.Block,
				Namespaces: r.Match.Namespace,
				Ops:        r.Match.Op,
			},
			Bucket: policy.Bucket{
				PerTenant:     r.Bucket.PerTenant,
				PerAgent:      r.Bucket.PerAgent,
				PerNamespace:  r.Bucket.PerNamespace,
				PerOp:         r.Bucket.PerOp,
				RatePerSecond: r.Bucket.RatePerSecond,
				Burst:         r.Bucket.Burst,
			},
		})
	}
	return policy.NewRuleEngine(spec)
}

func buildEmbedder(cfg config.EmbedderConfig) (embedder.Embedder, error) {
	switch cfg.Provider {
	case "fake":
		return embedder.NewFake(embedder.FakeOptions{Dimensions: cfg.Dimensions})
	case "ollama":
		return embollama.New(embollama.Options{
			BaseURL:    stringOpt(cfg.Options, "base_url"),
			Model:      cfg.Model,
			Dimensions: cfg.Dimensions,
			Timeout:    durationOpt(cfg.Options, "timeout", 30*time.Second),
		})
	case "openai":
		key := os.Getenv(stringOpt(cfg.Options, "api_key_env"))
		if key == "" {
			return nil, fmt.Errorf("openai: env var %q is empty (set api_key_env)", stringOpt(cfg.Options, "api_key_env"))
		}
		return embopenai.New(embopenai.Options{
			BaseURL:    stringOpt(cfg.Options, "base_url"),
			Model:      cfg.Model,
			APIKey:     key,
			Dimensions: cfg.Dimensions,
			Timeout:    durationOpt(cfg.Options, "timeout", 30*time.Second),
		})
	default:
		return nil, fmt.Errorf("unsupported embedder provider %q", cfg.Provider)
	}
}

func newEpisodicPostgresDriver(b config.BackendConfig) (*eppgdrv.Driver, error) {
	dsn, err := resolveDSN(b)
	if err != nil {
		return nil, err
	}
	maxConns := int32(intOpt(b.Options, "max_conns", 10))
	tail := durationOpt(b.Options, "tail_interval", 250*time.Millisecond)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	return eppgdrv.New(ctx, eppgdrv.Options{
		DSN:          dsn,
		MaxConns:     maxConns,
		TailInterval: tail,
	})
}

func stringOpt(opts map[string]interface{}, key string) string {
	if v, ok := opts[key].(string); ok {
		return v
	}
	return ""
}

func intOpt(opts map[string]interface{}, key string, def int) int {
	switch v := opts[key].(type) {
	case int:
		return v
	case int64:
		return int(v)
	case float64:
		return int(v)
	default:
		return def
	}
}

func durationOpt(opts map[string]interface{}, key string, def time.Duration) time.Duration {
	s, ok := opts[key].(string)
	if !ok || s == "" {
		return def
	}
	d, err := time.ParseDuration(s)
	if err != nil {
		return def
	}
	return d
}
