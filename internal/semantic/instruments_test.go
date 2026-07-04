package semantic_test

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"

	semanticv1 "memsidecar/gen/memsidecar/semantic/v1"
)

// TestService_O2Instruments verifies the service records backend.duration,
// result.size, and result.top_score with block/op/namespace labels. The meter
// provider is set globally before the service is constructed, since the
// instruments bind to whatever provider is current at NewService time.
func TestService_O2Instruments(t *testing.T) {
	reader := sdkmetric.NewManualReader()
	prev := otel.GetMeterProvider()
	otel.SetMeterProvider(sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader)))
	t.Cleanup(func() { otel.SetMeterProvider(prev) })

	c := newTestServer(t, fullScope())
	ctx := context.Background()

	_, err := c.Upsert(ctx, &semanticv1.UpsertRequest{
		Namespace: "notes",
		Records:   []*semanticv1.Record{{Id: "a", Content: "apple pie"}},
	})
	require.NoError(t, err)

	resp, err := c.Search(ctx, &semanticv1.SearchRequest{
		Namespace: "notes", QueryText: "apple pie", TopK: 5,
	})
	require.NoError(t, err)
	require.NotEmpty(t, resp.GetHits())

	var rm metricdata.ResourceMetrics
	require.NoError(t, reader.Collect(ctx, &rm))

	metrics := map[string]metricdata.Metrics{}
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			metrics[m.Name] = m
		}
	}

	// backend.duration recorded for both the upsert and the search.
	bd, ok := metrics["memsidecar.backend.duration"]
	require.True(t, ok, "backend.duration must be recorded")
	ops := opsWithLabel[float64](bd.Data.(metricdata.Histogram[float64]).DataPoints, "op")
	assert.Contains(t, ops, "semantic.upsert")
	assert.Contains(t, ops, "semantic.search")

	// result.size recorded; the search returned at least one hit.
	rs, ok := metrics["memsidecar.result.size"]
	require.True(t, ok, "result.size must be recorded")
	var searchSize int64 = -1
	for _, dp := range rs.Data.(metricdata.Histogram[int64]).DataPoints {
		if v, _ := dp.Attributes.Value(attribute.Key("op")); v.AsString() == "semantic.search" {
			assert.Equal(t, "notes", label(dp.Attributes, "namespace"))
			assert.Equal(t, "semantic", label(dp.Attributes, "block"))
			searchSize = int64(dp.Sum)
		}
	}
	assert.GreaterOrEqual(t, searchSize, int64(1))

	// top_score recorded for the search.
	_, ok = metrics["memsidecar.result.top_score"]
	assert.True(t, ok, "result.top_score must be recorded")
}

func label(set attribute.Set, key string) string {
	v, _ := set.Value(attribute.Key(key))
	return v.AsString()
}

func opsWithLabel[T int64 | float64](dps []metricdata.HistogramDataPoint[T], key string) map[string]bool {
	out := map[string]bool{}
	for _, dp := range dps {
		out[label(dp.Attributes, key)] = true
	}
	return out
}
