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
		if err := s.loadLexicalCandidates(ctx, ftsMatch(request.Query), candidateLimit, candidates); err != nil {
			return nil, err
		}
		if err := s.loadChunkLexicalCandidates(ctx, ftsMatch(request.Query), candidateLimit, candidates); err != nil {
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
	hits := make([]SearchHit, 0, len(candidates))
	for _, candidate := range candidates {
		if !matchesMetadata(candidate.hit, candidate.tagsText, request) {
			continue
		}
		if candidate.lexRank > 0 {
			candidate.hit.BM25Score = 1 / float64(candidate.lexRank)
		}
		if len(candidate.vectorRaw) > 0 && len(queryVector) > 0 {
			if vector, ok := decodeVector(candidate.vectorRaw, candidate.dims); ok {
				candidate.hit.VectorScore = max(0, cosine(queryVector, vector))
			}
		}
		if request.Query != "" && candidate.lexRank == 0 && candidate.hit.VectorScore <= 0 {
			continue
		}
		age := now.Sub(time.Unix(0, candidate.hit.MTimeNS))
		candidate.hit.MetadataScore = 1 / (1 + math.Max(0, age.Hours()/24)/365)
		candidate.hit.Score = 0.52*candidate.hit.BM25Score +
			0.38*candidate.hit.VectorScore + 0.10*candidate.hit.MetadataScore
		hits = append(hits, candidate.hit)
	}
	slices.SortFunc(hits, func(left, right SearchHit) int {
		if left.Score != right.Score {
			if left.Score > right.Score {
				return -1
			}
			return 1
		}
		return strings.Compare(left.Path, right.Path)
	})
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

// rerank re-scores the top candidates with the cross-encoder and folds the
// result into Score. The cross-encoder is applied only to the first-stage top-K
// (2×limit, at least 10), so its per-pair cost stays bounded even on large indexes.
// The cross-encoder logit becomes the final ranking signal; the fused first-stage
// score remains available on BM25Score/VectorScore/MetadataScore for diagnostics.
func (s *Store) rerank(ctx context.Context, request HybridRequest, hits []SearchHit) error {
	if s.reranker == nil || request.Query == "" || len(hits) == 0 {
		return nil
	}
	rerankCount := min(len(hits), max(request.Limit*2, 10))
	docs := make([]string, rerankCount)
	for index := 0; index < rerankCount; index++ {
		docs[index] = rerankDocument(hits[index])
	}
	scores, err := s.reranker.Score(ctx, request.Query, docs)
	if err != nil {
		return err
	}
	if len(scores) != rerankCount {
		return fmt.Errorf("cross-encoder returned %d scores for %d documents", len(scores), rerankCount)
	}
	for index := 0; index < rerankCount; index++ {
		rerank := sigmoid(float64(scores[index]))
		hits[index].RerankScore = rerank
		// Cross-encoder is the final authority for ranking; sigmoid keeps Score
		// in (0,1) and preserves the logit ordering (sigmoid is monotonic).
		hits[index].Score = rerank
	}
	slices.SortFunc(hits, func(left, right SearchHit) int {
		if left.Score != right.Score {
			if left.Score > right.Score {
				return -1
			}
			return 1
		}
		return strings.Compare(left.Path, right.Path)
	})
	return nil
}

// rerankDocument 取用于 cross-encoder 的文档文本：优先 chunk 级内容（含符号名），
// 否则用文件级 snippet。截断到合理长度，避免过长序列稀释 cross-encoder 的注意力。
func rerankDocument(hit SearchHit) string {
	text := hit.Snippet
	if text == "" {
		text = hit.Path
	}
	if hit.ChunkSymbol != "" {
		text = hit.ChunkSymbol + "\n" + text
	}
	return compactSnippet(text, 1800)
}

func (s *Store) loadChunkLexicalCandidates(ctx context.Context, match string, limit int, candidates map[int64]*hybridCandidate) error {
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
	queryTerms := queryTermsFromMatch(match)
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
	where, args, recallLimit, err := s.vectorRecall(ctx, "chunk_embeddings", s.embedder.Model(), vectorBucket(query), limit)
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
		if newOK && (!oldOK || cosine(query, newVector) > cosine(query, oldVector)) {
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

func (s *Store) loadLexicalCandidates(ctx context.Context, match string, limit int, candidates map[int64]*hybridCandidate) error {
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
	queryTerms := queryTermsFromMatch(match)
	var averageLength float64
	for _, document := range documents {
		averageLength += float64(document.length)
	}
	averageLength /= float64(max(1, len(documents)))
	for _, term := range queryTerms {
		documentFrequency := 0
		for _, document := range documents {
			if document.terms[strings.ToLower(term)] > 0 {
				documentFrequency++
			}
		}
		idf := math.Log(1 + (float64(len(documents)-documentFrequency)+0.5)/(float64(documentFrequency)+0.5))
		for index := range documents {
			frequency := float64(documents[index].terms[strings.ToLower(term)])
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
	for index, document := range documents {
		document.candidate.lexRank = index + 1
		candidates[document.candidate.id] = document.candidate
	}
	return nil
}

func queryTermsFromMatch(match string) []string {
	queryTerms := make([]string, 0, 8)
	for _, term := range strings.Fields(strings.ReplaceAll(match, `"`, "")) {
		if !strings.EqualFold(term, "OR") {
			queryTerms = append(queryTerms, strings.ToLower(term))
		}
	}
	return queryTerms
}

func termFrequencies(text string) map[string]int {
	frequencies := make(map[string]int)
	for _, term := range strings.FieldsFunc(strings.ToLower(text), func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsNumber(r) && r != '_' && r != '-'
	}) {
		frequencies[term]++
	}
	return frequencies
}

func compactSnippet(text string, limit int) string {
	text = strings.TrimSpace(text)
	if len(text) <= limit {
		return text
	}
	return text[:limit] + "…"
}

// vectorScanAllThreshold 是向量召回全量扫描的规模阈值。sign-LSH 桶过滤能把大索引的
// 候选查表降为亚线性，但 query 与文件 embedding 的前 8 维符号一旦不匹配，语义相关文件
// 会被整桶漏掉——小索引下这个漏召回尤其致命（可能一个都召回不到）。低于该阈值时改全量
// 扫描，几十万以内的 384 维向量做精确余弦，成本毫秒级，召回完整。
const vectorScanAllThreshold = 20_000

// vectorRecall 返回向量召回的 WHERE 片段、参数与内/外层 LIMIT。小索引全量扫描
// （不带 bucket 过滤，LIMIT 用总行数），大索引走桶过滤（LIMIT 用调用方 limit）。
func (s *Store) vectorRecall(ctx context.Context, table, model string, bucket int64, limit int) (where string, args []any, recallLimit int, err error) {
	var count int
	if err = s.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM "+table+" WHERE model=?", model).Scan(&count); err != nil {
		return "", nil, 0, fmt.Errorf("count vector rows: %w", err)
	}
	if count <= vectorScanAllThreshold {
		return "model=?", []any{model}, max(count, 1), nil
	}
	return "model=? AND bucket=?", []any{model, bucket}, limit, nil
}

func (s *Store) loadVectorCandidates(ctx context.Context, query []float32, limit int, candidates map[int64]*hybridCandidate) error {
	where, args, recallLimit, err := s.vectorRecall(ctx, "embeddings", s.embedder.Model(), vectorBucket(query), limit)
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

func ftsMatch(query string) string {
	words := strings.FieldsFunc(query, func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsNumber(r) && r != '_' && r != '-'
	})
	quoted := make([]string, 0, len(words))
	for _, word := range words {
		if englishStopwords[strings.ToLower(word)] {
			continue
		}
		quoted = append(quoted, `"`+strings.ReplaceAll(word, `"`, `""`)+`"`)
	}
	if len(quoted) == 0 {
		return `""`
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
