package memory

import (
	"context"
	"fmt"
	"testing"

	"memsidecar/internal/kv"
)

// BenchmarkPut measures the write path (O5 construction-vs-query).
func BenchmarkPut(b *testing.B) {
	d := New(Options{SweeperInterval: -1})
	ctx := context.Background()
	val := []byte("benchmark value payload")
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_, _ = d.Put(ctx, "ns", fmt.Sprintf("k%d", i), kv.PutOptions{Value: val})
	}
}

// BenchmarkGet measures the query path over a preloaded namespace.
func BenchmarkGet(b *testing.B) {
	d := New(Options{SweeperInterval: -1})
	ctx := context.Background()
	const n = 2000
	for i := 0; i < n; i++ {
		_, _ = d.Put(ctx, "ns", fmt.Sprintf("k%d", i), kv.PutOptions{Value: []byte("v")})
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_, _ = d.Get(ctx, "ns", fmt.Sprintf("k%d", i%n))
	}
}
