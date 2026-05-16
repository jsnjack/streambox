package media

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"time"

	"github.com/fsnotify/fsnotify"

	"streambox/internal/loglevel"
)

const flattenStabilityInterval = 3 * time.Second

// dirProfile snapshots the contents of a directory for stability checking.
type dirProfile struct {
	entries map[string]fileInfo
}

type fileInfo struct {
	size    int64
	modTime time.Time
}

func snapshotDir(dir string) dirProfile {
	p := dirProfile{entries: make(map[string]fileInfo)}
	if err := filepath.WalkDir(dir, func(path string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return nil
		}
		p.entries[path] = fileInfo{size: info.Size(), modTime: info.ModTime()}
		return nil
	}); err != nil {
		slog.Log(context.Background(), loglevel.LevelTrace, "snapshotDir: walk", "dir", dir, "err", err)
	}
	return p
}

func (a dirProfile) equal(b dirProfile) bool {
	if len(a.entries) != len(b.entries) {
		return false
	}
	for k, av := range a.entries {
		bv, ok := b.entries[k]
		if !ok || av.size != bv.size || !av.modTime.Equal(bv.modTime) {
			return false
		}
	}
	return true
}

// flattenDir moves all video files found anywhere under dir into root, then
// removes dir. Files that would overwrite an existing file are skipped.
func flattenDir(dir, root string) {
	var moved int
	if werr := filepath.WalkDir(dir, func(path string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		if _, ok := videoExts[filepath.Ext(d.Name())]; !ok {
			return nil
		}
		dst := filepath.Join(root, d.Name())
		if _, err := os.Stat(dst); err == nil {
			slog.Info("flatten: skip — destination exists", slog.String("file", d.Name()))
			return nil
		}
		if err := os.Rename(path, dst); err != nil {
			slog.Warn("flatten: move failed",
				slog.String("file", d.Name()),
				slog.Any("err", err))
			return nil
		}
		moved++
		slog.Info("flatten: moved",
			slog.String("src", path),
			slog.String("dst", dst))
		return nil
	}); werr != nil {
		slog.Log(context.Background(), loglevel.LevelTrace, "flattenDir: walk", "dir", dir, "err", werr)
	}
	if moved > 0 {
		if err := os.RemoveAll(dir); err != nil {
			slog.Warn("flatten: remove dir failed",
				slog.String("dir", dir),
				slog.Any("err", err))
		} else {
			slog.Info("flatten: removed dir", slog.String("dir", dir))
		}
	}
}

// hasVideoFiles reports whether dir contains at least one video file.
func hasVideoFiles(dir string) bool {
	found := false
	if err := filepath.WalkDir(dir, func(path string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		if _, ok := videoExts[filepath.Ext(d.Name())]; ok {
			found = true
			return filepath.SkipAll
		}
		return nil
	}); err != nil {
		slog.Log(context.Background(), loglevel.LevelTrace, "hasVideoFiles: walk", "dir", dir, "err", err)
	}
	return found
}

// WatchAndFlatten scans root for existing subdirectories on startup, then
// watches for new ones. When a subdirectory's contents stop changing for 5
// seconds, all video files inside it are moved into root and the subdirectory
// is deleted. onFlatten is called after each successful flatten operation.
func WatchAndFlatten(ctx context.Context, root string, onFlatten func()) error {
	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		return err
	}
	if err := watcher.Add(root); err != nil {
		if cerr := watcher.Close(); cerr != nil {
			slog.Log(ctx, loglevel.LevelTrace, "flatten: close watcher", "err", cerr)
		}
		return fmt.Errorf("flatten: watch root %q: %w", root, err)
	}

	// pending tracks directories waiting for their stability check.
	pending := make(map[string]dirProfile)

	// Seed pending with any subdirectories that already exist at startup.
	entries, err := os.ReadDir(root)
	if err != nil {
		if cerr := watcher.Close(); cerr != nil {
			slog.Log(ctx, loglevel.LevelTrace, "flatten: close watcher", "err", cerr)
		}
		return fmt.Errorf("flatten: read root %q: %w", root, err)
	}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		dir := filepath.Join(root, e.Name())
		slog.Info("flatten: existing dir queued", slog.String("dir", dir))
		pending[dir] = snapshotDir(dir)
	}

	// stabilityCheck fires every flattenStabilityInterval to re-evaluate pending directories.
	ticker := time.NewTicker(flattenStabilityInterval)

	go func() {
		defer func() {
			if cerr := watcher.Close(); cerr != nil {
				slog.Log(ctx, loglevel.LevelTrace, "flatten: close watcher", "err", cerr)
			}
		}()
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return

			case event, ok := <-watcher.Events:
				if !ok {
					return
				}
				if !event.Has(fsnotify.Create) {
					continue
				}
				info, err := os.Stat(event.Name)
				if err != nil || !info.IsDir() {
					continue
				}
				// Only watch direct children of root, not deeper nesting.
				if filepath.Dir(event.Name) != root {
					continue
				}
				slog.Info("flatten: new dir detected", slog.String("dir", event.Name))
				pending[event.Name] = snapshotDir(event.Name)

			case <-ticker.C:
				for dir, prev := range pending {
					// Dir may have been removed in the meantime.
					if _, err := os.Stat(dir); os.IsNotExist(err) {
						delete(pending, dir)
						continue
					}
					curr := snapshotDir(dir)
					if !curr.equal(prev) {
						// Still changing — update snapshot and wait another tick.
						pending[dir] = curr
						continue
					}
					delete(pending, dir)
					if len(curr.entries) == 0 {
						// Stable but empty — remove it.
						slog.Info("flatten: removing empty dir", slog.String("dir", dir))
						if err := os.RemoveAll(dir); err != nil {
							slog.Warn("flatten: remove failed",
								slog.String("dir", dir),
								slog.Any("err", err))
						}
					} else if !hasVideoFiles(dir) {
						// Stable but contains no video files — remove it.
						slog.Info("flatten: removing non-video dir", slog.String("dir", dir))
						if err := os.RemoveAll(dir); err != nil {
							slog.Warn("flatten: remove failed",
								slog.String("dir", dir),
								slog.Any("err", err))
						}
					} else {
						flattenDir(dir, root)
						onFlatten()
					}
				}

			case err, ok := <-watcher.Errors:
				if !ok {
					return
				}
				slog.Warn("flatten watcher error", slog.Any("err", err))
			}
		}
	}()
	return nil
}
