package agentfs

import (
	"context"
	"math"
	"math/bits"
	"slices"
	"strings"
	"testing"
	"unicode/utf8"
)

// stubReranker 是一个可预测的二阶段打分器，让级联排序的契约不必加载 90MB ONNX
// 模型也能验证。scores 按 doc 出现顺序取用。
type stubReranker struct {
	logits []float32
	calls  int
}

func (s *stubReranker) Score(_ context.Context, _ string, docs []string) ([]float32, error) {
	s.calls++
	scores := make([]float32, len(docs))
	for index := range docs {
		if index < len(s.logits) {
			scores[index] = s.logits[index]
		} else {
			scores[index] = -10
		}
	}
	return scores, nil
}

// TestRerankKeepsUnrerankedHitsBehind 锁死级联检索的契约：cross-encoder 只重排
// 第一阶段的 top-K，K 之外的候选不能凭第一阶段的融合分反超 K 之内的候选。
//
// 回归的是这样一个真实故障：cross-encoder 给 top-K 全打了低分（sigmoid ≈ 0.1），
// 而第 K+1 名还带着 0.5 的融合分——两套完全不可比的尺度一起排序，一个模型从没
// 看过的候选就被排到了第一位。
func TestRerankKeepsUnrerankedHitsBehind(t *testing.T) {
	t.Parallel()
	// 10 个命中，第一阶段分数递减。limit=1 → rerankCount = max(2,10) = 10 覆盖全部，
	// 所以这里用 limit=1 但只给 hits 12 条，让窗口是 10、窗口外有 2 条。
	hits := make([]SearchHit, 12)
	for index := range hits {
		hits[index] = SearchHit{
			Path:  string(rune('a'+index)) + ".go",
			Score: 1 - float64(index)*0.01, // 0.99 ... 0.88，全都远高于 sigmoid(-5)
		}
	}
	// 窗口内 10 个全部打成很低的 logit；sigmoid(-5) ≈ 0.0067，低于窗口外的 0.89/0.88。
	store := &Store{reranker: &stubReranker{logits: []float32{-5, -5, -5, -5, -5, -5, -5, -5, -5, -4}}}
	if err := store.rerank(context.Background(), HybridRequest{Query: "q", Limit: 1}, hits); err != nil {
		t.Fatalf("rerank: %v", err)
	}
	for index := range 10 {
		if hits[index].RerankScore <= 0 {
			t.Fatalf("hit %d (%s) is inside the rerank window but has no rerank score", index, hits[index].Path)
		}
	}
	for index := 10; index < len(hits); index++ {
		if hits[index].RerankScore != 0 {
			t.Fatalf("hit %d (%s) is outside the rerank window yet was scored", index, hits[index].Path)
		}
	}
	// 窗口内 logit 最高的那个（-4，原本排第 10）必须升到第一，而不是窗口外那两个
	// 融合分 0.89/0.88 的候选。
	if hits[0].RerankScore != sigmoid(-4) {
		t.Fatalf("top hit is not the best reranked candidate: %#v", hits[0])
	}
}

// TestReciprocalRankFusionSmoothsAdjacentRanks 锁死 RRF 的平滑性质：相邻名次的
// 贡献差必须小于「向量一路整路的最大贡献」，否则词法第一名会独裁排序。
func TestReciprocalRankFusionSmoothsAdjacentRanks(t *testing.T) {
	t.Parallel()
	if got := reciprocalRank(0); got != 0 {
		t.Fatalf("absent arm must contribute 0, got %v", got)
	}
	if got := reciprocalRank(1); math.Abs(got-1) > 1e-9 {
		t.Fatalf("rank 1 must contribute 1.0, got %v", got)
	}
	const lexWeight, vecWeight = 0.52, 0.38
	gap := lexWeight * (reciprocalRank(1) - reciprocalRank(2))
	if gap >= vecWeight*reciprocalRank(1) {
		t.Fatalf("lexical rank-1-vs-2 gap %v swamps the whole vector arm %v", gap, vecWeight)
	}
	for rank := 1; rank < 50; rank++ {
		if reciprocalRank(rank) <= reciprocalRank(rank+1) {
			t.Fatalf("reciprocalRank must be strictly decreasing at rank %d", rank)
		}
	}
}

// TestAssignVectorRanksOnlyRanksScoredCandidates 验证「缺席」与「排在最后」是两
// 件事：没有向量得分的候选不能拿到名次，否则它会白拿一份向量一路的贡献。
func TestAssignVectorRanksOnlyRanksScoredCandidates(t *testing.T) {
	t.Parallel()
	candidates := []*hybridCandidate{
		{hit: SearchHit{Path: "low.go", VectorScore: 0.1}},
		{hit: SearchHit{Path: "none.go", VectorScore: 0}},
		{hit: SearchHit{Path: "high.go", VectorScore: 0.9}},
	}
	assignVectorRanks(candidates)
	if candidates[2].vecRank != 1 {
		t.Fatalf("highest cosine must rank 1, got %d", candidates[2].vecRank)
	}
	if candidates[0].vecRank != 2 {
		t.Fatalf("lower cosine must rank 2, got %d", candidates[0].vecRank)
	}
	if candidates[1].vecRank != 0 {
		t.Fatalf("candidate without a vector score must stay unranked, got %d", candidates[1].vecRank)
	}
}

// TestCosineSimilarityIgnoresMagnitude 验证 cosine 是真余弦而不是裸点积：
// 把向量整体放大 10 倍，相似度必须不变，且始终落在 [-1,1]。
func TestCosineSimilarityIgnoresMagnitude(t *testing.T) {
	t.Parallel()
	left := []float32{3, 4, 0}
	right := []float32{3, 4, 0}
	if got := cosineSimilarity(left, right); math.Abs(got-1) > 1e-6 {
		t.Fatalf("identical direction must give 1.0, got %v", got)
	}
	scaled := []float32{30, 40, 0}
	if got := cosineSimilarity(left, scaled); math.Abs(got-1) > 1e-6 {
		t.Fatalf("magnitude must not change similarity, got %v", got)
	}
	if got := cosineSimilarity([]float32{1, 0}, []float32{0, 1}); math.Abs(got) > 1e-9 {
		t.Fatalf("orthogonal vectors must give 0, got %v", got)
	}
	if got := cosineSimilarity([]float32{0, 0}, []float32{1, 1}); got != 0 {
		t.Fatalf("zero vector must give 0, not NaN, got %v", got)
	}
	if got := cosineSimilarity([]float32{1, 2}, []float32{1, 2, 3}); got != 0 {
		t.Fatalf("mismatched dimensions must give 0, got %v", got)
	}
}

// TestCompactSnippetKeepsUTF8Boundary 验证按字节截断不会劈开多字节字符——本仓库
// 索引的正文本身就大量是中文。
func TestCompactSnippetKeepsUTF8Boundary(t *testing.T) {
	t.Parallel()
	// 每个汉字 3 字节；limit=7 落在第三个字的中间。
	snippet := compactSnippet("事务索引与检索", 7)
	trimmed := strings.TrimSuffix(snippet, "…")
	if !utf8.ValidString(trimmed) {
		t.Fatalf("compactSnippet produced invalid UTF-8: %q", trimmed)
	}
	if trimmed != "事务" {
		t.Fatalf("compactSnippet = %q, want %q", trimmed, "事务")
	}
	if got := compactSnippet("短", 100); got != "短" {
		t.Fatalf("under-limit text must pass through unchanged, got %q", got)
	}
}

// TestFTSMatchDropsUnusableQueries 验证全停用词/纯标点的 query 不会生成
// FTS5 语法错误的空短语。
func TestFTSMatchDropsUnusableQueries(t *testing.T) {
	t.Parallel()
	for _, query := range []string{"what is it", "the and or", "??? --- ...", "   "} {
		if got := ftsMatch(query); got != "" {
			t.Fatalf("ftsMatch(%q) = %q, want empty so the lexical arm is skipped", query, got)
		}
	}
	if got := ftsMatch("hybrid retrieval"); got != `"hybrid" OR "retrieval"` {
		t.Fatalf("ftsMatch lost usable terms: %q", got)
	}
}

// TestVectorProbesCoverHammingOne 锁死 multi-probe 的召回面：探查集必须包含精确
// 桶，以及每个维度各翻转一次得到的全部邻桶，且邻桶按「符号可靠性」从低到高排列。
func TestVectorProbesCoverHammingOne(t *testing.T) {
	t.Parallel()
	// 前 8 维全为正 → 精确桶 0b11111111 = 255。第 3 维最接近 0，符号最不可靠。
	vector := []float32{0.9, 0.8, 0.7, 0.01, 0.6, 0.5, 0.4, 0.3, 0.2}
	probes := vectorProbes(vector)
	if len(probes) != vectorBucketBits+1 {
		t.Fatalf("expected %d probes, got %d", vectorBucketBits+1, len(probes))
	}
	if probes[0] != vectorBucket(vector) {
		t.Fatalf("first probe must be the exact bucket, got %d", probes[0])
	}
	if probes[1] != probes[0]^(1<<3) {
		t.Fatalf("least-confident dimension must be probed first, got %d", probes[1])
	}
	seen := make(map[int64]bool, len(probes))
	for _, probe := range probes {
		if seen[probe] {
			t.Fatalf("duplicate probe bucket %d", probe)
		}
		seen[probe] = true
	}
	// 每个邻桶与精确桶的汉明距离必须恰好是 1。
	for _, probe := range probes[1:] {
		if bits.OnesCount64(uint64(probe^probes[0])) != 1 {
			t.Fatalf("probe %d is not at Hamming distance 1 from %d", probe, probes[0])
		}
	}
}

// TestVectorProbesRecallsNearMissBucket 是这次修复要解决的具体场景：两个语义几乎
// 一致的向量，只在一个近零维上符号相反，单桶精确匹配会把它整个漏掉。
func TestVectorProbesRecallsNearMissBucket(t *testing.T) {
	t.Parallel()
	query := []float32{0.9, 0.8, 0.7, 0.01, 0.6, 0.5, 0.4, 0.3}
	document := []float32{0.9, 0.8, 0.7, -0.01, 0.6, 0.5, 0.4, 0.3}
	if vectorBucket(query) == vectorBucket(document) {
		t.Fatal("test setup is wrong: the two vectors must land in different buckets")
	}
	if cosineSimilarity(query, document) < 0.99 {
		t.Fatal("test setup is wrong: the two vectors must be near-identical")
	}
	if !slices.Contains(vectorProbes(query), vectorBucket(document)) {
		t.Fatal("multi-probe must reach the near-identical document's bucket")
	}
}
