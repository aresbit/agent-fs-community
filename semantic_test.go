package agentfs

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestSemanticRetrieval 用三份语义域明确的源码对比 hash embedder 与 ONNX embedder
// 的检索质量：query 用同义/近义表达（不直接出现文件名里的词），只有真语义向量能命中。
func TestSemanticRetrieval(t *testing.T) {
	dir := t.TempDir()
	files := map[string]string{
		"auth.go": `package auth

// Login verifies the password and issues a session token for the caller.
func Login(username, password string) (string, error) {
	if username == "" || password == "" {
		return "", ErrEmptyCredential
	}
	return issueToken(username)
}
`,
		"network.go": `package network

// Connect opens a TCP socket to addr and retries on transient failure.
func Connect(addr string) (Conn, error) {
	for attempt := 0; attempt < 3; attempt++ {
		c, err := dial(addr)
		if err == nil {
			return c, nil
		}
	}
	return nil, ErrUnreachable
}
`,
		"store.go": `package store

// Query executes a SQL statement and returns the rows that match.
func Query(statement string) ([]Row, error) {
	rows, err := db.QueryContext(ctx, statement)
	if err != nil {
		return nil, err
	}
	return scanRows(rows)
}
`,
	}
	for name, content := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	onnx, err := NewONNXEmbedder("")
	if err != nil {
		t.Skipf("onnx embedder unavailable: %v", err)
	}
	defer onnx.Close()

	// 三个语义 query：刻意不用目标文件里出现的任何词（纯语义同义改写），
	// 词袋 hash embedder 因无词重叠而命中不了，只有真语义向量能对上。
	cases := []struct {
		query string
		want  string
	}{
		{"prove who you are before entry", "auth.go"},
		{"reach a machine across the wire", "network.go"},
		{"pull records out of a table", "store.go"},
	}

	store, err := Open(t.Context(), filepath.Join(dir, "fs.db"), Options{Embedder: onnx})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if _, err := store.Scan(t.Context(), dir, ScanOptions{}); err != nil {
		t.Fatal(err)
	}

	t.Logf("%-4s | %-50s | %-14s | %-12s", "case", "query", "onnx top-1", "hash top-1")
	for index, c := range cases {
		onnxHit := topHitPath(t, store, c.query)

		// 用同一份索引内容，但换成 hash embedder 重新开一个 store 对比。
		hashStore, err := Open(t.Context(), filepath.Join(dir, "fs-hash.db"), Options{Embedder: NewHashEmbedder(256)})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := hashStore.Scan(t.Context(), dir, ScanOptions{}); err != nil {
			t.Fatal(err)
		}
		hashHit := topHitPath(t, hashStore, c.query)
		hashStore.Close()

		onnxName := filepath.Base(onnxHit)
		hashName := filepath.Base(hashHit)
		t.Logf("%-4d | %-50s | %-14s | %-12s", index+1, c.query, onnxName, hashName)

		if onnxName != c.want {
			t.Errorf("query %q: ONNX top-1 = %s, want %s", c.query, onnxName, c.want)
		}
		if hashName == c.want && onnxName != c.want {
			t.Errorf("query %q: hash beat ONNX unexpectedly", c.query)
		}
	}
}

func topHitPath(t *testing.T, store *Store, query string) string {
	t.Helper()
	hits, err := store.HybridSearch(t.Context(), HybridRequest{Query: query, Limit: 1})
	if err != nil {
		t.Fatalf("hybrid search %q: %v", query, err)
	}
	if len(hits) == 0 {
		return "(none)"
	}
	return strings.TrimSpace(hits[0].Path)
}
