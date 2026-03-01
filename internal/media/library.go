package media

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
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
func NewLibrary(root string) (*Library, error) {
	l := &Library{
		objects:    make(map[string]Object),
		containers: make(map[string]*Container),
	}
	rootC := &Container{ID: "0", ParentID: "-1", Title: "root"}
	l.objects["0"] = rootC
	l.containers["0"] = rootC

	if err := l.scan(root, "0"); err != nil {
		return nil, err
	}
	return l, nil
}

func (l *Library) nextID() string {
	return strconv.FormatInt(l.counter.Add(1), 10)
}

func (l *Library) scan(dir, parentID string) error {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		path := filepath.Join(dir, entry.Name())
		if entry.IsDir() {
			id := l.nextID()
			c := &Container{ID: id, ParentID: parentID, Title: entry.Name()}
			l.mu.Lock()
			l.objects[id] = c
			l.containers[id] = c
			if parent, ok := l.containers[parentID]; ok {
				parent.children = append(parent.children, id)
			}
			l.mu.Unlock()
			if err := l.scan(path, id); err != nil {
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
			title := strings.TrimSuffix(entry.Name(), filepath.Ext(entry.Name()))
			item := &Item{
				ID: id, ParentID: parentID, Title: title,
				Path: path, MIMEType: mime, Size: info.Size(),
			}
			l.mu.Lock()
			l.objects[id] = item
			if parent, ok := l.containers[parentID]; ok {
				parent.children = append(parent.children, id)
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
