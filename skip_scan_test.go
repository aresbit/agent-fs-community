package agentfs

import (
	"context"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

// countingEmbedder wraps a real embedder and counts per-text embedding calls.
// A SkipUnchanged rescan over an untouched tree must not call it at all.
type countingEmbedder struct {
	inner Embedder
	mu    sync.Mutex
	calls int
}

func (c *countingEmbedder) Model() string   { return c.inner.Model() }
func (c *countingEmbedder) Dimensions() int { return c.inner.Dimensions() }
func (c *countingEmbedder) count() int      { c.mu.Lock(); defer c.mu.Unlock(); return c.calls }
func (c *countingEmbedder) Embed(ctx context.Context, text string) ([]float32, error) {
	c.mu.Lock()
	c.calls++
	c.mu.Unlock()
	return c.inner.Embed(ctx, text)
}

// forceMTime guarantees the file fingerprint changes even when writes land in
// the same nanosecond, so the reuse decision is deterministic.
func forceMTime(t *testing.T, path string) {
	t.Helper()
	stamp := time.Now().Add(2 * time.Second)
	if err := os.Chtimes(path, stamp, stamp); err != nil {
		t.Fatalf("Chtimes(%s): %v", path, err)
	}
}

func TestScanSkipUnchangedReusesRows(t *testing.T) {
	embedder := &countingEmbedder{inner: NewHashEmbedder(256)}
	store, root := newTestStore(t, Options{Embedder: embedder})
	docs := filepath.Join(root, "docs")
	mustMkdir(t, docs)
	alpha := filepath.Join(docs, "alpha.md")
	mustWrite(t, alpha, "alpha one two three")
	beta := filepath.Join(root, "beta.md")
	mustWrite(t, beta, "beta four five six")
	forceMTime(t, alpha)
	forceMTime(t, beta)

	// Initial scan: empty index, nothing to reuse.
	if _, err := store.Scan(t.Context(), root, ScanOptions{SkipUnchanged: true}); err != nil {
		t.Fatalf("initial SkipUnchanged Scan() error = %v", err)
	}
	initial := embedder.count()
	if initial == 0 {
		t.Fatal("initial scan embedded nothing")
	}

	// Rescan an untouched tree: SkipUnchanged must reuse every row, so the
	// embedder is not invoked and search still sees the same content.
	if _, err := store.Scan(t.Context(), root, ScanOptions{SkipUnchanged: true}); err != nil {
		t.Fatalf("unchanged SkipUnchanged Scan() error = %v", err)
	}
	if got := embedder.count(); got != initial {
		t.Fatalf("unchanged rescan ran %d more embeddings, want 0", got-initial)
	}
	if rows := mustSearchRows(t, store, "alpha"); rows != 1 {
		t.Fatalf("search after reuse scan rows = %d, want 1", rows)
	}

	// A changed file plus a new file must still be fully reprocessed while the
	// untouched sibling is reused.
	mustWrite(t, alpha, "alpha changed content")
	forceMTime(t, alpha)
	gamma := filepath.Join(root, "gamma.md")
	mustWrite(t, gamma, "gamma brand new")
	forceMTime(t, gamma)
	before := embedder.count()
	if _, err := store.Scan(t.Context(), root, ScanOptions{SkipUnchanged: true}); err != nil {
		t.Fatalf("changed SkipUnchanged Scan() error = %v", err)
	}
	delta := embedder.count() - before
	if delta == 0 {
		t.Fatal("changed/new files were not re-embedded")
	}
	if rows := mustSearchRows(t, store, "changed"); rows != 1 {
		t.Fatalf("updated content not searchable after reuse scan: rows=%d", rows)
	}
	if rows := mustSearchRows(t, store, "brand"); rows != 1 {
		t.Fatalf("new file not searchable after reuse scan: rows=%d", rows)
	}

	// Removing a file must clear its stale rows even under SkipUnchanged.
	if err := os.Remove(gamma); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Scan(t.Context(), root, ScanOptions{SkipUnchanged: true}); err != nil {
		t.Fatalf("post-remove SkipUnchanged Scan() error = %v", err)
	}
	result, err := store.Query(t.Context(), "SELECT count(*) FROM files WHERE path = ?", gamma)
	if err != nil {
		t.Fatalf("Query() error = %v", err)
	}
	if result.Rows[0][0] != int64(0) {
		t.Fatalf("stale row for removed file = %v, want 0", result.Rows[0][0])
	}

	report, err := store.Check(t.Context())
	if err != nil {
		t.Fatalf("Check() error = %v", err)
	}
	if report.Integrity != "ok" || report.FTSIntegrity != "ok" ||
		report.FileRows != report.FTSRows || len(report.ForeignKeyErrors) != 0 {
		t.Fatalf("Check() after reuse scans = %#v", report)
	}

	// A full (non-skip) scan remains the exact-rebuild path and stays correct.
	if _, err := store.Scan(t.Context(), root, ScanOptions{}); err != nil {
		t.Fatalf("full Scan() error = %v", err)
	}
	if rows := mustSearchRows(t, store, "changed"); rows != 1 {
		t.Fatalf("full scan content rows = %d, want 1", rows)
	}
}

// TestScanSkipUnchangedModelSwitchForcesRebuild verifies that when the active
// embedder changes, SkipUnchanged does not silently keep old-model vectors.
func TestScanSkipUnchangedModelSwitchForcesRebuild(t *testing.T) {
	embedder := &countingEmbedder{inner: NewHashEmbedder(256)}
	store, root := newTestStore(t, Options{Embedder: embedder})
	file := filepath.Join(root, "doc.md")
	mustWrite(t, file, "unique needle content")
	forceMTime(t, file)
	if _, err := store.Scan(t.Context(), root, ScanOptions{SkipUnchanged: true}); err != nil {
		t.Fatalf("first Scan() error = %v", err)
	}
	firstCount := embedder.count()

	// Switch the embedder model and rescan: even untouched files must be
	// reprocessed because the stored vectors no longer match Model().
	embedder.inner = NewHashEmbedder(512)
	if _, err := store.Scan(t.Context(), root, ScanOptions{SkipUnchanged: true}); err != nil {
		t.Fatalf("model-switch Scan() error = %v", err)
	}
	if got := embedder.count(); got <= firstCount {
		t.Fatalf("model switch re-embedded %d calls, want more than first %d", got, firstCount)
	}
	rows, err := store.Query(t.Context(), "SELECT count(*) FROM embeddings WHERE model = ?", embedder.Model())
	if err != nil {
		t.Fatalf("Query() error = %v", err)
	}
	if rows.Rows[0][0] == nil || rows.Rows[0][0].(int64) == 0 {
		t.Fatalf("no embeddings stored under new model %q", embedder.Model())
	}
}

func mustSearchRows(t *testing.T, store *Store, phrase string) int {
	t.Helper()
	result, err := store.Search(t.Context(), phrase)
	if err != nil {
		t.Fatalf("Search(%q) error = %v", phrase, err)
	}
	return len(result.Rows)
}
