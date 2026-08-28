package agentfs

import (
	"os"
	"path/filepath"
	"testing"
)

// TestExtractConceptsHeadingFiltersBoilerplate 验证概念提取：标题概念被提取、
// 样板小标题被过滤。
func TestExtractConceptsHeadingFiltersBoilerplate(t *testing.T) {
	t.Parallel()
	text := "## 3.1 分支预测\n分支预测是流水线核心技术。\n## 本章概要\n本章概要介绍基础。"
	got := map[string]string{}
	for _, c := range extractConcepts(text) {
		got[c.name] = c.kind
	}
	if got["分支预测"] != "heading" {
		t.Errorf("分支预测 kind = %q, want heading (got %v)", got["分支预测"], got)
	}
	if _, ok := got["本章概要"]; ok {
		t.Errorf("boilerplate 本章概要 must be filtered out: %v", got)
	}
	// bigram 术语：正文里「预测」出现多次，应收为 term。
	if got["预测"] != "term" {
		t.Errorf("预测 kind = %q, want term (got %v)", got["预测"], got)
	}
}

// TestConceptGraphBuiltByScan 验证 S1 验收：扫描后概念图三张表都有数据，同一
// chunk 内共现的概念产生共现边。用标题概念（整词）验证共现，避免 bigram 切分
// 把正文里的长词拆碎。
func TestConceptGraphBuiltByScan(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	content := "## 卡尔曼滤波\n## 协方差\n卡尔曼滤波和协方差都用于状态估计。\n"
	if err := os.WriteFile(filepath.Join(root, "kalman.md"), []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	store, _ := newTestStore(t, Options{})
	if _, err := store.Scan(t.Context(), root, ScanOptions{}); err != nil {
		t.Fatal(err)
	}

	// 三张表非空。
	for _, q := range []string{
		"SELECT count(*) FROM concepts",
		"SELECT count(*) FROM concept_occurrences",
		"SELECT count(*) FROM concept_edges",
	} {
		res, err := store.Query(t.Context(), q)
		if err != nil {
			t.Fatalf("%s: %v", q, err)
		}
		if n, ok := res.Rows[0][0].(int64); !ok || n == 0 {
			t.Fatalf("%s = %#v, want non-zero", q, res.Rows[0][0])
		}
	}

	// 「卡尔曼滤波」是 heading 概念，doc_count = 1（只出现在一个 chunk）。
	res, err := store.Query(t.Context(),
		"SELECT kind, doc_count FROM concepts WHERE name='卡尔曼滤波'")
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Rows) == 0 {
		t.Fatal("concept 卡尔曼滤波 not indexed")
	}
	if kind, _ := res.Rows[0][0].(string); kind != "heading" {
		t.Errorf("卡尔曼滤波 kind = %q, want heading", kind)
	}
	if docCount, _ := res.Rows[0][1].(int64); docCount != 1 {
		t.Errorf("卡尔曼滤波 doc_count = %d, want 1", docCount)
	}

	// 「卡尔曼滤波」与「协方差」同一 chunk 内共现，应有一条边。
	edges, err := store.Query(t.Context(), `
		SELECT count(*) FROM concept_edges e
		JOIN concepts c1 ON c1.id = e.src
		JOIN concepts c2 ON c2.id = e.dst
		WHERE (c1.name = '卡尔曼滤波' AND c2.name = '协方差')
		   OR (c1.name = '协方差' AND c2.name = '卡尔曼滤波')`)
	if err != nil {
		t.Fatal(err)
	}
	if n, _ := edges.Rows[0][0].(int64); n == 0 {
		t.Error("卡尔曼滤波 and 协方差 must co-occur in the same chunk")
	}
}
