package agentfs

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"time"
)

// SyncPath incrementally reconciles one path (or a newly-created directory
// subtree) without walking the rest of root.
func (s *Store) SyncPath(ctx context.Context, root, path string) (err error) {
	return s.SyncPaths(ctx, root, []string{path})
}

// SyncPaths reconciles a debounce batch in one SQLite transaction. A burst of
// thousands of filesystem events must not turn into thousands of WAL commits.
func (s *Store) SyncPaths(ctx context.Context, root string, paths []string) (err error) {
	if err := s.checkOpen(); err != nil {
		return err
	}
	root, err = normalizePath(root)
	if err != nil {
		return fmt.Errorf("sync root: %w", err)
	}
	seen := make(map[string]struct{}, len(paths))
	normalized := make([]string, 0, len(paths))
	excluded := make([]string, 0)
	for _, requestedPath := range paths {
		path, pathErr := normalizePath(requestedPath)
		if pathErr != nil {
			return fmt.Errorf("sync path: %w", pathErr)
		}
		if !isWithin(path, root) || s.isIndexArtifact(path) {
			return ErrUnsafePath
		}
		if _, ok := seen[path]; ok {
			continue
		}
		seen[path] = struct{}{}
		if s.isExcludedWithin(root, path) {
			excluded = append(excluded, path)
			continue
		}
		normalized = append(normalized, path)
	}
	type syncResult struct {
		path    string
		entries []scannedEntry
		missing bool
		err     error
	}
	jobs := make(chan string, len(normalized))
	results := make(chan syncResult, len(normalized))
	var workers sync.WaitGroup
	workerCount := min(len(normalized), min(16, max(1, runtime.GOMAXPROCS(0)*2)))
	for range workerCount {
		workers.Go(func() {
			for path := range jobs {
				info, statErr := os.Lstat(path)
				if errors.Is(statErr, fs.ErrNotExist) {
					results <- syncResult{path: path, missing: true}
					continue
				}
				if statErr != nil {
					results <- syncResult{path: path, err: fmt.Errorf("stat changed path %s: %w", path, statErr)}
					continue
				}
				if !info.IsDir() && !s.isIncludedFileName(filepath.Base(path)) {
					results <- syncResult{path: path, missing: true}
					continue
				}
				var found []scannedEntry
				var collectErr error
				if info.IsDir() {
					found, collectErr = s.collect(ctx, path)
				} else {
					var entry scannedEntry
					entry, collectErr = s.describePath(ctx, path, info)
					found = []scannedEntry{entry}
				}
				if errors.Is(collectErr, fs.ErrNotExist) {
					results <- syncResult{path: path, missing: true}
				} else {
					results <- syncResult{path: path, entries: found, err: collectErr}
				}
			}
		})
	}
	for _, path := range normalized {
		jobs <- path
	}
	close(jobs)
	workers.Wait()
	close(results)
	entriesByPath := make(map[string]scannedEntry, len(paths))
	missingSet := make(map[string]struct{})
	for _, path := range excluded {
		missingSet[path] = struct{}{}
	}
	for result := range results {
		if result.err != nil {
			return result.err
		}
		if result.missing {
			missingSet[result.path] = struct{}{}
			continue
		}
		for _, entry := range result.entries {
			entriesByPath[entry.path] = entry
		}
	}
	entries := make([]scannedEntry, 0, len(entriesByPath))
	for _, entry := range entriesByPath {
		entries = append(entries, entry)
	}
	slices.SortFunc(entries, func(left, right scannedEntry) int {
		return strings.Compare(left.path, right.path)
	})
	missing := make([]string, 0, len(missingSet))
	for path := range missingSet {
		missing = append(missing, path)
	}
	slices.Sort(missing)
	if len(entries) == 0 && len(missing) == 0 {
		return nil
	}
	return s.commitIncrementalBatch(ctx, root, entries, missing)
}

func (s *Store) commitIncrementalBatch(ctx context.Context, root string, entries []scannedEntry,
	missing []string) (err error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin incremental transaction: %w", err)
	}
	defer func() {
		if rollbackErr := tx.Rollback(); rollbackErr != nil && !errors.Is(rollbackErr, sql.ErrTxDone) {
			err = errors.Join(err, rollbackErr)
		}
	}()
	indexedAt := time.Now().UnixNano()
	if err := deleteMissingPaths(ctx, tx, missing); err != nil {
		return err
	}
	parentSet := make(map[string]struct{}, len(entries))
	for _, entry := range entries {
		parentPath := filepath.Dir(entry.path)
		if parentPath != entry.path {
			parentSet[parentPath] = struct{}{}
		}
	}
	parentPaths := make([]string, 0, len(parentSet))
	for path := range parentSet {
		parentPaths = append(parentPaths, path)
	}
	parentIDs, err := loadPathIDs(ctx, tx, parentPaths)
	if err != nil {
		return err
	}
	ids := make(map[string]int64, len(entries))
	files := make([]scannedEntry, 0, len(entries))
	for _, entry := range entries {
		if entry.kind != "dir" {
			files = append(files, entry)
			continue
		}
		parentID := incrementalParentID(entry.path, ids, parentIDs)
		var id int64
		var returnedPath string
		if err := tx.QueryRowContext(ctx, incrementalFileUpsertSQL("(?,?,?,?,?,?,?,?,?,?,?,?,?)"),
			parentID, root, entry.name, entry.path, entry.kind, entry.size,
			entry.mtimeNS, entry.linkTarget,
			entry.contentHead, entry.contentHash, entry.mime, entry.searchText, indexedAt).Scan(&id, &returnedPath); err != nil {
			return fmt.Errorf("incrementally upsert %s: %w", entry.path, err)
		}
		if returnedPath != entry.path {
			return fmt.Errorf("incremental directory upsert returned path %s for %s", returnedPath, entry.path)
		}
		ids[entry.path] = id
	}
	const fileBatchSize = 200
	for start := 0; start < len(files); start += fileBatchSize {
		end := min(start+fileBatchSize, len(files))
		arguments := make([]any, 0, (end-start)*incrementalFileColumns)
		for _, entry := range files[start:end] {
			arguments = append(arguments, incrementalParentID(entry.path, ids, parentIDs), root,
				entry.name, entry.path, entry.kind, entry.size, entry.mtimeNS, entry.linkTarget,
				entry.contentHead, entry.contentHash, entry.mime, entry.searchText, indexedAt)
		}
		rows, err := tx.QueryContext(ctx,
			incrementalFileUpsertSQL(valuesClause(end-start, incrementalFileColumns)), arguments...)
		if err != nil {
			return fmt.Errorf("incrementally upsert file batch: %w", err)
		}
		for rows.Next() {
			var id int64
			var path string
			if err := rows.Scan(&id, &path); err != nil {
				_ = rows.Close()
				return fmt.Errorf("read incremental file id: %w", err)
			}
			ids[path] = id
		}
		if err := rows.Err(); err != nil {
			_ = rows.Close()
			return fmt.Errorf("iterate incremental file ids: %w", err)
		}
		if err := rows.Close(); err != nil {
			return fmt.Errorf("close incremental file ids: %w", err)
		}
	}
	if len(ids) != len(entries) {
		return fmt.Errorf("incremental upsert returned %d ids for %d entries", len(ids), len(entries))
	}
	if err := s.writeIncrementalRelations(ctx, tx, entries, ids, indexedAt); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit incremental sync: %w", err)
	}
	return nil
}

func incrementalParentID(path string, inserted, indexed map[string]int64) any {
	parentPath := filepath.Dir(path)
	if parentPath == path {
		return nil
	}
	if id, ok := inserted[parentPath]; ok {
		return id
	}
	if id, ok := indexed[parentPath]; ok {
		return id
	}
	return nil
}

// incrementalFileColumns 是 incrementalFileUpsertSQL 绑定的列数。改列表必须同步改
// 这个常量和所有 valuesClause 调用——数错一个占位符，报错会出现在完全无关的地方。
const incrementalFileColumns = 13

func incrementalFileUpsertSQL(values string) string {
	return `INSERT INTO files(parent_id,scan_root,name,path,kind,size,
		mtime_ns,link_target,content_head,content_hash,mime,search_text,indexed_at_ns) VALUES ` + values + `
		ON CONFLICT(path) DO UPDATE SET parent_id=excluded.parent_id,
		scan_root=excluded.scan_root,name=excluded.name,kind=excluded.kind,
		size=excluded.size,
		mtime_ns=excluded.mtime_ns,link_target=excluded.link_target,
		content_head=excluded.content_head,content_hash=excluded.content_hash,
		mime=excluded.mime,search_text=excluded.search_text,
		indexed_at_ns=excluded.indexed_at_ns RETURNING id,path`
}

func loadPathIDs(ctx context.Context, tx *sql.Tx, paths []string) (map[string]int64, error) {
	ids := make(map[string]int64, len(paths))
	const batchSize = 500
	for start := 0; start < len(paths); start += batchSize {
		end := min(start+batchSize, len(paths))
		arguments := make([]any, end-start)
		for index, path := range paths[start:end] {
			arguments[index] = path
		}
		rows, err := tx.QueryContext(ctx, `SELECT path,id FROM files WHERE path IN (`+
			strings.TrimSuffix(strings.Repeat("?,", end-start), ",")+`)`, arguments...)
		if err != nil {
			return nil, fmt.Errorf("load parent path ids: %w", err)
		}
		for rows.Next() {
			var path string
			var id int64
			if err := rows.Scan(&path, &id); err != nil {
				_ = rows.Close()
				return nil, fmt.Errorf("read parent path id: %w", err)
			}
			ids[path] = id
		}
		if err := rows.Err(); err != nil {
			_ = rows.Close()
			return nil, fmt.Errorf("iterate parent path ids: %w", err)
		}
		if err := rows.Close(); err != nil {
			return nil, fmt.Errorf("close parent path ids: %w", err)
		}
	}
	return ids, nil
}

func (s *Store) writeIncrementalRelations(ctx context.Context, tx *sql.Tx,
	entries []scannedEntry, ids map[string]int64, indexedAt int64) error {
	const relationBatchSize = 250
	embedded := make([]scannedEntry, 0, len(entries))
	fileIDs := make([]int64, 0, len(entries))
	for _, entry := range entries {
		fileIDs = append(fileIDs, ids[entry.path])
		if len(entry.vector) > 0 {
			embedded = append(embedded, entry)
		}
	}
	for start := 0; start < len(embedded); start += relationBatchSize {
		end := min(start+relationBatchSize, len(embedded))
		arguments := make([]any, 0, (end-start)*7)
		for _, entry := range embedded[start:end] {
			arguments = append(arguments, ids[entry.path], s.embedder.Model(), len(entry.vector),
				vectorBucket(entry.vector), encodeVector(entry.vector), entry.contentHash, indexedAt)
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO embeddings(
			file_id,model,dimensions,bucket,vector,content_hash,updated_at_ns) VALUES `+
			valuesClause(end-start, 7)+` ON CONFLICT(file_id) DO UPDATE SET
			model=excluded.model,dimensions=excluded.dimensions,bucket=excluded.bucket,
			vector=excluded.vector,content_hash=excluded.content_hash,updated_at_ns=excluded.updated_at_ns`,
			arguments...); err != nil {
			return fmt.Errorf("update embedding batch: %w", err)
		}
	}
	for start := 0; start < len(fileIDs); start += 500 {
		end := min(start+500, len(fileIDs))
		arguments := make([]any, end-start)
		for index, id := range fileIDs[start:end] {
			arguments[index] = id
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM chunks WHERE file_id IN (`+
			strings.TrimSuffix(strings.Repeat("?,", end-start), ",")+`)`, arguments...); err != nil {
			return fmt.Errorf("delete changed chunks: %w", err)
		}
	}
	type fileChunk struct {
		fileID int64
		chunk  parsedChunk
	}
	chunks := make([]fileChunk, 0)
	for _, entry := range entries {
		for _, chunk := range entry.chunks {
			chunks = append(chunks, fileChunk{fileID: ids[entry.path], chunk: chunk})
		}
	}
	type chunkVector struct {
		id    int64
		chunk parsedChunk
	}
	vectors := make([]chunkVector, 0, len(chunks))
	for start := 0; start < len(chunks); start += relationBatchSize {
		end := min(start+relationBatchSize, len(chunks))
		arguments := make([]any, 0, (end-start)*9)
		for _, item := range chunks[start:end] {
			chunk := item.chunk
			arguments = append(arguments, item.fileID, chunk.ordinal, chunk.language, chunk.symbol,
				chunk.start, chunk.end, chunk.content, chunk.hash, chunk.searchText)
		}
		rows, err := tx.QueryContext(ctx, `INSERT INTO chunks(
			file_id,ordinal,language,symbol,start_line,end_line,content,content_hash,search_text) VALUES `+
			valuesClause(end-start, 9)+` RETURNING id,file_id,ordinal`, arguments...)
		if err != nil {
			return fmt.Errorf("insert changed chunk batch: %w", err)
		}
		chunkByKey := make(map[[2]int64]parsedChunk, end-start)
		for _, item := range chunks[start:end] {
			chunkByKey[[2]int64{item.fileID, int64(item.chunk.ordinal)}] = item.chunk
		}
		for rows.Next() {
			var id, fileID int64
			var ordinal int
			if err := rows.Scan(&id, &fileID, &ordinal); err != nil {
				_ = rows.Close()
				return fmt.Errorf("read changed chunk id: %w", err)
			}
			chunk, ok := chunkByKey[[2]int64{fileID, int64(ordinal)}]
			if !ok {
				_ = rows.Close()
				return fmt.Errorf("inserted chunk id has unknown file=%d ordinal=%d", fileID, ordinal)
			}
			if len(chunk.vector) > 0 {
				vectors = append(vectors, chunkVector{id: id, chunk: chunk})
			}
		}
		if err := rows.Err(); err != nil {
			_ = rows.Close()
			return fmt.Errorf("iterate changed chunk ids: %w", err)
		}
		if err := rows.Close(); err != nil {
			return fmt.Errorf("close changed chunk ids: %w", err)
		}
	}
	for start := 0; start < len(vectors); start += relationBatchSize {
		end := min(start+relationBatchSize, len(vectors))
		arguments := make([]any, 0, (end-start)*7)
		for _, item := range vectors[start:end] {
			chunk := item.chunk
			arguments = append(arguments, item.id, s.embedder.Model(), len(chunk.vector),
				vectorBucket(chunk.vector), encodeVector(chunk.vector), chunk.hash, indexedAt)
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO chunk_embeddings(
			chunk_id,model,dimensions,bucket,vector,content_hash,updated_at_ns) VALUES `+
			valuesClause(end-start, 7), arguments...); err != nil {
			return fmt.Errorf("insert changed chunk embedding batch: %w", err)
		}
	}
	// 符号图：增量更新符号定义与调用引用
	for _, entry := range entries {
		if err := s.replaceSymbols(ctx, tx, ids[entry.path], entry.symbols, entry.refs); err != nil {
			return err
		}
	}
	return nil
}

func valuesClause(rows, columns int) string {
	row := "(" + strings.TrimSuffix(strings.Repeat("?,", columns), ",") + ")"
	return strings.TrimSuffix(strings.Repeat(row+",", rows), ",")
}

func deleteMissingPaths(ctx context.Context, tx *sql.Tx, missing []string) error {
	// Bound host parameters for SQLite builds with a conservative variable
	// limit, while still collapsing hundreds of file removals into three SQL
	// statements per batch.
	const batchSize = 500
	for start := 0; start < len(missing); start += batchSize {
		end := min(start+batchSize, len(missing))
		batch := missing[start:end]
		placeholders := strings.TrimSuffix(strings.Repeat("?,", len(batch)), ",")
		arguments := make([]any, len(batch))
		for index, path := range batch {
			arguments[index] = path
		}
		rows, err := tx.QueryContext(ctx, `SELECT path,kind FROM files WHERE path IN (`+placeholders+`)`, arguments...)
		if err != nil {
			return fmt.Errorf("inspect missing path batch: %w", err)
		}
		direct := make([]string, 0, len(batch))
		directories := make([]string, 0)
		for rows.Next() {
			var path, kind string
			if err := rows.Scan(&path, &kind); err != nil {
				_ = rows.Close()
				return fmt.Errorf("read missing path kind: %w", err)
			}
			if kind == "dir" {
				directories = append(directories, path)
			} else {
				direct = append(direct, path)
			}
		}
		if err := rows.Close(); err != nil {
			return fmt.Errorf("close missing path kinds: %w", err)
		}
		// Paths absent from the index need no delete. Keeping only rows found by
		// the lookup also minimizes write-lock work.
		if len(direct) > 0 {
			directPlaceholders := strings.TrimSuffix(strings.Repeat("?,", len(direct)), ",")
			directArguments := make([]any, 0, len(direct))
			for _, path := range direct {
				directArguments = append(directArguments, path)
			}
			if _, err := tx.ExecContext(ctx, `DELETE FROM files WHERE path IN (`+directPlaceholders+`)
				`, directArguments...); err != nil {
				return fmt.Errorf("remove stale file batch: %w", err)
			}
		}
		for _, path := range directories {
			prefix := path + string(os.PathSeparator)
			if _, err := tx.ExecContext(ctx, `DELETE FROM files
				WHERE (path = ? OR substr(path, 1, length(?)) = ?)`,
				path, prefix, prefix); err != nil {
				return fmt.Errorf("remove stale directory %s: %w", path, err)
			}
		}
	}
	return nil
}

func (s *Store) forgetPath(ctx context.Context, root, path string) (err error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin incremental removal: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	prefix := path + string(os.PathSeparator)
	if _, err := tx.ExecContext(ctx, `DELETE FROM files
		WHERE (path = ? OR substr(path, 1, length(?)) = ?)`,
		path, prefix, prefix); err != nil {
		return fmt.Errorf("remove stale path %s: %w", path, err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit incremental removal: %w", err)
	}
	return nil
}

func cleanEventPath(path string) string {
	return filepath.Clean(strings.TrimSpace(path))
}
