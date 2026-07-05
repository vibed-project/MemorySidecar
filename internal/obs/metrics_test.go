package obs

import (
	"context"
	"testing"
	"time"

	"memsidecar/internal/config"
)

func TestSetupMetrics_None(t *testing.T) {
	setup, err := SetupMetrics(config.MetricsConfig{Exporter: "none"})
	if err != nil {
		t.Fatalf("none: %v", err)
	}
	if setup.MeterProvider == nil || setup.HTTPHandler != nil {
		t.Fatalf("none: want non-nil provider and nil handler")
	}
}

func TestSetupMetrics_OTLP(t *testing.T) {
	// The OTLP exporter connects lazily, so setup succeeds without a live
	// collector. There is no /metrics handler in push mode.
	setup, err := SetupMetrics(config.MetricsConfig{
		Exporter: "otlp",
		OTLP:     config.OTLPConfig{Endpoint: "localhost:4317", Insecure: true},
	})
	if err != nil {
		t.Fatalf("SetupMetrics(otlp): %v", err)
	}
	if setup.MeterProvider == nil {
		t.Fatal("otlp: nil MeterProvider")
	}
	if setup.HTTPHandler != nil {
		t.Fatal("otlp: HTTPHandler should be nil (push mode)")
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	_ = setup.Shutdown(ctx) // best-effort; nothing to flush to
}

func TestSetupMetrics_OTLPRequiresEndpoint(t *testing.T) {
	if _, err := SetupMetrics(config.MetricsConfig{Exporter: "otlp"}); err == nil {
		t.Fatal("otlp without endpoint should error")
	}
}

func TestSetupMetrics_UnknownExporter(t *testing.T) {
	if _, err := SetupMetrics(config.MetricsConfig{Exporter: "bogus"}); err == nil {
		t.Fatal("unknown exporter should error")
	}
}
