package interceptor

import (
	"context"
	"log/slog"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"
	"google.golang.org/grpc"
	"google.golang.org/grpc/status"

	"memsidecar/internal/auth"
)

// ObservabilityUnary records a span attribute set for the call's tenant /
// namespace / op, plus a structured access log line.
func ObservabilityUnary(log *slog.Logger) grpc.UnaryServerInterceptor {
	return func(ctx context.Context, req any, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
		start := time.Now()
		resp, err := handler(ctx, req)
		annotate(ctx, info.FullMethod, time.Since(start), err, log)
		return resp, err
	}
}

// ObservabilityStream is the streaming-call equivalent of ObservabilityUnary.
func ObservabilityStream(log *slog.Logger) grpc.StreamServerInterceptor {
	return func(srv any, ss grpc.ServerStream, info *grpc.StreamServerInfo, handler grpc.StreamHandler) error {
		start := time.Now()
		err := handler(srv, ss)
		annotate(ss.Context(), info.FullMethod, time.Since(start), err, log)
		return err
	}
}

func annotate(ctx context.Context, method string, latency time.Duration, callErr error, log *slog.Logger) {
	span := trace.SpanFromContext(ctx)
	attrs := []attribute.KeyValue{
		attribute.String("memsidecar.method", method),
	}
	if cap, ok := auth.FromContext(ctx); ok {
		attrs = append(attrs,
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
	log.Log(ctx, level, "rpc",
		slog.String("method", method),
		slog.String("code", code),
		slog.Duration("latency", latency),
	)
}
