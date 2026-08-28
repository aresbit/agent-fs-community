package agentfs

import (
	"context"
	"database/sql"
	_ "embed"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"

	_ "modernc.org/sqlite"
)

//go:embed schema.sql
var schemaSQL string

// Store owns the SQLite semantic index.
//
// Representation invariant (RI): path is absolute and non-empty; db is a live
// single-connection SQLite handle while closed is false; contentBytes and
// maxRows are positive. The abstraction function maps Store to a transactional
// index whose rows describe filesystem paths and whose FTS rows share files.id.
type Store struct {
	mu              sync.RWMutex
	db              *sql.DB
	path            string
	contentBytes    int
	extractBytes    int
	maxRows         int
	embedder        Embedder
	reranker        Reranker
	excludePatterns []string
	includeNames    map[string]struct{}
	includePatterns []string
	allFiles        bool
	conceptFusion   bool
	closed          bool
}

// Open creates or opens the index at path and applies all schema migrations.
//
// Precondition: path is non-empty and denotes a database file, not a directory.
// Modifies: creates path and its parent directory when absent; initializes the
// schema and SQLite journal settings.
// Postcondition: the returned Store satisfies its RI and is ready for queries.
func Open(ctx context.Context, path string, opts Options) (*Store, error) {
	path, err := normalizePath(path)
	if err != nil {
		return nil, fmt.Errorf("open index: %w", err)
	}
	if opts.ContentBytes < 0 || opts.MaxRows < 0 {
		return nil, fmt.Errorf("open index: negative option: %w", os.ErrInvalid)
	}
	if opts.ContentBytes == 0 {
		opts.ContentBytes = defaultContentBytes
	}
	if opts.ExtractBytes < 0 {
		return nil, fmt.Errorf("open index: negative extract bytes: %w", os.ErrInvalid)
	}
	if opts.ExtractBytes == 0 {
		opts.ExtractBytes = defaultExtractBytes
	}
	if opts.MaxRows == 0 {
		opts.MaxRows = defaultMaxRows
	}
	excludePatterns, err := buildExcludePatterns(opts.ExcludePatterns, opts.NoDefaultExcludes)
	if err != nil {
		return nil, fmt.Errorf("open index: %w", err)
	}
	includeNames, includePatterns, err := buildIncludePatterns(opts.IncludePatterns)
	if err != nil {
		return nil, fmt.Errorf("open index: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, fmt.Errorf("create index directory: %w", err)
	}

	db, err := sql.Open("sqlite", writableDSN(path))
	if err != nil {
		return nil, fmt.Errorf("open sqlite: %w", err)
	}
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)

	store := &Store{
		db:              db,
		path:            path,
		contentBytes:    opts.ContentBytes,
		extractBytes:    opts.ExtractBytes,
		maxRows:         opts.MaxRows,
		embedder:        opts.Embedder,
		reranker:        opts.Reranker,
		excludePatterns: excludePatterns,
		includeNames:    includeNames,
		includePatterns: includePatterns,
		allFiles:        opts.AllFiles,
		conceptFusion:   opts.ConceptFusion,
	}
	if store.embedder == nil {
		store.embedder = NewHashEmbedder(256)
	}
	if err := store.initialize(ctx); err != nil {
		if closeErr := db.Close(); closeErr != nil {
			err = errors.Join(err, fmt.Errorf("close failed index: %w", closeErr))
		}
		return nil, err
	}
	if err := store.recoverOperations(ctx); err != nil {
		if closeErr := db.Close(); closeErr != nil {
			err = errors.Join(err, closeErr)
		}
		return nil, fmt.Errorf("recover interrupted operations: %w", err)
	}
	if err := os.Chmod(path, 0o600); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("secure index permissions: %w", err)
	}
	return store, nil
}

func (s *Store) initialize(ctx context.Context) error {
	statements := []string{
		"PRAGMA journal_mode=WAL",
		"PRAGMA synchronous=NORMAL",
		"PRAGMA foreign_keys=ON",
		"PRAGMA busy_timeout=5000",
	}
	for _, statement := range statements {
		if _, err := s.db.ExecContext(ctx, statement); err != nil {
			return fmt.Errorf("initialize sqlite: %w", err)
		}
	}
	var version int
	if err := s.db.QueryRowContext(ctx, "PRAGMA user_version").Scan(&version); err != nil {
		return fmt.Errorf("read schema version: %w", err)
	}
	if version > 3 {
		return fmt.Errorf("community database version %d is incompatible; use a new --db path: %w",
			version, ErrIncompatibleSchema)
	}
	var hasFiles, hasScanRoot int
	if err := s.db.QueryRowContext(ctx,
		"SELECT EXISTS(SELECT 1 FROM sqlite_schema WHERE type='table' AND name='files')").Scan(&hasFiles); err != nil {
		return fmt.Errorf("inspect schema: %w", err)
	}
	if hasFiles != 0 {
		var permissionColumns int
		if err := s.db.QueryRowContext(ctx, `SELECT count(*) FROM pragma_table_info('files')
			WHERE name IN ('mode','uid','gid')`).Scan(&permissionColumns); err != nil {
			return fmt.Errorf("inspect community file columns: %w", err)
		}
		if permissionColumns != 0 {
			return fmt.Errorf("permission-aware database detected; community edition requires a new --db path: %w",
				ErrIncompatibleSchema)
		}
		if err := s.db.QueryRowContext(ctx,
			"SELECT EXISTS(SELECT 1 FROM pragma_table_info('files') WHERE name='scan_root')").Scan(&hasScanRoot); err != nil {
			return fmt.Errorf("inspect files schema: %w", err)
		}
		if hasScanRoot == 0 {
			return fmt.Errorf("legacy Python index detected; choose a new --db path and rescan: %w", ErrIncompatibleSchema)
		}
	}
	// v2 的结构准备必须跑在 schemaSQL 之前：schemaSQL 里的 FTS 表是
	// CREATE VIRTUAL TABLE IF NOT EXISTS，旧表还在就不会被换成新列集。
	migratedToV2 := hasFiles != 0 && version < 2
	if migratedToV2 {
		if err := s.migrateV2Prepare(ctx); err != nil {
			return err
		}
		// search_text 的补算同样要在 schemaSQL 之前：schemaSQL 会重建 *_au 触发器，
		// 而补算用 UPDATE 写 search_text，会触发 *_au 对刚建好的空 FTS 索引发
		// 'delete' 命令——外部内容索引对不存在的 rowid 发 delete 会报 267 损坏。
		// migrateV2Prepare 已把触发器删掉，这里只改基表；索引等 FTS 重建后再整体 rebuild。
		if err := s.backfillSearchText(ctx); err != nil {
			return err
		}
	}
	if _, err := s.db.ExecContext(ctx, schemaSQL); err != nil {
		return fmt.Errorf("apply schema: %w", err)
	}
	if migratedToV2 {
		if err := s.rebuildSearchText(ctx); err != nil {
			return err
		}
	}
	// v3 概念图：三张全新表由 schemaSQL 建出，这里只对迁移前已存在的 chunk 做
	// backfill。必须在 schemaSQL 之后（concepts 表已存在）才能跑。
	if hasFiles != 0 && version < 3 {
		if err := s.backfillConcepts(ctx); err != nil {
			return err
		}
	}
	return nil
}

// migrateV2Prepare 为 schema v2 做结构准备：给 files/chunks 补 search_text 列，并
// 丢弃旧的 FTS 影子表与触发器，让紧随其后的 schema.sql 用新的列集重建它们。
//
// 安全性：这里只删派生数据。files_fts/chunks_fts 是 external-content 索引，内容
// 全部能从 files/chunks 重算，删掉不丢任何事实数据；基表只做 ADD COLUMN，可加不可
// 减。最坏情况是迁移中断，下次 Open 重跑一遍——ADD COLUMN 有存在性检查，DROP 都带
// IF EXISTS，整个过程可重入。
//
// Modifies: files.search_text、chunks.search_text 两列的存在性；files_fts /
// chunks_fts 及其触发器的存在性。
func (s *Store) migrateV2Prepare(ctx context.Context) (err error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin schema v2 migration: %w", err)
	}
	defer func() {
		if rollbackErr := tx.Rollback(); rollbackErr != nil && !errors.Is(rollbackErr, sql.ErrTxDone) {
			err = errors.Join(err, rollbackErr)
		}
	}()
	for _, table := range []string{"files", "chunks"} {
		var exists int
		if err := tx.QueryRowContext(ctx,
			"SELECT EXISTS(SELECT 1 FROM sqlite_schema WHERE type='table' AND name=?)", table).Scan(&exists); err != nil {
			return fmt.Errorf("inspect %s for v2 migration: %w", table, err)
		}
		if exists == 0 {
			// 更早的库可能还没有 chunks 表；schemaSQL 会带着新列直接建出来。
			continue
		}
		var hasColumn int
		if err := tx.QueryRowContext(ctx,
			"SELECT EXISTS(SELECT 1 FROM pragma_table_info(?) WHERE name='search_text')", table).Scan(&hasColumn); err != nil {
			return fmt.Errorf("inspect %s columns for v2 migration: %w", table, err)
		}
		if hasColumn != 0 {
			continue
		}
		if _, err := tx.ExecContext(ctx,
			"ALTER TABLE "+table+" ADD COLUMN search_text TEXT NOT NULL DEFAULT ''"); err != nil {
			return fmt.Errorf("add %s.search_text: %w", table, err)
		}
	}
	for _, statement := range []string{
		"DROP TRIGGER IF EXISTS files_ai", "DROP TRIGGER IF EXISTS files_ad", "DROP TRIGGER IF EXISTS files_au",
		"DROP TRIGGER IF EXISTS chunks_ai", "DROP TRIGGER IF EXISTS chunks_ad", "DROP TRIGGER IF EXISTS chunks_au",
		"DROP TABLE IF EXISTS files_fts", "DROP TABLE IF EXISTS chunks_fts",
	} {
		if _, err := tx.ExecContext(ctx, statement); err != nil {
			return fmt.Errorf("drop stale full-text index: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit schema v2 migration: %w", err)
	}
	return nil
}

// backfillSearchText 为迁移前就存在的行补算 search_text。
//
// 必须在 schemaSQL 重建 *_au 触发器之前调用：补算用 UPDATE 写 search_text，若此时
// 触发器还在，UPDATE 会对刚建好的空 FTS 索引发 'delete' 命令，而外部内容索引对
// 不存在的 rowid 发 delete 会报 267（SQLITE_CORRUPT_VTAB）。migrateV2Prepare 已经把
// 触发器删掉，所以这里的 UPDATE 只改基表；索引由随后的 rebuildSearchText 整体重建。
//
// 这一步不能省：不补算的话，旧数据的 search_text 全是空串，中文检索会静默地一条都
// 查不到，而且没有任何错误提示——比直接报错更难排查。
func (s *Store) backfillSearchText(ctx context.Context) error {
	if err := s.backfillSegments(ctx, "files",
		"COALESCE(name,'')||' '||COALESCE(path,'')||' '||COALESCE(content_head,'')"); err != nil {
		return err
	}
	if err := s.backfillSegments(ctx, "chunks",
		"COALESCE(symbol,'')||' '||COALESCE(content,'')"); err != nil {
		return err
	}
	return nil
}

// rebuildSearchText 在 schemaSQL 重建 FTS 表和触发器之后，从基表整体重算 FTS 索引。
func (s *Store) rebuildSearchText(ctx context.Context) error {
	for _, table := range []string{"files_fts", "chunks_fts"} {
		if _, err := s.db.ExecContext(ctx,
			"INSERT INTO "+table+"("+table+") VALUES ('rebuild')"); err != nil {
			return fmt.Errorf("rebuild %s: %w", table, err)
		}
	}
	return nil
}

// backfillSegments 分批读出 source 表达式的文本，在 Go 里做 CJK 切分，写回
// search_text。
//
// 分批是必需的：一次把百万行读进内存会 OOM，一个大事务会让 WAL 无限膨胀。
// 循环不变量：id <= lastID 的行都已写回。变式：剩余未处理行数每轮严格减少，
// 因为每轮要么处理了 batchSize 行并推进 lastID，要么读到空批直接返回。
func (s *Store) backfillSegments(ctx context.Context, table, source string) error {
	const batchSize = 500
	var lastID int64
	for {
		rows, err := s.db.QueryContext(ctx,
			"SELECT id, "+source+" FROM "+table+" WHERE id > ? ORDER BY id LIMIT ?", lastID, batchSize)
		if err != nil {
			return fmt.Errorf("read %s for segmentation: %w", table, err)
		}
		updates := make([]searchTextUpdate, 0, batchSize)
		for rows.Next() {
			var id int64
			var text string
			if err := rows.Scan(&id, &text); err != nil {
				_ = rows.Close()
				return fmt.Errorf("scan %s row for segmentation: %w", table, err)
			}
			updates = append(updates, searchTextUpdate{id: id, segment: segmentCJKIndex(text)})
			lastID = id
		}
		if err := errors.Join(rows.Err(), rows.Close()); err != nil {
			return fmt.Errorf("iterate %s for segmentation: %w", table, err)
		}
		if len(updates) == 0 {
			return nil
		}
		if err := s.commitSegments(ctx, table, updates); err != nil {
			return err
		}
	}
}

// searchTextUpdate 是一行待写回的分词结果。
type searchTextUpdate struct {
	id      int64
	segment string
}

func (s *Store) commitSegments(ctx context.Context, table string, updates []searchTextUpdate) (err error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin %s segmentation batch: %w", table, err)
	}
	defer func() {
		if rollbackErr := tx.Rollback(); rollbackErr != nil && !errors.Is(rollbackErr, sql.ErrTxDone) {
			err = errors.Join(err, rollbackErr)
		}
	}()
	for _, update := range updates {
		if _, err := tx.ExecContext(ctx,
			"UPDATE "+table+" SET search_text=? WHERE id=?", update.segment, update.id); err != nil {
			return fmt.Errorf("write %s search text: %w", table, err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit %s segmentation batch: %w", table, err)
	}
	return nil
}

// chunkConceptRow 是概念图 backfill 的一行：一个待提取概念的 chunk。
type chunkConceptRow struct {
	id      int64
	content string
}

// backfillConcepts 为 v3 迁移前已存在的 chunk 提取概念并建图。分批处理，避免
// 一次把全部 chunk 读进内存。模式与 backfillSegments 一致：只读不改现有列，
// 概念图三张表从 chunk.content 全量重算。
func (s *Store) backfillConcepts(ctx context.Context) error {
	const batchSize = 200
	var lastID int64
	for {
		rows, err := s.db.QueryContext(ctx,
			"SELECT id, content FROM chunks WHERE id > ? ORDER BY id LIMIT ?", lastID, batchSize)
		if err != nil {
			return fmt.Errorf("read chunks for concept backfill: %w", err)
		}
		items := make([]chunkConceptRow, 0, batchSize)
		for rows.Next() {
			var r chunkConceptRow
			if err := rows.Scan(&r.id, &r.content); err != nil {
				_ = rows.Close()
				return fmt.Errorf("scan chunk for concept backfill: %w", err)
			}
			items = append(items, r)
			lastID = r.id
		}
		if err := errors.Join(rows.Err(), rows.Close()); err != nil {
			return fmt.Errorf("iterate chunks for concept backfill: %w", err)
		}
		if len(items) == 0 {
			return nil
		}
		if err := s.commitConcepts(ctx, items); err != nil {
			return err
		}
	}
}

func (s *Store) commitConcepts(ctx context.Context, items []chunkConceptRow) (err error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin concept backfill batch: %w", err)
	}
	defer func() {
		if rollbackErr := tx.Rollback(); rollbackErr != nil && !errors.Is(rollbackErr, sql.ErrTxDone) {
			err = errors.Join(err, rollbackErr)
		}
	}()
	for _, item := range items {
		if err := indexConcepts(ctx, tx, item.id, extractConcepts(item.content)); err != nil {
			return fmt.Errorf("index concepts for chunk %d: %w", item.id, err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit concept backfill batch: %w", err)
	}
	return nil
}

// Close releases the index and every model the Store owns. It is safe to call
// more than once, and safe to pair with a caller that also defers Close on the
// embedder it passed in (both Close paths are idempotent).
func (s *Store) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil
	}
	s.closed = true
	var errs []error
	if err := s.db.Close(); err != nil {
		errs = append(errs, fmt.Errorf("close index: %w", err))
	}
	// 顺序有意义：reranker 与 ONNX embedder 共用同一个进程级 ONNX 环境，而
	// embedder 的 Close 会 DestroyEnvironment。先释放 reranker 的 session，否则它
	// 会在一个已经销毁的环境上释放句柄。
	if closer, ok := s.reranker.(io.Closer); ok {
		if err := closer.Close(); err != nil {
			errs = append(errs, fmt.Errorf("close reranker: %w", err))
		}
	}
	// Store 是 embedder 的最后持有者，否则本地 ONNX 模型（session + 约 90MB 权重）
	// 会一直泄漏到进程退出。
	if closer, ok := s.embedder.(io.Closer); ok {
		if err := closer.Close(); err != nil {
			errs = append(errs, fmt.Errorf("close embedder: %w", err))
		}
	}
	return errors.Join(errs...)
}

// Path returns the absolute index path.
func (s *Store) Path() string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.path
}

func (s *Store) checkOpen() error {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.closed {
		return ErrClosed
	}
	return nil
}

func (s *Store) readOnlyDB() (*sql.DB, error) {
	if err := s.checkOpen(); err != nil {
		return nil, err
	}
	db, err := sql.Open("sqlite", readOnlyDSN(s.path))
	if err != nil {
		return nil, fmt.Errorf("open read-only index: %w", err)
	}
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	return db, nil
}

func writableDSN(path string) string {
	u := &url.URL{Scheme: "file", Path: path}
	query := u.Query()
	query.Add("_pragma", "busy_timeout(5000)")
	query.Add("_pragma", "foreign_keys(1)")
	u.RawQuery = query.Encode()
	return u.String()
}

func readOnlyDSN(path string) string {
	u := &url.URL{Scheme: "file", Path: path}
	query := u.Query()
	query.Set("mode", "ro")
	query.Add("_pragma", "query_only(1)")
	query.Add("_pragma", "busy_timeout(5000)")
	u.RawQuery = query.Encode()
	return u.String()
}

func normalizePath(path string) (string, error) {
	if strings.TrimSpace(path) == "" {
		return "", os.ErrInvalid
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	return filepath.Clean(abs), nil
}

func isWithin(path, root string) bool {
	rel, err := filepath.Rel(root, path)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(os.PathSeparator))
}
