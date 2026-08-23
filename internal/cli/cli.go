// Package cli implements the agent-fs command-line interface.
package cli

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"agentfs"
)

// Run executes agent-fs with args and returns a process exit code.
func Run(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	global := flag.NewFlagSet("agent-fs", flag.ContinueOnError)
	global.SetOutput(stderr)
	dbPath := global.String("db", defaultDBPath(), "SQLite index path (or AGENTFS_DB)")
	contentBytes := global.Int("content-bytes", 8*1024, "maximum text preview bytes per file")
	extractBytes := global.Int("extract-bytes", 2*1024*1024, "maximum extracted text bytes per file")
	maxRows := global.Int("max-rows", 1000, "maximum rows returned by one query")
	embeddingURL := global.String("embedding-url", os.Getenv("AGENTFS_EMBEDDING_URL"), "OpenAI-compatible embedding base URL")
	embeddingModel := global.String("embedding-model", os.Getenv("AGENTFS_EMBEDDING_MODEL"), "embedding model name")
	embeddingDimensions := global.Int("embedding-dimensions", envInt("AGENTFS_EMBEDDING_DIMENSIONS"), "embedding vector dimensions")
	embeddingKeyEnv := global.String("embedding-key-env", "AGENTFS_EMBEDDING_KEY", "environment variable containing embedding API key")
	global.Usage = func() { writeUsage(stderr) }
	if err := global.Parse(args); err != nil {
		return 2
	}
	remaining := global.Args()
	if len(remaining) == 0 {
		writeUsage(stderr)
		return 2
	}
	command, commandArgs := remaining[0], remaining[1:]
	if command == "help" || command == "--help" || command == "-h" {
		writeUsage(stdout)
		return 0
	}
	var embedder agentfs.Embedder
	if strings.TrimSpace(*embeddingURL) != "" {
		httpEmbedder, embedErr := agentfs.NewHTTPEmbedder(*embeddingURL, os.Getenv(*embeddingKeyEnv),
			*embeddingModel, *embeddingDimensions)
		if embedErr != nil {
			writeError(stderr, embedErr)
			return 2
		}
		embedder = httpEmbedder
	}
	store, err := agentfs.Open(ctx, *dbPath, agentfs.Options{
		ContentBytes: *contentBytes,
		ExtractBytes: *extractBytes,
		MaxRows:      *maxRows,
		Embedder:     embedder,
	})
	if err != nil {
		writeError(stderr, err)
		return 1
	}
	defer func() {
		if err := store.Close(); err != nil {
			writeError(stderr, err)
		}
	}()

	if err := dispatch(ctx, store, command, commandArgs, stdout, stderr); err != nil {
		writeError(stderr, err)
		return 1
	}
	return 0
}

func dispatch(ctx context.Context, store *agentfs.Store, command string, args []string, stdout, stderr io.Writer) error {
	switch command {
	case "init":
		if len(args) != 0 {
			return usageError("init takes no arguments")
		}
		return writeJSON(stdout, map[string]any{"database": store.Path(), "initialized": true})
	case "scan":
		flags := flag.NewFlagSet("scan", flag.ContinueOnError)
		flags.SetOutput(stderr)
		var tags stringList
		flags.Var(&tags, "tag", "tag to add to the scanned root (repeatable)")
		if err := flags.Parse(args); err != nil {
			return err
		}
		if flags.NArg() != 1 {
			return usageError("scan requires exactly one ROOT")
		}
		result, err := store.Scan(ctx, flags.Arg(0), agentfs.ScanOptions{Tags: tags})
		if err != nil {
			return err
		}
		return writeJSON(stdout, result)
	case "daemon", "serve":
		flags := flag.NewFlagSet("daemon", flag.ContinueOnError)
		flags.SetOutput(stderr)
		listen := flags.String("listen", "127.0.0.1:7337", "HTTP/MCP listen address")
		debounce := flags.Duration("debounce", 100*time.Millisecond, "watch event debounce")
		var roots, origins stringList
		flags.Var(&roots, "root", "filesystem root to watch (repeatable)")
		flags.Var(&origins, "allow-origin", "allowed browser Origin (repeatable)")
		if err := flags.Parse(args); err != nil {
			return err
		}
		if flags.NArg() != 0 || len(roots) == 0 {
			return usageError("daemon requires at least one --root and no positional arguments")
		}
		if !isLoopbackAddress(*listen) {
			return usageError("community daemon only listens on loopback addresses")
		}
		server, err := agentfs.NewHTTPServer(store, agentfs.ServerOptions{
			AllowedOrigins: origins,
		})
		if err != nil {
			return err
		}
		return runDaemon(ctx, store, server, *listen, roots, *debounce, stderr)
	case "query":
		if len(args) != 1 {
			return usageError("query requires one quoted SQL statement")
		}
		result, err := store.Query(ctx, args[0])
		if err != nil {
			return err
		}
		return writeJSON(stdout, result)
	case "ls":
		if len(args) != 1 {
			return usageError("ls requires PATH")
		}
		result, err := store.List(ctx, args[0])
		if err != nil {
			return err
		}
		return writeJSON(stdout, result)
	case "find":
		if len(args) != 1 {
			return usageError("find requires a phrase")
		}
		result, err := store.Search(ctx, args[0])
		if err != nil {
			return err
		}
		return writeJSON(stdout, result)
	case "big":
		if len(args) != 1 {
			return usageError("big requires a size in MiB")
		}
		megabytes, err := strconv.ParseFloat(args[0], 64)
		if err != nil || megabytes < 0 {
			return usageError("big size must be a non-negative number of MiB")
		}
		result, err := store.Big(ctx, int64(megabytes*1024*1024))
		if err != nil {
			return err
		}
		return writeJSON(stdout, result)
	case "du":
		if len(args) != 1 {
			return usageError("du requires PATH")
		}
		result, err := store.DiskUsage(ctx, args[0])
		if err != nil {
			return err
		}
		return writeJSON(stdout, result)
	case "by-tag":
		if len(args) != 1 {
			return usageError("by-tag requires TAG")
		}
		result, err := store.ByTag(ctx, args[0])
		if err != nil {
			return err
		}
		return writeJSON(stdout, result)
	case "tag", "untag":
		if len(args) != 2 {
			return usageError(command + " requires PATH TAG")
		}
		if command == "tag" {
			return store.Tag(ctx, args[0], args[1])
		}
		return store.Untag(ctx, args[0], args[1])
	case "rename":
		if len(args) != 2 {
			return usageError("rename requires PATH NEW_NAME")
		}
		path, err := store.Rename(ctx, args[0], args[1])
		if err != nil {
			return err
		}
		return writeJSON(stdout, map[string]string{"path": path})
	case "rm":
		flags := flag.NewFlagSet("rm", flag.ContinueOnError)
		flags.SetOutput(stderr)
		recursive := flags.Bool("recursive", false, "remove a non-empty directory subtree")
		flags.BoolVar(recursive, "r", false, "remove a non-empty directory subtree")
		if err := flags.Parse(args); err != nil {
			return err
		}
		if flags.NArg() != 1 {
			return usageError("rm requires PATH")
		}
		return store.Remove(ctx, flags.Arg(0), *recursive)
	case "rebuild-fts":
		if len(args) != 0 {
			return usageError("rebuild-fts takes no arguments")
		}
		return store.RebuildFTS(ctx)
	case "doctor":
		if len(args) != 0 {
			return usageError("doctor takes no arguments")
		}
		report, err := store.Check(ctx)
		if err != nil {
			return err
		}
		if report.Integrity != "ok" || report.FTSIntegrity != "ok" ||
			len(report.ForeignKeyErrors) > 0 || report.FileRows != report.FTSRows {
			return errors.Join(fmt.Errorf("index consistency check failed"), writeJSON(stdout, report))
		}
		return writeJSON(stdout, report)
	default:
		return usageError("unknown command " + strconv.Quote(command))
	}
}

type stringList []string

func (values *stringList) String() string { return strings.Join(*values, ",") }

func (values *stringList) Set(value string) error {
	*values = append(*values, value)
	return nil
}

func defaultDBPath() string {
	if path := strings.TrimSpace(os.Getenv("AGENTFS_DB")); path != "" {
		return path
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "fs.db"
	}
	return filepath.Join(home, ".agent-fs", "fs.db")
}

func envInt(name string) int {
	value := strings.TrimSpace(os.Getenv(name))
	if value == "" {
		return 0
	}
	parsed, err := strconv.Atoi(value)
	if err != nil {
		return 0
	}
	return parsed
}

func runDaemon(ctx context.Context, store *agentfs.Store, server *agentfs.HTTPServer, listen string,
	roots []string, debounce time.Duration, stderr io.Writer) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	errorsChannel := make(chan error, len(roots)+1)
	watchErrors := make(chan error, 64)
	var workers sync.WaitGroup
	workers.Go(func() {
		errorsChannel <- server.ListenAndServe(ctx, listen)
	})
	for _, root := range roots {
		root := root
		workers.Go(func() {
			err := store.Watch(ctx, agentfs.WatchOptions{
				Root: root, Debounce: debounce, Errors: watchErrors,
			})
			errorsChannel <- err
		})
	}
	_, _ = fmt.Fprintf(stderr, "agent-fs community daemon listening on http://%s; roots=%d\n", listen, len(roots))
	for {
		select {
		case err := <-errorsChannel:
			cancel()
			workers.Wait()
			if err == nil || errors.Is(err, context.Canceled) {
				return nil
			}
			return err
		case err := <-watchErrors:
			_, _ = fmt.Fprintf(stderr, "agent-fs watcher: %v\n", err)
		case <-ctx.Done():
			cancel()
			workers.Wait()
			return nil
		}
	}
}

func isLoopbackAddress(address string) bool {
	host, _, err := net.SplitHostPort(address)
	if err != nil {
		return false
	}
	return host == "localhost" || net.ParseIP(host).IsLoopback()
}

func writeJSON(writer io.Writer, value any) error {
	encoder := json.NewEncoder(writer)
	encoder.SetEscapeHTML(false)
	encoder.SetIndent("", "  ")
	if err := encoder.Encode(value); err != nil {
		return fmt.Errorf("write JSON: %w", err)
	}
	return nil
}

func writeError(writer io.Writer, err error) {
	_, _ = fmt.Fprintf(writer, "agent-fs: %v\n", err)
}

func usageError(message string) error {
	return fmt.Errorf("%s (run 'agent-fs help')", message)
}

func writeUsage(writer io.Writer) {
	_, _ = fmt.Fprint(writer, `agent-fs: query filesystem semantics through SQLite

Usage:
  agent-fs [--db PATH] [--content-bytes N] [--max-rows N] COMMAND [ARGS]

Commands:
  init                         initialize the database
  scan [--tag TAG] ROOT        transactionally scan a filesystem tree
  daemon --root ROOT [...]     watch roots and serve local HTTP + MCP
  query 'SELECT ...'           run bounded read-only SQL and emit JSON
  ls PATH                      list indexed direct children
  find PHRASE                  FTS5 phrase search over name/path/tag/content
  big MIB                      list regular files larger than MIB
  du PATH                      aggregate an indexed subtree
  by-tag TAG                   list paths carrying TAG
  tag PATH TAG                 add a semantic tag
  untag PATH TAG               remove a semantic tag
  rename PATH NEW_NAME         rename a path and its indexed subtree
  rm [-r|--recursive] PATH     remove through a recoverable tombstone
  rebuild-fts                  rebuild the FTS5 external-content index
  doctor                       check DB, FK, FTS, and filesystem consistency
`)
}
