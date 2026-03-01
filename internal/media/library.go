package media

import (
	"context"
	"log"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode"

	"github.com/fsnotify/fsnotify"
)

var videoExts = map[string]string{
	".mp4":  "video/mp4",
	".m4v":  "video/mp4",
	".mkv":  "video/x-matroska",
	".avi":  "video/avi",
	".mov":  "video/quicktime",
	".wmv":  "video/x-ms-wmv",
	".ts":   "video/mpeg",
	".m2ts": "video/mpeg",
	".flv":  "video/x-flv",
	".webm": "video/webm",
	".ogv":  "video/ogg",
	".3gp":  "video/3gpp",
}

// Title-cleaning regexes, ported from rygel-titlefix.
var (
	reYear   = regexp.MustCompile(`.*?([21]\d{3})`)
	reEpFull = regexp.MustCompile(`(?i).*?(S\d{2}E\d{2})`)
	reEpCode = regexp.MustCompile(`(?i)S\d{2}E\d{2}`)
)

// cleanTitle converts a raw filename stem into a human-readable title.
// Logic mirrors rygel-titlefix/cmd/utils.go processFilename():
//   - if a year is found, take everything before it as the title
//   - else if an episode code (SxxExx) is found, take the prefix
//   - replace dots with spaces, apply title case, append episode code if present
func cleanTitle(stem string) string {
	yearMatch := reYear.FindString(stem)
	episode := reEpCode.FindString(stem)

	var title string
	if yearMatch != "" && len(yearMatch) > 4 {
		title = clearDots(yearMatch[:len(yearMatch)-4])
	} else {
		epMatch := reEpFull.FindString(stem)
		if epMatch != "" {
			title = clearDots(epMatch[:len(epMatch)-len(episode)])
		} else {
			title = clearDots(stem)
		}
	}

	if episode != "" {
		ep := strings.ToUpper(episode)
		if idx := strings.Index(strings.ToUpper(title), strings.ToUpper(episode)); idx >= 0 {
			title = title[:idx] + ep
		} else {
			title += " " + ep
		}
	}

	return titleCase(title)
}

func clearDots(s string) string {
	return strings.TrimSpace(strings.ReplaceAll(s, ".", " "))
}

func titleCase(s string) string {
	prev := true
	runes := []rune(s)
	for i, r := range runes {
		if unicode.IsSpace(r) {
			prev = true
		} else if prev {
			runes[i] = unicode.ToUpper(r)
			prev = false
		}
	}
	return string(runes)
}

const (
	idRoot   = "0"
	idAll    = "1"
	idRecent = "2"
)

// Object is implemented by both Container and Item.
type Object interface {
	GetID() string
	GetParentID() string
	GetTitle() string
}

// Container represents a browsable folder.
type Container struct {
	ID       string
	ParentID string
	Title    string
	children []string
}

func (c *Container) GetID() string       { return c.ID }
func (c *Container) GetParentID() string { return c.ParentID }
func (c *Container) GetTitle() string    { return c.Title }

// Item represents a single video file.
type Item struct {
	ID       string
	ParentID string
	Title    string
	Path     string
	MIMEType string
	Size     int64
}

func (i *Item) GetID() string       { return i.ID }
func (i *Item) GetParentID() string { return i.ParentID }
func (i *Item) GetTitle() string    { return i.Title }

// Library holds an in-memory index of all media objects.
type Library struct {
	mu         sync.RWMutex
	objects    map[string]Object
	containers map[string]*Container
	counter    atomic.Int64
	videoCount int
}

// NewLibrary scans root recursively and builds the media index.
// recentDays controls how many days back the "Recent" virtual folder covers
// (0 = disable Recent).
func NewLibrary(root string, recentDays int) (*Library, error) {
	l := &Library{
		objects:    make(map[string]Object),
		containers: make(map[string]*Container),
	}
	// Reserve IDs 0 (root), 1 (All), 2 (Recent).
	l.counter.Store(2)

	rootC := &Container{ID: idRoot, ParentID: "-1", Title: "root"}
	l.objects[idRoot] = rootC
	l.containers[idRoot] = rootC

	allC := &Container{ID: idAll, ParentID: idRoot, Title: "All"}
	l.objects[idAll] = allC
	l.containers[idAll] = allC
	rootC.children = append(rootC.children, idAll)

	recentC := &Container{ID: idRecent, ParentID: idRoot, Title: "Recent"}
	l.objects[idRecent] = recentC
	l.containers[idRecent] = recentC
	rootC.children = append(rootC.children, idRecent)

	cutoff := time.Time{}
	if recentDays > 0 {
		cutoff = time.Now().AddDate(0, 0, -recentDays)
	}

	if err := l.scan(root, idAll, cutoff, true); err != nil {
		return nil, err
	}
	return l, nil
}

func (l *Library) nextID() string {
	return strconv.FormatInt(l.counter.Add(1), 10)
}

func (l *Library) scan(dir, parentID string, recentCutoff time.Time, flatten bool) error {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), ".") {
			continue
		}
		path := filepath.Join(dir, entry.Name())
		if entry.IsDir() {
			if flatten {
				if err := l.scan(path, parentID, recentCutoff, flatten); err != nil {
					return err
				}
				continue
			}
			id := l.nextID()
			c := &Container{ID: id, ParentID: parentID, Title: entry.Name()}
			l.mu.Lock()
			l.objects[id] = c
			l.containers[id] = c
			if parent, ok := l.containers[parentID]; ok {
				parent.children = append(parent.children, id)
			}
			l.mu.Unlock()
			if err := l.scan(path, id, recentCutoff, flatten); err != nil {
				return err
			}
		} else {
			mime, ok := videoExts[strings.ToLower(filepath.Ext(entry.Name()))]
			if !ok {
				continue
			}
			info, err := entry.Info()
			if err != nil {
				continue
			}
			id := l.nextID()
			stem := strings.TrimSuffix(entry.Name(), filepath.Ext(entry.Name()))
			item := &Item{
				ID:       id,
				ParentID: parentID,
				Title:    cleanTitle(stem),
				Path:     path,
				MIMEType: mime,
				Size:     info.Size(),
			}
			l.mu.Lock()
			l.objects[id] = item
			if parent, ok := l.containers[parentID]; ok {
				parent.children = append(parent.children, id)
			}
			if !recentCutoff.IsZero() && !info.ModTime().Before(recentCutoff) {
				l.containers[idRecent].children = append(l.containers[idRecent].children, id)
			}
			l.videoCount++
			l.mu.Unlock()
		}
	}
	return nil
}

// VideoCount returns the total number of indexed video files.
func (l *Library) VideoCount() int {
	l.mu.RLock()
	defer l.mu.RUnlock()
	return l.videoCount
}

// Get returns the object for a given ID.
func (l *Library) Get(id string) (Object, bool) {
	l.mu.RLock()
	defer l.mu.RUnlock()
	obj, ok := l.objects[id]
	return obj, ok
}

// Children returns the direct children of a container.
func (l *Library) Children(containerID string) []Object {
	l.mu.RLock()
	defer l.mu.RUnlock()
	c, ok := l.containers[containerID]
	if !ok {
		return nil
	}
	result := make([]Object, 0, len(c.children))
	for _, id := range c.children {
		if obj, ok := l.objects[id]; ok {
			result = append(result, obj)
		}
	}
	return result
}

// Reload rescans the media directory in-place under the write lock.
func (l *Library) Reload(root string, recentDays int) error {
	fresh, err := NewLibrary(root, recentDays)
	if err != nil {
		return err
	}
	l.mu.Lock()
	l.objects = fresh.objects
	l.containers = fresh.containers
	l.videoCount = fresh.videoCount
	l.counter.Store(fresh.counter.Load())
	l.mu.Unlock()
	return nil
}

// Watch monitors root for file additions/removals and calls onChange (debounced 2s).
// Subdirectories created at runtime are added to the watch automatically.
func Watch(ctx context.Context, root string, onChange func()) error {
	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		return err
	}

	// Add root and all existing subdirectories.
	if err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil || !d.IsDir() {
			return nil
		}
		return watcher.Add(path)
	}); err != nil {
		watcher.Close()
		return err
	}

	go func() {
		defer watcher.Close()
		timer := time.NewTimer(0)
		timer.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case event, ok := <-watcher.Events:
				if !ok {
					return
				}
				if event.Has(fsnotify.Create) || event.Has(fsnotify.Remove) || event.Has(fsnotify.Rename) {
					// Watch newly created subdirectories.
					if event.Has(fsnotify.Create) {
						if info, err := os.Stat(event.Name); err == nil && info.IsDir() {
							if err := watcher.Add(event.Name); err != nil {
								log.Printf("media watcher: add dir %s: %v", event.Name, err)
							}
						}
					}
					timer.Reset(2 * time.Second)
				}
			case <-timer.C:
				onChange()
			case err, ok := <-watcher.Errors:
				if !ok {
					return
				}
				log.Printf("media watcher: %v", err)
			}
		}
	}()
	return nil
}
