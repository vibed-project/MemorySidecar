package semantic_test

import (
	"reflect"
	"testing"

	"memsidecar/internal/semantic"
)

func TestRRFFuse(t *testing.T) {
	// Dense ranks a,b,c; sparse ranks c,d. c appears in both lanes (rank 3 dense,
	// rank 1 sparse) so it should fuse to the top.
	dense := []string{"a", "b", "c"}
	sparse := []string{"c", "d"}
	ids, scores := semantic.RRFFuse([][]string{dense, sparse}, 0)

	if ids[0] != "c" {
		t.Fatalf("expected c first (in both lanes), got %v", ids)
	}
	// c's score = 1/(60+3) + 1/(60+1); a's = 1/(60+1). c must outrank a.
	if scores["c"] <= scores["a"] {
		t.Fatalf("c score %.5f should exceed a score %.5f", scores["c"], scores["a"])
	}
	// All five distinct ids present.
	if len(ids) != 4 {
		t.Fatalf("expected 4 fused ids, got %d (%v)", len(ids), ids)
	}

	// topK truncation.
	top2, _ := semantic.RRFFuse([][]string{dense, sparse}, 2)
	if len(top2) != 2 {
		t.Fatalf("topK=2 returned %d", len(top2))
	}

	// Deterministic across calls (ties broken by id).
	again, _ := semantic.RRFFuse([][]string{dense, sparse}, 0)
	if !reflect.DeepEqual(ids, again) {
		t.Fatalf("RRFFuse not deterministic: %v vs %v", ids, again)
	}
}

func TestTermOverlap(t *testing.T) {
	q := semantic.Tokenize("Error CODE zzqq")
	if got := semantic.TermOverlap(q, "an obscure zzqq token appeared"); got != 1 {
		t.Fatalf("overlap = %d, want 1 (zzqq)", got)
	}
	if got := semantic.TermOverlap(q, "unrelated content"); got != 0 {
		t.Fatalf("overlap = %d, want 0", got)
	}
	if got := semantic.TermOverlap(q, "error code zzqq"); got != 3 {
		t.Fatalf("overlap = %d, want 3 (case-insensitive)", got)
	}
	if got := semantic.TermOverlap(semantic.Tokenize(""), "anything"); got != 0 {
		t.Fatalf("empty query overlap = %d, want 0", got)
	}
}
