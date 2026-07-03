package interceptor

import (
	"context"
	"log/slog"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/trace"
	"google.golang.org/grpc"
	"google.golang.org/grpc/status"

	"memsidecar/internal/auth"
)

// opDurationHistogram builds the memsidecar.op.duration histogram (seconds)
// from mp, falling back to the global meter provider when mp is nil. It returns
// nil only on construction error, which makes recording a no-op.
func opDurationHistogram(mp metric.MeterProvider) metric.Float64Histogram {
	if mp == nil {
		mp = otel.GetMeterProvider()
	}
	h, err := mp.Meter("memsidecar/interceptor").Float64Histogram(
		"memsidecar.op.duration",
		metric.WithUnit("s"),
		metric.WithDescription("Server-side RPC duration by building block and op class (write vs query)."),
	)
	if err != nil {
		return nil
	}
	return h
}

// ObservabilityUnary records a span attribute set for the call's tenant /
// namespace / op, an op-duration metric split by op class (write vs query),
// plus a structured access log line.
func ObservabilityUnary(log *slog.Logger, mp metric.MeterProvider) grpc.UnaryServerInterceptor {
	hist := opDurationHistogram(mp)
	return func(ctx context.Context, req any, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
		start := time.Now()
		resp, err := handler(ctx, req)
		annotate(ctx, info.FullMethod, namespaceFromRequest(req), time.Since(start), err, log, hist)
		return resp, err
	}
}

// ObservabilityStream is the streaming-call equivalent of ObservabilityUnary.
// Streaming requests do not expose a namespace at the interceptor boundary, so
// the namespace attribute is left empty (matching the policy interceptor).
func ObservabilityStream(log *slog.Logger, mp metric.MeterProvider) grpc.StreamServerInterceptor {
	hist := opDurationHistogram(mp)
	return func(srv any, ss grpc.ServerStream, info *grpc.StreamServerInfo, handler grpc.StreamHandler) error {
		start := time.Now()
		err := handler(srv, ss)
		annotate(ss.Context(), info.FullMethod, "", time.Since(start), err, log, hist)
		return err
	}
}

func annotate(ctx context.Context, method, namespace string, latency time.Duration, callErr error, log *slog.Logger, hist metric.Float64Histogram) {
	span := trace.SpanFromContext(ctx)
	attrs := []attribute.KeyValue{
		attribute.String("memsidecar.method", method),
	}
	if cap, ok := auth.FromContext(ctx); ok {
		attrs = append(
			attrs,
			attribute.String("memsidecar.tenant", cap.Scope.Tenant),
			attribute.String("memsidecar.agent", cap.Scope.Agent),
		)
	}
	span.SetAttributes(attrs...)

	level := slog.LevelInfo
	code := "OK"
	if callErr != nil {
		level = slog.LevelWarn
		code = status.Code(callErr).String()
	}
	log.Log(
		ctx, level, "rpc",
		slog.String("method", method),
		slog.String("code", code),
		slog.Duration("latency", latency),
	)

	// Record op.duration only for recognized building-block methods, so metric
	// cardinality stays bounded (health/reflection calls are skipped). op_class
	// reuses the write flag already maintained in methodToOp — the paper's
	// construction-vs-query split (RQ5/F5) for free.
	if hist == nil {
		return
	}
	m, ok := methodToOp[method]
	if !ok {
		return
	}
	opClass := "query"
	if m.write {
		opClass = "write"
	}
	hist.Record(ctx, latency.Seconds(), metric.WithAttributes(
		attribute.String("block", m.block),
		attribute.String("op", string(m.op)),
		attribute.String("op_class", opClass),
		attribute.String("namespace", namespace),
		attribute.String("code", code),
	))
}
