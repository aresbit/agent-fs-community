package agentfs

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
)

func TestGoASTChunksAreIndexed(t *testing.T) {
	store, root := newTestStore(t, Options{})
	path := filepath.Join(root, "service.go")
	mustWrite(t, path, `package service

type Worker struct{}

func (w *Worker) RebuildIndex(ctx context.Context) error {
	return nil
}
`)
	if _, err := store.Scan(t.Context(), root, ScanOptions{}); err != nil {
		t.Fatal(err)
	}
	var symbol, language string
	var start, end int
	if err := store.db.QueryRowContext(t.Context(), `SELECT symbol,language,start_line,end_line
		FROM chunks c JOIN files f ON f.id=c.file_id WHERE f.path=? AND symbol='Worker.RebuildIndex'`, path).
		Scan(&symbol, &language, &start, &end); err != nil {
		t.Fatal(err)
	}
	if symbol != "Worker.RebuildIndex" || language != "go" || start <= 0 || end < start {
		t.Fatalf("chunk = %q %q %d:%d", symbol, language, start, end)
	}
	hits, err := store.HybridSearch(t.Context(), HybridRequest{Query: "RebuildIndex", Limit: 5})
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) == 0 || hits[0].ChunkSymbol != "Worker.RebuildIndex" {
		t.Fatalf("AST hybrid hits = %#v", hits)
	}
}

func TestOfficeDOCXExtraction(t *testing.T) {
	path := filepath.Join(t.TempDir(), "design.docx")
	var archive bytes.Buffer
	writer := zip.NewWriter(&archive)
	entry, err := writer.Create("word/document.xml")
	if err != nil {
		t.Fatal(err)
	}
	_, _ = entry.Write([]byte(`<w:document xmlns:w="urn:w"><w:body><w:p><w:r><w:t>Local agent context</w:t></w:r></w:p><w:p><w:r><w:t>Crash recovery</w:t></w:r></w:p></w:body></w:document>`))
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, archive.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
	document, err := extractDocument(t.Context(), path, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(document.text, "Local agent context") || !strings.Contains(document.text, "Crash recovery") {
		t.Fatalf("DOCX text = %q", document.text)
	}
	if len(document.chunks) == 0 {
		t.Fatal("DOCX produced no chunks")
	}
}

func TestHTTPEmbedderBatch(t *testing.T) {
	var calls atomic.Int32
	var largest atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.Header.Get("Authorization") != "Bearer secret" {
			t.Error("missing embedding authorization")
		}
		var input struct {
			Input []string `json:"input"`
		}
		if err := json.NewDecoder(request.Body).Decode(&input); err != nil {
			t.Error(err)
			return
		}
		calls.Add(1)
		for {
			previous := largest.Load()
			if int32(len(input.Input)) <= previous || largest.CompareAndSwap(previous, int32(len(input.Input))) {
				break
			}
		}
		data := make([]map[string]any, len(input.Input))
		for index := range input.Input {
			data[index] = map[string]any{"index": index, "embedding": []float32{1, 0, 0, 0}}
		}
		_ = json.NewEncoder(writer).Encode(map[string]any{"data": data})
	}))
	defer server.Close()
	embedder, err := NewHTTPEmbedder(server.URL, "secret", "real-model", 4)
	if err != nil {
		t.Fatal(err)
	}
	vectors, err := embedder.EmbedBatch(context.Background(), []string{"one", "two"})
	if err != nil {
		t.Fatal(err)
	}
	if len(vectors) != 2 || len(vectors[0]) != 4 {
		t.Fatalf("vectors = %#v", vectors)
	}
	texts := make([]string, 130)
	for index := range texts {
		texts[index] = "bounded provider input"
	}
	vectors, err = embedTexts(t.Context(), embedder, texts)
	if err != nil {
		t.Fatal(err)
	}
	if len(vectors) != len(texts) || calls.Load() != 4 || largest.Load() != 64 {
		t.Fatalf("bounded batch: vectors=%d calls=%d largest=%d", len(vectors), calls.Load(), largest.Load())
	}
}
