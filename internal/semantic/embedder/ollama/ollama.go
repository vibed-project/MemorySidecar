// Package ollama adapts Ollama's HTTP /api/embed endpoint to the
// embedder.Embedder interface. Suitable for local-dev with zero API keys;
// the operator is responsible for running an Ollama server and pulling the
// model (e.g. `ollama pull nomic-embed-text`).
package ollama

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

const (
	defaultBaseURL = "http://localhost:11434"
	defaultTimeout = 30 * time.Second
)

// Options configures the adapter.
type Options struct {
	BaseURL    string
	Model      string
	Dimensions int
	Timeout    time.Duration
	HTTPClient *http.Client // nil → built from Timeout
}

// Ollama is an embedder.Embedder backed by an Ollama HTTP server.
type Ollama struct {
	baseURL string
	model   string
	dim     int
	client  *http.Client
}

// New builds an Ollama adapter. It does NOT call out to the server.
func New(opts Options) (*Ollama, error) {
	if opts.Model == "" {
		return nil, errors.New("embedder/ollama: model required")
	}
	if opts.Dimensions <= 0 {
		return nil, errors.New("embedder/ollama: dimensions must be > 0")
	}
	base := strings.TrimRight(opts.BaseURL, "/")
	if base == "" {
		base = defaultBaseURL
	}
	client := opts.HTTPClient
	if client == nil {
		t := opts.Timeout
		if t == 0 {
			t = defaultTimeout
		}
		client = &http.Client{Timeout: t}
	}
	return &Ollama{baseURL: base, model: opts.Model, dim: opts.Dimensions, client: client}, nil
}

func (o *Ollama) Dimensions() int { return o.dim }

type embedReq struct {
	Model string   `json:"model"`
	Input []string `json:"input"`
}

type embedResp struct {
	Embeddings [][]float32 `json:"embeddings"`
}

func (o *Ollama) Embed(ctx context.Context, texts []string) ([][]float32, error) {
	if len(texts) == 0 {
		return nil, nil
	}
	body, err := json.Marshal(embedReq{Model: o.model, Input: texts})
	if err != nil {
		return nil, fmt.Errorf("embedder/ollama: marshal: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, o.baseURL+"/api/embed", bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("embedder/ollama: request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := o.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("embedder/ollama: http: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<10))
		return nil, fmt.Errorf("embedder/ollama: status %d: %s", resp.StatusCode, bytes.TrimSpace(b))
	}
	var out embedResp
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, fmt.Errorf("embedder/ollama: decode: %w", err)
	}
	if len(out.Embeddings) != len(texts) {
		return nil, fmt.Errorf("embedder/ollama: got %d embeddings for %d inputs", len(out.Embeddings), len(texts))
	}
	for i, v := range out.Embeddings {
		if len(v) != o.dim {
			return nil, fmt.Errorf("embedder/ollama: model returned dim %d for input %d, configured %d", len(v), i, o.dim)
		}
	}
	return out.Embeddings, nil
}
