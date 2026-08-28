package agentfs

import (
	"bytes"
	"cmp"
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
	"slices"
	"strings"
	"time"
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

// Model 里的 v2 是分词版本，不是代码版本：v1 按 Unicode 字母类别切词，一整段中文
// 会变成一个词，两段中文之间几乎不可能有词重叠，兜底 embedder 对中文完全失效。
// v2 改用 analyzeTerms（CJK 切 bigram）。向量召回都带 `WHERE model=?`，所以旧的
// v1 向量会自动被忽略，重新 scan 一次即可，不需要 schema 迁移。
func (h *HashEmbedder) Model() string   { return fmt.Sprintf("agentfs-hash-v2-%d", h.dimensions) }
func (h *HashEmbedder) Dimensions() int { return h.dimensions }

func (h *HashEmbedder) Embed(ctx context.Context, text string) ([]float32, error) {
	vector := make([]float32, h.dimensions)
	words := analyzeTerms(text)
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

// vectorBucketBits 是 sign-LSH 用到的维度数：取 embedding 前 N 维的符号拼成一个
// N 位整数，共 2^N 个桶。改这个值需要重建 embeddings/chunk_embeddings 的 bucket 列。
const vectorBucketBits = 8

func vectorBucket(vector []float32) int64 {
	var bucket uint16
	for index := range min(vectorBucketBits, len(vector)) {
		if vector[index] >= 0 {
			bucket |= 1 << index
		}
	}
	return int64(bucket)
}

// vectorProbes 返回一次向量召回该扫描的桶：精确桶，加上把每一个「符号不可靠」的
// 维度各翻转一次得到的邻桶（汉明距离 ≤ 1）。
//
// 只查精确桶是这套 LSH 最大的漏召回来源。query 与文档 embedding 只要在前 8 维里
// 有任意一维符号不同，整桶文档就彻底看不见——而 sign-LSH 的分桶边界是超平面，
// 分量绝对值越接近 0 的维度，说明这个点离超平面越近，符号越是「差一点就翻过去」，
// 恰恰最不该被当成硬边界。语义上高度相关的一对向量，在某个近零维上符号相反是
// 完全正常的。
//
// 后置条件：返回 1+min(vectorBucketBits,len) 个互不相同的桶；邻桶按 |分量| 升序，
// 即最值得探查的排在最前面。
func vectorProbes(vector []float32) []int64 {
	exact := vectorBucket(vector)
	width := min(vectorBucketBits, len(vector))
	dimensions := make([]int, width)
	for index := range width {
		dimensions[index] = index
	}
	slices.SortFunc(dimensions, func(left, right int) int {
		return cmp.Compare(math.Abs(float64(vector[left])), math.Abs(float64(vector[right])))
	})
	probes := make([]int64, 0, width+1)
	probes = append(probes, exact)
	for _, dimension := range dimensions {
		probes = append(probes, exact^(1<<dimension))
	}
	return probes
}

// cosineSimilarity 返回两个向量的余弦相似度，值域 [-1,1]。
//
// 不要假设 embedding 已经 L2 归一化就退化成点积：HashEmbedder 归一化，本地 ONNX
// 模型是否归一化取决于导出的计算图，而 HTTPEmbedder 对接的是任意 OpenAI 兼容服务，
// 归一化与否完全由对方决定。未归一化时点积会随向量模长放大——长文本分数虚高，
// 还会超出 [0,1]，把融合权重（向量一路只该占 0.38）冲垮。
func cosineSimilarity(left, right []float32) float64 {
	if len(left) != len(right) || len(left) == 0 {
		return 0
	}
	var dot, leftNorm, rightNorm float64
	for index := range left {
		leftValue, rightValue := float64(left[index]), float64(right[index])
		dot += leftValue * rightValue
		leftNorm += leftValue * leftValue
		rightNorm += rightValue * rightValue
	}
	if leftNorm == 0 || rightNorm == 0 {
		return 0
	}
	return dot / math.Sqrt(leftNorm*rightNorm)
}
