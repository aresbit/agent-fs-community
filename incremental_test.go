package agentfs

import (
	"os"
	"path/filepath"
	"testing"
)

func TestSyncPathsBatchesFileAndDirectoryRemoval(t *testing.T) {
	store, root := newTestStore(t, Options{})
	directory := filepath.Join(root, "removed-directory")
	if err := os.Mkdir(directory, 0o700); err != nil {
		t.Fatal(err)
	}
	nested := filepath.Join(directory, "nested.txt")
	standalone := filepath.Join(root, "removed-file.txt")
	mustWrite(t, nested, "nested")
	mustWrite(t, standalone, "standalone")
	if _, err := store.Scan(t.Context(), root, ScanOptions{}); err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(directory); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(standalone); err != nil {
		t.Fatal(err)
	}
	if err := store.SyncPaths(t.Context(), root, []string{directory, standalone}); err != nil {
		t.Fatal(err)
	}
	var remaining int
	if err := store.db.QueryRowContext(t.Context(), `SELECT count(*) FROM files
		WHERE path IN (?,?,?)`, directory, nested, standalone).Scan(&remaining); err != nil {
		t.Fatal(err)
	}
	if remaining != 0 {
		t.Fatalf("removed paths remaining in index = %d", remaining)
	}
}

func TestSyncPathsReplacesChunksAndEmbeddings(t *testing.T) {
	store, root := newTestStore(t, Options{})
	path := filepath.Join(root, "worker.go")
	mustWrite(t, path, "package worker\n\nfunc OldSymbol() {}\n")
	if _, err := store.Scan(t.Context(), root, ScanOptions{}); err != nil {
		t.Fatal(err)
	}
	mustWrite(t, path, "package worker\n\nfunc NewSymbol() {}\n")
	if err := store.SyncPaths(t.Context(), root, []string{path}); err != nil {
		t.Fatal(err)
	}
	var oldCount, newCount, vectorCount int
	if err := store.db.QueryRowContext(t.Context(), `SELECT
		sum(CASE WHEN c.symbol='OldSymbol' THEN 1 ELSE 0 END),
		sum(CASE WHEN c.symbol='NewSymbol' THEN 1 ELSE 0 END),
		count(ce.chunk_id)
		FROM chunks c LEFT JOIN chunk_embeddings ce ON ce.chunk_id=c.id
		JOIN files f ON f.id=c.file_id WHERE f.path=?`, path).
		Scan(&oldCount, &newCount, &vectorCount); err != nil {
		t.Fatal(err)
	}
	if oldCount != 0 || newCount != 1 || vectorCount != 1 {
		t.Fatalf("incremental chunks old=%d new=%d vectors=%d", oldCount, newCount, vectorCount)
	}
}
