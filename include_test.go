package agentfs

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/fsnotify/fsnotify"
)

func TestScanIndexesSourceMarkdownAndConfigurationOnly(t *testing.T) {
	store, root := newTestStore(t, Options{})
	included := []string{
		"main.go", "lib.rs", "module.ml", "tool.py", "web.tsx", "README.md",
		"package.json", "Cargo.lock", "uv.lock", "dune-project", "Dockerfile",
	}
	for _, name := range included {
		mustWrite(t, filepath.Join(root, name), "searchable source context")
	}
	excluded := []string{
		"notes.txt", "image.png", "photo.jpg", "archive.zip", "bundle.tar.gz",
		"video.mp4", "audio.mp3", "manual.pdf", "document.docx", "slides.pptx",
		"sheet.xlsx", "database.db", "model.bin", "source.map", "font.woff2",
	}
	for _, name := range excluded {
		mustWriteBytes(t, filepath.Join(root, name), []byte{0, 1, 2, 3})
	}

	result, err := store.Scan(t.Context(), root, ScanOptions{})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{root}
	for _, name := range included {
		want = append(want, filepath.Join(root, name))
	}
	slices.Sort(want)
	if result.Entries != len(want) {
		t.Fatalf("Scan().Entries = %d, want %d", result.Entries, len(want))
	}
	paths, err := store.Query(t.Context(), "SELECT path FROM files ORDER BY path")
	if err != nil {
		t.Fatal(err)
	}
	if got := firstColumnStrings(t, paths); !slices.Equal(got, want) {
		t.Fatalf("indexed paths = %#v, want %#v", got, want)
	}
}

func TestCustomIncludeAndAllFiles(t *testing.T) {
	customStore, customRoot := newTestStore(t, Options{IncludePatterns: []string{"*.txt"}})
	note := filepath.Join(customRoot, "notes.txt")
	image := filepath.Join(customRoot, "image.png")
	mustWrite(t, note, "explicit text context")
	mustWriteBytes(t, image, []byte{0, 1, 2})
	if _, err := customStore.Scan(t.Context(), customRoot, ScanOptions{}); err != nil {
		t.Fatal(err)
	}
	assertIndexedCount(t, customStore, note, 1)
	assertIndexedCount(t, customStore, image, 0)

	allStore, allRoot := newTestStore(t, Options{AllFiles: true})
	allImage := filepath.Join(allRoot, "image.png")
	mustWrite(t, allImage, "legacy all-file preview")
	if _, err := allStore.Scan(t.Context(), allRoot, ScanOptions{}); err != nil {
		t.Fatal(err)
	}
	assertIndexedCount(t, allStore, allImage, 1)
}

func TestDefaultPolicyRemovesPreviouslyIndexedBinary(t *testing.T) {
	base := t.TempDir()
	root := filepath.Join(base, "tree")
	database := filepath.Join(base, "index", "fs.db")
	image := filepath.Join(root, "image.png")
	mustMkdir(t, root)
	mustWrite(t, image, "previously indexed image")

	oldStore, err := Open(t.Context(), database, Options{AllFiles: true})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := oldStore.Scan(t.Context(), root, ScanOptions{}); err != nil {
		t.Fatal(err)
	}
	if err := oldStore.Close(); err != nil {
		t.Fatal(err)
	}

	store, err := Open(t.Context(), database, Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if err := store.SyncPath(t.Context(), root, image); err != nil {
		t.Fatal(err)
	}
	assertIndexedCount(t, store, image, 0)
}

func TestWatchEventFilePolicy(t *testing.T) {
	store, root := newTestStore(t, Options{})
	image := filepath.Join(root, "image.png")
	source := filepath.Join(root, "main.go")
	directory := filepath.Join(root, "assets")
	mustWrite(t, image, "image")
	mustWrite(t, source, "package main")
	mustMkdir(t, directory)

	if store.shouldQueueWatchEvent(image, fsnotify.Create|fsnotify.Write) {
		t.Fatal("non-source create/write event was queued")
	}
	if !store.shouldQueueWatchEvent(source, fsnotify.Write) {
		t.Fatal("source write event was filtered")
	}
	if !store.shouldQueueWatchEvent(directory, fsnotify.Create) {
		t.Fatal("directory create event was filtered")
	}
	if !store.shouldQueueWatchEvent(image, fsnotify.Remove) {
		t.Fatal("remove event must be queued to reconcile a legacy row")
	}
}

func TestIncludePatternValidationAndCopies(t *testing.T) {
	base := t.TempDir()
	for _, pattern := range []string{"", "nested/path", "["} {
		_, err := Open(t.Context(), filepath.Join(base, filepath.Base(pattern)+"index.db"),
			Options{IncludePatterns: []string{pattern}})
		if !errors.Is(err, os.ErrInvalid) {
			t.Fatalf("Open(include-file %q) error = %v, want os.ErrInvalid", pattern, err)
		}
	}
	extensions := DefaultIncludeExtensions()
	extensions[0] = ".changed"
	if DefaultIncludeExtensions()[0] == ".changed" {
		t.Fatal("DefaultIncludeExtensions returned shared storage")
	}
	patterns := DefaultIncludePatterns()
	patterns[0] = "changed"
	if DefaultIncludePatterns()[0] == "changed" {
		t.Fatal("DefaultIncludePatterns returned shared storage")
	}
}

func assertIndexedCount(t *testing.T, store *Store, path string, want int64) {
	t.Helper()
	result, err := store.Query(t.Context(), "SELECT count(*) FROM files WHERE path=?", path)
	if err != nil {
		t.Fatal(err)
	}
	if got := result.Rows[0][0]; got != want {
		t.Fatalf("indexed count for %s = %v, want %d", path, got, want)
	}
}
