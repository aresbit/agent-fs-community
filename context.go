package agentfs

import (
	"cmp"
	"context"
	"fmt"
	"slices"
	"strings"
)

type ContextRequest struct {
	Query       string `json:"query"`
	TokenBudget int    `json:"token_budget,omitempty"`
	Limit       int    `json:"limit,omitempty"`
}

type ContextItem struct {
	Path      string  `json:"path"`
	Kind      string  `json:"kind"`
	Score     float64 `json:"score"`
	Symbol    string  `json:"symbol,omitempty"`
	StartLine int     `json:"start_line,omitempty"`
	EndLine   int     `json:"end_line,omitempty"`
	Content   string  `json:"content,omitempty"`
}

type ContextPack struct {
	Query           string        `json:"query"`
	Items           []ContextItem `json:"items"`
	EstimatedTokens int           `json:"estimated_tokens"`
	Truncated       bool          `json:"truncated"`
}

// BuildContextPack retrieves, ranks, deduplicates, and budget-trims local
// context in one call. Token estimation is deliberately conservative (roughly
// four UTF-8 bytes/token); benchmark tooling records actual model usage too.
func (s *Store) BuildContextPack(ctx context.Context, request ContextRequest) (ContextPack, error) {
	if request.TokenBudget <= 0 {
		request.TokenBudget = 4_000
	}
	request.TokenBudget = min(request.TokenBudget, 32_000)
	if request.Limit <= 0 {
		request.Limit = 12
	}
	hits, err := s.HybridSearch(ctx, HybridRequest{Query: request.Query, Limit: request.Limit})
	if err != nil {
		return ContextPack{}, err
	}
	pack := ContextPack{Query: request.Query, Items: make([]ContextItem, 0, len(hits))}
	remainingBytes := request.TokenBudget * 4
	for _, hit := range hits {
		content, err := s.loadContextContent(ctx, hit)
		if err != nil {
			return ContextPack{}, err
		}
		header := hit.Path + "\n"
		available := remainingBytes - len(header)
		if available <= 0 {
			pack.Truncated = true
			break
		}
		content = strings.TrimSpace(content)
		if len(content) > available {
			// 预算是按字节算的，但截断要落在字符边界上，否则末尾的多字节字符会被
			// 劈开，JSON 编码时变成 U+FFFD。
			content = truncateUTF8(content, available)
			pack.Truncated = true
		}
		pack.Items = append(pack.Items, ContextItem{
			Path: hit.Path, Kind: hit.Kind, Score: hit.Score, Symbol: hit.ChunkSymbol,
			StartLine: hit.StartLine, EndLine: hit.EndLine, Content: content,
		})
		remainingBytes -= len(header) + len(content)
		if remainingBytes <= 0 {
			break
		}
	}
	pack.EstimatedTokens = (request.TokenBudget*4 - max(0, remainingBytes) + 3) / 4
	return pack, nil
}

// contextChunksPerHit 是每个命中最多注入的 chunk 数。
const contextChunksPerHit = 6

// loadContextContent 取一个命中的上下文正文：命中的那个 chunk，加上它在源文件里
// 的邻居。
//
// 选取顺序是「命中的符号 → 离命中行最近 → ordinal」。按 ordinal 直接取前 6 个是
// 错的：命中大文件第 40 个 chunk 时，返回的会是那个 chunk 加上文件开头的 0-4 号
// chunk（import 和许可证头），把 token 预算喂给了和 query 毫无关系的内容。
// 渲染时再按 ordinal 排回源码顺序，保证读起来是连贯的。
func (s *Store) loadContextContent(ctx context.Context, hit SearchHit) (string, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT c.ordinal,c.symbol,c.start_line,c.end_line,c.content
		FROM chunks c JOIN files f ON f.id=c.file_id
		WHERE f.path=?
		ORDER BY CASE WHEN c.symbol=? AND ?!='' THEN 0 ELSE 1 END,
		         abs(c.start_line - ?), c.ordinal
		LIMIT ?`,
		hit.Path, hit.ChunkSymbol, hit.ChunkSymbol, hit.StartLine, contextChunksPerHit)
	if err != nil {
		return "", fmt.Errorf("load context chunks %s: %w", hit.Path, err)
	}
	defer rows.Close()
	type contextChunk struct {
		ordinal    int
		symbol     string
		start, end int
		body       string
	}
	selected := make([]contextChunk, 0, contextChunksPerHit)
	for rows.Next() {
		var chunk contextChunk
		if err := rows.Scan(&chunk.ordinal, &chunk.symbol, &chunk.start, &chunk.end, &chunk.body); err != nil {
			return "", fmt.Errorf("scan context chunk %s: %w", hit.Path, err)
		}
		selected = append(selected, chunk)
	}
	if err := rows.Err(); err != nil {
		return "", fmt.Errorf("iterate context chunks %s: %w", hit.Path, err)
	}
	slices.SortFunc(selected, func(left, right contextChunk) int {
		return cmp.Compare(left.ordinal, right.ordinal)
	})
	var content strings.Builder
	for _, chunk := range selected {
		if content.Len() > 0 {
			content.WriteString("\n\n")
		}
		if chunk.symbol != "" {
			_, _ = fmt.Fprintf(&content, "// %s lines %d-%d\n", chunk.symbol, chunk.start, chunk.end)
		}
		content.WriteString(chunk.body)
	}
	if content.Len() > 0 {
		return content.String(), nil
	}
	if hit.Snippet != "" {
		return hit.Snippet, nil
	}
	var fallback string
	if err := s.db.QueryRowContext(ctx, `SELECT content_head FROM files WHERE path=?`,
		hit.Path).Scan(&fallback); err != nil {
		return "", fmt.Errorf("load context %s: %w", hit.Path, err)
	}
	return fallback, nil
}
