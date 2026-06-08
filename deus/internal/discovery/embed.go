package discovery

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"strings"
	"time"
)

const defaultEmbedDim = 768

// Embedder maps text to a fixed-dimensional vector.
type Embedder interface {
	Embed(ctx context.Context, text string) ([]float32, error)
	Dim() int
	Model() string
	// Semantic reports whether vectors encode text meaning (false for hash dev stub).
	Semantic() bool
}

// HashEmbedder is a deterministic dev/test embedder (same text → same vector).
type HashEmbedder struct {
	dim   int
	model string
}

// NewHashEmbedder returns a 768-dim deterministic embedder.
func NewHashEmbedder() *HashEmbedder {
	return &HashEmbedder{dim: defaultEmbedDim, model: "hash-stub@v1"}
}

func (h *HashEmbedder) Dim() int          { return h.dim }
func (h *HashEmbedder) Model() string     { return h.model }
func (h *HashEmbedder) Semantic() bool    { return false }

func (h *HashEmbedder) Embed(_ context.Context, text string) ([]float32, error) {
	sum := sha256.Sum256([]byte(text))
	vec := make([]float32, h.dim)
	for i := 0; i < h.dim; i++ {
		off := (i * 4) % (len(sum) - 7)
		seed := binary.LittleEndian.Uint64(sum[off : off+8])
		vec[i] = float32(math.Sin(float64(seed%1000000) / 1000000.0 * 2 * math.Pi))
	}
	norm := float32(0)
	for _, v := range vec {
		norm += v * v
	}
	if norm > 0 {
		n := float32(1 / math.Sqrt(float64(norm)))
		for i := range vec {
			vec[i] *= n
		}
	}
	return vec, nil
}

// HTTPEmbedder calls an OpenAI-compatible embedding HTTP API.
type HTTPEmbedder struct {
	Endpoint  string
	modelName string
	Dimension int
	client    *http.Client
}

// NewHTTPEmbedder wires a remote embedder (Fireworks / gateway route).
func NewHTTPEmbedder(endpoint, model string, dim int) *HTTPEmbedder {
	if dim <= 0 {
		dim = defaultEmbedDim
	}
	return &HTTPEmbedder{
		Endpoint:  strings.TrimRight(endpoint, "/"),
		modelName: model,
		Dimension: dim,
		client:    &http.Client{Timeout: 15 * time.Second},
	}
}

func (h *HTTPEmbedder) Dim() int          { return h.Dimension }
func (h *HTTPEmbedder) Model() string     { return h.modelName }
func (h *HTTPEmbedder) Semantic() bool    { return true }

func (h *HTTPEmbedder) Embed(ctx context.Context, text string) ([]float32, error) {
	body, _ := json.Marshal(map[string]any{
		"input": text,
		"model": h.modelName,
	})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, h.Endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := h.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("discovery: embed http: %w", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 300 {
		return nil, fmt.Errorf("discovery: embed status %d: %s", resp.StatusCode, string(raw))
	}
	var out struct {
		Data []struct {
			Embedding []float32 `json:"embedding"`
		} `json:"data"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, err
	}
	if len(out.Data) == 0 || len(out.Data[0].Embedding) == 0 {
		return nil, fmt.Errorf("discovery: empty embedding response")
	}
	return out.Data[0].Embedding, nil
}

// NewEmbedderFromConfig picks HTTP or hash embedder.
func NewEmbedderFromConfig(endpoint, model string) Embedder {
	if strings.TrimSpace(endpoint) != "" {
		return NewHTTPEmbedder(endpoint, model, defaultEmbedDim)
	}
	return NewHashEmbedder()
}
