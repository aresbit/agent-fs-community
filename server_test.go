package agentfs

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestHybridSearchAndContextPack(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	root := t.TempDir()
	file := filepath.Join(root, "local.go")
	if err := os.WriteFile(file, []byte("package local\nfunc LocalContext() {}"), 0o600); err != nil {
		t.Fatal(err)
	}
	store, _ := newTestStore(t, Options{})
	if _, err := store.Scan(ctx, root, ScanOptions{}); err != nil {
		t.Fatal(err)
	}
	hits, err := store.HybridSearch(ctx, HybridRequest{Query: "LocalContext", Limit: 5})
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 1 || hits[0].Path != file {
		t.Fatalf("local hits = %#v", hits)
	}
	pack, err := store.BuildContextPack(ctx, ContextRequest{Query: "LocalContext", TokenBudget: 100})
	if err != nil {
		t.Fatal(err)
	}
	if len(pack.Items) != 1 || !strings.Contains(pack.Items[0].Content, "LocalContext") {
		t.Fatalf("context pack = %#v", pack)
	}
}

func TestMCPStatelessToolCall(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "design.md"), []byte("local context architecture"), 0o644); err != nil {
		t.Fatal(err)
	}
	store, _ := newTestStore(t, Options{})
	if _, err := store.Scan(ctx, root, ScanOptions{}); err != nil {
		t.Fatal(err)
	}
	server, err := NewHTTPServer(store, ServerOptions{})
	if err != nil {
		t.Fatal(err)
	}
	httpServer := httptest.NewServer(server.Handler())
	defer httpServer.Close()
	body := `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"context_pack","arguments":{"query":"architecture","token_budget":100}}}`
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, httpServer.URL+"/mcp", bytes.NewBufferString(body))
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("MCP-Protocol-Version", MCPProtocolVersion)
	request.Header.Set("Mcp-Method", "tools/call")
	request.Header.Set("Mcp-Name", "context_pack")
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	var rpc struct {
		Result struct {
			StructuredContent ContextPack `json:"structuredContent"`
		} `json:"result"`
		Error any `json:"error"`
	}
	if err := json.NewDecoder(response.Body).Decode(&rpc); err != nil {
		t.Fatal(err)
	}
	if rpc.Error != nil || len(rpc.Result.StructuredContent.Items) != 1 {
		t.Fatalf("MCP response = %#v", rpc)
	}
}

func TestMCPStreamableHTTPInitializeCompatibility(t *testing.T) {
	t.Parallel()
	store, _ := newTestStore(t, Options{})
	server, err := NewHTTPServer(store, ServerOptions{})
	if err != nil {
		t.Fatal(err)
	}
	httpServer := httptest.NewServer(server.Handler())
	defer httpServer.Close()
	body := `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"compat-test","version":"1"}}}`
	request, err := http.NewRequestWithContext(t.Context(), http.MethodPost, httpServer.URL+"/mcp", bytes.NewBufferString(body))
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Accept", "application/json, text/event-stream")
	request.Header.Set("Content-Type", "application/json")
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	var rpc struct {
		Result struct {
			ProtocolVersion string `json:"protocolVersion"`
			Capabilities    struct {
				Tools map[string]any `json:"tools"`
			} `json:"capabilities"`
		} `json:"result"`
	}
	if err := json.NewDecoder(response.Body).Decode(&rpc); err != nil {
		t.Fatal(err)
	}
	if rpc.Result.ProtocolVersion != "2025-06-18" || rpc.Result.Capabilities.Tools == nil {
		t.Fatalf("initialize response = %#v", rpc)
	}
}

func TestWatcherIncrementallyCreatesAndDeletes(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	store, _ := newTestStore(t, Options{})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ready := make(chan struct{}, 1)
	synced := make(chan string, 8)
	done := make(chan error, 1)
	go func() {
		done <- store.Watch(ctx, WatchOptions{Root: root,
			Debounce: 25 * time.Millisecond, Ready: ready,
			OnSynced: func(path string, _ time.Duration) { synced <- path }})
	}()
	select {
	case <-ready:
	case err := <-done:
		t.Fatal(err)
	case <-time.After(5 * time.Second):
		t.Fatal("watcher not ready")
	}
	path := filepath.Join(root, "fresh.txt")
	if err := os.WriteFile(path, []byte("fresh watcher context"), 0o644); err != nil {
		t.Fatal(err)
	}
	waitForSync(t, synced, path)
	hits, err := store.HybridSearch(context.Background(), HybridRequest{Query: "watcher context"})
	if err != nil || len(hits) == 0 {
		t.Fatalf("created file search: hits=%#v err=%v", hits, err)
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	waitForSync(t, synced, path)
	hits, err = store.HybridSearch(context.Background(), HybridRequest{Query: "watcher context"})
	if err != nil || len(hits) != 0 {
		t.Fatalf("deleted file search: hits=%#v err=%v", hits, err)
	}
}

func waitForSync(t *testing.T, synced <-chan string, path string) {
	t.Helper()
	deadline := time.After(5 * time.Second)
	for {
		select {
		case observed := <-synced:
			if observed == path {
				return
			}
		case <-deadline:
			t.Fatalf("timed out waiting for sync of %s", path)
		}
	}
}
