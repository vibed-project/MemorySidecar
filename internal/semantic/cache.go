package semantic

import (
	"container/list"
	"context"
	"crypto/sha256"
	"fmt"
	"sync"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"

	"github.com/vibed-project/mindD/internal/semantic/embedder"
)

// defaultEmbedCacheSize is the LRU capacity used when a semantic namespace
// enables caching without specifying a size.
const defaultEmbedCacheSize = 4096

// CacheOptions configures a caching embedder.
type CacheOptions struct {
	// Namespace and Model are used only as metric labels.
	Namespace string
	Model     string
	// Capacity is the maximum number of (content -> vector) entries retained.
	//   < 0  disables caching (NewCachingEmbedder returns the inner embedder)
	//   == 0 uses defaultEmbedCacheSize
	//   > 0  uses that many entries
	Capacity int
}

// NewCachingEmbedder wraps inner with a bounded LRU keyed on the SHA-256 of the
// input text, so identical content within a (namespace, model) is embedded at
// most once. Because each semantic namespace binds its own embedder instance,
// the wrapping is naturally scoped per (namespace, model) and the cache key is
// just the content hash.
//
// When Capacity < 0 the inner embedder is returned unwrapped, so a namespace
// that opts out pays zero overhead. The wrapper is safe for concurrent use and
// never changes the vectors inner would have produced — a miss simply re-embeds.
func NewCachingEmbedder(inner embedder.Embedder, opts CacheOptions) embedder.Embedder {
	if opts.Capacity < 0 {
		return inner
	}
	capacity := opts.Capacity
	if capacity == 0 {
		capacity = defaultEmbedCacheSize
	}
	m := otel.GetMeterProvider().Meter("mindd/semantic")
	hits, _ := m.Int64Counter("mindd.embedder.cache.hits",
		metric.WithDescription("Embedder cache hits (text already embedded, no provider call)."))
	misses, _ := m.Int64Counter("mindd.embedder.cache.misses",
		metric.WithDescription("Embedder cache misses (text embedded via the provider)."))
	return &cachingEmbedder{
		inner: inner,
		cap:   capacity,
		ll:    list.New(),
		items: make(map[[32]byte]*list.Element, capacity),
		hits:  hits,
		miss:  misses,
		attrs: metric.WithAttributes(
			attribute.String("namespace", opts.Namespace),
			attribute.String("model", opts.Model),
		),
	}
}

type cacheEntry struct {
	key [32]byte
	vec []float32
}

// cachingEmbedder is a bounded-LRU decorator around an embedder.Embedder.
type cachingEmbedder struct {
	inner embedder.Embedder
	cap   int

	mu    sync.Mutex
	ll    *list.List                 // front = most-recently used
	items map[[32]byte]*list.Element // content hash -> element holding *cacheEntry

	hits, miss metric.Int64Counter
	attrs      metric.MeasurementOption
}

func (c *cachingEmbedder) Dimensions() int { return c.inner.Dimensions() }

// Embed returns one vector per input text, serving cached content from the LRU
// and embedding only the misses (deduplicated within the batch) via inner.
func (c *cachingEmbedder) Embed(ctx context.Context, texts []string) ([][]float32, error) {
	out := make([][]float32, len(texts))

	// Partition into hits (filled now) and unique misses (embedded once).
	missText := make([]string, 0, len(texts))  // unique miss texts, in first-seen order
	missKey := make([][32]byte, 0, len(texts)) // parallel to missText
	missPos := make(map[[32]byte][]int)        // hash -> output positions awaiting it
	var nHits, nMiss int64
	for i, t := range texts {
		k := sha256.Sum256([]byte(t))
		if v, ok := c.get(k); ok {
			out[i] = cloneVec(v)
			nHits++
			continue
		}
		nMiss++
		if _, seen := missPos[k]; !seen {
			missText = append(missText, t)
			missKey = append(missKey, k)
		}
		missPos[k] = append(missPos[k], i)
	}
	if nHits > 0 && c.hits != nil {
		c.hits.Add(ctx, nHits, c.attrs)
	}
	if nMiss > 0 && c.miss != nil {
		c.miss.Add(ctx, nMiss, c.attrs)
	}
	if len(missText) == 0 {
		return out, nil
	}

	vecs, err := c.inner.Embed(ctx, missText)
	if err != nil {
		return nil, err
	}
	if len(vecs) != len(missText) {
		return nil, fmt.Errorf("caching embedder: inner returned %d vectors for %d inputs", len(vecs), len(missText))
	}
	for j, k := range missKey {
		v := vecs[j]
		c.put(k, cloneVec(v)) // store an isolated copy; callers never see the cached array
		for _, pos := range missPos[k] {
			out[pos] = cloneVec(v)
		}
	}
	return out, nil
}

func (c *cachingEmbedder) get(k [32]byte) ([]float32, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	el, ok := c.items[k]
	if !ok {
		return nil, false
	}
	c.ll.MoveToFront(el)
	return el.Value.(*cacheEntry).vec, true
}

func (c *cachingEmbedder) put(k [32]byte, vec []float32) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if el, ok := c.items[k]; ok {
		c.ll.MoveToFront(el)
		el.Value.(*cacheEntry).vec = vec
		return
	}
	el := c.ll.PushFront(&cacheEntry{key: k, vec: vec})
	c.items[k] = el
	if c.ll.Len() > c.cap {
		if back := c.ll.Back(); back != nil {
			c.ll.Remove(back)
			delete(c.items, back.Value.(*cacheEntry).key)
		}
	}
}

func cloneVec(v []float32) []float32 {
	if v == nil {
		return nil
	}
	out := make([]float32, len(v))
	copy(out, v)
	return out
}
