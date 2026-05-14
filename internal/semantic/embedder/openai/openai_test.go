package openai

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestEmbed_HappyPath(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "/v1/embeddings", r.URL.Path)
		assert.Equal(t, "Bearer test-key", r.Header.Get("Authorization"))
		var req embedReq
		require.NoError(t, json.NewDecoder(r.Body).Decode(&req))
		assert.Equal(t, "text-embedding-3-small", req.Model)
		assert.Equal(t, 4, req.Dimensions)
		assert.Equal(t, []string{"hello", "world"}, req.Input)
		_ = json.NewEncoder(w).Encode(embedResp{Data: []embedRespItem{
			{Index: 1, Embedding: []float32{0.5, 0.6, 0.7, 0.8}}, // out of order on purpose
			{Index: 0, Embedding: []float32{0.1, 0.2, 0.3, 0.4}},
		}})
	}))
	defer srv.Close()

	o, err := New(Options{BaseURL: srv.URL, Model: "text-embedding-3-small", APIKey: "test-key", Dimensions: 4})
	require.NoError(t, err)
	out, err := o.Embed(context.Background(), []string{"hello", "world"})
	require.NoError(t, err)
	require.Len(t, out, 2)
	assert.InDelta(t, float32(0.1), out[0][0], 1e-6) // index 0 → "hello"
	assert.InDelta(t, float32(0.5), out[1][0], 1e-6) // index 1 → "world"
}

func TestEmbed_DimMismatch(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(embedResp{Data: []embedRespItem{
			{Index: 0, Embedding: []float32{0.1, 0.2}},
		}})
	}))
	defer srv.Close()
	o, _ := New(Options{BaseURL: srv.URL, Model: "m", APIKey: "k", Dimensions: 4})
	_, err := o.Embed(context.Background(), []string{"x"})
	require.ErrorContains(t, err, "dim 2")
}

func TestEmbed_MissingIndex(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(embedResp{Data: []embedRespItem{
			{Index: 1, Embedding: []float32{0.1, 0.2, 0.3, 0.4}},
			{Index: 1, Embedding: []float32{0.5, 0.6, 0.7, 0.8}},
		}})
	}))
	defer srv.Close()
	o, _ := New(Options{BaseURL: srv.URL, Model: "m", APIKey: "k", Dimensions: 4})
	_, err := o.Embed(context.Background(), []string{"a", "b"})
	require.ErrorContains(t, err, "missing embedding for input 0")
}

func TestEmbed_HTTPError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":"invalid api key"}`))
	}))
	defer srv.Close()
	o, _ := New(Options{BaseURL: srv.URL, Model: "m", APIKey: "k", Dimensions: 4})
	_, err := o.Embed(context.Background(), []string{"x"})
	require.ErrorContains(t, err, "status 401")
}

func TestNew_Validation(t *testing.T) {
	_, err := New(Options{Dimensions: 4, APIKey: "k"})
	require.ErrorContains(t, err, "model required")
	_, err = New(Options{Model: "m", APIKey: "k"})
	require.ErrorContains(t, err, "dimensions")
	_, err = New(Options{Model: "m", Dimensions: 4})
	require.ErrorContains(t, err, "api_key required")
}
