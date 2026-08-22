package memory_test

import (
	"context"
	"fmt"
	"testing"

	"github.com/vibed-project/mindD/internal/graph"
	memdrv "github.com/vibed-project/mindD/internal/graph/drivers/memory"
)

// BenchmarkUpsertEdges measures the write path (O5 construction-vs-query).
func BenchmarkUpsertEdges(b *testing.B) {
	d := memdrv.New(memdrv.Options{})
	ctx := context.Background()
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = d.UpsertEdges(ctx, "g", []graph.Edge{
			{ID: fmt.Sprintf("e%d", i), Type: "L", From: "hub", To: fmt.Sprintf("n%d", i)},
		})
	}
}

// BenchmarkTraverse measures the query path over a preloaded star graph.
func BenchmarkTraverse(b *testing.B) {
	d := memdrv.New(memdrv.Options{})
	ctx := context.Background()
	nodes := []graph.Node{{ID: "hub"}}
	edges := make([]graph.Edge, 0, 1000)
	for i := 0; i < 1000; i++ {
		nid := fmt.Sprintf("n%d", i)
		nodes = append(nodes, graph.Node{ID: nid})
		edges = append(edges, graph.Edge{ID: "e" + nid, Type: "L", From: "hub", To: nid})
	}
	_ = d.UpsertNodes(ctx, "g", nodes)
	_ = d.UpsertEdges(ctx, "g", edges)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_, _ = d.Traverse(ctx, "g", graph.TraverseOptions{
			StartID: "hub", Direction: graph.DirectionOut, MaxDepth: 1, MaxNodes: 200, FanOut: 200,
		})
	}
}
