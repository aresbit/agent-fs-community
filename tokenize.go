package agentfs

import (
	"strings"
	"unicode"
)

// isSegmentedScript 判断字符是否属于「连写不加空格」的文字：汉字、平假名、片假名。
//
// 这些文字是本文件存在的全部理由。按 Unicode 字母类别切词时，每个汉字都是字母，
// 于是「测量S参数的方法」会切成一个词。一个词意味着：FTS 查「参数」永远不匹配，
// BM25 的词频恒为 1，词袋 embedder 与查询零重叠——同一个缺陷会在检索链路的三个
// 地方各失败一次。
func isSegmentedScript(r rune) bool {
	return unicode.Is(unicode.Han, r) ||
		unicode.Is(unicode.Hiragana, r) ||
		unicode.Is(unicode.Katakana, r)
}

// isTermRune 判断字符能否成为检索词的一部分。取字符集与 FTS5 unicode61 对齐
// （字母、数字），另外保留标识符里常见的 _ 和 -。
func isTermRune(r rune) bool {
	return unicode.IsLetter(r) || unicode.IsNumber(r) || r == '_' || r == '-'
}

// scriptRun 是一段同种文字的连续字符，已转小写。
type scriptRun struct {
	text string
	cjk  bool
}

// scriptRuns 把文本切成同种文字的连续段。空白和标点是分隔符，**脚本边界本身也是
// 分隔符**。
//
// 脚本边界必须切开：否则「S参数」是一段而「S 参数」是两段，用户写不写空格就会得到
// 完全不同的检索词。这正是「查 S 参数 找不到 S参数」的直接原因——分词边界跟着输入
// 里的空格跑，而不是跟着语言结构跑。
func scriptRuns(text string) []scriptRun {
	runs := make([]scriptRun, 0, 8)
	var current []rune
	var currentCJK bool
	flush := func() {
		if len(current) > 0 {
			runs = append(runs, scriptRun{text: string(current), cjk: currentCJK})
			current = current[:0]
		}
	}
	for _, r := range strings.ToLower(text) {
		switch {
		case isSegmentedScript(r):
			if !currentCJK {
				flush()
				currentCJK = true
			}
			current = append(current, r)
		case isTermRune(r):
			if currentCJK {
				flush()
				currentCJK = false
			}
			current = append(current, r)
		default:
			flush()
			currentCJK = false
		}
	}
	flush()
	return runs
}

// cjkBigrams 把一段汉字/假名切成重叠二字词：「参数索引」→ 参数, 数索, 索引。
// 单字段落原样返回。
//
// 为什么是 bigram 而不是别的：中文不带空格，做真正的分词需要词典和模型，代价与
// 收益不成比例；而 bigram 无需任何词典，双字词（中文最主流的词长）直接整词命中，
// 更长的词由相邻 bigram 叠加得分。这是 CJK 全文检索的标准做法。
//
// 后置条件：返回至少一个元素；长度为 n≥2 的段返回 n-1 个 bigram。
func cjkBigrams(run string) []string {
	runes := []rune(run)
	if len(runes) < 2 {
		return []string{run}
	}
	grams := make([]string, 0, len(runes)-1)
	for index := 0; index+1 < len(runes); index++ {
		grams = append(grams, string(runes[index:index+2]))
	}
	return grams
}

// analyzeTerms 把文本切成用于打分与向量化的检索词：拉丁/数字段保持整词，
// 汉字/假名段切成 bigram。
//
// 这是 termFrequencies（BM25 打分）、HashEmbedder（兜底向量）和 ftsMatch（FTS
// 表达式）共用的唯一分词入口。这三处原本各自手写了一遍 `unicode.IsLetter` 的切分
// 逻辑，于是同一个 CJK 缺陷被复制了三份——把切词收敛成一个函数，是让这类缺陷只
// 可能有一个修复点的前提。
func analyzeTerms(text string) []string {
	runs := scriptRuns(text)
	terms := make([]string, 0, len(runs)*2)
	for _, run := range runs {
		if run.cjk {
			terms = append(terms, cjkBigrams(run.text)...)
			continue
		}
		terms = append(terms, run.text)
	}
	return terms
}

// segmentCJKIndex 产出写进 FTS 索引的补充文本：只包含 CJK 段切出的 bigram，
// 每个 bigram 之间用空格隔开，拉丁段一概略过。
//
// 机制：unicode61 以空格为分隔符，所以「参数 数索 索引」会被切成三个独立 token。
// 查询侧 ftsMatch 对 CJK 段同样发 bigram，于是「参数」能命中「测量S参数的方法」
// ——FTS5 没有中缀匹配，但把中缀预先切成 token 存进去，中缀匹配就变成了整词匹配。
//
// 为什么只放 CJK：files_fts 仍然索引 name/path/content_head 原文，拉丁词在那里已经
// 被 unicode61 正确切分了。把拉丁词再复制一份进 search_text 只会让索引凭空大一倍，
// 却不改变任何检索结果。纯英文文件的 search_text 因此是空串，零额外开销。
func segmentCJKIndex(text string) string {
	var builder strings.Builder
	for _, run := range scriptRuns(text) {
		if !run.cjk {
			continue
		}
		for _, gram := range cjkBigrams(run.text) {
			if builder.Len() > 0 {
				builder.WriteByte(' ')
			}
			builder.WriteString(gram)
		}
	}
	return builder.String()
}

// distinctTerms 去重并保持首次出现的顺序。IDF 是按「查询里的不同词」累加的，
// 重复词会让同一个词的贡献被算两遍——中文切成 bigram 后重复词明显变多。
func distinctTerms(terms []string) []string {
	seen := make(map[string]struct{}, len(terms))
	distinct := make([]string, 0, len(terms))
	for _, term := range terms {
		if _, ok := seen[term]; ok {
			continue
		}
		seen[term] = struct{}{}
		distinct = append(distinct, term)
	}
	return distinct
}
