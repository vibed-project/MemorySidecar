package obs

import (
	"context"
	"testing"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
)

func newTestReader(t *testing.T) *sdkmetric.ManualReader {
	t.Helper()
	reader := sdkmetric.NewManualReader()
	mp := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	otel.SetMeterProvider(mp)
	t.Cleanup(func() { _ = mp.Shutdown(context.Background()) })
	return reader
}

func findMetric(t *testing.T, reader *sdkmetric.ManualReader, name string) metricdata.Metrics {
	t.Helper()
	var rm metricdata.ResourceMetrics
	if err := reader.Collect(context.Background(), &rm); err != nil {
		t.Fatalf("collect: %v", err)
	}
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			if m.Name == name {
				return m
			}
		}
	}
	t.Fatalf("metric %q not found", name)
	return metricdata.Metrics{}
}

func attr(set attribute.Set, key string) string {
	v, _ := set.Value(attribute.Key(key))
	return v.AsString()
}

func TestNamespaceItemsGauge(t *testing.T) {
	reader := newTestReader(t)
	if err := RegisterNamespaceItemsGauge([]NamespaceItemSource{
		{Block: "kv", Items: func(context.Context) map[string]int64 { return map[string]int64{"scratch": 3} }},
		{Block: "semantic", Items: func(context.Context) map[string]int64 { return map[string]int64{"notes": 7} }},
	}); err != nil {
		t.Fatalf("register: %v", err)
	}

	g, ok := findMetric(t, reader, "mindd.namespace.items").Data.(metricdata.Gauge[int64])
	if !ok {
		t.Fatal("namespace.items is not an int64 gauge")
	}
	got := map[string]int64{}
	for _, dp := range g.DataPoints {
		got[attr(dp.Attributes, "block")+"/"+attr(dp.Attributes, "namespace")] = dp.Value
	}
	if got["kv/scratch"] != 3 || got["semantic/notes"] != 7 {
		t.Fatalf("gauge points = %v, want kv/scratch=3 semantic/notes=7", got)
	}
}

func TestEvictionCounter(t *testing.T) {
	reader := newTestReader(t)
	ec := NewEvictionCounter()
	ec.Add("kv", "scratch", EvictionTTL, 2)
	ec.Add("kv", "scratch", EvictionTTL, 3)
	ec.Add("kv", "zero", EvictionTTL, 0) // no-op
	ec.Add("kv", "neg", EvictionTTL, -5) // no-op
	var nilCounter *EvictionCounter
	nilCounter.Add("kv", "x", EvictionTTL, 1) // must not panic

	s, ok := findMetric(t, reader, "mindd.eviction.total").Data.(metricdata.Sum[int64])
	if !ok {
		t.Fatal("eviction.total is not an int64 sum")
	}
	got := map[string]int64{}
	for _, dp := range s.DataPoints {
		key := attr(dp.Attributes, "block") + "/" + attr(dp.Attributes, "namespace") + "/" + attr(dp.Attributes, "cause")
		got[key] = dp.Value
	}
	if got["kv/scratch/ttl"] != 5 {
		t.Fatalf("kv/scratch/ttl = %d, want 5 (points=%v)", got["kv/scratch/ttl"], got)
	}
	if _, ok := got["kv/zero/ttl"]; ok {
		t.Fatalf("zero-count Add should record nothing (points=%v)", got)
	}
	if _, ok := got["kv/neg/ttl"]; ok {
		t.Fatalf("negative Add should record nothing (points=%v)", got)
	}
}
