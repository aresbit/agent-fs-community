package agentfs

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"slices"
	"sync/atomic"
	"time"
)

const MCPProtocolVersion = "2026-07-28"

type ServerOptions struct {
	AllowedOrigins []string
	MaxBodyBytes   int64
}

type HTTPServer struct {
	store          *Store
	allowedOrigins []string
	maxBodyBytes   int64
	requests       atomic.Uint64
	errors         atomic.Uint64
}

func NewHTTPServer(store *Store, opts ServerOptions) (*HTTPServer, error) {
	if store == nil {
		return nil, errors.New("agentfs: HTTP server requires a store")
	}
	if opts.MaxBodyBytes <= 0 {
		opts.MaxBodyBytes = 1 << 20
	}
	server := &HTTPServer{store: store, allowedOrigins: slices.Clone(opts.AllowedOrigins), maxBodyBytes: opts.MaxBodyBytes}
	return server, nil
}

func (s *HTTPServer) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", s.health)
	mux.HandleFunc("POST /v1/search", s.counted(s.search))
	mux.HandleFunc("POST /v1/context", s.counted(s.contextPack))
	mux.HandleFunc("POST /mcp", s.counted(s.mcp))
	return s.securityHeaders(mux)
}

// ListenAndServe binds the loopback daemon and shuts it down when ctx ends.
func (s *HTTPServer) ListenAndServe(ctx context.Context, address string) error {
	listener, err := net.Listen("tcp", address)
	if err != nil {
		return fmt.Errorf("listen %s: %w", address, err)
	}
	return s.Serve(ctx, listener)
}

// Serve runs the daemon on an existing listener. Evaluation harnesses use this
// to bind an ephemeral port without racing a second Listen call.
func (s *HTTPServer) Serve(ctx context.Context, listener net.Listener) error {
	server := &http.Server{
		Handler:           s.Handler(),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       15 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       60 * time.Second,
	}
	done := make(chan error, 1)
	go func() { done <- server.Serve(listener) }()
	select {
	case err := <-done:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return fmt.Errorf("serve agent-fs: %w", err)
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := server.Shutdown(shutdownCtx); err != nil {
			return fmt.Errorf("shutdown agent-fs: %w", err)
		}
		return nil
	}
}

func (s *HTTPServer) securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		origin := request.Header.Get("Origin")
		if origin != "" && !slices.Contains(s.allowedOrigins, origin) {
			http.Error(writer, "origin not allowed", http.StatusForbidden)
			return
		}
		writer.Header().Set("Cache-Control", "no-store")
		writer.Header().Set("X-Content-Type-Options", "nosniff")
		next.ServeHTTP(writer, request)
	})
}

func (s *HTTPServer) counted(next http.HandlerFunc) http.HandlerFunc {
	return func(writer http.ResponseWriter, request *http.Request) {
		s.requests.Add(1)
		next(writer, request)
	}
}

func (s *HTTPServer) health(writer http.ResponseWriter, _ *http.Request) {
	s.writeJSON(writer, http.StatusOK, map[string]any{
		"status": "ok", "protocol": MCPProtocolVersion,
		"requests": s.requests.Load(), "errors": s.errors.Load(),
	})
}

func (s *HTTPServer) search(writer http.ResponseWriter, request *http.Request) {
	var input HybridRequest
	if !s.decodeJSON(writer, request, &input) {
		return
	}
	hits, err := s.store.HybridSearch(request.Context(), input)
	if err != nil {
		s.writeAPIError(writer, err)
		return
	}
	s.writeJSON(writer, http.StatusOK, map[string]any{"hits": hits})
}

func (s *HTTPServer) contextPack(writer http.ResponseWriter, request *http.Request) {
	var input ContextRequest
	if !s.decodeJSON(writer, request, &input) {
		return
	}
	pack, err := s.store.BuildContextPack(request.Context(), input)
	if err != nil {
		s.writeAPIError(writer, err)
		return
	}
	s.writeJSON(writer, http.StatusOK, pack)
}

type rpcRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params"`
}

func (s *HTTPServer) mcp(writer http.ResponseWriter, request *http.Request) {
	var rpc rpcRequest
	if !s.decodeJSON(writer, request, &rpc) {
		return
	}
	if rpc.JSONRPC != "2.0" || rpc.Method == "" {
		s.rpcError(writer, rpc.ID, -32600, "invalid JSON-RPC request")
		return
	}
	protocol := request.Header.Get("MCP-Protocol-Version")
	stateless := protocol == MCPProtocolVersion
	if protocol != "" && protocol != MCPProtocolVersion && protocol != "2025-06-18" && protocol != "2025-03-26" {
		s.rpcError(writer, rpc.ID, -32600, "unsupported MCP protocol version")
		return
	}
	if stateless && request.Header.Get("Mcp-Method") != rpc.Method {
		s.rpcError(writer, rpc.ID, -32600, "Mcp-Method header and JSON-RPC method must agree")
		return
	}
	switch rpc.Method {
	case "initialize":
		var params struct {
			ProtocolVersion string `json:"protocolVersion"`
		}
		if err := json.Unmarshal(rpc.Params, &params); err != nil {
			s.rpcError(writer, rpc.ID, -32602, "invalid initialize parameters")
			return
		}
		negotiated := "2025-06-18"
		if params.ProtocolVersion == "2025-03-26" {
			negotiated = params.ProtocolVersion
		}
		s.rpcResult(writer, rpc.ID, map[string]any{
			"protocolVersion": negotiated,
			"serverInfo":      map[string]string{"name": "agent-fs", "version": "0.1.0"},
			"capabilities":    map[string]any{"tools": map[string]any{"listChanged": false}},
			"instructions":    "Call context_pack once with the user's complete task and a 4000-6000 token budget before using search. The pack includes related code chunks; avoid repeated searches unless the evidence is genuinely missing.",
		})
	case "notifications/initialized", "notifications/cancelled":
		writer.WriteHeader(http.StatusAccepted)
	case "server/discover":
		s.rpcResult(writer, rpc.ID, map[string]any{
			"protocolVersion": MCPProtocolVersion,
			"serverInfo":      map[string]string{"name": "agent-fs", "version": "0.1.0"},
			"capabilities":    map[string]any{"tools": map[string]any{"listChanged": false}},
		})
	case "tools/list":
		s.rpcResult(writer, rpc.ID, map[string]any{
			"tools": mcpTools(), "ttlMs": 60_000, "cacheScope": "local",
		})
	case "tools/call":
		var params struct {
			Name      string          `json:"name"`
			Arguments json.RawMessage `json:"arguments"`
		}
		if err := json.Unmarshal(rpc.Params, &params); err != nil {
			s.rpcError(writer, rpc.ID, -32602, "invalid tool parameters")
			return
		}
		if params.Name == "" || (stateless && request.Header.Get("Mcp-Name") != params.Name) {
			s.rpcError(writer, rpc.ID, -32600, "Mcp-Name header and tool name must agree")
			return
		}
		result, err := s.callTool(request.Context(), params.Name, params.Arguments)
		if err != nil {
			s.rpcError(writer, rpc.ID, -32000, err.Error())
			return
		}
		s.rpcResult(writer, rpc.ID, map[string]any{
			"content":           []map[string]any{{"type": "text", "text": mustJSONString(result)}},
			"structuredContent": result,
		})
	default:
		s.rpcError(writer, rpc.ID, -32601, "method not found")
	}
}

func mcpTools() []map[string]any {
	readOnlyAnnotations := map[string]any{
		"readOnlyHint": true, "destructiveHint": false,
		"idempotentHint": true, "openWorldHint": false,
	}
	return []map[string]any{
		{"name": "context_pack", "title": "Local context pack",
			"description": "Preferred first tool. Send the complete user task once with a 4000-6000 token budget; it returns ranked related code chunks, so repeated search/read calls are normally unnecessary.",
			"annotations": readOnlyAnnotations,
			"inputSchema": map[string]any{"type": "object", "required": []string{"query"}, "properties": map[string]any{
				"query":        map[string]string{"type": "string", "description": "Natural-language task, exact symbol, error text, or domain phrase."},
				"token_budget": map[string]string{"type": "integer", "description": "Maximum estimated context tokens, from 1 to 32000."},
				"limit":        map[string]string{"type": "integer", "description": "Maximum ranked chunks, from 1 to 200."}}}},
		{"name": "search", "title": "Local hybrid file search",
			"description": "Hybrid BM25, vector, and metadata search over locally indexed files. Read-only.",
			"annotations": readOnlyAnnotations,
			"inputSchema": map[string]any{"type": "object", "required": []string{"query"}, "properties": map[string]any{
				"query":       map[string]string{"type": "string", "description": "Natural-language query or exact code symbol."},
				"limit":       map[string]string{"type": "integer", "description": "Maximum result count, from 1 to 200."},
				"path_prefix": map[string]string{"type": "string", "description": "Optional absolute indexed path prefix."},
				"min_size":    map[string]string{"type": "integer", "description": "Minimum file size in bytes."},
				"max_size":    map[string]string{"type": "integer", "description": "Maximum file size in bytes."},
				"kinds":       map[string]any{"type": "array", "items": map[string]string{"type": "string"}},
				"tags":        map[string]any{"type": "array", "items": map[string]string{"type": "string"}}}}},
	}
}

func (s *HTTPServer) callTool(ctx context.Context, name string, raw json.RawMessage) (any, error) {
	switch name {
	case "context_pack":
		var request ContextRequest
		if err := json.Unmarshal(raw, &request); err != nil {
			return nil, fmt.Errorf("decode context_pack: %w", err)
		}
		return s.store.BuildContextPack(ctx, request)
	case "search":
		var request HybridRequest
		if err := json.Unmarshal(raw, &request); err != nil {
			return nil, fmt.Errorf("decode search: %w", err)
		}
		return s.store.HybridSearch(ctx, request)
	default:
		return nil, fmt.Errorf("unknown tool %q", name)
	}
}

func (s *HTTPServer) decodeJSON(writer http.ResponseWriter, request *http.Request, destination any) bool {
	decoder := json.NewDecoder(io.LimitReader(request.Body, s.maxBodyBytes))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(destination); err != nil {
		s.writeJSON(writer, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return false
	}
	return true
}

func (s *HTTPServer) writeAPIError(writer http.ResponseWriter, err error) {
	s.errors.Add(1)
	s.writeJSON(writer, http.StatusBadRequest, map[string]string{"error": err.Error()})
}

func (s *HTTPServer) writeJSON(writer http.ResponseWriter, status int, value any) {
	writer.Header().Set("Content-Type", "application/json")
	writer.WriteHeader(status)
	_ = json.NewEncoder(writer).Encode(value)
}

func (s *HTTPServer) rpcResult(writer http.ResponseWriter, id json.RawMessage, result any) {
	s.writeJSON(writer, http.StatusOK, map[string]any{"jsonrpc": "2.0", "id": id, "result": result})
}

func (s *HTTPServer) rpcError(writer http.ResponseWriter, id json.RawMessage, code int, message string) {
	s.errors.Add(1)
	s.writeJSON(writer, http.StatusOK, map[string]any{"jsonrpc": "2.0", "id": id,
		"error": map[string]any{"code": code, "message": message}})
}

func mustJSONString(value any) string {
	raw, _ := json.Marshal(value)
	return string(raw)
}
