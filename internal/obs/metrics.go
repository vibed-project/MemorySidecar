package obs

import (
	"context"
	"fmt"
	"net/http"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"go.opentelemetry.io/otel"
	otlpmetricgrpc "go.opentelemetry.io/otel/exporters/otlp/otlpmetric/otlpmetricgrpc"
	otelprom "go.opentelemetry.io/otel/exporters/prometheus"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/metric/noop"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"

	"github.com/vibed-project/mindD/internal/config"
)

// MetricsSetup is the result of bootstrapping the metric pipeline.
type MetricsSetup struct {
	// MeterProvider is wired into the global OTel registry and passed into
	// otelgrpc so RPC metrics flow through it.
	MeterProvider metric.MeterProvider
	// HTTPHandler serves /metrics in Prometheus format. nil when the
	// exporter is "none".
	HTTPHandler http.Handler
	// Addr is the listen address from config (":9090" by default).
	Addr string
	// Path is the URL path serving metrics (default "/metrics").
	Path string
	// Shutdown flushes and closes the provider.
	Shutdown func(context.Context) error
}

// SetupMetrics builds the meter provider. With Exporter="none" (or empty) it
// returns a no-op provider so downstream code can blindly call meter APIs.
func SetupMetrics(cfg config.MetricsConfig) (*MetricsSetup, error) {
	if cfg.Exporter == "" || cfg.Exporter == "none" {
		mp := noop.NewMeterProvider()
		otel.SetMeterProvider(mp)
		return &MetricsSetup{
			MeterProvider: mp,
			Shutdown:      func(context.Context) error { return nil },
		}, nil
	}
	switch cfg.Exporter {
	case "prometheus":
		return setupPrometheusMetrics(cfg)
	case "otlp":
		return setupOTLPMetrics(cfg)
	default:
		return nil, fmt.Errorf("unsupported metrics exporter %q (prometheus|otlp|none)", cfg.Exporter)
	}
}

func setupPrometheusMetrics(cfg config.MetricsConfig) (*MetricsSetup, error) {
	res, err := buildResource()
	if err != nil {
		return nil, err
	}

	registry := prometheus.NewRegistry()
	// Add the standard Go runtime + process collectors so /metrics reports
	// gc, goroutine, memory, fds out of the box.
	registry.MustRegister(
		collectors.NewGoCollector(),
		collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}),
	)

	exporter, err := otelprom.New(otelprom.WithRegisterer(registry))
	if err != nil {
		return nil, fmt.Errorf("build prometheus exporter: %w", err)
	}

	mp := sdkmetric.NewMeterProvider(
		sdkmetric.WithReader(exporter),
		sdkmetric.WithResource(res),
	)
	otel.SetMeterProvider(mp)

	addr := cfg.Prometheus.Addr
	if addr == "" {
		addr = ":9090"
	}
	path := cfg.Prometheus.Path
	if path == "" {
		path = "/metrics"
	}

	return &MetricsSetup{
		MeterProvider: mp,
		HTTPHandler:   promhttp.HandlerFor(registry, promhttp.HandlerOpts{}),
		Addr:          addr,
		Path:          path,
		Shutdown:      mp.Shutdown,
	}, nil
}

// setupOTLPMetrics ships metrics to an OTLP/gRPC collector via a PeriodicReader
// (push), reusing the same OTLP endpoint/headers config as tracing. There is no
// /metrics HTTP endpoint in this mode.
func setupOTLPMetrics(cfg config.MetricsConfig) (*MetricsSetup, error) {
	res, err := buildResource()
	if err != nil {
		return nil, err
	}
	exporter, err := newOTLPMetricExporter(context.Background(), cfg.OTLP)
	if err != nil {
		return nil, fmt.Errorf("build otlp metric exporter: %w", err)
	}
	mp := sdkmetric.NewMeterProvider(
		sdkmetric.WithReader(sdkmetric.NewPeriodicReader(exporter)),
		sdkmetric.WithResource(res),
	)
	otel.SetMeterProvider(mp)
	return &MetricsSetup{MeterProvider: mp, Shutdown: mp.Shutdown}, nil
}

func newOTLPMetricExporter(ctx context.Context, cfg config.OTLPConfig) (sdkmetric.Exporter, error) {
	if cfg.Endpoint == "" {
		return nil, fmt.Errorf("otlp.endpoint required")
	}
	opts := []otlpmetricgrpc.Option{otlpmetricgrpc.WithEndpoint(cfg.Endpoint)}
	if cfg.Insecure {
		opts = append(opts, otlpmetricgrpc.WithInsecure())
	}
	if cfg.Compression == "gzip" {
		opts = append(opts, otlpmetricgrpc.WithCompressor("gzip"))
	}
	headers, err := resolveOTLPHeaders(cfg) // shared with tracing (same package)
	if err != nil {
		return nil, err
	}
	if len(headers) > 0 {
		opts = append(opts, otlpmetricgrpc.WithHeaders(headers))
	}
	return otlpmetricgrpc.New(ctx, opts...)
}
