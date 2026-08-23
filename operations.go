package agentfs

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// Tag adds tag to an indexed path. The tags_text and FTS rows are updated by
// schema triggers in the same transaction.
func (s *Store) Tag(ctx context.Context, path, tag string) error {
	return s.changeTag(ctx, path, tag, true)
}

// Untag removes tag from an indexed path.
func (s *Store) Untag(ctx context.Context, path, tag string) error {
	return s.changeTag(ctx, path, tag, false)
}

func (s *Store) changeTag(ctx context.Context, path, tag string, add bool) error {
	if err := s.checkOpen(); err != nil {
		return err
	}
	path, err := normalizePath(path)
	if err != nil {
		return fmt.Errorf("tag path: %w", err)
	}
	tag = strings.TrimSpace(tag)
	if tag == "" {
		return fmt.Errorf("tag path: empty tag: %w", os.ErrInvalid)
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin tag transaction: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	id, err := indexedID(ctx, tx, path)
	if err != nil {
		return err
	}
	if add {
		_, err = tx.ExecContext(ctx, "INSERT OR IGNORE INTO tags(file_id, tag) VALUES (?, ?)", id, tag)
	} else {
		_, err = tx.ExecContext(ctx, "DELETE FROM tags WHERE file_id = ? AND tag = ?", id, tag)
	}
	if err != nil {
		return fmt.Errorf("change tag on %s: %w", path, err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit tag change: %w", err)
	}
	return nil
}

// Rename atomically renames the real path, then updates the indexed path and
// every indexed descendant in one database transaction. If the database step
// fails, Rename attempts to restore the original filesystem name and returns
// both errors if compensation also fails.
func (s *Store) Rename(ctx context.Context, path, newName string) (newPath string, err error) {
	if err := s.checkOpen(); err != nil {
		return "", err
	}
	path, err = normalizePath(path)
	if err != nil {
		return "", fmt.Errorf("rename path: %w", err)
	}
	if err := s.validateMutablePath(path); err != nil {
		return "", err
	}
	newName = strings.TrimSpace(newName)
	if newName == "" || newName == "." || newName == ".." || filepath.Base(newName) != newName {
		return "", fmt.Errorf("rename %s: invalid new name %q: %w", path, newName, os.ErrInvalid)
	}
	if _, err := s.indexedKind(ctx, path); err != nil {
		return "", err
	}
	newPath = filepath.Join(filepath.Dir(path), newName)
	if _, statErr := os.Lstat(newPath); statErr == nil {
		return "", fmt.Errorf("rename %s to %s: %w", path, newPath, fs.ErrExist)
	} else if !errors.Is(statErr, fs.ErrNotExist) {
		return "", fmt.Errorf("check rename destination %s: %w", newPath, statErr)
	}
	journalID, err := s.createJournal(ctx, "rename", path, newPath, "")
	if err != nil {
		return "", err
	}
	if err := os.Rename(path, newPath); err != nil {
		_ = s.updateJournal(ctx, journalID, "rolled_back", err)
		return "", fmt.Errorf("rename filesystem path: %w", err)
	}
	if err := s.updateJournal(ctx, journalID, "fs_applied", nil); err != nil {
		if restoreErr := os.Rename(newPath, path); restoreErr != nil {
			return "", errors.Join(err, restoreErr)
		}
		_ = s.updateJournal(ctx, journalID, "rolled_back", err)
		return "", err
	}

	if err := s.renameRows(ctx, path, newPath, newName); err != nil {
		if restoreErr := os.Rename(newPath, path); restoreErr != nil {
			_ = s.updateJournal(ctx, journalID, "fs_applied", errors.Join(err, restoreErr))
			return "", errors.Join(err, fmt.Errorf("restore filesystem rename: %w", restoreErr))
		}
		_ = s.updateJournal(ctx, journalID, "rolled_back", err)
		return "", err
	}
	if err := s.updateJournal(ctx, journalID, "db_applied", nil); err != nil {
		return "", err
	}
	if err := s.updateJournal(ctx, journalID, "done", nil); err != nil {
		return "", err
	}
	return newPath, nil
}

func (s *Store) renameRows(ctx context.Context, oldPath, newPath, newName string) (err error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin rename transaction: %w", err)
	}
	defer func() {
		if rollbackErr := tx.Rollback(); rollbackErr != nil && !errors.Is(rollbackErr, sql.ErrTxDone) {
			err = errors.Join(err, fmt.Errorf("rollback rename: %w", rollbackErr))
		}
	}()
	oldPrefix := oldPath + string(os.PathSeparator)
	result, err := tx.ExecContext(ctx, `
		UPDATE files
		SET path = ? || substr(path, length(?) + 1),
		    name = CASE WHEN path = ? THEN ? ELSE name END,
		    scan_root = CASE
		      WHEN scan_root = ? OR substr(scan_root, 1, length(?)) = ?
		      THEN ? || substr(scan_root, length(?) + 1)
		      ELSE scan_root
		    END
		WHERE path = ? OR substr(path, 1, length(?)) = ?`,
		newPath, oldPath, oldPath, newName,
		oldPath, oldPrefix, oldPrefix, newPath, oldPath,
		oldPath, oldPrefix, oldPrefix)
	if err != nil {
		return fmt.Errorf("update renamed subtree: %w", err)
	}
	changed, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("count renamed rows: %w", err)
	}
	if changed == 0 {
		return fmt.Errorf("rename %s: %w", oldPath, ErrNotIndexed)
	}
	if _, err := tx.ExecContext(ctx, `
		UPDATE scan_roots
		SET path = ? || substr(path, length(?) + 1)
		WHERE path = ? OR substr(path, 1, length(?)) = ?`,
		newPath, oldPath, oldPath, oldPrefix, oldPrefix); err != nil {
		return fmt.Errorf("update renamed scan root: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit rename: %w", err)
	}
	return nil
}

// Remove removes an indexed path. The path is first atomically renamed to a
// hidden sibling, the database transaction is committed, then the hidden path
// is unlinked. A pre-commit failure restores the original name.
func (s *Store) Remove(ctx context.Context, path string, recursive bool) (err error) {
	if err := s.checkOpen(); err != nil {
		return err
	}
	path, err = normalizePath(path)
	if err != nil {
		return fmt.Errorf("remove path: %w", err)
	}
	if err := s.validateMutablePath(path); err != nil {
		return err
	}
	kind, err := s.indexedKind(ctx, path)
	if err != nil {
		return err
	}
	if kind == "dir" && !recursive {
		empty, err := directoryEmpty(path)
		if err != nil {
			return fmt.Errorf("inspect directory %s: %w", path, err)
		}
		if !empty {
			return fmt.Errorf("remove non-empty directory %s without --recursive: %w", path, fs.ErrInvalid)
		}
	}
	stage, err := tombstonePath(path)
	if err != nil {
		return fmt.Errorf("create deletion tombstone: %w", err)
	}
	journalID, err := s.createJournal(ctx, "remove", path, "", stage)
	if err != nil {
		return err
	}
	if err := os.Rename(path, stage); err != nil {
		_ = s.updateJournal(ctx, journalID, "rolled_back", err)
		return fmt.Errorf("stage filesystem removal: %w", err)
	}
	if err := s.updateJournal(ctx, journalID, "fs_applied", nil); err != nil {
		if restoreErr := os.Rename(stage, path); restoreErr != nil {
			return errors.Join(err, restoreErr)
		}
		_ = s.updateJournal(ctx, journalID, "rolled_back", err)
		return err
	}

	if err := s.deleteRows(ctx, path); err != nil {
		if restoreErr := os.Rename(stage, path); restoreErr != nil {
			_ = s.updateJournal(ctx, journalID, "fs_applied", errors.Join(err, restoreErr))
			return errors.Join(err, fmt.Errorf("restore staged removal: %w", restoreErr))
		}
		_ = s.updateJournal(ctx, journalID, "rolled_back", err)
		return err
	}
	if err := s.updateJournal(ctx, journalID, "db_applied", nil); err != nil {
		return err
	}
	if err := os.RemoveAll(stage); err != nil {
		_ = s.updateJournal(ctx, journalID, "db_applied", err)
		return fmt.Errorf("clean committed removal tombstone %s: %w", stage, err)
	}
	if err := s.updateJournal(ctx, journalID, "done", nil); err != nil {
		return err
	}
	return nil
}

func (s *Store) deleteRows(ctx context.Context, path string) (err error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin remove transaction: %w", err)
	}
	defer func() {
		if rollbackErr := tx.Rollback(); rollbackErr != nil && !errors.Is(rollbackErr, sql.ErrTxDone) {
			err = errors.Join(err, fmt.Errorf("rollback remove: %w", rollbackErr))
		}
	}()
	id, err := indexedID(ctx, tx, path)
	if err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, "DELETE FROM files WHERE id = ?", id); err != nil {
		return fmt.Errorf("delete indexed subtree %s: %w", path, err)
	}
	prefix := path + string(os.PathSeparator)
	if _, err := tx.ExecContext(ctx,
		"DELETE FROM scan_roots WHERE path = ? OR substr(path, 1, length(?)) = ?", path, prefix, prefix); err != nil {
		return fmt.Errorf("delete scan roots under %s: %w", path, err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit remove: %w", err)
	}
	return nil
}

func (s *Store) validateMutablePath(path string) error {
	if filepath.Dir(path) == path || isWithin(s.path, path) {
		return fmt.Errorf("mutate %s: %w", path, ErrUnsafePath)
	}
	return nil
}

func (s *Store) indexedKind(ctx context.Context, path string) (string, error) {
	var kind string
	if err := s.db.QueryRowContext(ctx, "SELECT kind FROM files WHERE path = ?", path).Scan(&kind); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return "", fmt.Errorf("lookup %s: %w", path, ErrNotIndexed)
		}
		return "", fmt.Errorf("lookup %s: %w", path, err)
	}
	return kind, nil
}

type rowQuerier interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

func indexedID(ctx context.Context, queryer rowQuerier, path string) (int64, error) {
	var id int64
	if err := queryer.QueryRowContext(ctx, "SELECT id FROM files WHERE path = ?", path).Scan(&id); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return 0, fmt.Errorf("lookup %s: %w", path, ErrNotIndexed)
		}
		return 0, fmt.Errorf("lookup %s: %w", path, err)
	}
	return id, nil
}

func directoryEmpty(path string) (bool, error) {
	directory, err := os.Open(path)
	if err != nil {
		return false, err
	}
	_, readErr := directory.Readdirnames(1)
	closeErr := directory.Close()
	switch {
	case errors.Is(readErr, io.EOF):
		return true, closeErr
	case readErr != nil:
		return false, errors.Join(readErr, closeErr)
	default:
		return false, closeErr
	}
}

func tombstonePath(path string) (string, error) {
	random := make([]byte, 8)
	if _, err := rand.Read(random); err != nil {
		return "", err
	}
	name := ".agentfs-delete-" + hex.EncodeToString(random) + "-" + filepath.Base(path)
	stage := filepath.Join(filepath.Dir(path), name)
	if _, err := os.Lstat(stage); err == nil {
		return "", fs.ErrExist
	} else if !errors.Is(err, fs.ErrNotExist) {
		return "", err
	}
	return stage, nil
}
