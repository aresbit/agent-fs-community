package agentfs

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"strings"
)

// Query executes one bounded, read-only SQL query against the semantic index.
// The database is opened with both SQLite mode=ro and PRAGMA query_only=ON; the
// lexical check is an early diagnostic, not the security boundary.
//
// Precondition: statement starts with SELECT, WITH, EXPLAIN, or PRAGMA after
// whitespace/comments. Modifies: none. Postcondition: at most maxRows rows are
// returned and Truncated reports whether at least one additional row existed.
func (s *Store) Query(ctx context.Context, statement string, args ...any) (result QueryResult, err error) {
	if !isReadOnlyStatement(statement) {
		return QueryResult{}, ErrInvalidQuery
	}
	db, err := s.readOnlyDB()
	if err != nil {
		return QueryResult{}, err
	}
	defer func() {
		if closeErr := db.Close(); closeErr != nil {
			err = errors.Join(err, fmt.Errorf("close read-only index: %w", closeErr))
		}
	}()

	rows, err := db.QueryContext(ctx, statement, args...)
	if err != nil {
		return QueryResult{}, fmt.Errorf("query index: %w", err)
	}
	defer func() {
		if closeErr := rows.Close(); closeErr != nil {
			err = errors.Join(err, fmt.Errorf("close query rows: %w", closeErr))
		}
	}()
	columns, err := rows.Columns()
	if err != nil {
		return QueryResult{}, fmt.Errorf("query columns: %w", err)
	}
	result.Columns = columns
	result.Rows = make([][]any, 0, min(s.maxRows, 64))

	// Invariant: result.Rows contains exactly the scanned prefix, with each row
	// owning its byte values. Variant: maxRows-len(result.Rows) decreases until
	// the bound is reached or the finite SQLite result is exhausted.
	for rows.Next() {
		if len(result.Rows) == s.maxRows {
			result.Truncated = true
			break
		}
		values := make([]any, len(columns))
		destinations := make([]any, len(columns))
		for index := range destinations {
			destinations[index] = &values[index]
		}
		if err := rows.Scan(destinations...); err != nil {
			return QueryResult{}, fmt.Errorf("scan query row: %w", err)
		}
		for index, value := range values {
			if raw, ok := value.([]byte); ok {
				values[index] = bytes.Clone(raw)
			}
		}
		result.Rows = append(result.Rows, values)
	}
	if err := rows.Err(); err != nil {
		return QueryResult{}, fmt.Errorf("iterate query rows: %w", err)
	}
	return result, nil
}

// List returns direct children of path.
func (s *Store) List(ctx context.Context, path string) (QueryResult, error) {
	path, err := normalizePath(path)
	if err != nil {
		return QueryResult{}, fmt.Errorf("list path: %w", err)
	}
	return s.Query(ctx, `
		SELECT name, kind, size, mtime_ns, path
		FROM files
		WHERE parent_id = (SELECT id FROM files WHERE path = ?)
		ORDER BY kind, name`, path)
}

// Search performs an FTS5 phrase search over names, paths, tags, and previews.
func (s *Store) Search(ctx context.Context, phrase string) (QueryResult, error) {
	phrase = strings.TrimSpace(phrase)
	if phrase == "" {
		return QueryResult{}, fmt.Errorf("search: empty phrase: %w", os.ErrInvalid)
	}
	// 走与 HybridSearch 相同的编译器，中文才能命中 search_text 里的 bigram。
	// 原来的做法是把整句话当一个 FTS5 短语，对中文等于要求整段连写完全一致。
	match := ftsMatch(phrase)
	if match == "" {
		return QueryResult{}, fmt.Errorf("search: no searchable terms in %q: %w", phrase, os.ErrInvalid)
	}
	return s.Query(ctx, `
		SELECT f.path, f.kind, f.size, f.mtime_ns,
		       snippet(files_fts, 3, '[', ']', '…', 24) AS snippet,
		       bm25(files_fts) AS rank
		FROM files_fts
		JOIN files AS f ON f.id = files_fts.rowid
		WHERE files_fts MATCH ?
		ORDER BY rank, f.path`, match)
}

// Big returns regular files larger than minBytes.
func (s *Store) Big(ctx context.Context, minBytes int64) (QueryResult, error) {
	if minBytes < 0 {
		return QueryResult{}, fmt.Errorf("big files: negative size: %w", os.ErrInvalid)
	}
	return s.Query(ctx, `
		SELECT path, size, mtime_ns
		FROM files
		WHERE kind = 'file' AND size > ?
		ORDER BY size DESC, path`, minBytes)
}

// DiskUsage aggregates the indexed subtree rooted at path.
func (s *Store) DiskUsage(ctx context.Context, path string) (QueryResult, error) {
	path, err := normalizePath(path)
	if err != nil {
		return QueryResult{}, fmt.Errorf("disk usage path: %w", err)
	}
	return s.Query(ctx, `
		WITH RECURSIVE subtree(id) AS (
			SELECT id FROM files WHERE path = ?
			UNION ALL
			SELECT f.id FROM files AS f JOIN subtree AS s ON f.parent_id = s.id
		)
		SELECT COUNT(*) AS entries,
		       COALESCE(SUM(CASE WHEN kind = 'file' THEN size ELSE 0 END), 0) AS total_bytes
		FROM files
		WHERE id IN (SELECT id FROM subtree)`, path)
}

// ByTag returns indexed paths carrying tag.
func (s *Store) ByTag(ctx context.Context, tag string) (QueryResult, error) {
	tag = strings.TrimSpace(tag)
	if tag == "" {
		return QueryResult{}, fmt.Errorf("query tag: empty tag: %w", os.ErrInvalid)
	}
	return s.Query(ctx, `
		SELECT f.path, f.kind, f.size, f.mtime_ns
		FROM files AS f
		JOIN tags AS t ON t.file_id = f.id
		WHERE t.tag = ?
		ORDER BY f.size DESC, f.path`, tag)
}

func isReadOnlyStatement(statement string) bool {
	if !isSingleStatement(statement) {
		return false
	}
	rest := strings.TrimSpace(statement)
	for {
		switch {
		case strings.HasPrefix(rest, "--"):
			newline := strings.IndexByte(rest, '\n')
			if newline < 0 {
				return false
			}
			rest = strings.TrimSpace(rest[newline+1:])
		case strings.HasPrefix(rest, "/*"):
			end := strings.Index(rest[2:], "*/")
			if end < 0 {
				return false
			}
			rest = strings.TrimSpace(rest[end+4:])
		default:
			fields := strings.Fields(rest)
			if len(fields) == 0 {
				return false
			}
			switch strings.ToUpper(fields[0]) {
			case "SELECT", "WITH", "EXPLAIN":
				return true
			case "PRAGMA":
				return isSafePragma(rest)
			default:
				return false
			}
		}
	}
}

func isSingleStatement(statement string) bool {
	const (
		sqlNormal = iota
		sqlSingleQuote
		sqlDoubleQuote
		sqlBacktick
		sqlBracket
		sqlLineComment
		sqlBlockComment
	)
	state := sqlNormal
	for index := 0; index < len(statement); index++ {
		char := statement[index]
		switch state {
		case sqlNormal:
			switch {
			case char == '\'':
				state = sqlSingleQuote
			case char == '"':
				state = sqlDoubleQuote
			case char == '`':
				state = sqlBacktick
			case char == '[':
				state = sqlBracket
			case char == '-' && index+1 < len(statement) && statement[index+1] == '-':
				state = sqlLineComment
				index++
			case char == '/' && index+1 < len(statement) && statement[index+1] == '*':
				state = sqlBlockComment
				index++
			case char == ';':
				return onlySQLTrivia(statement[index+1:])
			}
		case sqlSingleQuote, sqlDoubleQuote, sqlBacktick:
			quote := byte('\'')
			if state == sqlDoubleQuote {
				quote = '"'
			} else if state == sqlBacktick {
				quote = '`'
			}
			if char == quote {
				if index+1 < len(statement) && statement[index+1] == quote {
					index++
				} else {
					state = sqlNormal
				}
			}
		case sqlBracket:
			if char == ']' {
				state = sqlNormal
			}
		case sqlLineComment:
			if char == '\n' {
				state = sqlNormal
			}
		case sqlBlockComment:
			if char == '*' && index+1 < len(statement) && statement[index+1] == '/' {
				state = sqlNormal
				index++
			}
		}
	}
	return true
}

func onlySQLTrivia(statement string) bool {
	rest := strings.TrimSpace(statement)
	for rest != "" {
		switch {
		case strings.HasPrefix(rest, "--"):
			newline := strings.IndexByte(rest, '\n')
			if newline < 0 {
				return true
			}
			rest = strings.TrimSpace(rest[newline+1:])
		case strings.HasPrefix(rest, "/*"):
			end := strings.Index(rest[2:], "*/")
			if end < 0 {
				return false
			}
			rest = strings.TrimSpace(rest[end+4:])
		default:
			return false
		}
	}
	return true
}

func isSafePragma(statement string) bool {
	rest := strings.TrimSpace(statement[len("PRAGMA"):])
	if strings.Contains(rest, "=") {
		return false
	}
	name := rest
	if open := strings.IndexAny(name, "( ;\t\r\n"); open >= 0 {
		name = name[:open]
	}
	if dot := strings.LastIndexByte(name, '.'); dot >= 0 {
		name = name[dot+1:]
	}
	switch strings.ToLower(name) {
	case "compile_options", "database_list", "foreign_key_check", "foreign_key_list",
		"index_info", "index_list", "index_xinfo", "integrity_check", "journal_mode",
		"quick_check", "table_info", "table_list", "table_xinfo", "user_version":
		return true
	default:
		return false
	}
}

// RebuildFTS reconstructs both external-content full-text indexes.
//
// search_text 也会一并重算：它是 Go 侧分词的产物，SQLite 自己算不出来，而 'rebuild'
// 只会照着基表现有的列值重建倒排索引。只 rebuild 不重算分词，等于把旧的分词结果
// 又原样索引一遍。
func (s *Store) RebuildFTS(ctx context.Context) error {
	if err := s.checkOpen(); err != nil {
		return err
	}
	if err := s.backfillSearchText(ctx); err != nil {
		return err
	}
	return s.rebuildSearchText(ctx)
}

// Check verifies SQLite integrity, foreign keys, FTS cardinality, and indexed
// path existence. It does not mutate the index.
func (s *Store) Check(ctx context.Context) (report CheckReport, err error) {
	if err := s.checkOpen(); err != nil {
		return CheckReport{}, err
	}
	// For an external-content FTS5 table, rank=1 makes integrity-check compare
	// the inverted-index checksum with a fresh tokenization of files.
	if _, err := s.db.ExecContext(ctx,
		"INSERT INTO files_fts(files_fts, rank) VALUES ('integrity-check', 1)"); err != nil {
		return CheckReport{}, fmt.Errorf("full-text integrity check: %w", err)
	}
	report.FTSIntegrity = "ok"
	db, err := s.readOnlyDB()
	if err != nil {
		return CheckReport{}, err
	}
	defer func() {
		if closeErr := db.Close(); closeErr != nil {
			err = errors.Join(err, fmt.Errorf("close checked index: %w", closeErr))
		}
	}()
	if err := db.QueryRowContext(ctx, "PRAGMA quick_check").Scan(&report.Integrity); err != nil {
		return CheckReport{}, fmt.Errorf("quick check: %w", err)
	}
	if err := db.QueryRowContext(ctx, "SELECT count(*) FROM files").Scan(&report.FileRows); err != nil {
		return CheckReport{}, fmt.Errorf("count files: %w", err)
	}
	if err := db.QueryRowContext(ctx, "SELECT count(*) FROM files_fts").Scan(&report.FTSRows); err != nil {
		return CheckReport{}, fmt.Errorf("count full-text rows: %w", err)
	}

	fkRows, err := db.QueryContext(ctx, "PRAGMA foreign_key_check")
	if err != nil {
		return CheckReport{}, fmt.Errorf("foreign key check: %w", err)
	}
	for fkRows.Next() {
		var table string
		var rowID sql.NullInt64
		var parent string
		var constraint int
		if err := fkRows.Scan(&table, &rowID, &parent, &constraint); err != nil {
			_ = fkRows.Close()
			return CheckReport{}, fmt.Errorf("scan foreign key error: %w", err)
		}
		report.ForeignKeyErrors = append(report.ForeignKeyErrors,
			fmt.Sprintf("table=%s rowid=%d parent=%s constraint=%d", table, rowID.Int64, parent, constraint))
	}
	if err := errors.Join(fkRows.Err(), fkRows.Close()); err != nil {
		return CheckReport{}, fmt.Errorf("iterate foreign key check: %w", err)
	}

	pathRows, err := db.QueryContext(ctx, "SELECT path FROM files ORDER BY path")
	if err != nil {
		return CheckReport{}, fmt.Errorf("list indexed paths: %w", err)
	}
	for pathRows.Next() && len(report.MissingPaths) < s.maxRows {
		var path string
		if err := pathRows.Scan(&path); err != nil {
			_ = pathRows.Close()
			return CheckReport{}, fmt.Errorf("scan indexed path: %w", err)
		}
		if _, err := os.Lstat(path); errors.Is(err, os.ErrNotExist) {
			report.MissingPaths = append(report.MissingPaths, path)
		} else if err != nil {
			_ = pathRows.Close()
			return CheckReport{}, fmt.Errorf("stat indexed path %s: %w", path, err)
		}
	}
	if err := errors.Join(pathRows.Err(), pathRows.Close()); err != nil {
		return CheckReport{}, fmt.Errorf("iterate indexed paths: %w", err)
	}
	return report, nil
}
