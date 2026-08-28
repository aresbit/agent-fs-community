package agentfs

import (
	"slices"
	"strings"
	"testing"
)

// TestScriptRunsSplitAtScriptBoundary 锁死这次修复的核心：脚本边界是切分点，
// 所以「S参数」和「S 参数」必须产出同一组检索词。用户写不写那个空格，不应该改变
// 分词结果——这正是原来「查 S 参数 找不到 S参数」的直接原因。
func TestScriptRunsSplitAtScriptBoundary(t *testing.T) {
	t.Parallel()
	packed := analyzeTerms("S参数")
	spaced := analyzeTerms("S 参数")
	if !slices.Equal(packed, spaced) {
		t.Fatalf("space changed tokenization: %q vs %q", packed, spaced)
	}
	if !slices.Equal(packed, []string{"s", "参数"}) {
		t.Fatalf("analyzeTerms(\"S参数\") = %q, want [s 参数]", packed)
	}
}

// TestAnalyzeTermsBigramsCJK 验证连写的中文被切成重叠二字词，而不是一个巨型 token。
func TestAnalyzeTermsBigramsCJK(t *testing.T) {
	t.Parallel()
	got := analyzeTerms("参数索引")
	want := []string{"参数", "数索", "索引"}
	if !slices.Equal(got, want) {
		t.Fatalf("analyzeTerms = %q, want %q", got, want)
	}
	if got := analyzeTerms("测量S参数的方法"); !slices.Contains(got, "参数") {
		t.Fatalf("a CJK run must yield the 2-char word 参数, got %q", got)
	}
	// 单字段落保留原样，不能因为凑不够 bigram 就丢掉。
	if got := analyzeTerms("中"); !slices.Equal(got, []string{"中"}) {
		t.Fatalf("single CJK char must survive, got %q", got)
	}
}

// TestAnalyzeTermsKeepsLatinWordsWhole 验证拉丁词不被切碎——代码符号必须整词可查。
func TestAnalyzeTermsKeepsLatinWordsWhole(t *testing.T) {
	t.Parallel()
	got := analyzeTerms("HybridSearch load_vector-candidates 384")
	want := []string{"hybridsearch", "load_vector-candidates", "384"}
	if !slices.Equal(got, want) {
		t.Fatalf("analyzeTerms = %q, want %q", got, want)
	}
}

// TestTermFrequenciesOverlapForCJK 是这次修复对 BM25 的实际效果：一个中文 query 与
// 一段包含它的中文正文必须有共同的词。原来两边各是一个巨型 token，交集恒为空，
// 词频恒为 1，BM25 对中文完全失效。
func TestTermFrequenciesOverlapForCJK(t *testing.T) {
	t.Parallel()
	document := termFrequencies("本文讨论S参数的测量方法与校准流程")
	shared := 0
	for _, term := range analyzeTerms("S参数测量") {
		if document[term] > 0 {
			shared++
		}
	}
	if shared == 0 {
		t.Fatal("query and document share no terms; CJK BM25 is still dead")
	}
}

// TestHashEmbedderSeparatesCJKTopics 验证兜底 embedder 对中文恢复了区分度：
// 同主题的两段中文相似度必须高于不同主题的两段。v1 分词下三者两两相似度都是 0。
func TestHashEmbedderSeparatesCJKTopics(t *testing.T) {
	t.Parallel()
	embedder := NewHashEmbedder(256)
	embed := func(text string) []float32 {
		vector, err := embedder.Embed(t.Context(), text)
		if err != nil {
			t.Fatalf("embed %q: %v", text, err)
		}
		return vector
	}
	query := embed("S参数测量")
	related := embed("本文讨论S参数的测量方法")
	unrelated := embed("数据库事务与并发控制")

	relatedScore := cosineSimilarity(query, related)
	unrelatedScore := cosineSimilarity(query, unrelated)
	if relatedScore <= 0 {
		t.Fatalf("related Chinese texts have zero overlap: %v", relatedScore)
	}
	if relatedScore <= unrelatedScore {
		t.Fatalf("related %.4f must beat unrelated %.4f", relatedScore, unrelatedScore)
	}
	if !strings.HasPrefix(embedder.Model(), "agentfs-hash-v2-") {
		t.Fatalf("tokenizer change must bump the model id, got %q", embedder.Model())
	}
}

// TestFTSMatchMixedScriptQuery 验证生成的 FTS 表达式同时包含整段短语和 bigram：
// 前者命中当前 unicode61 索引里的巨型 token，后者是索引改成分词存储后生效的那一半。
func TestFTSMatchMixedScriptQuery(t *testing.T) {
	t.Parallel()
	match := ftsMatch("S参数索引")
	for _, want := range []string{`"s"`, `"参数索引"`, `"参数"`, `"数索"`, `"索引"`} {
		if !strings.Contains(match, want) {
			t.Fatalf("ftsMatch(\"S参数索引\") = %s, missing %s", match, want)
		}
	}
	// 空格不该改变表达式的内容。
	if ftsMatch("S 参数索引") != match {
		t.Fatalf("space changed the FTS expression:\n%s\n%s", ftsMatch("S 参数索引"), match)
	}
	// 纯符号和纯停用词仍然产出空串，让调用方整路跳过。
	for _, query := range []string{"the and or", "--- ...", "   "} {
		if got := ftsMatch(query); got != "" {
			t.Fatalf("ftsMatch(%q) = %q, want empty", query, got)
		}
	}
}

// TestDistinctTermsPreservesFirstOccurrence 验证去重不打乱顺序：IDF 按不同词累加，
// 重复词会让同一个词的贡献算两遍，而中文切 bigram 后重复明显变多。
func TestDistinctTermsPreservesFirstOccurrence(t *testing.T) {
	t.Parallel()
	got := distinctTerms([]string{"参数", "索引", "参数", "测量", "索引"})
	want := []string{"参数", "索引", "测量"}
	if !slices.Equal(got, want) {
		t.Fatalf("distinctTerms = %q, want %q", got, want)
	}
}
