package agentfs

import (
	"context"
	"fmt"
	"os"
	"sync"

	"github.com/clems4ever/all-minilm-l6-v2-go/all_minilm_l6_v2"
)

// ONNXEmbedder 用本地 all-MiniLM-L6-v2 模型（ONNX runtime）做真 embedding，
// 替代默认的 feature-hash embedder。384 维，与原始 sentence-transformers 模型对齐。
//
// Embedder 接口要求实现并发安全，但底层是单个 ONNX session 加一个有状态的
// tokenizer，两者都不能被多个 goroutine 同时使用；daemon 会从并发的 HTTP handler
// 里调过来。mu 把推理串行化，兑现接口声明的契约（LSP：结构上满足接口还不够，
// 行为上也得满足）。串行化不是瓶颈：Store 本身也只持有一条 SQLite 连接。
type ONNXEmbedder struct {
	mu     sync.Mutex
	model  *all_minilm_l6_v2.Model
	dims   int
	closed bool
}

// NewONNXEmbedder 加载本地 ONNX embedding 模型。runtimePath 指向 libonnxruntime.so；
// 为空时按常见位置（/usr/local/lib、ONNXRUNTIME_LIB_PATH、系统 dlopen 搜索路径）自动探测。
func NewONNXEmbedder(runtimePath string) (*ONNXEmbedder, error) {
	runtimePath = resolveRuntimePath(runtimePath)
	model, err := all_minilm_l6_v2.NewModel(all_minilm_l6_v2.WithRuntimePath(runtimePath))
	if err != nil {
		return nil, fmt.Errorf("load all-MiniLM-L6-v2 model: %w", err)
	}
	return &ONNXEmbedder{model: model, dims: 384}, nil
}

// resolveRuntimePath 把空路径解析成 dlopen 可直接打开的文件路径。onnxruntime_go 用
// dlopen 按字节串打开该路径，裸名 "libonnxruntime.so" 依赖 ld.so.cache，而 cache 常因
// 刚拷贝完 .so 尚未 ldconfig 而过期。这里优先返回可 stat 到的绝对路径。
func resolveRuntimePath(runtimePath string) string {
	if runtimePath != "" {
		return runtimePath
	}
	for _, candidate := range []string{
		"/usr/local/lib/libonnxruntime.so",
		"/usr/lib/libonnxruntime.so",
		"/usr/lib/x86_64-linux-gnu/libonnxruntime.so",
		"/lib/libonnxruntime.so",
	} {
		if _, err := os.Stat(candidate); err == nil {
			return candidate
		}
	}
	// 留空，交给库回退到 ONNXRUNTIME_LIB_PATH 环境变量或 dlopen 默认搜索路径。
	return ""
}

func (e *ONNXEmbedder) Model() string   { return "all-MiniLM-L6-v2" }
func (e *ONNXEmbedder) Dimensions() int { return e.dims }

// Close 释放 session 与进程级 ONNX 环境。可重复调用：调用方常常同时 defer 关闭
// embedder 和持有它的 Store，而底层的 DestroyEnvironment 不能执行两次。
func (e *ONNXEmbedder) Close() error {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.closed {
		return nil
	}
	e.closed = true
	return e.model.Close()
}

func (e *ONNXEmbedder) Embed(_ context.Context, text string) ([]float32, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.closed {
		return nil, fmt.Errorf("all-MiniLM-L6-v2 embedder is closed")
	}
	return e.model.Compute(text, true)
}

func (e *ONNXEmbedder) EmbedBatch(_ context.Context, texts []string) ([][]float32, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.closed {
		return nil, fmt.Errorf("all-MiniLM-L6-v2 embedder is closed")
	}
	return e.model.ComputeBatch(texts, true)
}
