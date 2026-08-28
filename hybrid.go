package agentfs

import (
	"context"
	"fmt"
	"math"
	"os"
	"slices"
	"strings"
	"time"
	"unicode"
)

type HybridRequest struct {
	Query      string   `json:"query"`
	Limit      int      `json:"limit,omitempty"`
	Kinds      []string `json:"kinds,omitempty"`
	Tags       []string `json:"tags,omitempty"`
	PathPrefix string   `json:"path_prefix,omitempty"`
	MinSize    int64    `json:"min_size,omitempty"`
	MaxSize    int64    `json:"max_size,omitempty"`
}

type SearchHit struct {
	Path          string  `json:"path"`
	Kind          string  `json:"kind"`
	Size          int64   `json:"size"`
	MTimeNS       int64   `json:"mtime_ns"`
	MIME          string  `json:"mime,omitempty"`
	Snippet       string  `json:"snippet,omitempty"`
	ChunkSymbol   string  `json:"chunk_symbol,omitempty"`
	StartLine     int     `json:"start_line,omitempty"`
	EndLine       int     `json:"end_line,omitempty"`
	Score         float64 `json:"score"`
	BM25Score     float64 `json:"bm25_score,omitempty"`
	VectorScore   float64 `json:"vector_score,omitempty"`
	MetadataScore float64 `json:"metadata_score,omitempty"`
	RerankScore   float64 `json:"rerank_score,omitempty"`
}

type hybridCandidate struct {
	id        int64
	hit       SearchHit
	lexRank   int
	vecRank   int
	vectorRaw []byte
	dims      int
	tagsText  string
}

// HybridSearch combines ranked FTS5/BM25 candidates, sign-LSH vector
// candidates, and metadata filters using reciprocal-rank fusion.
func (s *Store) HybridSearch(ctx context.Context, request HybridRequest) ([]SearchHit, error) {
	if err := s.checkOpen(); err != nil {
		return nil, err
	}
	request.Query = strings.TrimSpace(request.Query)
	if request.Query == "" && request.PathPrefix == "" && len(request.Kinds) == 0 && len(request.Tags) == 0 {
		return nil, fmt.Errorf("hybrid search requires query or metadata filter: %w", os.ErrInvalid)
	}
	if request.Limit <= 0 {
		request.Limit = 20
	}
	request.Limit = min(request.Limit, 200)
	candidateLimit := max(256, request.Limit*12)
	candidates := make(map[int64]*hybridCandidate, candidateLimit)

	var queryVector []float32
	if request.Query != "" {
		var err error
		queryVector, err = s.embedder.Embed(ctx, request.Query)
		if err != nil {
			return nil, fmt.Errorf("embed search query: %w", err)
		}
		// 两个 loader 收原始 query，自己去编译 FTS 表达式并在没有可检索词时跳过。
		// 打分要用的词必须来自同一个分词器（analyzeTerms），不能从 FTS 表达式反解
		// ——那样等于把分词逻辑分叉成两份。
		if err := s.loadLexicalCandidates(ctx, request.Query, candidateLimit, candidates); err != nil {
			return nil, err
		}
		if err := s.loadChunkLexicalCandidates(ctx, request.Query, candidateLimit, candidates); err != nil {
			return nil, err
		}
		if err := s.loadVectorCandidates(ctx, queryVector, candidateLimit*4, candidates); err != nil {
			return nil, err
		}
		if err := s.loadChunkVectorCandidates(ctx, queryVector, candidateLimit*4, candidates); err != nil {
			return nil, err
		}
	}
	if err := s.loadMetadataCandidates(ctx, request, candidateLimit, candidates); err != nil {
		return nil, err
	}

	now := time.Now()
	surviving := make([]*hybridCandidate, 0, len(candidates))
	for _, candidate := range candidates {
		if !matchesMetadata(candidate.hit, candidate.tagsText, request) {
			continue
		}
		if len(candidate.vectorRaw) > 0 && len(queryVector) > 0 {
			if vector, ok := decodeVector(candidate.vectorRaw, candidate.dims); ok {
				candidate.hit.VectorScore = max(0, cosineSimilarity(queryVector, vector))
			}
		}
		if request.Query != "" && candidate.lexRank == 0 && candidate.hit.VectorScore <= 0 {
			continue
		}
		age := now.Sub(time.Unix(0, candidate.hit.MTimeNS))
		candidate.hit.MetadataScore = 1 / (1 + math.Max(0, age.Hours()/24)/365)
		surviving = append(surviving, candidate)
	}
	assignVectorRanks(surviving)
	hits := make([]SearchHit, 0, len(surviving))
	for _, candidate := range surviving {
		candidate.hit.BM25Score = reciprocalRank(candidate.lexRank)
		candidate.hit.Score = 0.52*candidate.hit.BM25Score +
			0.38*reciprocalRank(candidate.vecRank) +
			0.10*candidate.hit.MetadataScore
		hits = append(hits, candidate.hit)
	}
	slices.SortFunc(hits, byDescendingScore)
	if err := s.rerank(ctx, request, hits); err != nil {
		// Rerank is a best-effort refinement: on any error we keep the fused
		// first-stage ranking rather than failing the whole search.
		_, _ = fmt.Fprintf(os.Stderr, "agent-fs: rerank skipped: %v\n", err)
	}
	if len(hits) > request.Limit {
		hits = hits[:request.Limit]
	}
	return hits, nil
}

// rrfK 是 reciprocal-rank fusion 的平滑常数。一路召回排名 rank 的贡献是
// (k+1)/(k+rank)，归一到 (0,1]，第一名恰好 1.0。k 决定相邻名次的落差：k=0 时
// 第一名的贡献是第二名的两倍，词法一路的头名会直接压死任何语义命中，向量一路
// 形同虚设；文献惯用 k=60，让两路的名次都真正参与决策。
const rrfK = 60

// reciprocalRank 把 1 起算的名次换算成融合贡献。rank<=0 表示该路召回没有命中
// 这个候选，贡献 0——缺席与「排在最后」必须区分开。
func reciprocalRank(rank int) float64 {
	if rank <= 0 {
		return 0
	}
	return float64(rrfK+1) / float64(rrfK+rank)
}

// assignVectorRanks 按余弦相似度给候选排名，使向量一路与词法一路在融合时用同一
// 种尺度（名次），而不是拿一个原始相似度去和一个倒数名次相加。
//
// 前置条件：每个候选的 hit.VectorScore 已算好。
// 后置条件：有正相似度的候选按相似度降序拿到 1..n 的 vecRank；其余保持 0。
func assignVectorRanks(candidates []*hybridCandidate) {
	ranked := make([]*hybridCandidate, 0, len(candidates))
	for _, candidate := range candidates {
		if candidate.hit.VectorScore > 0 {
			ranked = append(ranked, candidate)
		}
	}
	slices.SortFunc(ranked, func(left, right *hybridCandidate) int {
		if left.hit.VectorScore != right.hit.VectorScore {
			if left.hit.VectorScore > right.hit.VectorScore {
				return -1
			}
			return 1
		}
		return strings.Compare(left.hit.Path, right.hit.Path)
	})
	for index, candidate := range ranked {
		candidate.vecRank = index + 1
	}
}

func byDescendingScore(left, right SearchHit) int {
	if left.Score != right.Score {
		if left.Score > right.Score {
			return -1
		}
		return 1
	}
	return strings.Compare(left.Path, right.Path)
}

// rerank re-scores the first-stage top-K with the cross-encoder. K is 2×limit
// (at least 10), so the per-pair cost stays bounded even on large indexes.
//
// Modifies: the first K elements of hits, in place.
// Postcondition: hits[:K] is ordered by cross-encoder score; hits[K:] keeps its
// first-stage order and stays strictly behind hits[:K].
func (s *Store) rerank(ctx context.Context, request HybridRequest, hits []SearchHit) error {
	if s.reranker == nil || request.Query == "" || len(hits) == 0 {
		return nil
	}
	rerankCount := min(len(hits), max(request.Limit*2, 10), maxRerankCandidates)
	docs := make([]string, rerankCount)
	for index := range rerankCount {
		docs[index] = rerankDocument(hits[index])
	}
	scores, err := s.reranker.Score(ctx, request.Query, docs)
	if err != nil {
		return err
	}
	if len(scores) != rerankCount {
		return fmt.Errorf("cross-encoder returned %d scores for %d documents", len(scores), rerankCount)
	}
	for index := range rerankCount {
		rerank := sigmoid(float64(scores[index]))
		hits[index].RerankScore = rerank
		// Cross-encoder is the final authority within the reranked window;
		// sigmoid keeps Score in (0,1) and preserves the logit ordering.
		hits[index].Score = rerank
	}
	// 只在被重排的窗口内部重排序。窗口之外的候选仍带着第一阶段的融合分，那是与
	// sigmoid 完全不可比的另一套尺度——一起排序时，一个 cross-encoder 从没看过的
	// 尾部候选会凭融合分反超真正被重排的头部，甚至挤进最终结果。级联检索的契约是
	// 第二阶段只重排第一阶段的 top-K，不改变 K 内外的相对次序。
	slices.SortFunc(hits[:rerankCount], byDescendingScore)
	return nil
}

// rerankDocumentBytes 是送进 cross-encoder 的文档字节上限。
//
// 这个值要和 tokenizer.json 的 truncation.max_length（512）对齐才有意义：代码大约
// 3~4 字节一个 token，1800 字节 ≈ 450~600 token，恰好把一个 windowChunks 产出的
// chunk（1200 rune）完整喂进去，又不会大幅超过模型截断点而白做分词。
// 改这个常数之前先改 tokenizer.json——真正的硬上限在那里。
const rerankDocumentBytes = 1800

// maxRerankCandidates 是二阶段重排的窗口上限。
//
// 窗口原本只有 2×limit 一个约束，而 limit 最大 200，于是最坏情况要跑 400 个
// (query, doc) 对。把 cross-encoder 的序列从 128 放宽到 512 之后，这个乘积必须
// 收口，否则一次搜索的推理量会失控。
//
// 成本对照（以 token 位置计，batch×seq）：
//   旧：400 对 × 128 = 51200，但文档只有 ~100 token 进得了模型
//   新： 50 对 × 512 = 25600，文档整块可见
// 也就是最坏情况反而比原来便宜一半，而重排看到的上下文多了 5 倍。用 top-50 重排
// 出 top-N 本来也是级联检索的常规深度，重排 400 个候选只是在浪费算力。
const maxRerankCandidates = 50

// rerankDocument 取用于 cross-encoder 的文档文本：优先 chunk 级内容（含符号名），
// 否则用文件级 snippet。
func rerankDocument(hit SearchHit) string {
	text := hit.Snippet
	if text == "" {
		text = hit.Path
	}
	if hit.ChunkSymbol != "" {
		text = hit.ChunkSymbol + "\n" + text
	}
	return compactSnippet(text, rerankDocumentBytes)
}

func (s *Store) loadChunkLexicalCandidates(ctx context.Context, query string, limit int, candidates map[int64]*hybridCandidate) error {
	match := ftsMatch(query)
	if match == "" {
		// query 里没有任何可检索的词（全是停用词或全是标点）。整路跳过：把空短语
		// 交给 FTS5 是语法错误，会让整次搜索失败，而向量一路仍能独立回答。
		return nil
	}
	rowIDRows, err := s.db.QueryContext(ctx, `SELECT rowid FROM chunks_fts
		WHERE chunks_fts MATCH ? LIMIT ?`, match, limit)
	if err != nil {
		return fmt.Errorf("recall chunk FTS candidates: %w", err)
	}
	rowIDs := make([]int64, 0, limit)
	for rowIDRows.Next() {
		var id int64
		if err := rowIDRows.Scan(&id); err != nil {
			_ = rowIDRows.Close()
			return fmt.Errorf("scan chunk FTS rowid: %w", err)
		}
		rowIDs = append(rowIDs, id)
	}
	if err := rowIDRows.Close(); err != nil {
		return fmt.Errorf("close chunk FTS recall: %w", err)
	}
	if len(rowIDs) == 0 {
		return nil
	}
	arguments := make([]any, 0, len(rowIDs))
	placeholders := make([]string, len(rowIDs))
	for index, id := range rowIDs {
		placeholders[index] = "?"
		arguments = append(arguments, id)
	}
	rows, err := s.db.QueryContext(ctx, `SELECT c.id, c.symbol, c.start_line, c.end_line,
		c.content, f.id, f.path, f.kind, f.size, f.mtime_ns, f.mime, f.tags_text
		FROM chunks c JOIN files f ON f.id=c.file_id
		WHERE c.id IN (`+strings.Join(placeholders, ",")+`)`, arguments...)
	if err != nil {
		return fmt.Errorf("load recalled chunks: %w", err)
	}
	defer rows.Close()
	type chunkDocument struct {
		candidate *hybridCandidate
		terms     map[string]int
		length    int
		score     float64
	}
	documents := make([]chunkDocument, 0, len(rowIDs))
	for rows.Next() {
		candidate := &hybridCandidate{}
		var chunkID int64
		if err := rows.Scan(&chunkID, &candidate.hit.ChunkSymbol, &candidate.hit.StartLine,
			&candidate.hit.EndLine, &candidate.hit.Snippet, &candidate.id, &candidate.hit.Path,
			&candidate.hit.Kind, &candidate.hit.Size, &candidate.hit.MTimeNS, &candidate.hit.MIME,
			&candidate.tagsText); err != nil {
			return fmt.Errorf("scan recalled chunk: %w", err)
		}
		terms := termFrequencies(candidate.hit.ChunkSymbol + " " + candidate.hit.Snippet)
		length := 0
		for _, count := range terms {
			length += count
		}
		documents = append(documents, chunkDocument{candidate: candidate, terms: terms, length: length})
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("iterate recalled chunks: %w", err)
	}
	queryTerms := distinctTerms(analyzeTerms(query))
	var averageLength float64
	for _, document := range documents {
		averageLength += float64(document.length)
	}
	averageLength /= float64(max(1, len(documents)))
	for _, term := range queryTerms {
		documentFrequency := 0
		for _, document := range documents {
			if document.terms[term] > 0 {
				documentFrequency++
			}
		}
		idf := math.Log(1 + (float64(len(documents)-documentFrequency)+0.5)/(float64(documentFrequency)+0.5))
		for index := range documents {
			frequency := float64(documents[index].terms[term])
			if frequency == 0 {
				continue
			}
			denominator := frequency + 1.2*(0.25+0.75*float64(documents[index].length)/max(1, averageLength))
			documents[index].score += idf * frequency * 2.2 / denominator
		}
	}
	slices.SortFunc(documents, func(left, right chunkDocument) int {
		if left.score > right.score {
			return -1
		}
		if left.score < right.score {
			return 1
		}
		return strings.Compare(left.candidate.hit.Path, right.candidate.hit.Path)
	})
	for index, document := range documents {
		rank := index + 1
		existing := candidates[document.candidate.id]
		if existing == nil {
			document.candidate.lexRank = rank
			candidates[document.candidate.id] = document.candidate
		} else if existing.lexRank == 0 || rank <= existing.lexRank {
			existing.lexRank = rank
			existing.hit.Snippet = document.candidate.hit.Snippet
			existing.hit.ChunkSymbol = document.candidate.hit.ChunkSymbol
			existing.hit.StartLine = document.candidate.hit.StartLine
			existing.hit.EndLine = document.candidate.hit.EndLine
		}
	}
	return nil
}

func (s *Store) loadChunkVectorCandidates(ctx context.Context, query []float32, limit int, candidates map[int64]*hybridCandidate) error {
	where, args, recallLimit, err := s.vectorRecall(ctx, "chunk_embeddings", s.embedder.Model(), vectorProbes(query), limit)
	if err != nil {
		return err
	}
	args = append(args, recallLimit, recallLimit)
	rows, err := s.db.QueryContext(ctx, `WITH ev AS MATERIALIZED (
		SELECT chunk_id, vector, dimensions FROM chunk_embeddings
		WHERE `+where+` LIMIT ?
	) SELECT f.id, f.path, f.kind, f.size, f.mtime_ns, f.mime,
		f.tags_text, c.symbol, c.start_line, c.end_line, c.content, ev.vector, ev.dimensions
		FROM ev JOIN chunks c ON c.id=ev.chunk_id JOIN files f ON f.id=c.file_id
		LIMIT ?`, args...)
	if err != nil {
		return fmt.Errorf("load chunk vector candidates: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		candidate := &hybridCandidate{}
		if err := rows.Scan(&candidate.id, &candidate.hit.Path, &candidate.hit.Kind,
			&candidate.hit.Size, &candidate.hit.MTimeNS, &candidate.hit.MIME, &candidate.tagsText, &candidate.hit.ChunkSymbol,
			&candidate.hit.StartLine, &candidate.hit.EndLine, &candidate.hit.Snippet,
			&candidate.vectorRaw, &candidate.dims); err != nil {
			return fmt.Errorf("scan chunk vector candidate: %w", err)
		}
		existing := candidates[candidate.id]
		if existing == nil {
			candidates[candidate.id] = candidate
			continue
		}
		newVector, newOK := decodeVector(candidate.vectorRaw, candidate.dims)
		oldVector, oldOK := decodeVector(existing.vectorRaw, existing.dims)
		if newOK && (!oldOK || cosineSimilarity(query, newVector) > cosineSimilarity(query, oldVector)) {
			existing.vectorRaw = candidate.vectorRaw
			existing.dims = candidate.dims
			if existing.lexRank == 0 {
				existing.hit.Snippet = candidate.hit.Snippet
				existing.hit.ChunkSymbol = candidate.hit.ChunkSymbol
				existing.hit.StartLine = candidate.hit.StartLine
				existing.hit.EndLine = candidate.hit.EndLine
			}
		}
	}
	return rows.Err()
}

func (s *Store) loadLexicalCandidates(ctx context.Context, query string, limit int, candidates map[int64]*hybridCandidate) error {
	match := ftsMatch(query)
	if match == "" {
		return nil
	}
	rowIDRows, err := s.db.QueryContext(ctx, `SELECT rowid FROM files_fts
		WHERE files_fts MATCH ? LIMIT ?`, match, limit)
	if err != nil {
		return fmt.Errorf("recall FTS candidates: %w", err)
	}
	rowIDs := make([]int64, 0, limit)
	for rowIDRows.Next() {
		var id int64
		if err := rowIDRows.Scan(&id); err != nil {
			_ = rowIDRows.Close()
			return fmt.Errorf("scan FTS rowid: %w", err)
		}
		rowIDs = append(rowIDs, id)
	}
	if err := rowIDRows.Close(); err != nil {
		return fmt.Errorf("close FTS recall: %w", err)
	}
	if len(rowIDs) == 0 {
		return nil
	}
	arguments := make([]any, 0, len(rowIDs))
	placeholders := make([]string, len(rowIDs))
	for index, id := range rowIDs {
		placeholders[index] = "?"
		arguments = append(arguments, id)
	}
	rows, err := s.db.QueryContext(ctx, `SELECT f.id, f.name, f.path, f.kind, f.size,
		f.mtime_ns, f.mime, f.tags_text, f.content_head
		FROM files f
		WHERE f.id IN (`+strings.Join(placeholders, ",")+`)`, arguments...)
	if err != nil {
		return fmt.Errorf("load recalled files: %w", err)
	}
	defer rows.Close()
	type lexicalDocument struct {
		candidate *hybridCandidate
		terms     map[string]int
		length    int
		score     float64
	}
	documents := make([]lexicalDocument, 0, len(rowIDs))
	for rows.Next() {
		candidate := &hybridCandidate{}
		var name, content string
		if err := rows.Scan(&candidate.id, &name, &candidate.hit.Path, &candidate.hit.Kind,
			&candidate.hit.Size, &candidate.hit.MTimeNS, &candidate.hit.MIME,
			&candidate.tagsText, &content); err != nil {
			return fmt.Errorf("scan recalled file: %w", err)
		}
		terms := termFrequencies(name + " " + candidate.hit.Path + " " + candidate.tagsText + " " + content)
		length := 0
		for _, count := range terms {
			length += count
		}
		candidate.hit.Snippet = compactSnippet(content, 320)
		documents = append(documents, lexicalDocument{candidate: candidate, terms: terms, length: length})
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("iterate recalled files: %w", err)
	}
	queryTerms := distinctTerms(analyzeTerms(query))
	var averageLength float64
	for _, document := range documents {
		averageLength += float64(document.length)
	}
	averageLength /= float64(max(1, len(documents)))
	for _, term := range queryTerms {
		documentFrequency := 0
		for _, document := range documents {
			if document.terms[term] > 0 {
				documentFrequency++
			}
		}
		idf := math.Log(1 + (float64(len(documents)-documentFrequency)+0.5)/(float64(documentFrequency)+0.5))
		for index := range documents {
			frequency := float64(documents[index].terms[term])
			if frequency == 0 {
				continue
			}
			denominator := frequency + 1.2*(1-0.75+0.75*float64(documents[index].length)/max(1, averageLength))
			documents[index].score += idf * frequency * 2.2 / denominator
		}
	}
	slices.SortFunc(documents, func(left, right lexicalDocument) int {
		if left.score > right.score {
			return -1
		}
		if left.score < right.score {
			return 1
		}
		return strings.Compare(left.candidate.hit.Path, right.candidate.hit.Path)
	})
	// 与 loadChunkLexicalCandidates 一样按 id 合并，而不是覆盖。覆盖只在「这一路
	// 恰好第一个跑」时才碰巧正确，调用顺序一变就会静默丢掉别路已经攒下的向量与
	// chunk 定位信息。
	for index, document := range documents {
		rank := index + 1
		existing := candidates[document.candidate.id]
		if existing == nil {
			document.candidate.lexRank = rank
			candidates[document.candidate.id] = document.candidate
			continue
		}
		if existing.lexRank == 0 || rank < existing.lexRank {
			existing.lexRank = rank
		}
		if existing.hit.Snippet == "" {
			existing.hit.Snippet = document.candidate.hit.Snippet
		}
	}
	return nil
}

func termFrequencies(text string) map[string]int {
	frequencies := make(map[string]int)
	for _, term := range analyzeTerms(text) {
		frequencies[term]++
	}
	return frequencies
}

// compactSnippet 把正文压到 limit 字节以内。limit 是字节数，但截断落在字符边界上：
// 直接 text[:limit] 会把一个多字节字符劈成两半，JSON 编码时被替换成 U+FFFD，
// 交给 agent 的中文/日文片段末尾就是一个坏字符。
func compactSnippet(text string, limit int) string {
	text = strings.TrimSpace(text)
	if len(text) <= limit {
		return text
	}
	return truncateUTF8(text, limit) + "…"
}

// vectorScanAllThreshold 是向量召回全量扫描的规模阈值。低于该阈值时不做任何 LSH
// 过滤，直接对所有向量算精确余弦：几万条 384 维向量的点积是毫秒级，而任何近似
// 分桶在小索引上都可能一条都召回不到，代价完全不对等。
//
// 超过阈值才启用 sign-LSH，并且是 multi-probe（见 vectorProbes）而不是单桶精确
// 匹配——单桶把「前 8 维符号完全一致」当成了硬性准入条件，那不是相似性判据。
const vectorScanAllThreshold = 20_000

// vectorRecall 返回向量召回的 WHERE 片段、参数与 LIMIT。小索引全量精确扫描（不带
// bucket 过滤，LIMIT 用总行数），大索引走 multi-probe 桶过滤。
func (s *Store) vectorRecall(ctx context.Context, table, model string, probes []int64, limit int) (where string, args []any, recallLimit int, err error) {
	// 这个计数只用来在「全量精确扫描」与「LSH 桶过滤」之间二选一，不需要真实总数。
	// 直接 COUNT(*) 会在每次搜索时扫完整张向量表（百万行索引上是两次全索引扫描，
	// 只为了做一个布尔判断）。子查询 LIMIT 把代价封顶在 threshold+1 行。
	var count int
	if err = s.db.QueryRowContext(ctx,
		"SELECT COUNT(*) FROM (SELECT 1 FROM "+table+" WHERE model=? LIMIT ?)",
		model, vectorScanAllThreshold+1).Scan(&count); err != nil {
		return "", nil, 0, fmt.Errorf("count vector rows: %w", err)
	}
	if count <= vectorScanAllThreshold {
		return "model=?", []any{model}, max(count, 1), nil
	}
	placeholders := make([]string, len(probes))
	args = make([]any, 0, len(probes)+1)
	args = append(args, model)
	for index, probe := range probes {
		placeholders[index] = "?"
		args = append(args, probe)
	}
	// 召回上限按探查桶数放大。沿用单桶的 limit 是没有意义的：多出来的桶只会去挤占
	// 同一个配额，把原本能召回的文档顶掉，一点召回率都换不来。多探桶的全部价值就在
	// 于多看候选——而候选多了只花 CPU（解码 + 余弦），不会伤精度，因为最终排序用的
	// 是真实余弦而不是桶。
	// idx_(chunk_)embeddings_model_bucket 让 IN 走 len(probes) 次索引 seek。
	return "model=? AND bucket IN (" + strings.Join(placeholders, ",") + ")",
		args, limit * len(probes), nil
}

func (s *Store) loadVectorCandidates(ctx context.Context, query []float32, limit int, candidates map[int64]*hybridCandidate) error {
	where, args, recallLimit, err := s.vectorRecall(ctx, "embeddings", s.embedder.Model(), vectorProbes(query), limit)
	if err != nil {
		return err
	}
	args = append(args, recallLimit, recallLimit)
	rows, err := s.db.QueryContext(ctx, `WITH ev AS MATERIALIZED (
		SELECT file_id, vector, dimensions FROM embeddings
		WHERE `+where+` LIMIT ?
	) SELECT f.id, f.path, f.kind, f.size, f.mtime_ns,
		f.mime, f.tags_text, ev.vector, ev.dimensions
		FROM ev JOIN files f ON f.id=ev.file_id
		LIMIT ?`, args...)
	if err != nil {
		return fmt.Errorf("load vector candidates: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		candidate := &hybridCandidate{}
		if err := rows.Scan(&candidate.id, &candidate.hit.Path, &candidate.hit.Kind,
			&candidate.hit.Size, &candidate.hit.MTimeNS, &candidate.hit.MIME,
			&candidate.tagsText, &candidate.vectorRaw, &candidate.dims); err != nil {
			return fmt.Errorf("scan vector candidate: %w", err)
		}
		if existing := candidates[candidate.id]; existing != nil {
			existing.vectorRaw = candidate.vectorRaw
			existing.dims = candidate.dims
		} else {
			candidates[candidate.id] = candidate
		}
	}
	return rows.Err()
}

func (s *Store) loadMetadataCandidates(ctx context.Context, request HybridRequest, limit int, candidates map[int64]*hybridCandidate) error {
	if request.PathPrefix == "" && len(request.Kinds) == 0 && len(request.Tags) == 0 {
		return nil
	}
	statement := `SELECT f.id, f.path, f.kind, f.size, f.mtime_ns,
		f.mime, f.tags_text FROM files f WHERE 1=1`
	arguments := make([]any, 0)
	if request.PathPrefix != "" {
		statement += " AND substr(f.path,1,length(?))=?"
		arguments = append(arguments, request.PathPrefix, request.PathPrefix)
	}
	if request.MinSize > 0 {
		statement += " AND f.size>=?"
		arguments = append(arguments, request.MinSize)
	}
	if request.MaxSize > 0 {
		statement += " AND f.size<=?"
		arguments = append(arguments, request.MaxSize)
	}
	if len(request.Kinds) > 0 {
		placeholders := make([]string, len(request.Kinds))
		for index, kind := range request.Kinds {
			placeholders[index] = "?"
			arguments = append(arguments, kind)
		}
		statement += " AND f.kind IN (" + strings.Join(placeholders, ",") + ")"
	}
	for _, tag := range request.Tags {
		statement += " AND EXISTS (SELECT 1 FROM tags t WHERE t.file_id=f.id AND t.tag=?)"
		arguments = append(arguments, tag)
	}
	statement += " ORDER BY f.mtime_ns DESC LIMIT ?"
	arguments = append(arguments, limit)
	rows, err := s.db.QueryContext(ctx, statement, arguments...)
	if err != nil {
		return fmt.Errorf("load metadata candidates: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		candidate := &hybridCandidate{}
		if err := rows.Scan(&candidate.id, &candidate.hit.Path, &candidate.hit.Kind,
			&candidate.hit.Size, &candidate.hit.MTimeNS, &candidate.hit.MIME,
			&candidate.tagsText); err != nil {
			return fmt.Errorf("scan metadata candidate: %w", err)
		}
		if candidates[candidate.id] == nil {
			candidates[candidate.id] = candidate
		}
	}
	return rows.Err()
}

// englishStopwords 是 FTS 查询里忽略的高频虚词。它们会命中文档里同样无意义的虚词
// （如 query 里的 "the" 匹配注释里的 "the"），产生虚假的 BM25 分数。BM25 权重 0.52
// 高于向量 0.38，一个虚词命中就能把纯语义的向量匹配淹没在噪声里，所以查询侧直接丢弃。
var englishStopwords = map[string]bool{
	"a": true, "an": true, "the": true, "and": true, "or": true, "but": true,
	"of": true, "to": true, "in": true, "on": true, "at": true, "for": true,
	"with": true, "by": true, "is": true, "are": true, "was": true, "were": true,
	"be": true, "been": true, "being": true, "do": true, "does": true, "did": true,
	"have": true, "has": true, "had": true, "it": true, "its": true, "this": true,
	"that": true, "these": true, "those": true, "as": true, "from": true, "not": true,
	"no": true, "we": true, "you": true, "they": true, "he": true, "she": true,
	"i": true, "my": true, "your": true, "our": true, "their": true, "will": true,
	"would": true, "can": true, "could": true, "should": true, "what": true,
	"which": true, "who": true, "how": true, "when": true, "where": true, "why": true,
	"all": true, "some": true, "any": true, "into": true, "over": true, "under": true,
	"about": true, "between": true, "through": true, "out": true, "off": true,
	"up": true, "down": true, "then": true, "than": true, "so": true, "very": true,
	"just": true, "here": true, "there": true, "now": true, "too": true,
}

// ftsMatch 把自然语言 query 编译成 FTS5 表达式。
//
// CJK 段同时产出两种形式，OR 在一起：
//   - 整段短语。files_fts/chunks_fts 目前用 unicode61，一整段连写的汉字在索引里就是
//     一个 token，只有整段短语能命中它。不发这个会让今天已经能查到的用例退化。
//   - 逐个 bigram。unicode61 不做中缀匹配，所以 bigram 现在基本命不中；等 FTS 改成
//     索引分词后的文本（见 tokenize.go），bigram 才是真正生效的那一半。现在就发出来
//     没有副作用（OR 只扩召回），换索引时查询侧不用再动。
func ftsMatch(query string) string {
	quoted := make([]string, 0, 8)
	seen := make(map[string]struct{}, 8)
	add := func(term string) {
		// 跳过不含字母或数字的纯符号词（如 "---"）：FTS5 里既没有检索意义，
		// 又容易触发语法问题。
		if !strings.ContainsFunc(term, func(r rune) bool {
			return unicode.IsLetter(r) || unicode.IsNumber(r)
		}) {
			return
		}
		if _, ok := seen[term]; ok {
			return
		}
		seen[term] = struct{}{}
		quoted = append(quoted, `"`+strings.ReplaceAll(term, `"`, `""`)+`"`)
	}
	for _, run := range scriptRuns(query) {
		if !run.cjk {
			if englishStopwords[run.text] {
				continue
			}
			add(run.text)
			continue
		}
		add(run.text)
		for _, gram := range cjkBigrams(run.text) {
			add(gram)
		}
	}
	if len(quoted) == 0 {
		// 没有可检索的词。返回空串让调用方整路跳过；曾经返回的 `""` 是一个空短语，
		// FTS5 视其为语法错误，会把「全是停用词的 query」变成整次搜索失败。
		return ""
	}
	return strings.Join(quoted, " OR ")
}

func matchesMetadata(hit SearchHit, tagsText string, request HybridRequest) bool {
	if request.PathPrefix != "" && !strings.HasPrefix(hit.Path, request.PathPrefix) {
		return false
	}
	if request.MinSize > 0 && hit.Size < request.MinSize {
		return false
	}
	if request.MaxSize > 0 && hit.Size > request.MaxSize {
		return false
	}
	if len(request.Kinds) > 0 && !slices.Contains(request.Kinds, hit.Kind) {
		return false
	}
	availableTags := strings.Fields(tagsText)
	for _, tag := range request.Tags {
		if !slices.Contains(availableTags, tag) {
			return false
		}
	}
	return true
}
