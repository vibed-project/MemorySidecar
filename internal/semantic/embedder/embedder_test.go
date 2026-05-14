package embedder

import (
	"context"
	"math"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestFake_Deterministic(t *testing.T) {
	e, err := NewFake(FakeOptions{Dimensions: 64})
	require.NoError(t, err)
	v1, _ := e.Embed(context.Background(), []string{"hello world"})
	v2, _ := e.Embed(context.Background(), []string{"hello world"})
	assert.Equal(t, v1[0], v2[0])
}

func TestFake_DifferentTexts_DifferentVectors(t *testing.T) {
	e, _ := NewFake(FakeOptions{Dimensions: 32})
	v, _ := e.Embed(context.Background(), []string{"alpha", "beta"})
	assert.NotEqual(t, v[0], v[1])
}

func TestFake_Dimensions(t *testing.T) {
	e, _ := NewFake(FakeOptions{Dimensions: 128})
	v, _ := e.Embed(context.Background(), []string{"x"})
	assert.Len(t, v[0], 128)
}

func TestFake_UnitNorm(t *testing.T) {
	e, _ := NewFake(FakeOptions{Dimensions: 96})
	v, _ := e.Embed(context.Background(), []string{"the quick brown fox"})
	var sum float64
	for _, x := range v[0] {
		sum += float64(x) * float64(x)
	}
	assert.InDelta(t, 1.0, math.Sqrt(sum), 1e-5)
}

func TestFake_ZeroDimensions(t *testing.T) {
	_, err := NewFake(FakeOptions{Dimensions: 0})
	require.Error(t, err)
}
