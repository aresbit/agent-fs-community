package agentfs

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"testing"
)

func TestRenameAndRemoveSubtree(t *testing.T) {
	store, root := newTestStore(t, Options{})
	directory := filepath.Join(root, "old")
	mustMkdir(t, directory)
	child := filepath.Join(directory, "child.txt")
	mustWrite(t, child, "child data")
	if _, err := store.Scan(t.Context(), root, ScanOptions{}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Scan(t.Context(), directory, ScanOptions{}); err != nil {
		t.Fatal(err)
	}
	if err := store.Tag(t.Context(), child, "kept"); err != nil {
		t.Fatal(err)
	}

	renamed, err := store.Rename(t.Context(), directory, "new")
	if err != nil {
		t.Fatalf("Rename() error = %v", err)
	}
	newChild := filepath.Join(renamed, "child.txt")
	if _, err := os.Stat(newChild); err != nil {
		t.Fatalf("renamed child stat: %v", err)
	}
	paths, err := store.Query(t.Context(), "SELECT path FROM files WHERE path LIKE ? ORDER BY path", renamed+"%")
	if err != nil {
		t.Fatal(err)
	}
	if got := firstColumnStrings(t, paths); len(got) != 2 || got[0] != renamed || got[1] != newChild {
		t.Fatalf("renamed indexed paths = %#v", got)
	}
	tagged, err := store.ByTag(t.Context(), "kept")
	if err != nil {
		t.Fatal(err)
	}
	if len(tagged.Rows) != 1 || tagged.Rows[0][0] != newChild {
		t.Fatalf("tag after rename = %#v", tagged.Rows)
	}
	rootMetadata, err := store.Query(t.Context(),
		"SELECT scan_root FROM files WHERE path = ?", newChild)
	if err != nil {
		t.Fatal(err)
	}
	if rootMetadata.Rows[0][0] != renamed {
		t.Fatalf("nested scan_root after rename = %v, want %s", rootMetadata.Rows[0][0], renamed)
	}
	usage, err := store.DiskUsage(t.Context(), root)
	if err != nil {
		t.Fatal(err)
	}
	if usage.Rows[0][0] != int64(3) {
		t.Fatalf("outer tree lost nested scan root: entries = %v", usage.Rows[0][0])
	}

	if err := store.Remove(t.Context(), renamed, false); err == nil {
		t.Fatal("Remove(non-recursive non-empty directory) unexpectedly succeeded")
	}
	if _, err := os.Stat(newChild); err != nil {
		t.Fatalf("failed remove changed filesystem: %v", err)
	}
	if err := store.Remove(t.Context(), renamed, true); err != nil {
		t.Fatalf("Remove(recursive) error = %v", err)
	}
	if _, err := os.Stat(renamed); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("removed path stat error = %v, want not exist", err)
	}
	remaining, err := store.Query(t.Context(), "SELECT count(*) FROM files WHERE path = ? OR path LIKE ?", renamed, renamed+"/%")
	if err != nil {
		t.Fatal(err)
	}
	if remaining.Rows[0][0] != int64(0) {
		t.Fatalf("removed indexed rows = %v", remaining.Rows[0][0])
	}
	matches, err := filepath.Glob(filepath.Join(root, ".agentfs-delete-*"))
	if err != nil {
		t.Fatal(err)
	}
	if len(matches) != 0 {
		t.Fatalf("deletion tombstones remain: %#v", matches)
	}
}

func TestMutationProtectsIndexDatabase(t *testing.T) {
	base := t.TempDir()
	root := filepath.Join(base, "root")
	mustMkdir(t, root)
	store, err := Open(t.Context(), filepath.Join(root, ".index", "fs.db"), Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	if _, err := store.Scan(t.Context(), root, ScanOptions{}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Rename(t.Context(), root, "renamed"); !errors.Is(err, ErrUnsafePath) {
		t.Fatalf("Rename(index parent) error = %v, want ErrUnsafePath", err)
	}
	if err := store.Remove(t.Context(), root, true); !errors.Is(err, ErrUnsafePath) {
		t.Fatalf("Remove(index parent) error = %v, want ErrUnsafePath", err)
	}
}

func TestOpenRecoversInterruptedRename(t *testing.T) {
	base := t.TempDir()
	root := filepath.Join(base, "tree")
	mustMkdir(t, root)
	oldPath := filepath.Join(root, "old.txt")
	newPath := filepath.Join(root, "new.txt")
	mustWrite(t, oldPath, "durable rename")
	database := filepath.Join(base, "index", "fs.db")
	store, err := Open(t.Context(), database, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Scan(t.Context(), root, ScanOptions{}); err != nil {
		t.Fatal(err)
	}
	journalID, err := store.createJournal(t.Context(), "rename", oldPath, newPath, "")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(oldPath, newPath); err != nil {
		t.Fatal(err)
	}
	if err := store.updateJournal(t.Context(), journalID, "fs_applied", nil); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	store, err = Open(t.Context(), database, Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if exists, err := store.indexedPathExists(t.Context(), newPath); err != nil || !exists {
		t.Fatalf("new path indexed=%v err=%v", exists, err)
	}
	if exists, err := store.indexedPathExists(t.Context(), oldPath); err != nil || exists {
		t.Fatalf("old path indexed=%v err=%v", exists, err)
	}
	var state string
	if err := store.db.QueryRowContext(t.Context(), "SELECT state FROM operation_journal WHERE id=?", journalID).Scan(&state); err != nil {
		t.Fatal(err)
	}
	if state != "done" {
		t.Fatalf("journal state = %q", state)
	}
}

func TestOpenRecoversInterruptedRemove(t *testing.T) {
	base := t.TempDir()
	root := filepath.Join(base, "tree")
	mustMkdir(t, root)
	path := filepath.Join(root, "delete.txt")
	mustWrite(t, path, "durable remove")
	database := filepath.Join(base, "index", "fs.db")
	store, err := Open(t.Context(), database, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Scan(t.Context(), root, ScanOptions{}); err != nil {
		t.Fatal(err)
	}
	stage, err := tombstonePath(path)
	if err != nil {
		t.Fatal(err)
	}
	journalID, err := store.createJournal(t.Context(), "remove", path, "", stage)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(path, stage); err != nil {
		t.Fatal(err)
	}
	if err := store.updateJournal(t.Context(), journalID, "fs_applied", nil); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	store, err = Open(t.Context(), database, Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if _, err := os.Lstat(stage); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("tombstone still exists: %v", err)
	}
	if exists, err := store.indexedPathExists(t.Context(), path); err != nil || exists {
		t.Fatalf("removed path indexed=%v err=%v", exists, err)
	}
	var state string
	if err := store.db.QueryRowContext(t.Context(), "SELECT state FROM operation_journal WHERE id=?", journalID).Scan(&state); err != nil {
		t.Fatal(err)
	}
	if state != "done" {
		t.Fatalf("journal state = %q", state)
	}
}
