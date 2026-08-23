package agentfs

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"
	"unicode/utf8"
)

type scannedEntry struct {
	name        string
	path        string
	kind        string
	size        int64
	mtimeNS     int64
	linkTarget  string
	contentHead string
	contentHash string
	mime        string
	vector      []float32
	chunks      []parsedChunk
	symbols     []symbolDef
	refs        []symbolRef
}

// Scan replaces the indexed view of root in one SQLite transaction.
//
// Precondition: root exists and every visited path is readable enough to stat.
// Modifies: files, tags for requested root tags, FTS rows, and scan_roots.
// Postcondition: every path observed under root has exactly one files row; rows
// previously owned by root but no longer observed are absent; parent_id mirrors
// the observed directory tree. A failed scan leaves the prior index unchanged.
func (s *Store) Scan(ctx context.Context, root string, opts ScanOptions) (ScanResult, error) {
	if err := s.checkOpen(); err != nil {
		return ScanResult{}, err
	}
	root, err := normalizePath(root)
	if err != nil {
		return ScanResult{}, fmt.Errorf("scan root: %w", err)
	}
	if root == s.path {
		return ScanResult{}, ErrUnsafePath
	}
	started := time.Now()
	entries, err := s.collect(ctx, root)
	if err != nil {
		return ScanResult{}, err
	}
	if len(entries) == 0 {
		return ScanResult{}, fmt.Errorf("scan %s: root was excluded: %w", root, ErrUnsafePath)
	}
	if err := s.commitScan(ctx, root, entries, opts, started); err != nil {
		return ScanResult{}, err
	}
	return ScanResult{Root: root, Entries: len(entries), Duration: time.Since(started)}, nil
}

func (s *Store) collect(ctx context.Context, root string) ([]scannedEntry, error) {
	entries := make([]scannedEntry, 0, 256)
	err := filepath.WalkDir(root, func(path string, dirEntry fs.DirEntry, walkErr error) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if walkErr != nil {
			if errors.Is(walkErr, fs.ErrNotExist) {
				return nil
			}
			return walkErr
		}
		if path != root && s.isExcludedName(dirEntry.Name()) {
			if dirEntry.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if s.isIndexArtifact(path) {
			if dirEntry.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if !dirEntry.IsDir() && !s.isIncludedFileName(dirEntry.Name()) {
			return nil
		}
		info, err := dirEntry.Info()
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				return nil
			}
			return err
		}
		entry, err := s.describePath(ctx, path, info)
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				return nil
			}
			return err
		}
		entries = append(entries, entry)
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("walk %s: %w", root, err)
	}

	// Invariant: the prefix [0, i) is ordered by depth and then path.
	// Variant: len(entries)-i decreases as slices.SortFunc advances.
	slices.SortFunc(entries, func(left, right scannedEntry) int {
		leftDepth := strings.Count(left.path, string(os.PathSeparator))
		rightDepth := strings.Count(right.path, string(os.PathSeparator))
		if leftDepth != rightDepth {
			return leftDepth - rightDepth
		}
		return strings.Compare(left.path, right.path)
	})
	return entries, nil
}

func (s *Store) describePath(ctx context.Context, path string, info fs.FileInfo) (scannedEntry, error) {
	entry := scannedEntry{
		name:    filepath.Base(path),
		path:    filepath.Clean(path),
		size:    info.Size(),
		mtimeNS: info.ModTime().UnixNano(),
	}
	switch {
	case info.Mode()&os.ModeSymlink != 0:
		entry.kind = "symlink"
		target, err := os.Readlink(path)
		if err != nil {
			return scannedEntry{}, fmt.Errorf("read link %s: %w", path, err)
		}
		entry.linkTarget = target
	case info.IsDir():
		entry.kind = "dir"
		entry.size = 0
		entry.mime = "inode/directory"
	case info.Mode().IsRegular():
		entry.kind = "file"
		document, err := extractDocument(ctx, path, s.extractBytes)
		if err != nil {
			return scannedEntry{}, fmt.Errorf("extract %s: %w", path, err)
		}
		entry.contentHead = truncateUTF8(document.text, s.contentBytes)
		entry.contentHash = document.hash
		entry.mime = document.mime
		entry.chunks = document.chunks
		if extension := strings.ToLower(filepath.Ext(path)); tsLanguageForExtension(extension) != nil {
			entry.symbols, entry.refs = treeSitterSymbols(document.text, extension)
		}
	default:
		entry.kind = "other"
	}
	vector, err := s.embedder.Embed(ctx, entry.name+"\n"+entry.path+"\n"+entry.contentHead)
	if err != nil {
		return scannedEntry{}, fmt.Errorf("embed %s: %w", path, err)
	}
	entry.vector = vector
	if len(entry.chunks) > 0 {
		texts := make([]string, len(entry.chunks))
		for index := range entry.chunks {
			texts[index] = entry.chunks[index].symbol + "\n" + entry.chunks[index].content
		}
		vectors, err := embedTexts(ctx, s.embedder, texts)
		if err != nil {
			return scannedEntry{}, fmt.Errorf("embed chunks for %s: %w", path, err)
		}
		for index := range entry.chunks {
			entry.chunks[index].vector = vectors[index]
		}
	}
	return entry, nil
}

func truncateUTF8(text string, limit int) string {
	if len(text) <= limit {
		return text
	}
	text = text[:limit]
	for len(text) > 0 && !utf8.ValidString(text) {
		text = text[:len(text)-1]
	}
	return text
}

func (s *Store) isIndexArtifact(path string) bool {
	return path == s.path || path == s.path+"-wal" || path == s.path+"-shm" || path == s.path+"-journal"
}

func (s *Store) commitScan(ctx context.Context, root string, entries []scannedEntry, opts ScanOptions, started time.Time) (err error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin scan transaction: %w", err)
	}
	defer func() {
		if rollbackErr := tx.Rollback(); rollbackErr != nil && !errors.Is(rollbackErr, sql.ErrTxDone) {
			err = errors.Join(err, fmt.Errorf("rollback scan: %w", rollbackErr))
		}
	}()

	if _, err := tx.ExecContext(ctx, `CREATE TEMP TABLE IF NOT EXISTS agentfs_seen (
		path TEXT PRIMARY KEY
	) WITHOUT ROWID`); err != nil {
		return fmt.Errorf("create scan set: %w", err)
	}
	if _, err := tx.ExecContext(ctx, "DELETE FROM agentfs_seen"); err != nil {
		return fmt.Errorf("clear scan set: %w", err)
	}

	ids := make(map[string]int64, len(entries))
	indexedAt := time.Now().UnixNano()
	// Invariant: before each iteration, every prior entry is indexed, present in
	// agentfs_seen, and ids maps its path to the committed candidate row id.
	// Variant: len(entries)-processed is non-negative and decreases each step.
	for _, entry := range entries {
		var parentID any
		if entry.path == root && filepath.Dir(root) != root {
			var indexedParent int64
			err := tx.QueryRowContext(ctx, "SELECT id FROM files WHERE path = ?", filepath.Dir(root)).Scan(&indexedParent)
			if err == nil {
				parentID = indexedParent
			} else if !errors.Is(err, sql.ErrNoRows) {
				return fmt.Errorf("lookup indexed parent of %s: %w", root, err)
			}
		} else if entry.path != root {
			id, ok := ids[filepath.Dir(entry.path)]
			if !ok {
				return fmt.Errorf("index %s: parent was not scanned", entry.path)
			}
			parentID = id
		}
		var id int64
		err := tx.QueryRowContext(ctx, `
			INSERT INTO files(
				parent_id, scan_root, name, path, kind, size, mtime_ns,
				link_target, content_head, content_hash, mime, indexed_at_ns
			) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
			ON CONFLICT(path) DO UPDATE SET
				parent_id=excluded.parent_id,
				scan_root=excluded.scan_root,
				name=excluded.name,
				kind=excluded.kind,
				size=excluded.size,
				mtime_ns=excluded.mtime_ns,
				link_target=excluded.link_target,
				content_head=excluded.content_head,
				content_hash=excluded.content_hash,
				mime=excluded.mime,
				indexed_at_ns=excluded.indexed_at_ns
			RETURNING id`, parentID, root, entry.name, entry.path, entry.kind,
			entry.size, entry.mtimeNS, entry.linkTarget,
			entry.contentHead, entry.contentHash, entry.mime, indexedAt).Scan(&id)
		if err != nil {
			return fmt.Errorf("upsert %s: %w", entry.path, err)
		}
		ids[entry.path] = id
		if len(entry.vector) > 0 {
			if _, err := tx.ExecContext(ctx, `
				INSERT INTO embeddings(file_id, model, dimensions, bucket, vector, content_hash, updated_at_ns)
				VALUES (?, ?, ?, ?, ?, ?, ?)
				ON CONFLICT(file_id) DO UPDATE SET model=excluded.model,
				  dimensions=excluded.dimensions, bucket=excluded.bucket, vector=excluded.vector,
				  content_hash=excluded.content_hash, updated_at_ns=excluded.updated_at_ns`,
				id, s.embedder.Model(), len(entry.vector), vectorBucket(entry.vector),
				encodeVector(entry.vector), entry.contentHash, indexedAt); err != nil {
				return fmt.Errorf("store embedding for %s: %w", entry.path, err)
			}
		}
		if err := s.replaceChunks(ctx, tx, id, entry.chunks, indexedAt); err != nil {
			return fmt.Errorf("replace chunks for %s: %w", entry.path, err)
		}
		if err := s.replaceSymbols(ctx, tx, id, entry.symbols, entry.refs); err != nil {
			return fmt.Errorf("replace symbols for %s: %w", entry.path, err)
		}
		if _, err := tx.ExecContext(ctx, "INSERT INTO agentfs_seen(path) VALUES (?)", entry.path); err != nil {
			return fmt.Errorf("mark %s seen: %w", entry.path, err)
		}
	}

	rootPrefix := root
	if !strings.HasSuffix(rootPrefix, string(os.PathSeparator)) {
		rootPrefix += string(os.PathSeparator)
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM files
		WHERE (path = ? OR substr(path, 1, length(?)) = ?)
		  AND path NOT IN (SELECT path FROM agentfs_seen)`,
		root, rootPrefix, rootPrefix); err != nil {
		return fmt.Errorf("delete stale rows: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `
		DELETE FROM scan_roots
		WHERE path != ?
		  AND substr(path, 1, length(?)) = ?
		  AND path NOT IN (SELECT path FROM agentfs_seen)`, root, rootPrefix, rootPrefix); err != nil {
		return fmt.Errorf("delete stale nested scan roots: %w", err)
	}
	rootID := ids[root]
	for _, tag := range opts.Tags {
		tag = strings.TrimSpace(tag)
		if tag == "" {
			return fmt.Errorf("tag root: empty tag: %w", os.ErrInvalid)
		}
		if _, err := tx.ExecContext(ctx, "INSERT OR IGNORE INTO tags(file_id, tag) VALUES (?, ?)", rootID, tag); err != nil {
			return fmt.Errorf("tag root %s: %w", root, err)
		}
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO scan_roots(path, last_scan_ns, last_duration_ns, entry_count)
		VALUES (?, ?, ?, ?)
		ON CONFLICT(path) DO UPDATE SET
			last_scan_ns=excluded.last_scan_ns,
			last_duration_ns=excluded.last_duration_ns,
			entry_count=excluded.entry_count`,
		root, time.Now().UnixNano(), time.Since(started).Nanoseconds(), len(entries)); err != nil {
		return fmt.Errorf("record scan: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit scan: %w", err)
	}
	return nil
}

func (s *Store) replaceChunks(ctx context.Context, tx *sql.Tx, fileID int64, chunks []parsedChunk, indexedAt int64) error {
	if _, err := tx.ExecContext(ctx, "DELETE FROM chunks WHERE file_id=?", fileID); err != nil {
		return fmt.Errorf("delete old chunks: %w", err)
	}
	for _, chunk := range chunks {
		var chunkID int64
		if err := tx.QueryRowContext(ctx, `INSERT INTO chunks(
			file_id, ordinal, language, symbol, start_line, end_line, content, content_hash
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?) RETURNING id`, fileID, chunk.ordinal, chunk.language,
			chunk.symbol, chunk.start, chunk.end, chunk.content, chunk.hash).Scan(&chunkID); err != nil {
			return fmt.Errorf("insert chunk %d: %w", chunk.ordinal, err)
		}
		if len(chunk.vector) == 0 {
			continue
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO chunk_embeddings(
			chunk_id, model, dimensions, bucket, vector, content_hash, updated_at_ns
		) VALUES (?, ?, ?, ?, ?, ?, ?)`, chunkID, s.embedder.Model(), len(chunk.vector),
			vectorBucket(chunk.vector), encodeVector(chunk.vector), chunk.hash, indexedAt); err != nil {
			return fmt.Errorf("insert chunk embedding %d: %w", chunk.ordinal, err)
		}
	}
	return nil
}

// replaceSymbols 重写一个文件的符号定义与调用引用（符号图）。
func (s *Store) replaceSymbols(ctx context.Context, tx *sql.Tx, fileID int64, symbols []symbolDef, refs []symbolRef) error {
	if _, err := tx.ExecContext(ctx, "DELETE FROM symbols WHERE file_id=?", fileID); err != nil {
		return fmt.Errorf("delete symbols: %w", err)
	}
	if _, err := tx.ExecContext(ctx, "DELETE FROM symbol_refs WHERE file_id=?", fileID); err != nil {
		return fmt.Errorf("delete references: %w", err)
	}
	for _, sym := range symbols {
		if _, err := tx.ExecContext(ctx,
			"INSERT INTO symbols(file_id, symbol, kind, start_line, end_line) VALUES (?,?,?,?,?)",
			fileID, sym.symbol, sym.kind, sym.startLine, sym.endLine); err != nil {
			return fmt.Errorf("insert symbol %s: %w", sym.symbol, err)
		}
	}
	for _, ref := range refs {
		if _, err := tx.ExecContext(ctx,
			"INSERT INTO symbol_refs(file_id, caller_symbol, callee_symbol, line) VALUES (?,?,?,?)",
			fileID, ref.caller, ref.callee, ref.line); err != nil {
			return fmt.Errorf("insert reference %s: %w", ref.callee, err)
		}
	}
	return nil
}
