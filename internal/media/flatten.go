package media

import (
	"context"
	"log"
	"os"
	"path/filepath"
	"time"

	"github.com/fsnotify/fsnotify"
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
	_ = filepath.WalkDir(dir, func(path string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return nil
		}
		p.entries[path] = fileInfo{size: info.Size(), modTime: info.ModTime()}
		return nil
	})
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
	_ = filepath.WalkDir(dir, func(path string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		if _, ok := videoExts[filepath.Ext(d.Name())]; !ok {
			return nil
		}
		dst := filepath.Join(root, d.Name())
		if _, err := os.Stat(dst); err == nil {
			log.Printf("flatten: skip %s — destination already exists", d.Name())
			return nil
		}
		if err := os.Rename(path, dst); err != nil {
			log.Printf("flatten: move %s: %v", d.Name(), err)
			return nil
		}
		moved++
		log.Printf("flatten: moved %s → %s", path, dst)
		return nil
	})
	if moved > 0 {
		if err := os.RemoveAll(dir); err != nil {
			log.Printf("flatten: remove dir %s: %v", dir, err)
		} else {
			log.Printf("flatten: removed %s", dir)
		}
	}
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
		watcher.Close()
		return err
	}

	// pending tracks directories waiting for their stability check.
	pending := make(map[string]dirProfile)

	// Seed pending with any subdirectories that already exist at startup.
	entries, err := os.ReadDir(root)
	if err != nil {
		watcher.Close()
		return err
	}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		dir := filepath.Join(root, e.Name())
		log.Printf("flatten: existing dir queued: %s", dir)
		pending[dir] = snapshotDir(dir)
	}

	// stabilityCheck fires every flattenStabilityInterval to re-evaluate pending directories.
	ticker := time.NewTicker(flattenStabilityInterval)

	go func() {
		defer watcher.Close()
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
				log.Printf("flatten: new dir detected: %s", event.Name)
				pending[event.Name] = snapshotDir(event.Name)

			case <-ticker.C:
				for dir, prev := range pending {
					// Dir may have been removed in the meantime.
					if _, err := os.Stat(dir); os.IsNotExist(err) {
						delete(pending, dir)
						continue
					}
					curr := snapshotDir(dir)
					if curr.equal(prev) && len(curr.entries) > 0 {
						delete(pending, dir)
						flattenDir(dir, root)
						onFlatten()
					} else {
						// Still changing — update snapshot and wait another tick.
						pending[dir] = curr
					}
				}

			case err, ok := <-watcher.Errors:
				if !ok {
					return
				}
				log.Printf("flatten watcher: %v", err)
			}
		}
	}()
	return nil
}
