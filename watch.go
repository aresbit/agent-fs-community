package agentfs

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"time"

	"github.com/fsnotify/fsnotify"
)

type WatchOptions struct {
	Root        string
	Debounce    time.Duration
	SkipInitial bool
	Errors      chan<- error
	OnSynced    func(path string, freshness time.Duration)
	Ready       chan<- struct{}
}

// Watch performs a recursive initial scan and then incrementally applies
// coalesced filesystem notifications until ctx is canceled.
func (s *Store) Watch(ctx context.Context, opts WatchOptions) error {
	root, err := normalizePath(opts.Root)
	if err != nil {
		return fmt.Errorf("watch root: %w", err)
	}
	if opts.Debounce <= 0 {
		opts.Debounce = 100 * time.Millisecond
	}
	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		return fmt.Errorf("create filesystem watcher: %w", err)
	}
	defer watcher.Close()
	if err := s.addWatchTree(watcher, root); err != nil {
		return err
	}

	ticker := time.NewTicker(max(25*time.Millisecond, opts.Debounce/2))
	defer ticker.Stop()
	pending := make(map[string]time.Time)
	initializing := !opts.SkipInitial
	initialDone := make(chan error, 1)
	if initializing {
		go func() {
			_, err := s.Scan(ctx, root, ScanOptions{})
			if err != nil {
				err = fmt.Errorf("initial watch scan: %w", err)
			}
			initialDone <- err
		}()
	} else {
		signalWatchReady(opts.Ready)
	}
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case event, ok := <-watcher.Events:
			if !ok {
				return nil
			}
			if event.Op&(fsnotify.Create|fsnotify.Write|fsnotify.Remove|fsnotify.Rename|fsnotify.Chmod) != 0 {
				path := cleanEventPath(event.Name)
				if isWithin(path, root) && !s.isIndexArtifact(path) && !s.isExcludedWithin(root, path) &&
					s.shouldQueueWatchEvent(path, event.Op) {
					pending[path] = time.Now()
				}
			}
		case watchErr, ok := <-watcher.Errors:
			if !ok {
				return nil
			}
			s.reportWatchError(opts.Errors, watchErr)
		case initialErr := <-initialDone:
			if initialErr != nil {
				return initialErr
			}
			initializing = false
			signalWatchReady(opts.Ready)
		case now := <-ticker.C:
			if initializing {
				continue
			}
			mature := make([]string, 0, len(pending))
			observedAt := make(map[string]time.Time, len(pending))
			for path, observed := range pending {
				if now.Sub(observed) < opts.Debounce {
					continue
				}
				delete(pending, path)
				mature = append(mature, path)
				observedAt[path] = observed
			}
			if len(mature) == 0 {
				continue
			}
			if err := s.SyncPaths(ctx, root, mature); err != nil && !errors.Is(err, fs.ErrNotExist) {
				s.reportWatchError(opts.Errors, fmt.Errorf("sync %d event paths: %w", len(mature), err))
				// Retry the batch after another debounce interval. Transient races
				// such as an editor replacing a file must not permanently stale it.
				for _, path := range mature {
					pending[path] = now
				}
				continue
			}
			for _, path := range mature {
				if info, err := os.Lstat(path); err == nil && info.IsDir() {
					if err := s.addWatchTree(watcher, path); err != nil {
						s.reportWatchError(opts.Errors, err)
					}
				}
				if opts.OnSynced != nil {
					opts.OnSynced(path, time.Since(observedAt[path]))
				}
			}
		}
	}
}

func (s *Store) shouldQueueWatchEvent(path string, operation fsnotify.Op) bool {
	if s.isIncludedFileName(filepath.Base(path)) || operation&(fsnotify.Remove|fsnotify.Rename) != 0 {
		return true
	}
	info, err := os.Lstat(path)
	return err == nil && info.IsDir()
}

func signalWatchReady(destination chan<- struct{}) {
	if destination == nil {
		return
	}
	select {
	case destination <- struct{}{}:
	default:
	}
}

func (s *Store) addWatchTree(watcher *fsnotify.Watcher, root string) error {
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if path != root && s.isExcludedName(entry.Name()) {
			if entry.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if s.isIndexArtifact(path) {
			if entry.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if entry.IsDir() {
			if err := watcher.Add(path); err != nil {
				return fmt.Errorf("watch directory %s: %w", path, err)
			}
		}
		return nil
	})
	if err != nil {
		return fmt.Errorf("walk watch root %s: %w", root, err)
	}
	return nil
}

func (s *Store) reportWatchError(destination chan<- error, err error) {
	if destination == nil {
		return
	}
	select {
	case destination <- err:
	default:
	}
}
