package agentfs

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"testing"
)

func TestScanQueryTagAndReconcile(t *testing.T) {
	store, root := newTestStore(t, Options{})
	docs := filepath.Join(root, "docs")
	mustMkdir(t, docs)
	readme := filepath.Join(docs, "readme.txt")
	mustWrite(t, readme, "hello agent database")
	blob := filepath.Join(root, "blob.bin")
	mustWriteBytes(t, blob, []byte{0, 1, 2, 3})

	result, err := store.Scan(t.Context(), root, ScanOptions{Tags: []string{"project"}})
	if err != nil {
		t.Fatalf("Scan() error = %v", err)
	}
	if result.Entries != 4 {
		t.Fatalf("Scan().Entries = %d, want 4", result.Entries)
	}

	search, err := store.Search(t.Context(), "agent database")
	if err != nil {
		t.Fatalf("Search() error = %v", err)
	}
	if len(search.Rows) != 1 || search.Rows[0][0] != readme {
		t.Fatalf("Search() rows = %#v, want %s", search.Rows, readme)
	}
	tagged, err := store.ByTag(t.Context(), "project")
	if err != nil {
		t.Fatalf("ByTag() error = %v", err)
	}
	if len(tagged.Rows) != 1 || tagged.Rows[0][0] != root {
		t.Fatalf("ByTag() rows = %#v, want root", tagged.Rows)
	}
	if err := store.Tag(t.Context(), readme, "important"); err != nil {
		t.Fatalf("Tag() error = %v", err)
	}
	tagSearch, err := store.Search(t.Context(), "important")
	if err != nil {
		t.Fatalf("Search(tag) error = %v", err)
	}
	if len(tagSearch.Rows) != 1 || tagSearch.Rows[0][0] != readme {
		t.Fatalf("Search(tag) rows = %#v, want %s", tagSearch.Rows, readme)
	}

	usage, err := store.DiskUsage(t.Context(), root)
	if err != nil {
		t.Fatalf("DiskUsage() error = %v", err)
	}
	if got := usage.Rows[0][0]; got != int64(4) {
		t.Fatalf("DiskUsage entries = %v, want 4", got)
	}
	if got := usage.Rows[0][1]; got != int64(len("hello agent database")+4) {
		t.Fatalf("DiskUsage bytes = %v", got)
	}

	if err := os.Remove(blob); err != nil {
		t.Fatal(err)
	}
	fresh := filepath.Join(root, "fresh.txt")
	mustWrite(t, fresh, "new")
	if _, err := store.Scan(t.Context(), root, ScanOptions{}); err != nil {
		t.Fatalf("second Scan() error = %v", err)
	}
	paths, err := store.Query(t.Context(), "SELECT path FROM files ORDER BY path")
	if err != nil {
		t.Fatalf("Query(paths) error = %v", err)
	}
	gotPaths := firstColumnStrings(t, paths)
	wantPaths := []string{root, docs, readme, fresh}
	slices.Sort(wantPaths)
	if !slices.Equal(gotPaths, wantPaths) {
		t.Fatalf("paths = %#v, want %#v", gotPaths, wantPaths)
	}

	report, err := store.Check(t.Context())
	if err != nil {
		t.Fatalf("Check() error = %v", err)
	}
	if report.Integrity != "ok" || report.FTSIntegrity != "ok" ||
		report.FileRows != report.FTSRows || len(report.ForeignKeyErrors) != 0 {
		t.Fatalf("Check() = %#v", report)
	}
}

func TestReadOnlyQueryAndBound(t *testing.T) {
	store, root := newTestStore(t, Options{MaxRows: 1})
	mustWrite(t, filepath.Join(root, "a.txt"), "a")
	mustWrite(t, filepath.Join(root, "b.txt"), "b")
	if _, err := store.Scan(t.Context(), root, ScanOptions{}); err != nil {
		t.Fatal(err)
	}

	result, err := store.Query(t.Context(), "SELECT path FROM files ORDER BY path")
	if err != nil {
		t.Fatalf("Query() error = %v", err)
	}
	if len(result.Rows) != 1 || !result.Truncated {
		t.Fatalf("Query() = %#v, want one truncated row", result)
	}
	if _, err := store.Query(t.Context(), "DELETE FROM files"); !errors.Is(err, ErrInvalidQuery) {
		t.Fatalf("Query(DELETE) error = %v, want ErrInvalidQuery", err)
	}
	if _, err := store.Query(t.Context(), "WITH chosen AS (SELECT id FROM files) DELETE FROM files"); err == nil {
		t.Fatal("Query(WITH DELETE) unexpectedly succeeded on read-only connection")
	}
	attached := filepath.Join(t.TempDir(), "attached.db")
	attack := "PRAGMA query_only=OFF; ATTACH DATABASE '" + attached + "' AS attack; CREATE TABLE attack.pwned(value)"
	if _, err := store.Query(t.Context(), attack); !errors.Is(err, ErrInvalidQuery) {
		t.Fatalf("Query(multi-statement) error = %v, want ErrInvalidQuery", err)
	}
	if _, err := os.Stat(attached); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("multi-statement query created %s", attached)
	}
	if _, err := store.Query(t.Context(), "PRAGMA writable_schema=ON"); !errors.Is(err, ErrInvalidQuery) {
		t.Fatalf("Query(writable PRAGMA) error = %v, want ErrInvalidQuery", err)
	}
	if _, err := store.Query(t.Context(), "SELECT ';' AS semicolon;"); err != nil {
		t.Fatalf("Query(single statement with semicolon) error = %v", err)
	}
	count, err := store.Query(t.Context(), "SELECT count(*) FROM files")
	if err != nil {
		t.Fatal(err)
	}
	if count.Rows[0][0] != int64(3) {
		t.Fatalf("count after rejected writes = %v, want 3", count.Rows[0][0])
	}
}

func TestCancelledScanPreservesPreviousSnapshot(t *testing.T) {
	store, root := newTestStore(t, Options{})
	file := filepath.Join(root, "value.txt")
	mustWrite(t, file, "before")
	if _, err := store.Scan(t.Context(), root, ScanOptions{}); err != nil {
		t.Fatal(err)
	}
	mustWrite(t, file, "after")
	cancelled, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := store.Scan(cancelled, root, ScanOptions{}); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled Scan() error = %v, want context.Canceled", err)
	}
	result, err := store.Search(t.Context(), "before")
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Rows) != 1 {
		t.Fatalf("previous snapshot lost: %#v", result.Rows)
	}
}

func TestOuterScanReconcilesStaleNestedRootRows(t *testing.T) {
	store, root := newTestStore(t, Options{})
	nested := filepath.Join(root, "nested")
	mustMkdir(t, nested)
	file := filepath.Join(nested, "gone.txt")
	mustWrite(t, file, "gone")
	if _, err := store.Scan(t.Context(), root, ScanOptions{}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Scan(t.Context(), nested, ScanOptions{}); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(file); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Scan(t.Context(), root, ScanOptions{}); err != nil {
		t.Fatal(err)
	}
	result, err := store.Query(t.Context(), "SELECT count(*) FROM files WHERE path = ?", file)
	if err != nil {
		t.Fatal(err)
	}
	if result.Rows[0][0] != int64(0) {
		t.Fatalf("stale nested row count = %v, want 0", result.Rows[0][0])
	}
}

func TestConcurrentReadQueries(t *testing.T) {
	store, root := newTestStore(t, Options{})
	mustWrite(t, filepath.Join(root, "file.txt"), "concurrent")
	if _, err := store.Scan(t.Context(), root, ScanOptions{}); err != nil {
		t.Fatal(err)
	}
	var wait sync.WaitGroup
	errorsFound := make(chan error, 16)
	for range 16 {
		wait.Go(func() {
			result, err := store.Search(t.Context(), "concurrent")
			if err == nil && len(result.Rows) != 1 {
				err = errors.New("unexpected concurrent result")
			}
			errorsFound <- err
		})
	}
	wait.Wait()
	close(errorsFound)
	for err := range errorsFound {
		if err != nil {
			t.Fatal(err)
		}
	}
}

func TestOpenRejectsNewerSchema(t *testing.T) {
	base := t.TempDir()
	database := filepath.Join(base, "fs.db")
	store, err := Open(t.Context(), database, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.ExecContext(t.Context(), "PRAGMA user_version=99"); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(t.Context(), database, Options{}); !errors.Is(err, ErrIncompatibleSchema) {
		t.Fatalf("Open(newer schema) error = %v, want ErrIncompatibleSchema", err)
	}
}

func TestOpenRejectsPermissionAwareSchema(t *testing.T) {
	base := t.TempDir()
	database := filepath.Join(base, "fs.db")
	store, err := Open(t.Context(), database, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.ExecContext(t.Context(), "ALTER TABLE files ADD COLUMN mode INTEGER"); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(t.Context(), database, Options{}); !errors.Is(err, ErrIncompatibleSchema) {
		t.Fatalf("Open(permission schema) error = %v, want ErrIncompatibleSchema", err)
	}
}

func newTestStore(t *testing.T, opts Options) (*Store, string) {
	t.Helper()
	base := t.TempDir()
	root := filepath.Join(base, "tree")
	mustMkdir(t, root)
	store, err := Open(t.Context(), filepath.Join(base, "index", "fs.db"), opts)
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	t.Cleanup(func() {
		if err := store.Close(); err != nil {
			t.Errorf("Close() error = %v", err)
		}
	})
	return store, root
}

func mustMkdir(t *testing.T, path string) {
	t.Helper()
	if err := os.MkdirAll(path, 0o755); err != nil {
		t.Fatal(err)
	}
}

func mustWrite(t *testing.T, path, content string) {
	t.Helper()
	mustWriteBytes(t, path, []byte(content))
}

func mustWriteBytes(t *testing.T, path string, content []byte) {
	t.Helper()
	if err := os.WriteFile(path, content, 0o644); err != nil {
		t.Fatal(err)
	}
}

func firstColumnStrings(t *testing.T, result QueryResult) []string {
	t.Helper()
	values := make([]string, 0, len(result.Rows))
	for _, row := range result.Rows {
		value, ok := row[0].(string)
		if !ok {
			t.Fatalf("row value %T is not string", row[0])
		}
		values = append(values, value)
	}
	return values
}
