// Package embedder defines the interface for turning text into vectors and
// ships a deterministic "fake" embedder useful for tests and zero-dependency
// dev. Real provider adapters (Ollama, OpenAI, Cohere, Voyage) live in
// sibling subpackages.
package embedder

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"fmt"
	"math"
	"strings"
)

// Embedder turns one or more pieces of text into vectors of fixed dimension.
type Embedder interface {
	// Embed returns one vector per input text, each of length Dimensions().
	Embed(ctx context.Context, texts []string) ([][]float32, error)
	Dimensions() int
}

// FakeOptions configures a Fake embedder.
type FakeOptions struct {
	// Dimensions is the output vector size. Must be > 0.
	Dimensions int
}

// Fake is a deterministic content-derived embedder. The vector is computed
// from a SHA-256 hash of the text, expanded to Dimensions float32 values in
// [-1, 1], then L2-normalised. It is NOT semantically meaningful — identical
// text produces identical vectors, but unrelated texts may be arbitrarily
// close or far. Useful for tests and zero-dependency dev only.
type Fake struct {
	dim int
}

// NewFake builds a Fake embedder.
func NewFake(opts FakeOptions) (*Fake, error) {
	if opts.Dimensions <= 0 {
		return nil, fmt.Errorf("embedder/fake: dimensions must be > 0")
	}
	return &Fake{dim: opts.Dimensions}, nil
}

func (f *Fake) Dimensions() int { return f.dim }

func (f *Fake) Embed(_ context.Context, texts []string) ([][]float32, error) {
	out := make([][]float32, len(texts))
	for i, t := range texts {
		out[i] = f.embedOne(t)
	}
	return out, nil
}

func (f *Fake) embedOne(text string) []float32 {
	v := make([]float32, f.dim)
	// Expand a 32-byte SHA-256 digest to len(v) floats by chained hashing.
	seed := sha256.Sum256([]byte(strings.ToLower(text)))
	cur := seed
	for i := 0; i < f.dim; i++ {
		if i > 0 && i%8 == 0 {
			cur = sha256.Sum256(cur[:])
		}
		off := (i % 8) * 4
		u := binary.LittleEndian.Uint32(cur[off : off+4])
		// Map uniformly to [-1, 1].
		v[i] = (float32(u)/float32(math.MaxUint32))*2 - 1
	}
	normalise(v)
	return v
}

// normalise scales v in place so that ||v||_2 = 1. Returns silently if v is
// the zero vector.
func normalise(v []float32) {
	var sum float64
	for _, x := range v {
		sum += float64(x) * float64(x)
	}
	if sum == 0 {
		return
	}
	n := float32(math.Sqrt(sum))
	for i := range v {
		v[i] /= n
	}
}
