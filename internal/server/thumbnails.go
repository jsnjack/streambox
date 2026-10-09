package server

import (
	"bytes"
	"container/list"
	"context"
	"fmt"
	"image/jpeg"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"streambox/internal/loglevel"
	"streambox/internal/media"
)

const (
	thumbnailLimit    = 256
	thumbnailTimeout  = 20 * time.Second
	thumbnailRetry    = time.Minute
	thumbnailSize     = 160 // JPEG_TN permits at most 160 pixels on either axis.
	thumbnailFeatures = "DLNA.ORG_PN=JPEG_TN;DLNA.ORG_OP=01;DLNA.ORG_CI=1;DLNA.ORG_FLAGS=00F00000000000000000000000000000"
)

type thumbnailEntry struct {
	path    string
	size    int64
	modTime time.Time
	data    []byte
	err     error
	retryAt time.Time
}

// Cache failures too: one video appears in several web and DLNA listings.
type thumbnailCache struct {
	mu       sync.Mutex
	entries  map[string]*list.Element
	order    list.List
	gate     chan struct{}
	generate func(context.Context, string) ([]byte, error)
}

func newThumbnailCache() *thumbnailCache {
	ffmpeg, err := exec.LookPath("ffmpeg")
	if err != nil {
		slog.Warn("thumbnails disabled: install FFmpeg and restart streambox", "err", err)
		return nil
	}
	return &thumbnailCache{
		entries: make(map[string]*list.Element),
		gate:    make(chan struct{}, 1),
		generate: func(ctx context.Context, path string) ([]byte, error) {
			return extractThumbnail(ctx, ffmpeg, path)
		},
	}
}

func (c *thumbnailCache) cached(path string, info os.FileInfo) ([]byte, error, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	element, ok := c.entries[path]
	if !ok {
		return nil, nil, false
	}
	entry := element.Value.(thumbnailEntry)
	if entry.size != info.Size() || !entry.modTime.Equal(info.ModTime()) ||
		(entry.err != nil && !time.Now().Before(entry.retryAt)) {
		return nil, nil, false
	}
	c.order.MoveToFront(element)
	return entry.data, entry.err, true
}

func (c *thumbnailCache) get(ctx context.Context, path string, info os.FileInfo) ([]byte, error) {
	if data, err, ok := c.cached(path, info); ok {
		return data, err
	}
	ctx, cancel := context.WithTimeout(ctx, thumbnailTimeout)
	defer cancel()
	select {
	case c.gate <- struct{}{}:
		defer func() { <-c.gate }()
	case <-ctx.Done():
		return nil, fmt.Errorf("wait for thumbnail generation: %w", ctx.Err())
	}
	if data, err, ok := c.cached(path, info); ok {
		return data, err
	}
	data, err := c.generate(ctx, path)
	if ctx.Err() != nil {
		return nil, fmt.Errorf("generate thumbnail for %q: %w", path, ctx.Err())
	}
	if err != nil {
		slog.Warn("generate thumbnail failed", "path", path, "err", err)
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if old, ok := c.entries[path]; ok {
		c.order.Remove(old)
	}
	c.entries[path] = c.order.PushFront(thumbnailEntry{
		path: path, size: info.Size(), modTime: info.ModTime(),
		data: data, err: err, retryAt: time.Now().Add(thumbnailRetry),
	})
	if c.order.Len() > thumbnailLimit {
		old := c.order.Back()
		delete(c.entries, old.Value.(thumbnailEntry).path)
		c.order.Remove(old)
	}
	return data, err
}

func extractThumbnail(ctx context.Context, ffmpeg, path string) ([]byte, error) {
	// Absolute paths keep filenames beginning with '-' or containing ':' from
	// being interpreted as options or input protocols by FFmpeg.
	path, err := filepath.Abs(path)
	if err != nil {
		return nil, fmt.Errorf("resolve thumbnail source: %w", err)
	}
	for _, seek := range []string{"5", "0"} {
		cmd := exec.CommandContext(ctx, ffmpeg,
			"-hide_banner", "-loglevel", "error", "-nostdin",
			"-ss", seek, "-threads", "1", "-i", path,
			"-map", "0:v:0", "-frames:v", "1", "-an", "-sn",
			"-vf", fmt.Sprintf("scale=%d:%d:force_original_aspect_ratio=decrease,setsar=1", thumbnailSize, thumbnailSize),
			"-threads", "1", "-filter_threads", "1", "-c:v", "mjpeg", "-pix_fmt", "yuvj420p", "-q:v", "3",
			"-f", "image2pipe", "pipe:1")
		var stderr processLog
		cmd.Stderr = &stderr
		data, err := cmd.Output()
		if err != nil {
			return nil, fmt.Errorf("extract video frame from %q: %w: %s", path, err, strings.TrimSpace(stderr.String()))
		}
		// Seeking past the end of a short video succeeds with no output. Retry
		// from the start rather than returning an empty or broken JPEG.
		if len(data) == 0 {
			continue
		}
		image, err := jpeg.DecodeConfig(bytes.NewReader(data))
		if err != nil {
			return nil, fmt.Errorf("decode thumbnail from %q: %w", path, err)
		}
		if image.Width > thumbnailSize || image.Height > thumbnailSize {
			return nil, fmt.Errorf("thumbnail from %q exceeds JPEG_TN dimensions", path)
		}
		return data, nil
	}
	return nil, fmt.Errorf("no video frame found in %q", path)
}

func (s *Server) serveThumbnail(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if s.thumbnails == nil {
		http.NotFound(w, r)
		return
	}
	obj, ok := s.cfg.Library.Get(strings.TrimPrefix(r.URL.Path, "/thumbnails/"))
	item, isItem := obj.(*media.Item)
	if !ok || !isItem {
		http.NotFound(w, r)
		return
	}
	info, err := os.Stat(item.Path)
	if err != nil {
		slog.Log(r.Context(), loglevel.LevelTrace, "stat thumbnail source", "path", item.Path, "err", err)
		http.NotFound(w, r)
		return
	}
	if !info.Mode().IsRegular() {
		http.NotFound(w, r)
		return
	}
	data, err := s.thumbnails.get(r.Context(), item.Path, info)
	if err != nil {
		slog.Log(r.Context(), loglevel.LevelTrace, "serve thumbnail", "path", item.Path, "err", err)
		http.Error(w, "thumbnail unavailable", http.StatusServiceUnavailable)
		return
	}
	w.Header().Set("Content-Type", "image/jpeg")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("ETag", fmt.Sprintf(`"%s-%d-%d"`, item.ID, info.Size(), info.ModTime().UnixNano()))
	w.Header().Set("transferMode.dlna.org", "Interactive")
	w.Header().Set("contentFeatures.dlna.org", thumbnailFeatures)
	http.ServeContent(w, r, "thumbnail.jpg", info.ModTime(), bytes.NewReader(data))
}
