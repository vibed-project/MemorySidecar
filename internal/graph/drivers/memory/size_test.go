package memory_test

import (
	"context"
	"testing"

	"github.com/vibed-project/mindD/internal/graph"
	memdrv "github.com/vibed-project/mindD/internal/graph/drivers/memory"
)

func TestSizeCountsNodesNotEdges(t *testing.T) {
	ctx := context.Background()
	d := memdrv.New(memdrv.Options{})

	if n, _ := d.Size(ctx, "ns"); n != 0 {
		t.Fatalf("empty Size = %d, want 0", n)
	}
	if err := d.UpsertNodes(ctx, "ns", []graph.Node{{ID: "a"}, {ID: "b"}}); err != nil {
		t.Fatal(err)
	}
	if err := d.UpsertEdges(ctx, "ns", []graph.Edge{{ID: "e1", Type: "LINK", From: "a", To: "b"}}); err != nil {
		t.Fatal(err)
	}
	// The edge must not inflate the node-based item count.
	if n, _ := d.Size(ctx, "ns"); n != 2 {
		t.Fatalf("Size(ns) = %d, want 2 (nodes only, edges excluded)", n)
	}
	if n, _ := d.Size(ctx, "unused"); n != 0 {
		t.Fatalf("Size(unused) = %d, want 0", n)
	}
}
