package agentfs

import (
	"os"
	"path/filepath"
	"testing"
)

// TestRerankEndToEnd loads the real cross-encoder model and verifies that a
// hybrid query returns hits with non-zero rerank scores in descending order.
func TestRerankEndToEnd(t *testing.T) {
	modelPath := "models/cross-encoder-msmarco-MiniLM-L6-v2.onnx"
	if _, err := os.Stat(modelPath); err != nil {
		t.Skip("cross-encoder model not present, skipping rerank test")
	}

	dir := t.TempDir()
	files := map[string]string{
		"auth.go": "package auth\n\n// Verify checks a session token against the store.\nfunc Verify(token string) bool { return token != \"\" }\n",
		"network.go": "package network\n\n// Dial opens a TCP connection with retry and backoff.\nfunc Dial(addr string) (Conn, error) { return nil, nil }\n",
		"notes.md": "## Shopping list\n\nmilk, eggs, bread, and a new keyboard for the office.\n",
	}
	for name, content := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	embedder, err := NewONNXEmbedder("")
	if err != nil {
		t.Skipf("onnx embedder unavailable: %v", err)
	}
	defer embedder.Close()

	reranker, err := NewCrossEncoder(modelPath, "")
	if err != nil {
		t.Fatalf("load cross-encoder: %v", err)
	}

	store, err := Open(t.Context(), filepath.Join(dir, "fs.db"), Options{
		Embedder: embedder,
		Reranker: reranker,
	})
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer store.Close()

	if _, err := store.Scan(t.Context(), dir, ScanOptions{}); err != nil {
		t.Fatalf("scan: %v", err)
	}

	hits, err := store.HybridSearch(t.Context(), HybridRequest{Query: "session token authentication", Limit: 5})
	if err != nil {
		t.Fatalf("hybrid search: %v", err)
	}
	if len(hits) == 0 {
		t.Fatal("expected at least one hit")
	}
	for index, hit := range hits {
		if hit.RerankScore <= 0 {
			t.Errorf("hit %q has non-positive rerank score %f", hit.Path, hit.RerankScore)
		}
		if index > 0 && hits[index-1].RerankScore < hit.RerankScore {
			t.Errorf("rerank scores not descending: %f then %f", hits[index-1].RerankScore, hit.RerankScore)
		}
	}
	t.Logf("top hit: %s (rerank=%.4f, score=%.4f)", hits[0].Path, hits[0].RerankScore, hits[0].Score)
}
