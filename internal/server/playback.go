package server

import (
	"context"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"path/filepath"
	"regexp"
	"strings"

	"streambox/internal/loglevel"
	"streambox/internal/media"
)

//go:embed static/*
var playerAssets embed.FS

var playbackSegmentName = regexp.MustCompile(`^segment[0-9]{6,}\.m4s$`)

// Close stops browser conversions and removes their temporary media buffers.
func (s *Server) Close() {
	if s.playback != nil {
		s.playback.close()
	}
}

func (s *Server) servePlayerAsset(w http.ResponseWriter, r *http.Request) {
	name := strings.TrimPrefix(r.URL.Path, "/ui/assets/")
	switch name {
	case "player.js", "hls.light.min.js":
		w.Header().Set("Content-Type", "text/javascript; charset=utf-8")
	case "player.css":
		w.Header().Set("Content-Type", "text/css; charset=utf-8")
	default:
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Cache-Control", "no-cache")
	file, err := playerAssets.Open("static/" + name)
	if err != nil {
		slog.Error("open player asset", "name", name, "err", err)
		http.Error(w, "player asset unavailable", http.StatusInternalServerError)
		return
	}
	defer func() { logFprintErr(r.Context(), "close player asset", file.Close()) }()
	stat, err := file.Stat()
	if err != nil {
		slog.Error("stat player asset", "name", name, "err", err)
		http.Error(w, "player asset unavailable", http.StatusInternalServerError)
		return
	}
	seeker, ok := file.(io.ReadSeeker)
	if !ok {
		slog.Error("player asset does not support seeking", "name", name)
		http.Error(w, "player asset unavailable", http.StatusInternalServerError)
		return
	}
	http.ServeContent(w, r, name, stat.ModTime(), seeker)
}

func (s *Server) playbackItem(w http.ResponseWriter, r *http.Request, id string) (*media.Item, bool) {
	obj, exists := s.cfg.Library.Get(id)
	item, ok := obj.(*media.Item)
	if !exists || !ok {
		http.NotFound(w, r)
		return nil, false
	}
	return item, true
}

func writePlayerJSON(w http.ResponseWriter, r *http.Request, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	logFprintErr(r.Context(), "write browser playback response", json.NewEncoder(w).Encode(value))
}

func playbackMethod(w http.ResponseWriter, r *http.Request, method string) bool {
	if r.Method == method {
		return true
	}
	w.Header().Set("Allow", method)
	http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	return false
}

func (s *Server) servePlaybackInfo(w http.ResponseWriter, r *http.Request) {
	if !playbackMethod(w, r, http.MethodGet) {
		return
	}
	item, ok := s.playbackItem(w, r, r.URL.Query().Get("id"))
	if !ok {
		return
	}
	info, err := s.playback.inspect(r.Context(), item.Path)
	if err != nil {
		slog.Debug("browser video metadata unavailable", "path", item.Path, "err", err)
		// Browsers may play the original file without any FFmpeg installation.
		info.DirectType = item.MIMEType
	}
	writePlayerJSON(w, r, struct {
		Duration   float64 `json:"duration"`
		DirectType string  `json:"directType"`
		DirectURL  string  `json:"directURL"`
		CanConvert bool    `json:"canConvert"`
	}{info.Duration, info.DirectType, "/files/" + item.ID, err == nil && s.playback.ffmpeg != "" && info.Duration > 0})
}

func (s *Server) servePlaybackStart(w http.ResponseWriter, r *http.Request) {
	if !playbackMethod(w, r, http.MethodPost) {
		return
	}
	var request struct {
		ID    string  `json:"id"`
		Start float64 `json:"start"`
		Force bool    `json:"force"`
	}
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1024))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&request); err != nil {
		slog.Log(r.Context(), loglevel.LevelTrace, "decode browser playback request", "err", err)
		http.Error(w, "invalid playback request", http.StatusBadRequest)
		return
	}
	item, ok := s.playbackItem(w, r, request.ID)
	if !ok {
		return
	}
	info, err := s.playback.inspect(r.Context(), item.Path)
	if err != nil {
		slog.Warn("inspect browser video", "path", item.Path, "err", err)
		http.Error(w, "Cannot inspect this video. Check that FFmpeg and ffprobe are installed on the server.", http.StatusServiceUnavailable)
		return
	}
	if request.Start < 0 || request.Start >= info.Duration {
		http.Error(w, "invalid playback position", http.StatusBadRequest)
		return
	}
	session, err := s.playback.start(r.Context(), item.Path, info, request.Start, request.Force)
	if err != nil {
		if errors.Is(err, context.Canceled) {
			slog.Log(r.Context(), loglevel.LevelTrace, "browser playback request cancelled", "path", item.Path, "err", err)
			return
		}
		slog.Warn("prepare browser video", "path", item.Path, "err", err)
		if errors.Is(err, errPlaybackBusy) {
			http.Error(w, err.Error(), http.StatusTooManyRequests)
		} else {
			http.Error(w, "Cannot prepare this video for the browser. Check the server logs and FFmpeg installation.", http.StatusServiceUnavailable)
		}
		return
	}
	if s.cfg.History != nil {
		s.cfg.History.Record(item)
	}
	writePlayerJSON(w, r, struct {
		Token    string  `json:"token"`
		URL      string  `json:"url"`
		Duration float64 `json:"duration"`
		Start    float64 `json:"start"`
	}{session.token, "/playback/" + session.token + "/" + playbackPlaylistName, info.Duration, session.start})
}

func (s *Server) servePlaybackStop(w http.ResponseWriter, r *http.Request) {
	if !playbackMethod(w, r, http.MethodPost) {
		return
	}
	s.playback.stop(r.URL.Query().Get("token"))
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) servePlaybackFile(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	parts := strings.Split(strings.TrimPrefix(r.URL.Path, "/playback/"), "/")
	if len(parts) != 2 || (parts[1] != playbackPlaylistName && parts[1] != playbackInitName && !playbackSegmentName.MatchString(parts[1])) {
		http.NotFound(w, r)
		return
	}
	session, ok := s.playback.get(parts[0])
	if !ok {
		http.Error(w, "playback session expired", http.StatusGone)
		return
	}
	session.mu.Lock()
	err := session.err
	session.mu.Unlock()
	if err != nil {
		http.Error(w, "video conversion interrupted", http.StatusServiceUnavailable)
		return
	}
	if parts[1] == playbackPlaylistName {
		w.Header().Set("Content-Type", "application/vnd.apple.mpegurl")
		w.Header().Set("Cache-Control", "no-store")
	} else {
		w.Header().Set("Content-Type", "video/mp4")
		w.Header().Set("Cache-Control", "private, max-age=60")
	}
	http.ServeFile(w, r, filepath.Join(session.dir, parts[1]))
}

const uiWatchPage = `<!DOCTYPE html><html lang="en"><head><meta charset="utf-8">
<meta name="viewport" content="width=device-width,initial-scale=1">
<title>%s · StreamBox</title>
<link rel="stylesheet" href="/ui/assets/player.css">
<script src="/ui/assets/player.js" defer></script>
</head><body data-id="%s">
<header><a class="back" href="/ui" aria-label="Back to library">← Library</a><span class="brand">STREAMBOX</span></header>
<main><div class="heading"><div><p class="eyebrow">NOW PLAYING</p><h1>%s</h1></div><a class="download" href="/files/%s?download=1" download>Download original ↓</a></div>
<div class="player" id="player">
<video id="video" playsinline preload="metadata"></video>
<div class="overlay" id="overlay"><button id="start" class="start" aria-label="Play video">▶</button><p id="status" role="status" aria-live="polite">Preparing your video…</p></div>
<div class="controls">
<input id="timeline" class="timeline" type="range" min="0" max="0" step="0.1" value="0" aria-label="Playback position" disabled>
<div class="control-row"><button id="toggle" aria-label="Play" title="Play / pause (Space)">▶</button><button id="rewind" aria-label="Back 10 seconds" title="Back 10 seconds (←)">↶ 10</button><button id="forward" aria-label="Forward 10 seconds" title="Forward 10 seconds (→)">10 ↷</button><span id="time" class="time">0:00 / 0:00</span><div class="spacer"></div><button id="mute" aria-label="Mute" title="Mute (M)">♪</button><input id="volume" type="range" min="0" max="1" step="0.05" value="1" aria-label="Volume"><button id="fullscreen" aria-label="Fullscreen" title="Fullscreen (F)">⛶</button></div>
</div></div>
<div class="below"><p>Space to pause · ← → to skip · F for fullscreen</p><button id="retry" hidden>Retry playback</button></div>
</main></body></html>`

func (s *Server) renderWatchPage(w http.ResponseWriter, r *http.Request, item *media.Item) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, err := fmt.Fprintf(w, uiWatchPage, escXML(item.Title), item.ID, escXML(item.Title), item.ID)
	logFprintErr(r.Context(), "ui: write watch page", err)
}
