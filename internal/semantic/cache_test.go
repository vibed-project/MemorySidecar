package semantic_test

import (
	"context"
	"testing"

	"memsidecar/internal/semantic"
	"memsidecar/internal/semantic/embedder"
)

// countingEmbedder wraps a real Fake embedder and records how many texts were
// actually sent to the underlying provider, so tests can assert the cache
// suppressed repeat work.
type countingEmbedder struct {
	inner    embedder.Embedder
	calls    int // number of Embed invocations
	embedded int // total texts embedded
}

func newCounting(t *testing.T, dim int) *countingEmbedder {
	t.Helper()
	f, err := embedder.NewFake(embedder.FakeOptions{Dimensions: dim})
	if err != nil {
		t.Fatalf("NewFake: %v", err)
	}
	return &countingEmbedder{inner: f}
}

func (c *countingEmbedder) Embed(ctx context.Context, texts []string) ([][]float32, error) {
	c.calls++
	c.embedded += len(texts)
	return c.inner.Embed(ctx, texts)
}

func (c *countingEmbedder) Dimensions() int { return c.inner.Dimensions() }

func vecEqual(a, b []float32) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func TestCachingEmbedder_HitsAndCorrectness(t *testing.T) {
	ctx := context.Background()
	ce := newCounting(t, 8)
	cache := semantic.NewCachingEmbedder(ce, semantic.CacheOptions{Namespace: "notes", Model: "m"})

	fake, _ := embedder.NewFake(embedder.FakeOptions{Dimensions: 8})
	want, _ := fake.Embed(ctx, []string{"apple"})

	got1, err := cache.Embed(ctx, []string{"apple", "banana"})
	if err != nil {
		t.Fatalf("Embed: %v", err)
	}
	if !vecEqual(got1[0], want[0]) {
		t.Fatalf("cached embed differs from provider: got %v want %v", got1[0], want[0])
	}

	got2, err := cache.Embed(ctx, []string{"apple"})
	if err != nil {
		t.Fatalf("Embed: %v", err)
	}
	if ce.embedded != 2 {
		t.Fatalf("provider embedded %d texts; want 2 (apple served from cache on the second call)", ce.embedded)
	}
	if !vecEqual(got2[0], want[0]) {
		t.Fatalf("cache hit returned wrong vector")
	}
	if ce.Dimensions() != cache.Dimensions() {
		t.Fatalf("Dimensions not delegated: %d vs %d", ce.Dimensions(), cache.Dimensions())
	}
}

func TestCachingEmbedder_BatchDedup(t *testing.T) {
	ctx := context.Background()
	ce := newCounting(t, 8)
	cache := semantic.NewCachingEmbedder(ce, semantic.CacheOptions{})

	got, err := cache.Embed(ctx, []string{"x", "x", "y"})
	if err != nil {
		t.Fatalf("Embed: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("got %d vectors; want 3", len(got))
	}
	if ce.embedded != 2 {
		t.Fatalf("provider embedded %d texts; want 2 (duplicate 'x' embedded once)", ce.embedded)
	}
	if !vecEqual(got[0], got[1]) {
		t.Fatalf("duplicate inputs produced different vectors")
	}
}

func TestCachingEmbedder_Eviction(t *testing.T) {
	ctx := context.Background()
	ce := newCounting(t, 8)
	cache := semantic.NewCachingEmbedder(ce, semantic.CacheOptions{Capacity: 2})

	for _, s := range []string{"a", "b", "c", "a"} {
		if _, err := cache.Embed(ctx, []string{s}); err != nil {
			t.Fatalf("Embed(%q): %v", s, err)
		}
	}
	// Capacity 2: inserting c evicts a (LRU); the final "a" therefore misses and
	// re-embeds → 4 provider embeds, not 3.
	if ce.embedded != 4 {
		t.Fatalf("provider embedded %d texts; want 4 (a evicted then re-embedded)", ce.embedded)
	}
}

func TestCachingEmbedder_Disabled(t *testing.T) {
	ctx := context.Background()
	ce := newCounting(t, 8)
	cache := semantic.NewCachingEmbedder(ce, semantic.CacheOptions{Capacity: -1})

	for i := 0; i < 3; i++ {
		if _, err := cache.Embed(ctx, []string{"a"}); err != nil {
			t.Fatalf("Embed: %v", err)
		}
	}
	if ce.embedded != 3 {
		t.Fatalf("provider embedded %d texts; want 3 (caching disabled ⇒ no suppression)", ce.embedded)
	}
}

func TestCachingEmbedder_NoAliasing(t *testing.T) {
	ctx := context.Background()
	ce := newCounting(t, 8)
	cache := semantic.NewCachingEmbedder(ce, semantic.CacheOptions{})

	// Mutating a returned vector must not corrupt the cached copy.
	got1, _ := cache.Embed(ctx, []string{"a"})
	got1[0][0] = 999
	got2, _ := cache.Embed(ctx, []string{"a"}) // cache hit
	if got2[0][0] == 999 {
		t.Fatalf("cache hit returned an aliased/mutated vector")
	}

	// Deduplicated positions within one batch must not share a backing array.
	got, _ := cache.Embed(ctx, []string{"z", "z"})
	got[0][0] = 999
	if got[1][0] == 999 {
		t.Fatalf("deduplicated batch positions alias the same array")
	}
}
