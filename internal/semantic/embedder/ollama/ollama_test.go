package ollama

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
		assert.Equal(t, "/api/embed", r.URL.Path)
		var req embedReq
		require.NoError(t, json.NewDecoder(r.Body).Decode(&req))
		assert.Equal(t, "nomic-embed-text", req.Model)
		assert.Equal(t, []string{"hello", "world"}, req.Input)
		_ = json.NewEncoder(w).Encode(embedResp{Embeddings: [][]float32{
			{0.1, 0.2, 0.3, 0.4},
			{0.5, 0.6, 0.7, 0.8},
		}})
	}))
	defer srv.Close()

	o, err := New(Options{BaseURL: srv.URL, Model: "nomic-embed-text", Dimensions: 4})
	require.NoError(t, err)

	out, err := o.Embed(context.Background(), []string{"hello", "world"})
	require.NoError(t, err)
	require.Len(t, out, 2)
	assert.InDelta(t, float32(0.1), out[0][0], 1e-6)
}

func TestEmbed_EmptyInputs(t *testing.T) {
	o, _ := New(Options{BaseURL: "http://unused", Model: "m", Dimensions: 4})
	out, err := o.Embed(context.Background(), nil)
	require.NoError(t, err)
	assert.Nil(t, out)
}

func TestEmbed_DimMismatch(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(embedResp{Embeddings: [][]float32{{0.1, 0.2}}})
	}))
	defer srv.Close()
	o, _ := New(Options{BaseURL: srv.URL, Model: "m", Dimensions: 4})
	_, err := o.Embed(context.Background(), []string{"x"})
	require.ErrorContains(t, err, "model returned dim 2")
}

func TestEmbed_CountMismatch(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(embedResp{Embeddings: [][]float32{{0.1, 0.2, 0.3, 0.4}}})
	}))
	defer srv.Close()
	o, _ := New(Options{BaseURL: srv.URL, Model: "m", Dimensions: 4})
	_, err := o.Embed(context.Background(), []string{"a", "b"})
	require.ErrorContains(t, err, "got 1 embeddings for 2 inputs")
}

func TestEmbed_HTTPError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"error":"boom"}`))
	}))
	defer srv.Close()
	o, _ := New(Options{BaseURL: srv.URL, Model: "m", Dimensions: 4})
	_, err := o.Embed(context.Background(), []string{"x"})
	require.ErrorContains(t, err, "status 500")
}

func TestNew_Validation(t *testing.T) {
	_, err := New(Options{Dimensions: 4})
	require.ErrorContains(t, err, "model required")
	_, err = New(Options{Model: "m"})
	require.ErrorContains(t, err, "dimensions")
}
