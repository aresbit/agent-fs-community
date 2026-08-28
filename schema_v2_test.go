package agentfs

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestSegmentCJKIndexOnlyEmitsCJK 验证补充索引列只装 CJK bigram：拉丁词已经被
// files_fts 的其余列正常索引，再复制一份只会让索引凭空变大一倍。
func TestSegmentCJKIndexOnlyEmitsCJK(t *testing.T) {
	t.Parallel()
	if got := segmentCJKIndex("package main\nfunc HybridSearch() {}"); got != "" {
		t.Fatalf("pure Latin text must produce an empty search column, got %q", got)
	}
	got := segmentCJKIndex("测量S参数的方法")
	// CJK 段是 [测量] 和 [参数的方法]，分别切成 bigram。
	for _, want := range []string{"测量", "参数", "数的", "的方", "方法"} {
		if !strings.Contains(got, want) {
			t.Fatalf("segmentCJKIndex = %q, missing %q", got, want)
		}
	}
	// bigram 之间必须有空格，否则 unicode61 又会把它们粘成一个 token。
	if !strings.Contains(got, " ") {
		t.Fatalf("bigrams must be space separated, got %q", got)
	}
	if strings.Contains(got, "s") {
		t.Fatalf("Latin runs must be skipped, got %q", got)
	}
}

// TestChineseInfixSearch 是这次 schema v2 的验收：一个中文词出现在正文中间时，
// 必须能被查到。unicode61 会把整段连写的汉字当成一个 token，所以在 v2 之前，
// 「参数」这种中缀词是查不出来的。
func TestChineseInfixSearch(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	files := map[string]string{
		"rf.md":    "本文讨论S参数的测量方法与校准流程。",
		"other.md": "数据库事务与并发控制的实现要点。",
	}
	for name, content := range files {
		if err := os.WriteFile(filepath.Join(root, name), []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	store, _ := newTestStore(t, Options{})
	if _, err := store.Scan(t.Context(), root, ScanOptions{}); err != nil {
		t.Fatal(err)
	}

	// 「参数」在正文中间，且左边紧邻一个拉丁字母——这正是原来查不到的形状。
	for _, query := range []string{"S参数", "S 参数", "参数", "测量方法"} {
		hits, err := store.HybridSearch(t.Context(), HybridRequest{Query: query, Limit: 5})
		if err != nil {
			t.Fatalf("search %q: %v", query, err)
		}
		if len(hits) == 0 {
			t.Fatalf("search %q returned nothing", query)
		}
		if filepath.Base(hits[0].Path) != "rf.md" {
			t.Errorf("search %q top hit = %s, want rf.md", query, filepath.Base(hits[0].Path))
		}
	}
}

// TestSearchTextIsIndexedAndQueryable 直接检查列被写进去了、也进了 FTS 倒排索引。
func TestSearchTextIsIndexedAndQueryable(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "doc.md"), []byte("阻抗匹配与S参数测量"), 0o600); err != nil {
		t.Fatal(err)
	}
	store, _ := newTestStore(t, Options{})
	if _, err := store.Scan(t.Context(), root, ScanOptions{}); err != nil {
		t.Fatal(err)
	}
	stored, err := store.Query(t.Context(), "SELECT search_text FROM files WHERE kind='file'")
	if err != nil {
		t.Fatalf("read search_text: %v", err)
	}
	if len(stored.Rows) == 0 {
		t.Fatal("no indexed file rows")
	}
	segment, _ := stored.Rows[0][0].(string)
	if !strings.Contains(segment, "参数") {
		t.Fatalf("files.search_text = %q, want it to contain the bigram 参数", segment)
	}
	// 中缀词必须能通过 FTS 反查回这一行。
	matched, err := store.Query(t.Context(),
		`SELECT count(*) FROM files_fts WHERE files_fts MATCH '"参数"'`)
	if err != nil {
		t.Fatalf("match search_text: %v", err)
	}
	if count, ok := matched.Rows[0][0].(int64); !ok || count == 0 {
		t.Fatalf("FTS did not match the infix bigram: %#v", matched.Rows[0][0])
	}
}

// TestMigrationFromV1BackfillsSearchText 验证升级路径：一个已经装了数据的 v1 库
// 被打开时，旧行要补上 search_text 并重建 FTS，而不是静默地查不到中文。
func TestMigrationFromV1BackfillsSearchText(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "legacy.md"), []byte("旧索引里的S参数测量记录"), 0o600); err != nil {
		t.Fatal(err)
	}
	dbPath := filepath.Join(t.TempDir(), "fs.db")

	store, err := Open(t.Context(), dbPath, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Scan(t.Context(), root, ScanOptions{}); err != nil {
		t.Fatal(err)
	}
	// 把库退回 v1 的形状：清空分词列，装回旧版本号。
	for _, statement := range []string{
		"UPDATE files SET search_text=''",
		"UPDATE chunks SET search_text=''",
		"PRAGMA user_version = 1",
	} {
		if _, err := store.db.ExecContext(t.Context(), statement); err != nil {
			t.Fatalf("downgrade %q: %v", statement, err)
		}
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	reopened, err := Open(t.Context(), dbPath, Options{})
	if err != nil {
		t.Fatalf("reopen must migrate, not fail: %v", err)
	}
	defer reopened.Close()

	var version int
	if err := reopened.db.QueryRowContext(t.Context(), "PRAGMA user_version").Scan(&version); err != nil {
		t.Fatal(err)
	}
	if version != 3 {
		t.Fatalf("user_version = %d, want 3", version)
	}
	stored, err := reopened.Query(t.Context(),
		"SELECT search_text FROM files WHERE kind='file' AND search_text != ''")
	if err != nil {
		t.Fatalf("read backfilled search_text: %v", err)
	}
	if len(stored.Rows) == 0 {
		t.Fatal("migration did not backfill search_text for pre-existing rows")
	}
	hits, err := reopened.HybridSearch(t.Context(), HybridRequest{Query: "参数", Limit: 5})
	if err != nil {
		t.Fatalf("search after migration: %v", err)
	}
	if len(hits) == 0 {
		t.Fatal("migrated index still cannot find a Chinese infix term")
	}
}
