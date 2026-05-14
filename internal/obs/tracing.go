package obs

import (
	"context"
	"fmt"
	"os"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc"
	"go.opentelemetry.io/otel/exporters/stdout/stdouttrace"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.40.0"

	"memsidecar/internal/config"
	"memsidecar/internal/version"
)

const ServiceName = "memsidecar"

// SetupTracing builds a TracerProvider, registers it globally, and returns a
// shutdown function that flushes spans.
func SetupTracing(ctx context.Context, cfg config.TracingConfig) (func(context.Context) error, error) {
	if cfg.Exporter == "none" {
		return func(context.Context) error { return nil }, nil
	}

	res, err := buildResource()
	if err != nil {
		return nil, err
	}

	var exporter sdktrace.SpanExporter
	switch cfg.Exporter {
	case "stdout", "":
		exporter, err = stdouttrace.New(stdouttrace.WithWriter(os.Stdout))
	case "otlp":
		exporter, err = newOTLPExporter(ctx, cfg.OTLP)
	default:
		return nil, fmt.Errorf("unsupported tracing exporter %q (stdout|otlp|none)", cfg.Exporter)
	}
	if err != nil {
		return nil, fmt.Errorf("build exporter: %w", err)
	}

	ratio := cfg.SampleRatio
	if ratio <= 0 {
		ratio = 1.0
	}
	tp := sdktrace.NewTracerProvider(
		sdktrace.WithBatcher(exporter),
		sdktrace.WithResource(res),
		sdktrace.WithSampler(sdktrace.ParentBased(sdktrace.TraceIDRatioBased(ratio))),
	)
	otel.SetTracerProvider(tp)
	return tp.Shutdown, nil
}

func buildResource() (*resource.Resource, error) {
	r, err := resource.Merge(resource.Default(), resource.NewWithAttributes(
		semconv.SchemaURL,
		semconv.ServiceNameKey.String(ServiceName),
		semconv.ServiceVersionKey.String(version.Version),
	))
	if err != nil {
		return nil, fmt.Errorf("build otel resource: %w", err)
	}
	return r, nil
}

func newOTLPExporter(ctx context.Context, cfg config.OTLPConfig) (sdktrace.SpanExporter, error) {
	if cfg.Endpoint == "" {
		return nil, fmt.Errorf("otlp.endpoint required")
	}
	opts := []otlptracegrpc.Option{otlptracegrpc.WithEndpoint(cfg.Endpoint)}
	if cfg.Insecure {
		opts = append(opts, otlptracegrpc.WithInsecure())
	}
	if cfg.Compression == "gzip" {
		opts = append(opts, otlptracegrpc.WithCompressor("gzip"))
	}
	headers, err := resolveOTLPHeaders(cfg)
	if err != nil {
		return nil, err
	}
	if len(headers) > 0 {
		opts = append(opts, otlptracegrpc.WithHeaders(headers))
	}
	return otlptrace.New(ctx, otlptracegrpc.NewClient(opts...))
}

func resolveOTLPHeaders(cfg config.OTLPConfig) (map[string]string, error) {
	out := make(map[string]string, len(cfg.Headers)+len(cfg.HeadersEnv))
	for k, v := range cfg.Headers {
		out[k] = v
	}
	for k, envName := range cfg.HeadersEnv {
		v := os.Getenv(envName)
		if v == "" {
			return nil, fmt.Errorf("otlp header %q: env %q is empty", k, envName)
		}
		out[k] = v
	}
	return out, nil
}
