package obs

import (
	"context"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
)

const appMeterName = "memsidecar/obs"

// NamespaceItemSource reports the current item count per namespace for one
// building block. The Items func is typically a registry method value, e.g.
// kv.Registry.NamespaceItems.
type NamespaceItemSource struct {
	Block string
	Items func(ctx context.Context) map[string]int64
}

// RegisterNamespaceItemsGauge registers a single observable gauge,
// memsidecar.namespace.items{block,namespace}, whose callback polls every
// source on each metric collection. Sources must be cheap: memory drivers
// report map length and the pgvector driver uses a reltuples estimate — never
// count(*) on the hot path. Safe to call under a no-op meter provider (records
// nothing, returns nil).
func RegisterNamespaceItemsGauge(sources []NamespaceItemSource) error {
	m := otel.GetMeterProvider().Meter(appMeterName)
	gauge, err := m.Int64ObservableGauge(
		"memsidecar.namespace.items",
		metric.WithDescription("Live item count per (block, namespace)."),
	)
	if err != nil {
		return err
	}
	_, err = m.RegisterCallback(func(ctx context.Context, o metric.Observer) error {
		for _, src := range sources {
			for ns, n := range src.Items(ctx) {
				o.ObserveInt64(gauge, n, metric.WithAttributes(
					attribute.String("block", src.Block),
					attribute.String("namespace", ns),
				))
			}
		}
		return nil
	}, gauge)
	return err
}

// Eviction causes for the eviction.total counter.
const (
	EvictionTTL           = "ttl"           // key/record dropped because its TTL elapsed
	EvictionConsolidation = "consolidation" // reserved for future memory consolidation
)

// EvictionCounter records items removed from a namespace, labelled by cause. It
// keeps drivers free of an OTel dependency: a driver takes a plain callback and
// this type is wired in at the composition root.
type EvictionCounter struct {
	c metric.Int64Counter
}

// NewEvictionCounter builds memsidecar.eviction.total from the global meter
// provider. Safe under the no-op provider.
func NewEvictionCounter() *EvictionCounter {
	m := otel.GetMeterProvider().Meter(appMeterName)
	c, _ := m.Int64Counter(
		"memsidecar.eviction.total",
		metric.WithDescription("Items evicted from a namespace, by cause (e.g. ttl expiry)."),
	)
	return &EvictionCounter{c: c}
}

// Add records n evictions for (block, namespace, cause). No-op when n <= 0 or
// the counter is nil, so callers can wire it unconditionally.
func (e *EvictionCounter) Add(block, namespace, cause string, n int64) {
	if e == nil || e.c == nil || n <= 0 {
		return
	}
	e.c.Add(context.Background(), n, metric.WithAttributes(
		attribute.String("block", block),
		attribute.String("namespace", namespace),
		attribute.String("cause", cause),
	))
}
