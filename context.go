package agentfs

import (
	"context"
	"fmt"
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
			content = content[:available]
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

func (s *Store) loadContextContent(ctx context.Context, hit SearchHit) (string, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT c.symbol,c.start_line,c.end_line,c.content
		FROM chunks c JOIN files f ON f.id=c.file_id
		WHERE f.path=? ORDER BY CASE WHEN c.symbol=? AND ?!='' THEN 0 ELSE 1 END,c.ordinal LIMIT 6`,
		hit.Path, hit.ChunkSymbol, hit.ChunkSymbol)
	if err != nil {
		return "", fmt.Errorf("load context chunks %s: %w", hit.Path, err)
	}
	defer rows.Close()
	var content strings.Builder
	for rows.Next() {
		var symbol, chunk string
		var start, end int
		if err := rows.Scan(&symbol, &start, &end, &chunk); err != nil {
			return "", fmt.Errorf("scan context chunk %s: %w", hit.Path, err)
		}
		if content.Len() > 0 {
			content.WriteString("\n\n")
		}
		if symbol != "" {
			_, _ = fmt.Fprintf(&content, "// %s lines %d-%d\n", symbol, start, end)
		}
		content.WriteString(chunk)
	}
	if err := rows.Err(); err != nil {
		return "", fmt.Errorf("iterate context chunks %s: %w", hit.Path, err)
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
