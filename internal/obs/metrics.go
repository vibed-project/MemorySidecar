package obs

import (
	"context"
	"fmt"
	"net/http"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"go.opentelemetry.io/otel"
	otelprom "go.opentelemetry.io/otel/exporters/prometheus"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/metric/noop"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"

	"memsidecar/internal/config"
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
	if cfg.Exporter != "prometheus" {
		return nil, fmt.Errorf("unsupported metrics exporter %q (prometheus|none)", cfg.Exporter)
	}

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
