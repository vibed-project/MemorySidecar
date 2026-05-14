// Package openai adapts OpenAI's /v1/embeddings endpoint (and any
// OpenAI-compatible server, including Azure OpenAI front-doors and several
// open-source alternatives) to the embedder.Embedder interface.
package openai

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
	defaultBaseURL = "https://api.openai.com"
	defaultTimeout = 30 * time.Second
)

// Options configures the adapter.
type Options struct {
	BaseURL    string
	Model      string
	APIKey     string
	Dimensions int
	Timeout    time.Duration
	HTTPClient *http.Client
}

// OpenAI is an embedder.Embedder backed by an OpenAI-compatible HTTP server.
type OpenAI struct {
	baseURL string
	model   string
	apiKey  string
	dim     int
	client  *http.Client
}

// New builds an OpenAI adapter. It does NOT call out to the server.
func New(opts Options) (*OpenAI, error) {
	if opts.Model == "" {
		return nil, errors.New("embedder/openai: model required")
	}
	if opts.Dimensions <= 0 {
		return nil, errors.New("embedder/openai: dimensions must be > 0")
	}
	if opts.APIKey == "" {
		return nil, errors.New("embedder/openai: api_key required")
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
	return &OpenAI{
		baseURL: base, model: opts.Model, apiKey: opts.APIKey, dim: opts.Dimensions, client: client,
	}, nil
}

func (o *OpenAI) Dimensions() int { return o.dim }

type embedReq struct {
	Model      string   `json:"model"`
	Input      []string `json:"input"`
	Dimensions int      `json:"dimensions,omitempty"`
}

type embedRespItem struct {
	Index     int       `json:"index"`
	Embedding []float32 `json:"embedding"`
}

type embedResp struct {
	Data []embedRespItem `json:"data"`
}

func (o *OpenAI) Embed(ctx context.Context, texts []string) ([][]float32, error) {
	if len(texts) == 0 {
		return nil, nil
	}
	// text-embedding-3-* models accept a `dimensions` request parameter to
	// truncate the returned vector; older ada-002 ignores it. Sending it is
	// safe in both cases.
	body, err := json.Marshal(embedReq{Model: o.model, Input: texts, Dimensions: o.dim})
	if err != nil {
		return nil, fmt.Errorf("embedder/openai: marshal: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, o.baseURL+"/v1/embeddings", bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("embedder/openai: request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+o.apiKey)

	resp, err := o.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("embedder/openai: http: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<10))
		return nil, fmt.Errorf("embedder/openai: status %d: %s", resp.StatusCode, bytes.TrimSpace(b))
	}
	var out embedResp
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, fmt.Errorf("embedder/openai: decode: %w", err)
	}
	if len(out.Data) != len(texts) {
		return nil, fmt.Errorf("embedder/openai: got %d embeddings for %d inputs", len(out.Data), len(texts))
	}

	// OpenAI returns items indexed by request position; preserve that order.
	vecs := make([][]float32, len(texts))
	for _, item := range out.Data {
		if item.Index < 0 || item.Index >= len(texts) {
			return nil, fmt.Errorf("embedder/openai: response index %d out of range", item.Index)
		}
		if len(item.Embedding) != o.dim {
			return nil, fmt.Errorf("embedder/openai: dim %d for input %d, configured %d", len(item.Embedding), item.Index, o.dim)
		}
		vecs[item.Index] = item.Embedding
	}
	for i, v := range vecs {
		if v == nil {
			return nil, fmt.Errorf("embedder/openai: missing embedding for input %d", i)
		}
	}
	return vecs, nil
}
