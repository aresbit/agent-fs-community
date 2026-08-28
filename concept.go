package agentfs

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode"

	"github.com/go-ego/gse"
)

// concept.go 实现「概念共现图」的提取与索引，是 schema v3 的第三个关系层：
// 文件检索（files_fts/chunks_fts）→ 符号图（symbols/symbol_refs）→ 概念图（本文件）。
//
// 概念词表两层：Markdown 标题（结构化概念）+ bigram 高频术语（正文概念）。
// 唯一分词入口是 analyzeTerms（tokenize.go 的 bigram），不引入 jieba。

// conceptTerm 是一个提取出的概念：名称 + 来源（标题 or 术语）。
type conceptTerm struct {
	name string
	kind string // "heading" | "term"
}

// headingRe 匹配 Markdown h1/h2/h3 标题。windowChunks 按 1200 字符切窗口，标题
// 若落在窗口内部，行首仍是 '#'，所以按行首匹配即可。
var headingRe = regexp.MustCompile(`^\s*(#{1,3})\s+(.+?)\s*$`)

// leadRe 去掉标题编号前缀（"3.1 分支预测" → "分支预测"、"一、" → 去掉）。
var leadRe = regexp.MustCompile(`^[\d\.、\s\(\)（）:：]+`)

// conceptBoilerplate 是笔记模板的样板小标题：每章都重复出现，作概念会污染共现
// （让所有概念都和「本章概要」「核心 Takeaway」相关）。按子串判定，所以
// 「一、本章概要 (Overview)」也命中「本章概要」。
var conceptBoilerplate = []string{
	"本章概要", "本章要点", "本章小结", "章节定位", "章节目录",
	"核心 Takeaway", "核心概念", "核心心智模型", "关键结论", "延伸阅读",
	"练习与思考", "思考题", "术语中英对照", "术语表", "素材说明",
}

// conceptStopwords 是 bigram 术语的停用词：通用虚词 + 高频泛化词。这些词和所有
// 概念都共现，没有区分度，从术语概念里剔除（标题概念仍保留，因为标题是人写的）。
var conceptStopwords = map[string]bool{
	"一个": true, "这个": true, "那个": true, "我们": true, "他们": true, "自己": true,
	"什么": true, "怎么": true, "可以": true, "能够": true, "需要": true, "进行": true,
	"通过": true, "对于": true, "关于": true, "以及": true, "或者": true, "但是": true,
	"因为": true, "所以": true, "如果": true, "然后": true, "就是": true, "还是": true,
	"没有": true, "不是": true, "也是": true, "这种": true, "那种": true, "一种": true,
	"一些": true, "很多": true, "所有": true, "每个": true, "不同": true, "相同": true,
	"重要": true, "主要": true, "基本": true, "核心": true, "部分": true, "问题": true,
	"方法": true, "方式": true, "情况": true, "内容": true, "介绍": true, "说明": true,
	"例如": true, "比如": true, "包括": true, "其中": true, "这样": true, "那样": true,
	"上述": true, "下面": true, "上面": true, "最后": true, "第一": true, "第二": true,
	"实现": true, "使用": true, "采用": true, "常见": true, "典型": true, "相关": true,
	"对应": true, "基于": true, "针对": true,
	// gse CutStop 停用词表不覆盖的虚词/代词（整词概念里应剔除）。
	"的": true, "与": true, "是": true, "和": true, "了": true, "在": true, "有": true,
	"也": true, "就": true, "都": true, "这": true, "那": true, "被": true, "把": true,
	"从": true, "到": true, "等": true, "或": true, "及": true, "并": true, "而": true,
}

// isBoilerplate 判断一个概念是否命中样板小标题（子串判定）。
func isBoilerplate(term string) bool {
	for _, bp := range conceptBoilerplate {
		if strings.Contains(term, bp) {
			return true
		}
	}
	return false
}

// normHeading 归一化标题：去掉编号前缀，去掉首尾空白。
func normHeading(heading string) string {
	return leadRe.ReplaceAllString(strings.TrimSpace(heading), "")
}

// gse 分词器是进程级单例。gse 纯 Go、词典 go:embed 内嵌，无需 CGO 或外部词典文件
// （gojieba 依赖 cppjieba 的 C++ 与外部词典，破坏 agent-fs 的纯 Go 自包含约束，故弃）。
var (
	segOnce sync.Once
	segMu   sync.Mutex
	segInst *gse.Segmenter
)

func segmenter() *gse.Segmenter {
	segOnce.Do(func() {
		seg, err := gse.NewEmbed()
		if err != nil {
			panic(fmt.Errorf("load gse embedded dictionary: %w", err))
		}
		segInst = &seg
	})
	return segInst
}

// stripMarkdown 去掉 Markdown/LaTeX/HTML 语法噪声，避免「div」「text」「**」「_{\」
// 这类标记被当成概念。chunk.content 是原始笔记文本，含标题标记、代码块、公式、表格。
func stripMarkdown(text string) string {
	text = regexp.MustCompile("```.*?```").ReplaceAllString(text, " ")
	text = regexp.MustCompile("`[^`]*`").ReplaceAllString(text, " ")
	text = regexp.MustCompile(`\$\$.*?\$\$`).ReplaceAllString(text, " ")
	text = regexp.MustCompile(`\$[^$]*\$`).ReplaceAllString(text, " ")
	text = regexp.MustCompile(`\\[a-zA-Z]+\{[^}]*\}`).ReplaceAllString(text, " ")
	text = regexp.MustCompile(`\\[a-zA-Z]+`).ReplaceAllString(text, " ")
	text = regexp.MustCompile(`</?[a-zA-Z][^>]*>`).ReplaceAllString(text, " ")
	text = regexp.MustCompile(`\[([^\]]*)\]\([^)]*\)`).ReplaceAllString(text, "$1")
	text = regexp.MustCompile(`(?m)^\s*#{1,6}\s*`).ReplaceAllString(text, " ")
	text = regexp.MustCompile(`(?m)^\s*[-*+]\s+`).ReplaceAllString(text, " ")
	text = regexp.MustCompile(`(?m)^\s*\d+[\.\)]\s+`).ReplaceAllString(text, " ")
	text = regexp.MustCompile(`[*_>~|]+`).ReplaceAllString(text, " ")
	return text
}

// hasLetter 判断一个词是否含至少一个字母或数字，过滤纯标点/符号残留。
func hasLetter(word string) bool {
	for _, r := range word {
		if unicode.IsLetter(r) || unicode.IsNumber(r) {
			return true
		}
	}
	return false
}

// extractTerms 用 gse 整词分词 + 去停用词，统计词频，取出现 ≥2 次的整词（按词频
// 降序，最多 topK 个）。整词分词才能拿到「数据冒险」「协方差」这类真概念，bigram
// 会把它们切成「数据/据冒/冒险」碎片。
func extractTerms(text string, topK int) []string {
	segMu.Lock()
	defer segMu.Unlock()
	freq := make(map[string]int, 64)
	for _, word := range segmenter().CutStop(stripMarkdown(text), true) {
		word = strings.TrimSpace(word)
		if len(word) < 2 || !hasLetter(word) || conceptStopwords[word] || isBoilerplate(word) {
			continue
		}
		freq[word]++
	}
	terms := make([]string, 0, len(freq))
	for word, count := range freq {
		if count >= 2 {
			terms = append(terms, word)
		}
	}
	sort.Slice(terms, func(i, j int) bool { return freq[terms[i]] > freq[terms[j]] })
	if len(terms) > topK {
		terms = terms[:topK]
	}
	return terms
}

// segmentQuery 把查询切成概念词：gse 整词分词，去停用词/标点/样板，不设词频阈值
// （查询短，每个词都是候选概念）。与 extractTerms 的区别：extractTerms 面向长文本
// 取高频概念，segmentQuery 面向短查询取所有概念词。
func segmentQuery(text string) []string {
	segMu.Lock()
	defer segMu.Unlock()
	seen := make(map[string]bool, 16)
	out := make([]string, 0, 16)
	for _, word := range segmenter().CutStop(stripMarkdown(text), true) {
		word = strings.TrimSpace(word)
		if len(word) < 2 || !hasLetter(word) || conceptStopwords[word] || isBoilerplate(word) {
			continue
		}
		if !seen[word] {
			seen[word] = true
			out = append(out, word)
		}
	}
	return out
}

// extractConcepts 从一段 chunk 文本提取概念：标题（结构化）+ 整词关键词（正文）。
// 返回已去重的概念列表。标题概念优先、术语概念补充——两者可能在文本里重叠，用 seen
// 去重，标题胜出（kind 记 heading）。
func extractConcepts(text string) []conceptTerm {
	seen := make(map[string]string, 32) // name -> kind
	order := make([]string, 0, 32)

	add := func(name, kind string) {
		name = strings.TrimSpace(name)
		if name == "" {
			return
		}
		if _, ok := seen[name]; ok {
			return
		}
		seen[name] = kind
		order = append(order, name)
	}

	// 1. 标题概念：Markdown h1/h2/h3。
	for _, line := range strings.Split(text, "\n") {
		m := headingRe.FindStringSubmatch(line)
		if m == nil {
			continue
		}
		h := normHeading(m[2])
		if len(h) < 2 || isBoilerplate(h) {
			continue
		}
		add(h, "heading")
	}

	// 2. 术语概念：gse 整词分词 + 词频统计（对齐 Python 版 jieba 的整词粒度）。
	//    bigram 会把「数据冒险」切成「数据/据冒/冒险」碎片化严重；整词分词才能
	//    拿到「数据冒险」「协方差」这类真概念。
	for _, kw := range extractTerms(text, 10) {
		add(kw, "term")
	}

	concepts := make([]conceptTerm, 0, len(order))
	for _, name := range order {
		concepts = append(concepts, conceptTerm{name: name, kind: seen[name]})
	}
	return concepts
}

// upsertConcept 插入或取回一个概念节点，返回其 id。kind 只在首次插入时生效：
// 同一概念先以 heading 出现、后以 term 出现时，保留 heading 这一更强来源。
func upsertConcept(ctx context.Context, tx *sql.Tx, name, kind string) (int64, error) {
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO concepts(name, kind, doc_count, created_at_ns) VALUES (?, ?, 0, ?)
		 ON CONFLICT(name) DO NOTHING`,
		name, kind, time.Now().UnixNano()); err != nil {
		return 0, fmt.Errorf("insert concept %q: %w", name, err)
	}
	var id int64
	if err := tx.QueryRowContext(ctx,
		"SELECT id FROM concepts WHERE name=?", name).Scan(&id); err != nil {
		return 0, fmt.Errorf("lookup concept %q: %w", name, err)
	}
	return id, nil
}

// indexConcepts 在事务里为一个 chunk 写入概念图：概念节点、出现关系、共现边。
// 调用方保证 concepts 已去重；本函数负责把同一 chunk 内的概念两两共现累加进
// concept_edges，并在概念首次出现在该 chunk 时递增 concepts.doc_count。
func indexConcepts(ctx context.Context, tx *sql.Tx, chunkID int64, concepts []conceptTerm) error {
	if len(concepts) == 0 {
		return nil
	}
	ids := make([]int64, 0, len(concepts))
	for _, c := range concepts {
		id, err := upsertConcept(ctx, tx, c.name, c.kind)
		if err != nil {
			return err
		}
		res, err := tx.ExecContext(ctx,
			`INSERT OR IGNORE INTO concept_occurrences(concept_id, chunk_id) VALUES (?, ?)`,
			id, chunkID)
		if err != nil {
			return fmt.Errorf("insert occurrence %q: %w", c.name, err)
		}
		if inserted, _ := res.RowsAffected(); inserted > 0 {
			// 概念首次出现在该 chunk，doc_count 才 +1。
			if _, err := tx.ExecContext(ctx,
				"UPDATE concepts SET doc_count = doc_count + 1 WHERE id=?", id); err != nil {
				return fmt.Errorf("bump doc_count %q: %w", c.name, err)
			}
		}
		ids = append(ids, id)
	}
	// 同一 chunk 内的概念两两共现，累加 co_count。无向边存成 (min, max)。
	for i := 0; i < len(ids); i++ {
		for j := i + 1; j < len(ids); j++ {
			src, dst := ids[i], ids[j]
			if src > dst {
				src, dst = dst, src
			}
			if _, err := tx.ExecContext(ctx,
				`INSERT INTO concept_edges(src, dst, co_count) VALUES (?, ?, 1)
				 ON CONFLICT(src, dst) DO UPDATE SET co_count = co_count + 1`,
				src, dst); err != nil {
				return fmt.Errorf("bump edge (%d,%d): %w", src, dst, err)
			}
		}
	}
	return nil
}

// unindexConcepts 撤销一个 chunk 对概念图的贡献：把它的概念两两共现从 concept_edges
// 里减去，并递减 doc_count。在 chunk 被替换/删除前调用，保证 concept_edges 与
// concept_occurrences 一致（后者靠外键 CASCADE 自动删，前者必须显式减）。
func unindexConcepts(ctx context.Context, tx *sql.Tx, chunkID int64) error {
	rows, err := tx.QueryContext(ctx,
		"SELECT concept_id FROM concept_occurrences WHERE chunk_id=?", chunkID)
	if err != nil {
		return fmt.Errorf("read occurrences for chunk %d: %w", chunkID, err)
	}
	ids := make([]int64, 0, 16)
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			_ = rows.Close()
			return fmt.Errorf("scan occurrence: %w", err)
		}
		ids = append(ids, id)
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		return fmt.Errorf("iterate occurrences: %w", err)
	}
	for i := 0; i < len(ids); i++ {
		for j := i + 1; j < len(ids); j++ {
			src, dst := ids[i], ids[j]
			if src > dst {
				src, dst = dst, src
			}
			if _, err := tx.ExecContext(ctx,
				"UPDATE concept_edges SET co_count = co_count - 1 WHERE src=? AND dst=?",
				src, dst); err != nil {
				return fmt.Errorf("decrement edge (%d,%d): %w", src, dst, err)
			}
		}
	}
	for _, id := range ids {
		if _, err := tx.ExecContext(ctx,
			"UPDATE concepts SET doc_count = doc_count - 1 WHERE id=? AND doc_count > 0",
			id); err != nil {
			return fmt.Errorf("decrement doc_count %d: %w", id, err)
		}
	}
	if _, err := tx.ExecContext(ctx,
		"DELETE FROM concept_edges WHERE co_count <= 0"); err != nil {
		return fmt.Errorf("prune zero edges: %w", err)
	}
	return nil
}

// Related 返回与 name 共现强度最高的概念，按 co_count·PMI 降序。
//
// 排序键是「频次截断 + 关联度量」两段式（见 docs/concept-graph.md D2）：对固定的
// 查询概念 a，log(N) 和 log(doc_a) 是常数，不影响排序，所以 score 退化为
// co_count·(ln(co_count) − ln(doc_b))——共现次数相对目标概念出现次数的对数比。
// 这同时惩罚「和谁都共现」的泛化词（doc_b 大 → 分数低）和偶然共现的罕见词
// （co_count 小 → 分数低），避开裸 PMI 偏向罕见词的坑。
func (s *Store) Related(ctx context.Context, name string, limit int) (QueryResult, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return QueryResult{}, fmt.Errorf("related: empty concept: %w", os.ErrInvalid)
	}
	if limit <= 0 {
		limit = 20
	}
	return s.Query(ctx, `
		SELECT
			CASE WHEN c1.name = ? THEN c2.name ELSE c1.name END AS concept,
			CASE WHEN c1.name = ? THEN c2.kind ELSE c1.kind END AS kind,
			e.co_count,
			CASE WHEN c1.name = ? THEN c2.doc_count ELSE c1.doc_count END AS doc_count,
			e.co_count * (ln(e.co_count) - ln(CASE WHEN c1.name = ? THEN c2.doc_count ELSE c1.doc_count END)) AS score
		FROM concept_edges e
		JOIN concepts c1 ON c1.id = e.src
		JOIN concepts c2 ON c2.id = e.dst
		WHERE c1.name = ? OR c2.name = ?
		ORDER BY score DESC, e.co_count DESC
		LIMIT ?`,
		name, name, name, name, name, name, limit)
}

// ConceptOccurrences 返回 name 出现的文件（chunk 的 source 位置），复用概念图的
// concept_occurrences → chunks → files 三级关系，回答「这个概念在哪被讲到」。
func (s *Store) ConceptOccurrences(ctx context.Context, name string, limit int) (QueryResult, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return QueryResult{}, fmt.Errorf("concept occurrences: empty concept: %w", os.ErrInvalid)
	}
	if limit <= 0 {
		limit = 20
	}
	return s.Query(ctx, `
		SELECT f.path, c.symbol, c.start_line, c.end_line
		FROM concept_occurrences co
		JOIN concepts cpt ON cpt.id = co.concept_id
		JOIN chunks c ON c.id = co.chunk_id
		JOIN files f ON f.id = c.file_id
		WHERE cpt.name = ?
		ORDER BY f.path, c.start_line
		LIMIT ?`,
		name, limit)
}
