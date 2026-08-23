package agentfs

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"
)

type journalRecord struct {
	id        int64
	operation string
	state     string
	oldPath   string
	newPath   string
	stagePath string
}

func (s *Store) createJournal(ctx context.Context, operation, oldPath, newPath, stagePath string) (int64, error) {
	now := time.Now().UnixNano()
	result, err := s.db.ExecContext(ctx, `INSERT INTO operation_journal(
		operation,state,old_path,new_path,stage_path,created_at_ns,updated_at_ns
	) VALUES (?, 'prepared', ?, ?, ?, ?, ?)`, operation, oldPath, newPath, stagePath, now, now)
	if err != nil {
		return 0, fmt.Errorf("record %s intent: %w", operation, err)
	}
	id, err := result.LastInsertId()
	if err != nil {
		return 0, fmt.Errorf("read %s journal id: %w", operation, err)
	}
	return id, nil
}

func (s *Store) updateJournal(ctx context.Context, id int64, state string, operationErr error) error {
	lastError := ""
	if operationErr != nil {
		lastError = operationErr.Error()
	}
	_, err := s.db.ExecContext(ctx, `UPDATE operation_journal
		SET state=?, last_error=?, updated_at_ns=? WHERE id=?`, state, lastError, time.Now().UnixNano(), id)
	if err != nil {
		return fmt.Errorf("update operation journal %d to %s: %w", id, state, err)
	}
	return nil
}

// recoverOperations completes filesystem/index operations interrupted by a
// process or machine crash. Every transition is idempotent.
func (s *Store) recoverOperations(ctx context.Context) error {
	rows, err := s.db.QueryContext(ctx, `SELECT id,operation,state,old_path,new_path,stage_path
		FROM operation_journal WHERE state NOT IN ('done','rolled_back') ORDER BY id`)
	if err != nil {
		return fmt.Errorf("load recovery journal: %w", err)
	}
	records := make([]journalRecord, 0, 8)
	for rows.Next() {
		var record journalRecord
		if err := rows.Scan(&record.id, &record.operation, &record.state, &record.oldPath,
			&record.newPath, &record.stagePath); err != nil {
			_ = rows.Close()
			return fmt.Errorf("scan recovery journal: %w", err)
		}
		records = append(records, record)
	}
	if err := rows.Close(); err != nil {
		return fmt.Errorf("close recovery journal: %w", err)
	}
	for _, record := range records {
		var recoveryErr error
		switch record.operation {
		case "rename":
			recoveryErr = s.recoverRename(ctx, record)
		case "remove":
			recoveryErr = s.recoverRemove(ctx, record)
		default:
			recoveryErr = fmt.Errorf("unknown journal operation %q", record.operation)
		}
		if recoveryErr != nil {
			_ = s.updateJournal(ctx, record.id, record.state, recoveryErr)
			return fmt.Errorf("recover operation %d: %w", record.id, recoveryErr)
		}
	}
	return nil
}

func (s *Store) recoverRename(ctx context.Context, record journalRecord) error {
	oldExists, err := lstatExists(record.oldPath)
	if err != nil {
		return err
	}
	newExists, err := lstatExists(record.newPath)
	if err != nil {
		return err
	}
	oldIndexed, err := s.indexedPathExists(ctx, record.oldPath)
	if err != nil {
		return err
	}
	newIndexed, err := s.indexedPathExists(ctx, record.newPath)
	if err != nil {
		return err
	}
	switch {
	case newExists && !oldExists:
		if oldIndexed {
			if err := s.renameRows(ctx, record.oldPath, record.newPath, filepath.Base(record.newPath)); err != nil {
				return err
			}
		} else if !newIndexed {
			return errors.New("renamed filesystem path exists but neither indexed path exists")
		}
		return s.updateJournal(ctx, record.id, "done", nil)
	case oldExists && !newExists:
		if newIndexed && !oldIndexed {
			if err := s.renameRows(ctx, record.newPath, record.oldPath, filepath.Base(record.oldPath)); err != nil {
				return err
			}
		}
		return s.updateJournal(ctx, record.id, "rolled_back", nil)
	case oldExists && newExists:
		return errors.New("both rename source and destination exist")
	default:
		return errors.New("neither rename source nor destination exists")
	}
}

func (s *Store) recoverRemove(ctx context.Context, record journalRecord) error {
	if !validTombstone(record.oldPath, record.stagePath) {
		return fmt.Errorf("unsafe removal tombstone %q: %w", record.stagePath, ErrUnsafePath)
	}
	oldExists, err := lstatExists(record.oldPath)
	if err != nil {
		return err
	}
	stageExists, err := lstatExists(record.stagePath)
	if err != nil {
		return err
	}
	switch {
	case oldExists && !stageExists:
		return s.updateJournal(ctx, record.id, "rolled_back", nil)
	case !oldExists && stageExists:
		indexed, err := s.indexedPathExists(ctx, record.oldPath)
		if err != nil {
			return err
		}
		if indexed {
			if err := s.deleteRows(ctx, record.oldPath); err != nil {
				return err
			}
		}
		if err := os.RemoveAll(record.stagePath); err != nil {
			return fmt.Errorf("clean recovered tombstone: %w", err)
		}
		return s.updateJournal(ctx, record.id, "done", nil)
	case !oldExists && !stageExists:
		indexed, err := s.indexedPathExists(ctx, record.oldPath)
		if err != nil {
			return err
		}
		if indexed {
			if err := s.deleteRows(ctx, record.oldPath); err != nil {
				return err
			}
		}
		return s.updateJournal(ctx, record.id, "done", nil)
	default:
		return errors.New("both removal source and tombstone exist")
	}
}

func (s *Store) indexedPathExists(ctx context.Context, path string) (bool, error) {
	var exists int
	if err := s.db.QueryRowContext(ctx, "SELECT EXISTS(SELECT 1 FROM files WHERE path=?)", path).Scan(&exists); err != nil {
		return false, fmt.Errorf("inspect indexed path %s: %w", path, err)
	}
	return exists != 0, nil
}

func lstatExists(path string) (bool, error) {
	_, err := os.Lstat(path)
	if err == nil {
		return true, nil
	}
	if errors.Is(err, fs.ErrNotExist) {
		return false, nil
	}
	return false, fmt.Errorf("inspect recovery path %s: %w", path, err)
}

func validTombstone(oldPath, stagePath string) bool {
	return filepath.Dir(oldPath) == filepath.Dir(stagePath) &&
		strings.HasPrefix(filepath.Base(stagePath), ".agentfs-delete-") &&
		filepath.Base(stagePath) != ".agentfs-delete-"
}
