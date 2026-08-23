package agentfs

import (
	"context"
	"database/sql"
	_ "embed"
	"errors"
	"fmt"
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
	excludePatterns []string
	includeNames    map[string]struct{}
	includePatterns []string
	allFiles        bool
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
		excludePatterns: excludePatterns,
		includeNames:    includeNames,
		includePatterns: includePatterns,
		allFiles:        opts.AllFiles,
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
	if version != 0 && version != 1 {
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
	if _, err := s.db.ExecContext(ctx, schemaSQL); err != nil {
		return fmt.Errorf("apply schema: %w", err)
	}
	return nil
}

// Close releases the index. It is safe to call more than once.
func (s *Store) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil
	}
	s.closed = true
	if err := s.db.Close(); err != nil {
		return fmt.Errorf("close index: %w", err)
	}
	return nil
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
