package agentfs

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"hash/fnv"
	"io"
	"math"
	"net/http"
	"net/url"
	"strings"
	"time"
	"unicode"
)

// Embedder maps text into a fixed-dimensional vector. Implementations must be
// safe for concurrent calls. The daemon defaults to a deterministic local
// hashing embedder so indexing remains private and needs no network service.
type Embedder interface {
	Model() string
	Dimensions() int
	Embed(context.Context, string) ([]float32, error)
}

type HashEmbedder struct{ dimensions int }

// NewHashEmbedder returns a compact, zero-dependency feature-hashing embedder.
func NewHashEmbedder(dimensions int) *HashEmbedder {
	if dimensions < 32 {
		dimensions = 32
	}
	return &HashEmbedder{dimensions: dimensions}
}

func (h *HashEmbedder) Model() string   { return fmt.Sprintf("agentfs-hash-v1-%d", h.dimensions) }
func (h *HashEmbedder) Dimensions() int { return h.dimensions }

func (h *HashEmbedder) Embed(ctx context.Context, text string) ([]float32, error) {
	vector := make([]float32, h.dimensions)
	words := strings.FieldsFunc(strings.ToLower(text), func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsNumber(r) && r != '_' && r != '-'
	})
	for index, word := range words {
		if index&255 == 0 {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
		}
		hasher := fnv.New64a()
		_, _ = hasher.Write([]byte(word))
		hash := hasher.Sum64()
		value := float32(1)
		if hash&(1<<63) != 0 {
			value = -1
		}
		vector[int(hash%uint64(h.dimensions))] += value
	}
	var norm float64
	for _, value := range vector {
		norm += float64(value * value)
	}
	if norm > 0 {
		scale := float32(1 / math.Sqrt(norm))
		for index := range vector {
			vector[index] *= scale
		}
	}
	return vector, nil
}

func (h *HashEmbedder) EmbedBatch(ctx context.Context, texts []string) ([][]float32, error) {
	vectors := make([][]float32, len(texts))
	for index, value := range texts {
		vector, err := h.Embed(ctx, value)
		if err != nil {
			return nil, err
		}
		vectors[index] = vector
	}
	return vectors, nil
}

// HTTPEmbedder calls an OpenAI-compatible /v1/embeddings endpoint. It works
// with hosted providers as well as private services such as vLLM and Ollama's
// OpenAI compatibility layer.
type HTTPEmbedder struct {
	endpoint   string
	apiKey     string
	model      string
	dimensions int
	client     *http.Client
}

func NewHTTPEmbedder(endpoint, apiKey, model string, dimensions int) (*HTTPEmbedder, error) {
	endpoint = strings.TrimSpace(endpoint)
	model = strings.TrimSpace(model)
	if endpoint == "" || model == "" || dimensions <= 0 {
		return nil, fmt.Errorf("HTTP embedder requires endpoint, model, and positive dimensions")
	}
	parsed, err := url.Parse(endpoint)
	if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" {
		return nil, fmt.Errorf("invalid embedding endpoint %q", endpoint)
	}
	if !strings.HasSuffix(strings.TrimRight(endpoint, "/"), "/v1/embeddings") {
		endpoint = strings.TrimRight(endpoint, "/") + "/v1/embeddings"
	}
	return &HTTPEmbedder{
		endpoint: endpoint, apiKey: apiKey, model: model, dimensions: dimensions,
		client: &http.Client{Timeout: 45 * time.Second},
	}, nil
}

func (h *HTTPEmbedder) Model() string   { return h.model }
func (h *HTTPEmbedder) Dimensions() int { return h.dimensions }

func (h *HTTPEmbedder) Embed(ctx context.Context, text string) ([]float32, error) {
	vectors, err := h.EmbedBatch(ctx, []string{text})
	if err != nil {
		return nil, err
	}
	return vectors[0], nil
}

func (h *HTTPEmbedder) EmbedBatch(ctx context.Context, texts []string) ([][]float32, error) {
	if len(texts) == 0 {
		return nil, nil
	}
	payload, err := json.Marshal(map[string]any{
		"model": h.model, "input": texts, "dimensions": h.dimensions,
	})
	if err != nil {
		return nil, fmt.Errorf("encode embedding request: %w", err)
	}
	var lastErr error
	for attempt := 0; attempt < 3; attempt++ {
		request, err := http.NewRequestWithContext(ctx, http.MethodPost, h.endpoint, bytes.NewReader(payload))
		if err != nil {
			return nil, fmt.Errorf("create embedding request: %w", err)
		}
		request.Header.Set("Content-Type", "application/json")
		if h.apiKey != "" {
			request.Header.Set("Authorization", "Bearer "+h.apiKey)
		}
		response, err := h.client.Do(request)
		if err == nil {
			vectors, retry, decodeErr := h.decodeResponse(response, len(texts))
			if decodeErr == nil {
				return vectors, nil
			}
			lastErr = decodeErr
			if !retry {
				return nil, decodeErr
			}
		} else {
			lastErr = err
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(time.Duration(attempt+1) * 200 * time.Millisecond):
		}
	}
	return nil, fmt.Errorf("embedding request failed after retries: %w", lastErr)
}

func (h *HTTPEmbedder) decodeResponse(response *http.Response, expected int) (vectors [][]float32, retry bool, err error) {
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		var message bytes.Buffer
		_, _ = message.ReadFrom(io.LimitReader(response.Body, 64*1024))
		return nil, response.StatusCode == http.StatusTooManyRequests || response.StatusCode >= 500,
			fmt.Errorf("embedding endpoint returned %s: %s", response.Status, strings.TrimSpace(message.String()))
	}
	var result struct {
		Data []struct {
			Index     int       `json:"index"`
			Embedding []float32 `json:"embedding"`
		} `json:"data"`
	}
	decoder := json.NewDecoder(io.LimitReader(response.Body, 64<<20))
	if err := decoder.Decode(&result); err != nil {
		return nil, false, fmt.Errorf("decode embedding response: %w", err)
	}
	vectors = make([][]float32, expected)
	for _, item := range result.Data {
		if item.Index < 0 || item.Index >= expected || len(item.Embedding) != h.dimensions {
			return nil, false, fmt.Errorf("invalid embedding index or dimension")
		}
		vectors[item.Index] = item.Embedding
	}
	for _, vector := range vectors {
		if len(vector) != h.dimensions {
			return nil, false, errors.New("embedding response omitted an input")
		}
	}
	return vectors, false, nil
}

type batchEmbedder interface {
	EmbedBatch(context.Context, []string) ([][]float32, error)
}

func embedTexts(ctx context.Context, embedder Embedder, texts []string) ([][]float32, error) {
	if batch, ok := embedder.(batchEmbedder); ok {
		// Keep provider requests bounded. Some OpenAI-compatible endpoints reject
		// otherwise valid requests solely because a large source file produced
		// hundreds of chunks.
		const maxBatch = 64
		vectors := make([][]float32, 0, len(texts))
		for start := 0; start < len(texts); start += maxBatch {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
			end := min(start+maxBatch, len(texts))
			part, err := batch.EmbedBatch(ctx, texts[start:end])
			if err != nil {
				return nil, fmt.Errorf("embed batch %d-%d: %w", start, end, err)
			}
			if len(part) != end-start {
				return nil, fmt.Errorf("embed batch %d-%d returned %d vectors", start, end, len(part))
			}
			vectors = append(vectors, part...)
		}
		return vectors, nil
	}
	vectors := make([][]float32, len(texts))
	for index, text := range texts {
		vector, err := embedder.Embed(ctx, text)
		if err != nil {
			return nil, err
		}
		vectors[index] = vector
	}
	return vectors, nil
}

func encodeVector(vector []float32) []byte {
	raw := make([]byte, len(vector)*4)
	for index, value := range vector {
		binary.LittleEndian.PutUint32(raw[index*4:], math.Float32bits(value))
	}
	return raw
}

func decodeVector(raw []byte, dimensions int) ([]float32, bool) {
	if dimensions <= 0 || len(raw) != dimensions*4 {
		return nil, false
	}
	vector := make([]float32, dimensions)
	for index := range vector {
		vector[index] = math.Float32frombits(binary.LittleEndian.Uint32(raw[index*4:]))
	}
	return vector, true
}

func vectorBucket(vector []float32) int64 {
	var bucket uint16
	for index := 0; index < min(8, len(vector)); index++ {
		if vector[index] >= 0 {
			bucket |= 1 << index
		}
	}
	return int64(bucket)
}

func cosine(left, right []float32) float64 {
	if len(left) != len(right) {
		return 0
	}
	var dot float64
	for index := range left {
		dot += float64(left[index] * right[index])
	}
	return dot
}
