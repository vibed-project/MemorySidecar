package interceptor

import (
	"context"
	"io"
	"log/slog"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel/attribute"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

type nsReq struct{ ns string }

func (r nsReq) GetNamespace() string { return r.ns }

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func collectOpDuration(t *testing.T, reader sdkmetric.Reader) metricdata.Histogram[float64] {
	t.Helper()
	var rm metricdata.ResourceMetrics
	require.NoError(t, reader.Collect(context.Background(), &rm))
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			if m.Name == "memsidecar.op.duration" {
				h, ok := m.Data.(metricdata.Histogram[float64])
				require.True(t, ok, "op.duration must be a float histogram")
				return h
			}
		}
	}
	t.Fatal("memsidecar.op.duration metric not found")
	return metricdata.Histogram[float64]{}
}

func attrVal(set attribute.Set, key string) string {
	v, _ := set.Value(attribute.Key(key))
	return v.AsString()
}

// TestObservabilityUnary_OpDurationSplit verifies the op.duration histogram is
// recorded with the block / op / op_class / namespace / code attributes, that
// op_class reflects the write flag, and that unrecognized methods are skipped.
func TestObservabilityUnary_OpDurationSplit(t *testing.T) {
	reader := sdkmetric.NewManualReader()
	mp := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	intercept := ObservabilityUnary(discardLogger(), mp)

	ok := func(context.Context, any) (any, error) { return "ok", nil }

	// A write method on kv.
	_, err := intercept(context.Background(), nsReq{ns: "scratchpad"},
		&grpc.UnaryServerInfo{FullMethod: "/memsidecar.kv.v1.KV/Put"}, ok)
	require.NoError(t, err)

	// A read method on semantic.
	_, err = intercept(context.Background(), nsReq{ns: "notes"},
		&grpc.UnaryServerInfo{FullMethod: "/memsidecar.semantic.v1.Semantic/Search"}, ok)
	require.NoError(t, err)

	// An unrecognized method must not produce a data point.
	_, err = intercept(context.Background(), nsReq{ns: "x"},
		&grpc.UnaryServerInfo{FullMethod: "/grpc.health.v1.Health/Check"}, ok)
	require.NoError(t, err)

	hist := collectOpDuration(t, reader)
	require.Len(t, hist.DataPoints, 2, "one series per recognized method; health call skipped")

	byOp := map[string]metricdata.HistogramDataPoint[float64]{}
	for _, dp := range hist.DataPoints {
		byOp[attrVal(dp.Attributes, "op")] = dp
	}

	put, ok2 := byOp["kv.put"]
	require.True(t, ok2)
	assert.Equal(t, "kv", attrVal(put.Attributes, "block"))
	assert.Equal(t, "write", attrVal(put.Attributes, "op_class"))
	assert.Equal(t, "scratchpad", attrVal(put.Attributes, "namespace"))
	assert.Equal(t, "OK", attrVal(put.Attributes, "code"))
	assert.Equal(t, uint64(1), put.Count)

	search, ok3 := byOp["semantic.search"]
	require.True(t, ok3)
	assert.Equal(t, "semantic", attrVal(search.Attributes, "block"))
	assert.Equal(t, "query", attrVal(search.Attributes, "op_class"))
	assert.Equal(t, "notes", attrVal(search.Attributes, "namespace"))
}

// TestObservabilityUnary_ErrorCode records the gRPC status code on failure.
func TestObservabilityUnary_ErrorCode(t *testing.T) {
	reader := sdkmetric.NewManualReader()
	mp := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	intercept := ObservabilityUnary(discardLogger(), mp)

	fail := func(context.Context, any) (any, error) {
		return nil, status.Error(codes.PermissionDenied, "nope")
	}
	_, err := intercept(context.Background(), nsReq{ns: "scratchpad"},
		&grpc.UnaryServerInfo{FullMethod: "/memsidecar.kv.v1.KV/Get"}, fail)
	require.Error(t, err)

	hist := collectOpDuration(t, reader)
	require.Len(t, hist.DataPoints, 1)
	dp := hist.DataPoints[0]
	assert.Equal(t, "query", attrVal(dp.Attributes, "op_class"))
	assert.Equal(t, "PermissionDenied", attrVal(dp.Attributes, "code"))
}
