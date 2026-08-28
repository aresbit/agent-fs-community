package agentfs

import (
	"os"
	"path/filepath"
	"testing"
)

// TestConceptFusionRecallsRelatedFile 验证 S3 的核心：概念融合能把「不含查询词、
// 但与查询概念强共现」的文件召回，而不是只靠 FTS 命中查询词。
//
// 场景：cache.md 直接讲缓存（FTS 命中「缓存」）；memory.md 讲主存，正文只提「缓存
// 命中率」的关联概念，不含「缓存」作为独立词频信号。baseline 只召回 cache.md，
// 概念融合应额外召回 memory.md。
func TestConceptFusionRecallsRelatedFile(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	files := map[string]string{
		"cache.md":  "## 缓存\n缓存是体系结构的关键部件。缓存命中率直接影响性能。缓存用 AMAT 评估平均访存。缓存预取提高命中率。多级缓存和缓存回填都是缓存设计。\n",
		"memory.md": "## 主存\n主存是存储层次的关键部件。主存通过 AMAT 与命中率交互。主存的命中率影响性能。主存预取提高访存效率。主存回填策略也很重要。\n",
	}
	for name, content := range files {
		if err := os.WriteFile(filepath.Join(root, name), []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	baseline, _ := newTestStore(t, Options{})
	if _, err := baseline.Scan(t.Context(), root, ScanOptions{}); err != nil {
		t.Fatal(err)
	}
	fused, _ := newTestStore(t, Options{ConceptFusion: true})
	if _, err := fused.Scan(t.Context(), root, ScanOptions{}); err != nil {
		t.Fatal(err)
	}

	baselineHits, err := baseline.HybridSearch(t.Context(), HybridRequest{Query: "缓存", Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	fusedHits, err := fused.HybridSearch(t.Context(), HybridRequest{Query: "缓存", Limit: 10})
	if err != nil {
		t.Fatal(err)
	}

	paths := func(hits []SearchHit) map[string]bool {
		out := make(map[string]bool, len(hits))
		for _, hit := range hits {
			out[filepath.Base(hit.Path)] = true
		}
		return out
	}
	baselinePaths := paths(baselineHits)
	fusedPaths := paths(fusedHits)

	if baselinePaths["memory.md"] {
		t.Errorf("baseline should not recall memory.md (no lexical match for 缓存): %v", baselinePaths)
	}
	if !fusedPaths["memory.md"] {
		t.Errorf("concept fusion should recall memory.md via co-occurrence: baseline=%v fused=%v",
			baselinePaths, fusedPaths)
	}
}
