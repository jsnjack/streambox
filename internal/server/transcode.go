package server

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	playbackProbeTimeout = 10 * time.Second
	playbackStartTimeout = 30 * time.Second
	playbackIdleTimeout  = time.Minute
	playbackPollInterval = 100 * time.Millisecond
	playbackSessionLimit = 2
	playbackProbeLimit   = 256
	playbackMaxWidth     = 1920
	playbackMaxHeight    = 1080
	playbackCPUThreads   = "2"
	playbackMaxRate      = "8M"
	playbackBufferSize   = "16M"
	processLogLimit      = 8 * 1024
	playbackSegmentTime  = "2"
	playbackPlaylistName = "index.m3u8"
	playbackInitName     = "init.mp4"
)

var errPlaybackBusy = errors.New("another video is being converted; stop it before starting this one")

type processLog struct{ data []byte }

func (log *processLog) Write(data []byte) (int, error) {
	n := len(data)
	if len(data) > processLogLimit {
		data = data[len(data)-processLogLimit:]
	}
	if overflow := len(log.data) + len(data) - processLogLimit; overflow > 0 {
		log.data = log.data[overflow:]
	}
	log.data = append(log.data, data...)
	return n, nil
}

func (log *processLog) String() string { return string(log.data) }

type videoStream struct {
	Index       int    `json:"index"`
	CodecType   string `json:"codec_type"`
	Codec       string `json:"codec_name"`
	PixelFormat string `json:"pix_fmt"`
	Profile     string `json:"profile"`
	Level       int    `json:"level"`
	Width       int    `json:"width"`
	Height      int    `json:"height"`
	Duration    string `json:"duration"`
	Disposition struct {
		AttachedPicture int `json:"attached_pic"`
	} `json:"disposition"`
}

type playbackInfo struct {
	Duration    float64
	SourceStart float64
	Video       videoStream
	Audio       *videoStream
	DirectType  string
}

func (info playbackInfo) copyVideo() bool {
	return info.Video.Codec == "h264" &&
		(info.Video.PixelFormat == "yuv420p" || info.Video.PixelFormat == "yuvj420p")
}

func directMediaType(path string, info playbackInfo) string {
	var video, audio string
	switch strings.ToLower(filepath.Ext(path)) {
	case ".mp4", ".m4v", ".mov":
		if !info.copyVideo() {
			return ""
		}
		profile := 100
		switch info.Video.Profile {
		case "Baseline", "Constrained Baseline":
			profile = 66
		case "Main":
			profile = 77
		}
		level := info.Video.Level
		if level <= 0 || level > 255 {
			level = 41
		}
		video = fmt.Sprintf("avc1.%02x00%02x", profile, level)
		if info.Audio != nil {
			switch info.Audio.Codec {
			case "aac":
				if info.Audio.Profile != "LC" {
					return ""
				}
				audio = ",mp4a.40.2"
			case "mp3":
				audio = ",mp4a.40.34"
			default:
				return ""
			}
		}
		// Firefox does not reliably play the QuickTime container.
		if strings.EqualFold(filepath.Ext(path), ".mov") {
			return ""
		}
		return `video/mp4; codecs="` + video + audio + `"`
	case ".webm":
		switch info.Video.Codec {
		case "vp8":
			video = "vp8"
		case "vp9":
			if info.Video.PixelFormat != "yuv420p" {
				return ""
			}
			video = "vp9"
		default:
			return ""
		}
		if info.Audio != nil {
			switch info.Audio.Codec {
			case "opus", "vorbis":
				audio = "," + info.Audio.Codec
			default:
				return ""
			}
		}
		return `video/webm; codecs="` + video + audio + `"`
	}
	return ""
}

type probedVideo struct {
	size    int64
	modTime time.Time
	info    playbackInfo
}

type browserPlayback struct {
	mu       sync.Mutex
	ffmpeg   string
	ffprobe  string
	device   string
	probes   map[string]probedVideo
	sessions map[string]*playbackSession
	closed   bool
}

func newBrowserPlayback() *browserPlayback {
	m := &browserPlayback{probes: make(map[string]probedVideo), sessions: make(map[string]*playbackSession)}
	for name, dest := range map[string]*string{"ffmpeg": &m.ffmpeg, "ffprobe": &m.ffprobe} {
		path, err := exec.LookPath(name)
		if err != nil {
			slog.Warn("browser conversion unavailable: install FFmpeg and restart streambox", "tool", name, "err", err)
			continue
		}
		*dest = path
	}
	devices, err := filepath.Glob("/dev/dri/renderD*")
	if err != nil {
		slog.Warn("detect video hardware", "err", err)
	}
	if len(devices) > 0 {
		m.device = devices[0]
	}
	return m
}

func (m *browserPlayback) inspect(ctx context.Context, path string) (playbackInfo, error) {
	if m.ffprobe == "" {
		return playbackInfo{}, errors.New("install FFmpeg (including ffprobe) on the server to enable browser conversion")
	}
	info, err := os.Stat(path)
	if err != nil {
		return playbackInfo{}, fmt.Errorf("stat playback source: %w", err)
	}
	m.mu.Lock()
	cached, ok := m.probes[path]
	m.mu.Unlock()
	if ok && cached.size == info.Size() && cached.modTime.Equal(info.ModTime()) {
		return cached.info, nil
	}
	absPath, err := filepath.Abs(path)
	if err != nil {
		return playbackInfo{}, fmt.Errorf("resolve playback source: %w", err)
	}
	ctx, cancel := context.WithTimeout(ctx, playbackProbeTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, m.ffprobe, "-v", "error", "-threads", "1", "-show_entries",
		"format=duration,start_time:stream=index,codec_type,codec_name,pix_fmt,profile,level,width,height,duration:stream_disposition=attached_pic", "-of", "json", absPath)
	var stderr processLog
	cmd.Stderr = &stderr
	output, err := cmd.Output()
	if err != nil {
		return playbackInfo{}, fmt.Errorf("inspect video: %w: %s", err, strings.TrimSpace(stderr.String()))
	}
	var probe struct {
		Format struct {
			Duration string `json:"duration"`
			Start    string `json:"start_time"`
		} `json:"format"`
		Streams []videoStream `json:"streams"`
	}
	if err := json.Unmarshal(output, &probe); err != nil {
		return playbackInfo{}, fmt.Errorf("decode video metadata: %w", err)
	}
	result := playbackInfo{}
	// Some containers lack duration metadata but still support direct playback.
	if duration, err := strconv.ParseFloat(probe.Format.Duration, 64); err == nil {
		result.Duration = duration
	}
	if start, err := strconv.ParseFloat(probe.Format.Start, 64); err == nil {
		result.SourceStart = start
	}
	for _, stream := range probe.Streams {
		if stream.CodecType == "video" && stream.Disposition.AttachedPicture == 0 && result.Video.Codec == "" {
			result.Video = stream
			if result.Duration <= 0 {
				if duration, err := strconv.ParseFloat(stream.Duration, 64); err == nil {
					result.Duration = duration
				}
			}
		}
		if stream.CodecType == "audio" && result.Audio == nil {
			copy := stream
			result.Audio = &copy
		}
	}
	if result.Video.Codec == "" {
		return playbackInfo{}, errors.New("no playable video stream found")
	}
	result.DirectType = directMediaType(path, result)
	if math.IsNaN(result.Duration) || math.IsInf(result.Duration, 0) {
		result.Duration = 0
	}
	m.mu.Lock()
	if len(m.probes) >= playbackProbeLimit {
		clear(m.probes)
	}
	m.probes[path] = probedVideo{size: info.Size(), modTime: info.ModTime(), info: result}
	m.mu.Unlock()
	return result, nil
}

type playbackSession struct {
	token  string
	dir    string
	start  float64
	copy   bool
	cancel context.CancelFunc
	done   chan struct{}
	ready  chan error
	timer  *time.Timer // protected by browserPlayback.mu
	mu     sync.Mutex
	err    error
}

func (m *browserPlayback) start(ctx context.Context, path string, info playbackInfo, start float64, force bool) (*playbackSession, error) {
	if m.ffmpeg == "" {
		return nil, errors.New("install FFmpeg on the server to enable browser conversion")
	}
	if math.IsNaN(start) || math.IsInf(start, 0) || start < 0 || info.Duration <= 0 ||
		math.IsNaN(info.Duration) || math.IsInf(info.Duration, 0) || start >= info.Duration {
		return nil, errors.New("invalid playback position or unavailable video duration")
	}
	path, err := filepath.Abs(path)
	if err != nil {
		return nil, fmt.Errorf("resolve conversion source: %w", err)
	}
	var token [16]byte
	if _, err := rand.Read(token[:]); err != nil {
		return nil, fmt.Errorf("create playback token: %w", err)
	}
	sessionCtx, cancel := context.WithCancel(context.Background())
	session := &playbackSession{token: hex.EncodeToString(token[:]), start: start,
		copy: info.copyVideo() && !force, cancel: cancel, done: make(chan struct{}), ready: make(chan error, 1)}
	if session.copy && start > 0 {
		keyframe, err := m.keyframeStart(ctx, path, info, start)
		if err != nil {
			slog.Debug("seek requires video encoding", "err", err)
			session.copy = false
		} else {
			session.start = keyframe
		}
	}
	m.mu.Lock()
	busy := m.closed || len(m.sessions) >= playbackSessionLimit
	for _, active := range m.sessions {
		if !session.copy && !active.copy {
			busy = true
		}
	}
	if busy {
		m.mu.Unlock()
		cancel()
		return nil, errPlaybackBusy
	}
	dir, err := os.MkdirTemp("", "streambox-playback-")
	if err != nil {
		m.mu.Unlock()
		cancel()
		return nil, fmt.Errorf("create playback buffer: %w", err)
	}
	session.dir = dir
	m.sessions[session.token] = session
	go session.run(sessionCtx, m.ffmpeg, m.device, path, info)
	m.mu.Unlock()
	ctx, stopWaiting := context.WithTimeout(ctx, playbackStartTimeout)
	defer stopWaiting()
	select {
	case err := <-session.ready:
		if err != nil {
			m.stop(session.token)
			return nil, err
		}
	case <-ctx.Done():
		m.stop(session.token)
		return nil, fmt.Errorf("prepare browser playback: %w", ctx.Err())
	case <-session.done:
		select {
		case err := <-session.ready:
			if err != nil {
				m.stop(session.token)
				return nil, err
			}
		default:
			m.stop(session.token)
			return nil, errors.New("playback stopped during startup")
		}
	}
	m.mu.Lock()
	if _, exists := m.sessions[session.token]; !exists {
		m.mu.Unlock()
		return nil, errors.New("playback stopped during startup")
	}
	session.timer = time.AfterFunc(playbackIdleTimeout, func() { m.stop(session.token) })
	m.mu.Unlock()
	return session, nil
}

// Stream copying seeks to a keyframe. Returning its real position lets the
// browser skip the preroll instead of showing an earlier scene at a wrong time.
func (m *browserPlayback) keyframeStart(ctx context.Context, path string, info playbackInfo, start float64) (float64, error) {
	ctx, cancel := context.WithTimeout(ctx, playbackProbeTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, m.ffprobe, "-v", "error", "-select_streams", strconv.Itoa(info.Video.Index),
		"-read_intervals", fmt.Sprintf("%.3f%%+#1", start+info.SourceStart), "-show_packets", "-show_entries", "packet=pts_time,flags", "-of", "json", path)
	output, err := cmd.Output()
	if err != nil {
		return 0, fmt.Errorf("find seek keyframe: %w", err)
	}
	var result struct {
		Packets []struct {
			PTS   string `json:"pts_time"`
			Flags string `json:"flags"`
		} `json:"packets"`
	}
	if err := json.Unmarshal(output, &result); err != nil {
		return 0, fmt.Errorf("decode seek keyframe: %w", err)
	}
	if len(result.Packets) != 1 || !strings.Contains(result.Packets[0].Flags, "K") {
		return 0, errors.New("no seek keyframe found")
	}
	pts, err := strconv.ParseFloat(result.Packets[0].PTS, 64)
	if err != nil {
		return 0, fmt.Errorf("parse seek keyframe: %w", err)
	}
	pts -= info.SourceStart
	if math.IsNaN(pts) || math.IsInf(pts, 0) || pts > start || pts < 0 {
		return 0, errors.New("invalid seek keyframe")
	}
	return pts, nil
}

func (m *browserPlayback) get(token string) (*playbackSession, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	session, ok := m.sessions[token]
	if ok && session.timer != nil {
		session.timer.Reset(playbackIdleTimeout)
	}
	return session, ok
}

func (m *browserPlayback) stop(token string) {
	m.mu.Lock()
	session, ok := m.sessions[token]
	if ok {
		delete(m.sessions, token)
		if session.timer != nil {
			session.timer.Stop()
		}
	}
	m.mu.Unlock()
	if !ok {
		return
	}
	session.cancel()
	<-session.done
	if err := os.RemoveAll(session.dir); err != nil {
		slog.Warn("remove playback buffer", "err", err)
	}
	slog.Debug("browser playback stopped", "session", token)
}

func (m *browserPlayback) close() {
	m.mu.Lock()
	m.closed = true
	tokens := make([]string, 0, len(m.sessions))
	for token := range m.sessions {
		tokens = append(tokens, token)
	}
	m.mu.Unlock()
	for _, token := range tokens {
		m.stop(token)
	}
}

func (s *playbackSession) run(ctx context.Context, ffmpeg, device, path string, info playbackInfo) {
	defer close(s.done)
	modes := []string{"software"}
	if s.copy {
		modes = []string{"copy"}
	} else if device != "" {
		modes = []string{"vaapi", "vaapi-upload", "software"}
	}
	var lastErr error
	for _, mode := range modes {
		ready, err := s.runAttempt(ctx, ffmpeg, playbackArgs(path, s.dir, device, mode, info, s.start))
		if ctx.Err() != nil {
			return
		}
		if ready {
			if err != nil {
				s.mu.Lock()
				s.err = err
				s.mu.Unlock()
				slog.Warn("browser playback interrupted", "mode", mode, "err", err)
			}
			return
		}
		lastErr = err
		slog.Warn("browser conversion attempt failed", "mode", mode, "err", err)
		if err := os.RemoveAll(s.dir); err != nil {
			lastErr = fmt.Errorf("reset playback buffer: %w", err)
			break
		}
		if err := os.Mkdir(s.dir, 0o700); err != nil {
			lastErr = fmt.Errorf("recreate playback buffer: %w", err)
			break
		}
	}
	s.ready <- fmt.Errorf("could not prepare browser video: %w", lastErr)
}

func (s *playbackSession) runAttempt(ctx context.Context, ffmpeg string, args []string) (bool, error) {
	cmd := exec.CommandContext(ctx, ffmpeg, args...)
	var stderr processLog
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		return false, fmt.Errorf("start browser conversion: %w", err)
	}
	finished := make(chan error, 1)
	go func() { finished <- cmd.Wait() }()
	ticker := time.NewTicker(playbackPollInterval)
	defer ticker.Stop()
	ready := false
	markReady := func() {
		if ready {
			return
		}
		playlist, err := os.ReadFile(filepath.Join(s.dir, playbackPlaylistName))
		if err == nil && bytes.Contains(playlist, []byte("#EXTINF:")) {
			ready = true
			s.ready <- nil
			slog.Debug("browser stream ready", "session", s.token, "start", s.start, "copy_video", s.copy)
		}
	}
	for {
		select {
		case <-ticker.C:
			markReady()
		case err := <-finished:
			markReady()
			if err != nil {
				return ready, fmt.Errorf("convert browser video: %w: %s", err, strings.TrimSpace(stderr.String()))
			}
			if !ready {
				return false, errors.New("conversion produced no video segments")
			}
			return true, nil
		}
	}
}

func playbackArgs(path, dir, device, mode string, info playbackInfo, start float64) []string {
	args := []string{"-hide_banner", "-loglevel", "error", "-nostdin", "-y"}
	if mode == "vaapi" || mode == "vaapi-upload" {
		args = append(args, "-vaapi_device", device)
	}
	if mode == "vaapi" {
		args = append(args, "-hwaccel", "vaapi", "-hwaccel_output_format", "vaapi")
	}
	args = append(args, "-ss", strconv.FormatFloat(start, 'f', 3, 64), "-readrate", "1", "-threads", playbackCPUThreads, "-i", path,
		"-map", fmt.Sprintf("0:%d", info.Video.Index), "-sn", "-dn", "-map_metadata", "-1")
	if info.Audio != nil {
		args = append(args, "-map", fmt.Sprintf("0:%d", info.Audio.Index))
		if info.Audio.Codec == "aac" && info.Audio.Profile == "LC" {
			args = append(args, "-c:a", "copy")
		} else {
			args = append(args, "-c:a", "aac", "-b:a", "160k", "-ac", "2")
		}
	}
	switch mode {
	case "copy":
		args = append(args, "-c:v", "copy")
	case "vaapi", "vaapi-upload":
		filter := fmt.Sprintf("scale_vaapi=w='min(%d,iw)':h='min(%d,ih)':force_original_aspect_ratio=decrease:force_divisible_by=2:format=nv12", playbackMaxWidth, playbackMaxHeight)
		if mode == "vaapi-upload" {
			filter = "format=nv12,hwupload," + filter
		}
		args = append(args, "-vf", filter, "-c:v", "h264_vaapi", "-profile:v", "high", "-b:v", "6M", "-maxrate", playbackMaxRate, "-bufsize", playbackBufferSize, "-bf", "0")
	default:
		filter := fmt.Sprintf("scale=w='min(%d,iw)':h='min(%d,ih)':force_original_aspect_ratio=decrease:force_divisible_by=2,setsar=1", playbackMaxWidth, playbackMaxHeight)
		args = append(args, "-vf", filter,
			"-c:v", "libx264", "-preset", "ultrafast", "-crf", "23", "-pix_fmt", "yuv420p", "-maxrate", playbackMaxRate, "-bufsize", playbackBufferSize, "-threads", playbackCPUThreads, "-filter_threads", "1")
	}
	if mode != "copy" {
		args = append(args, "-force_key_frames", "expr:gte(t,n_forced*"+playbackSegmentTime+")", "-flags", "+cgop")
	}
	// Restarting on seek avoids encoding skipped video and bounds disk use.
	args = append(args, "-f", "hls", "-hls_segment_type", "fmp4", "-hls_time", playbackSegmentTime,
		"-hls_list_size", "12", "-hls_delete_threshold", "2", "-hls_flags", "delete_segments+temp_file",
		"-hls_fmp4_init_filename", playbackInitName, "-hls_segment_filename", filepath.Join(dir, "segment%06d.m4s"), filepath.Join(dir, playbackPlaylistName))
	return args
}
