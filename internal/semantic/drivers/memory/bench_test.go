package memory

import (
	"context"
	"fmt"
	"math"
	"testing"

	"github.com/vibed-project/mindD/internal/semantic"
)

const benchDim = 128

func benchVector(seed int) []float32 {
	v := make([]float32, benchDim)
	x := float64(seed)*1.6180339887 + 1
	for i := range v {
		x = math.Mod(x*1.1+float64(i)+1, 997)
		v[i] = float32(x) + 1
	}
	return v
}

// BenchmarkUpsert measures the write/index path (O5 construction-vs-query).
func BenchmarkUpsert(b *testing.B) {
	d, err := New(Options{Dimensions: benchDim})
	if err != nil {
		b.Fatal(err)
	}
	ctx := context.Background()
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = d.Upsert(ctx, []semantic.Record{{ID: fmt.Sprintf("r%d", i), Vector: benchVector(i)}})
	}
}

// BenchmarkSearch measures the query path over a preloaded namespace.
func BenchmarkSearch(b *testing.B) {
	d, err := New(Options{Dimensions: benchDim})
	if err != nil {
		b.Fatal(err)
	}
	ctx := context.Background()
	for i := 0; i < 2000; i++ {
		_ = d.Upsert(ctx, []semantic.Record{{ID: fmt.Sprintf("r%d", i), Vector: benchVector(i)}})
	}
	q := benchVector(42)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_, _ = d.Search(ctx, semantic.SearchOptions{QueryVector: q, TopK: 10})
	}
}
