package semantic

import (
	"context"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"

	"memsidecar/internal/auth"
)

// instruments holds the service-layer metrics that separate backend (engine)
// time from sidecar overhead and expose result-set shape (optimization O2).
// Subtracting memsidecar.backend.duration from the interceptor's
// memsidecar.op.duration (same block/op/namespace labels) yields sidecar
// overhead — the RQ5/F5 construction-vs-query cost split at the driver boundary.
type instruments struct {
	backendDuration metric.Float64Histogram // memsidecar.backend.duration {block, op, namespace}
	resultSize      metric.Int64Histogram   // memsidecar.result.size {block, op, namespace}
	topScore        metric.Float64Histogram // memsidecar.result.top_score {block, namespace}
}

// newInstruments builds the semantic service instruments from the global meter
// provider (set by obs.SetupMetrics at startup; a no-op provider otherwise, in
// which case every Record below is a no-op).
func newInstruments() *instruments {
	m := otel.GetMeterProvider().Meter("memsidecar/semantic")
	bd, _ := m.Float64Histogram("memsidecar.backend.duration",
		metric.WithUnit("s"),
		metric.WithDescription("Time spent in the semantic driver/engine, excluding sidecar overhead."))
	rs, _ := m.Int64Histogram("memsidecar.result.size",
		metric.WithDescription("Records returned or affected by a semantic op."))
	ts, _ := m.Float64Histogram("memsidecar.result.top_score",
		metric.WithDescription("Top-1 cosine similarity of a semantic Search (evidence-completion proxy)."))
	return &instruments{backendDuration: bd, resultSize: rs, topScore: ts}
}

func opAttrs(op auth.Op, namespace string) metric.MeasurementOption {
	return metric.WithAttributes(
		attribute.String("block", Block),
		attribute.String("op", string(op)),
		attribute.String("namespace", namespace),
	)
}

// backend records the driver-call latency for op in namespace.
func (in *instruments) backend(ctx context.Context, op auth.Op, namespace string, d time.Duration) {
	if in == nil || in.backendDuration == nil {
		return
	}
	in.backendDuration.Record(ctx, d.Seconds(), opAttrs(op, namespace))
}

// result records the number of records returned or affected by op in namespace.
func (in *instruments) result(ctx context.Context, op auth.Op, namespace string, n int) {
	if in == nil || in.resultSize == nil {
		return
	}
	in.resultSize.Record(ctx, int64(n), opAttrs(op, namespace))
}

// searchTopScore records the top-1 similarity for a Search in namespace.
func (in *instruments) searchTopScore(ctx context.Context, namespace string, score float32) {
	if in == nil || in.topScore == nil {
		return
	}
	in.topScore.Record(ctx, float64(score), metric.WithAttributes(
		attribute.String("block", Block),
		attribute.String("namespace", namespace),
	))
}
