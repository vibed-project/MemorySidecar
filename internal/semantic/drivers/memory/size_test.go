package memory

import (
	"context"
	"testing"

	"memsidecar/internal/semantic"
)

func TestSizeCountsRecords(t *testing.T) {
	ctx := context.Background()
	d, err := New(Options{Dimensions: 2})
	if err != nil {
		t.Fatal(err)
	}

	if n, _ := d.Size(ctx); n != 0 {
		t.Fatalf("empty Size = %d, want 0", n)
	}
	if err := d.Upsert(ctx, []semantic.Record{
		{ID: "a", Vector: []float32{1, 0}},
		{ID: "b", Vector: []float32{0, 1}},
	}); err != nil {
		t.Fatal(err)
	}
	if n, _ := d.Size(ctx); n != 2 {
		t.Fatalf("Size = %d, want 2", n)
	}
	// Re-upserting an existing id updates in place; the count stays 2.
	if err := d.Upsert(ctx, []semantic.Record{{ID: "a", Vector: []float32{1, 0}}}); err != nil {
		t.Fatal(err)
	}
	if n, _ := d.Size(ctx); n != 2 {
		t.Fatalf("Size after re-upsert = %d, want 2", n)
	}
}
