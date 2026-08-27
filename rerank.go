package agentfs

import (
	"bytes"
	"context"
	_ "embed"
	"fmt"
	"math"
	"sync"

	"github.com/sugarme/tokenizer"
	"github.com/sugarme/tokenizer/pretrained"
	ort "github.com/yalue/onnxruntime_go"
)

//go:embed tokenizer.json
var rerankTokenizerJSON []byte

// Reranker 给 (query, doc) 对打相关性分，用于二阶段重排。分数只用于相对排序，
// 不要求任何绝对尺度；返回的 scores 必须与 docs 一一对应、等长。实现必须并发安全。
//
// 这里刻意是接口而不是直接用 *CrossEncoder：级联排序的契约（第二阶段只重排
// top-K，不改变 K 内外次序）是纯逻辑，不该只能靠加载 90MB ONNX 模型才验证得了。
type Reranker interface {
	Score(ctx context.Context, query string, docs []string) ([]float32, error)
}

// CrossEncoder 用 cross-encoder（ms-marco-MiniLM-L-6-v2）对 query 与每个候选文档联合
// 打分，做二阶段重排。bi-encoder 把 query 和 doc 分别编码成独立向量，无法建模两者之间的
// 词级交互；cross-encoder 把 `[CLS] query [SEP] doc [SEP]` 一起送进模型，检索精度更高，
// 代价是每个 (query, doc) 对都要过一遍模型，所以只用来重排第一阶段召回的 top-K。
//
// mu 串行化推理：一个 CrossEncoder 被整个 daemon 共享，而 ONNX session 与
// tokenizer 都不能并发使用。
type CrossEncoder struct {
	mu        sync.Mutex
	tokenizer tokenizer.Tokenizer
	session   *ort.DynamicAdvancedSession
}

// NewCrossEncoder 从本地 ONNX 文件加载 cross-encoder。modelPath 指向
// ms-marco-MiniLM-L-6-v2 的 onnx/model.onnx；runtimePath 为空时自动探测 libonnxruntime.so。
// 若 ONNX 环境已被 bi-encoder 初始化，则复用之，避免二次 InitializeEnvironment 报错。
func NewCrossEncoder(modelPath, runtimePath string) (*CrossEncoder, error) {
	if modelPath == "" {
		return nil, fmt.Errorf("cross-encoder requires a model path")
	}
	loadedTokenizer, err := pretrained.FromReader(bytes.NewBuffer(rerankTokenizerJSON))
	if err != nil {
		return nil, fmt.Errorf("load rerank tokenizer: %w", err)
	}
	if !ort.IsInitialized() {
		if runtimePath == "" {
			runtimePath = resolveRuntimePath("")
		}
		if runtimePath != "" {
			ort.SetSharedLibraryPath(runtimePath)
		}
		if err := ort.InitializeEnvironment(); err != nil {
			return nil, fmt.Errorf("initialize onnx runtime: %w", err)
		}
	}
	session, err := ort.NewDynamicAdvancedSession(modelPath,
		[]string{"input_ids", "attention_mask", "token_type_ids"},
		[]string{"logits"}, nil)
	if err != nil {
		return nil, fmt.Errorf("create cross-encoder session: %w", err)
	}
	return &CrossEncoder{tokenizer: *loadedTokenizer, session: session}, nil
}

// Close 销毁 session。可重复调用；不销毁进程级 ONNX 环境——那是 ONNXEmbedder
// 的职责，两者共用同一个环境。
func (c *CrossEncoder) Close() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.session == nil {
		return nil
	}
	session := c.session
	c.session = nil
	return session.Destroy()
}

// Score 返回每个 doc 相对 query 的相关性 logit（未归一化，仅用于相对排序）。
// docs 与返回的 scores 一一对应，长度相同。
func (c *CrossEncoder) Score(_ context.Context, query string, docs []string) ([]float32, error) {
	if len(docs) == 0 {
		return nil, nil
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.session == nil {
		return nil, fmt.Errorf("cross-encoder is closed")
	}
	inputs := make([]tokenizer.EncodeInput, len(docs))
	for index, doc := range docs {
		inputs[index] = tokenizer.NewDualEncodeInput(
			tokenizer.NewRawInputSequence(query),
			tokenizer.NewRawInputSequence(doc),
		)
	}
	encodings, err := c.tokenizer.EncodeBatch(inputs, true)
	if err != nil {
		return nil, fmt.Errorf("tokenize rerank pairs: %w", err)
	}
	batchSize := len(encodings)
	// 取批内最长序列做张量宽度，短的补 pad（id 0，attention_mask 0，BERT 的约定）。
	// 直接拿 encodings[0] 的长度当宽度，是在赌 tokenizer.json 配了固定长度 padding：
	// 当前 bundle 确实配了 Fixed 128，但换一份没配 padding 的 tokenizer.json 就会
	// 在第一个更长的样本上越界 panic——一个数据文件的改动不该能让服务崩掉。
	seqLength := 0
	for _, encoding := range encodings {
		seqLength = max(seqLength, len(encoding.Ids))
	}
	if seqLength == 0 {
		return nil, fmt.Errorf("tokenizer produced empty rerank sequences")
	}

	inputIDs := make([]int64, batchSize*seqLength)
	attentionMask := make([]int64, batchSize*seqLength)
	tokenTypeIDs := make([]int64, batchSize*seqLength)
	for batch, encoding := range encodings {
		base := batch * seqLength
		for position, id := range encoding.Ids {
			inputIDs[base+position] = int64(id)
		}
		for position, mask := range encoding.AttentionMask {
			attentionMask[base+position] = int64(mask)
		}
		for position, typeID := range encoding.TypeIds {
			tokenTypeIDs[base+position] = int64(typeID)
		}
	}

	inputShape := ort.NewShape(int64(batchSize), int64(seqLength))
	inputIDsTensor, err := ort.NewTensor(inputShape, inputIDs)
	if err != nil {
		return nil, fmt.Errorf("create input_ids tensor: %w", err)
	}
	defer inputIDsTensor.Destroy()
	attentionMaskTensor, err := ort.NewTensor(inputShape, attentionMask)
	if err != nil {
		return nil, fmt.Errorf("create attention_mask tensor: %w", err)
	}
	defer attentionMaskTensor.Destroy()
	tokenTypeIDsTensor, err := ort.NewTensor(inputShape, tokenTypeIDs)
	if err != nil {
		return nil, fmt.Errorf("create token_type_ids tensor: %w", err)
	}
	defer tokenTypeIDsTensor.Destroy()

	outputTensor, err := ort.NewEmptyTensor[float32](ort.NewShape(int64(batchSize), 1))
	if err != nil {
		return nil, fmt.Errorf("create logits tensor: %w", err)
	}
	defer outputTensor.Destroy()

	if err := c.session.Run(
		[]ort.Value{inputIDsTensor, attentionMaskTensor, tokenTypeIDsTensor},
		[]ort.Value{outputTensor},
	); err != nil {
		return nil, fmt.Errorf("run cross-encoder: %w", err)
	}

	logits := outputTensor.GetData()
	scores := make([]float32, batchSize)
	copy(scores, logits)
	return scores, nil
}

// sigmoid 把 cross-encoder logit 压到 (0,1)，与第一阶段融合分数同尺度，便于加权合并。
func sigmoid(logit float64) float64 {
	if logit >= 0 {
		return 1 / (1 + math.Exp(-logit))
	}
	exponential := math.Exp(logit)
	return exponential / (1 + exponential)
}
