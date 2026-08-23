package agentfs

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/fsnotify/fsnotify"
)

func TestScanPrunesDefaultEngineeringDirectories(t *testing.T) {
	store, root := newTestStore(t, Options{})
	excluded := []string{
		".git/objects/pack/data",
		".cache/tool/result",
		"bazel-project/bin/output",
		"node_modules/package/index.js",
		".npm/_cacache/content",
		".parcel-cache/bundle/data",
		".venv/lib/python/site-packages/module.py",
		".nox/test/lib/module.py",
		"package.egg-info/PKG-INFO",
		"_build/default/module.cmx",
		"_opam/lib/package/META",
		"target/debug/binary",
	}
	for _, relative := range excluded {
		path := filepath.Join(root, filepath.FromSlash(relative))
		mustMkdir(t, filepath.Dir(path))
		mustWrite(t, path, "must not be indexed")
	}
	source := filepath.Join(root, "src", "main.go")
	mustMkdir(t, filepath.Dir(source))
	mustWrite(t, source, "package main\nfunc main() {}")
	preserved := []string{
		"dune-project", "Cargo.toml", "Cargo.lock", "pyproject.toml", "uv.lock",
		"package.json", "package-lock.json", "pnpm-lock.yaml", "yarn.lock",
	}
	for _, name := range preserved {
		mustWrite(t, filepath.Join(root, name), "authoritative project configuration")
	}
	cargoConfig := filepath.Join(root, ".cargo", "config.toml")
	mustMkdir(t, filepath.Dir(cargoConfig))
	mustWrite(t, cargoConfig, "[build]")

	result, err := store.Scan(t.Context(), root, ScanOptions{})
	if err != nil {
		t.Fatal(err)
	}
	paths, err := store.Query(t.Context(), "SELECT path FROM files ORDER BY path")
	if err != nil {
		t.Fatal(err)
	}
	want := []string{root, filepath.Dir(source), source, filepath.Dir(cargoConfig), cargoConfig}
	for _, name := range preserved {
		want = append(want, filepath.Join(root, name))
	}
	slices.Sort(want)
	if result.Entries != len(want) {
		t.Fatalf("Scan().Entries = %d, want %d", result.Entries, len(want))
	}
	if got := firstColumnStrings(t, paths); !slices.Equal(got, want) {
		t.Fatalf("indexed paths = %#v, want %#v", got, want)
	}
}

func TestCustomExcludeAndNoDefaultExcludes(t *testing.T) {
	customStore, customRoot := newTestStore(t, Options{ExcludePatterns: []string{"generated-*"}})
	generated := filepath.Join(customRoot, "generated-api", "client.go")
	mustMkdir(t, filepath.Dir(generated))
	mustWrite(t, generated, "generated client")
	kept := filepath.Join(customRoot, "source", "client.go")
	mustMkdir(t, filepath.Dir(kept))
	mustWrite(t, kept, "source client")
	if _, err := customStore.Scan(t.Context(), customRoot, ScanOptions{}); err != nil {
		t.Fatal(err)
	}
	if result, err := customStore.Query(t.Context(), "SELECT count(*) FROM files WHERE path=?", generated); err != nil {
		t.Fatal(err)
	} else if result.Rows[0][0] != int64(0) {
		t.Fatalf("custom-excluded path count = %v, want 0", result.Rows[0][0])
	}

	allStore, allRoot := newTestStore(t, Options{NoDefaultExcludes: true, AllFiles: true})
	gitHead := filepath.Join(allRoot, ".git", "HEAD")
	mustMkdir(t, filepath.Dir(gitHead))
	mustWrite(t, gitHead, "ref: refs/heads/main")
	if _, err := allStore.Scan(t.Context(), allRoot, ScanOptions{}); err != nil {
		t.Fatal(err)
	}
	if result, err := allStore.Query(t.Context(), "SELECT count(*) FROM files WHERE path=?", gitHead); err != nil {
		t.Fatal(err)
	} else if result.Rows[0][0] != int64(1) {
		t.Fatalf("default-disabled path count = %v, want 1", result.Rows[0][0])
	}
}

func TestAddWatchTreePrunesExcludedDirectories(t *testing.T) {
	store, root := newTestStore(t, Options{})
	kept := filepath.Join(root, "src", "internal")
	excluded := filepath.Join(root, ".git", "objects", "pack")
	bazel := filepath.Join(root, "bazel-workspace", "external", "dependency")
	mustMkdir(t, kept)
	mustMkdir(t, excluded)
	mustMkdir(t, bazel)

	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		t.Fatal(err)
	}
	defer watcher.Close()
	if err := store.addWatchTree(watcher, root); err != nil {
		t.Fatal(err)
	}
	watches := watcher.WatchList()
	want := []string{root, filepath.Join(root, "src"), kept}
	for _, path := range want {
		if !slices.Contains(watches, path) {
			t.Fatalf("watch list %#v does not contain %s", watches, path)
		}
	}
	for _, path := range watches {
		if isWithin(path, filepath.Join(root, ".git")) || isWithin(path, filepath.Join(root, "bazel-workspace")) {
			t.Fatalf("excluded path received a watch: %s", path)
		}
	}
}

func TestExcludedPathsReconcilePreviouslyIndexedTrees(t *testing.T) {
	base := t.TempDir()
	root := filepath.Join(base, "tree")
	database := filepath.Join(base, "index", "fs.db")
	gitObject := filepath.Join(root, ".git", "objects", "object")
	cacheEntry := filepath.Join(root, ".cache", "tool", "entry")
	mustMkdir(t, filepath.Dir(gitObject))
	mustWrite(t, gitObject, "old indexed object")
	mustMkdir(t, filepath.Dir(cacheEntry))
	mustWrite(t, cacheEntry, "old indexed cache")

	oldStore, err := Open(t.Context(), database, Options{NoDefaultExcludes: true})
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
	if err := store.SyncPath(t.Context(), root, filepath.Join(root, ".git")); err != nil {
		t.Fatal(err)
	}
	result, err := store.Query(t.Context(), "SELECT count(*) FROM files WHERE path=?", gitObject)
	if err != nil {
		t.Fatal(err)
	}
	if result.Rows[0][0] != int64(0) {
		t.Fatalf("incrementally reconciled path count = %v, want 0", result.Rows[0][0])
	}
	if _, err := store.Scan(t.Context(), root, ScanOptions{}); err != nil {
		t.Fatal(err)
	}
	result, err = store.Query(t.Context(), "SELECT count(*) FROM files WHERE path=?", cacheEntry)
	if err != nil {
		t.Fatal(err)
	}
	if result.Rows[0][0] != int64(0) {
		t.Fatalf("full-scan reconciled path count = %v, want 0", result.Rows[0][0])
	}
}

func TestExcludePatternValidationAndCopy(t *testing.T) {
	base := t.TempDir()
	for _, pattern := range []string{"", "nested/path", "["} {
		_, err := Open(t.Context(), filepath.Join(base, filepath.Base(pattern)+"index.db"),
			Options{ExcludePatterns: []string{pattern}})
		if !errors.Is(err, os.ErrInvalid) {
			t.Fatalf("Open(exclude %q) error = %v, want os.ErrInvalid", pattern, err)
		}
	}
	patterns := DefaultExcludePatterns()
	patterns[0] = "changed"
	if DefaultExcludePatterns()[0] == "changed" {
		t.Fatal("DefaultExcludePatterns returned shared storage")
	}
}
