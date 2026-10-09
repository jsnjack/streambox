package server

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestBrowserCodecCompatibility(t *testing.T) {
	for _, test := range []struct {
		name, path, video, pixel, audio, audioProfile string
		wantDirect, wantCopy                          bool
	}{
		{name: "MP4 H264 AAC plays directly", path: "film.mp4", video: "h264", pixel: "yuv420p", audio: "aac", audioProfile: "LC", wantDirect: true, wantCopy: true},
		{name: "silent MP4 plays directly", path: "film.MP4", video: "h264", pixel: "yuv420p", wantDirect: true, wantCopy: true},
		{name: "MKV copies H264 video", path: "film.mkv", video: "h264", pixel: "yuv420p", audio: "aac", audioProfile: "LC", wantCopy: true},
		{name: "AC3 only needs audio conversion", path: "film.mp4", video: "h264", pixel: "yuv420p", audio: "ac3", wantCopy: true},
		{name: "HE AAC is converted conservatively", path: "film.mp4", video: "h264", pixel: "yuv420p", audio: "aac", audioProfile: "HE-AAC", wantCopy: true},
		{name: "HEVC needs video conversion", path: "film.mp4", video: "hevc", pixel: "yuv420p", audio: "aac"},
		{name: "10 bit H264 needs video conversion", path: "film.mp4", video: "h264", pixel: "yuv420p10le"},
		{name: "VP9 WebM plays directly", path: "film.webm", video: "vp9", pixel: "yuv420p", audio: "opus", wantDirect: true},
		{name: "VP8 WebM plays directly", path: "film.webm", video: "vp8", pixel: "yuv420p", audio: "vorbis", wantDirect: true},
		{name: "QuickTime is remuxed", path: "film.mov", video: "h264", pixel: "yuv420p", wantCopy: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			info := playbackInfo{Video: videoStream{Codec: test.video, PixelFormat: test.pixel, Profile: "High", Level: 41}}
			if test.audio != "" {
				info.Audio = &videoStream{Codec: test.audio, Profile: test.audioProfile}
			}
			if got := directMediaType(test.path, info); (got != "") != test.wantDirect {
				t.Errorf("direct media type = %q, want direct %t", got, test.wantDirect)
			}
			if got := info.copyVideo(); got != test.wantCopy {
				t.Errorf("copy video = %t, want %t", got, test.wantCopy)
			}
		})
	}
}

func TestProcessLogRetainsRecentErrors(t *testing.T) {
	for _, test := range []struct {
		name   string
		chunks []string
	}{
		{name: "small output", chunks: []string{"decode ", "failed"}},
		{name: "large single write", chunks: []string{strings.Repeat("old errors\n", processLogLimit) + "encoder failed"}},
		{name: "many writes", chunks: []string{strings.Repeat("a", processLogLimit), strings.Repeat("b", processLogLimit/2), "device unavailable"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			var log processLog
			for _, chunk := range test.chunks {
				n, err := log.Write([]byte(chunk))
				if err != nil || n != len(chunk) {
					t.Fatalf("write = %d, %v", n, err)
				}
			}
			want := strings.Join(test.chunks, "")
			if len(want) > processLogLimit {
				want = want[len(want)-processLogLimit:]
			}
			if got := log.String(); got != want {
				t.Error("log lost the most recent error context")
			}
		})
	}
}

func TestPlaybackCostAndResolution(t *testing.T) {
	for _, mode := range []string{"copy", "vaapi", "vaapi-upload", "software"} {
		t.Run(mode, func(t *testing.T) {
			info := playbackInfo{Video: videoStream{Index: 1}, Audio: &videoStream{Index: 2, Codec: "ac3"}}
			args := strings.Join(playbackArgs("/videos/film.mkv", "/buffer", "/dev/dri/renderD128", mode, info, 120), " ")
			for _, required := range []string{"-readrate 1", "-ss 120.000", "-map 0:1", "-map 0:2", "-c:a aac", "-hls_segment_type fmp4", "delete_segments+temp_file"} {
				if !strings.Contains(args, required) {
					t.Errorf("missing %q in %s", required, args)
				}
			}
			if mode == "copy" {
				if !strings.Contains(args, "-c:v copy") || strings.Contains(args, "-vf") {
					t.Error("remuxing must avoid video decoding filters and encoding")
				}
			} else {
				if !strings.Contains(args, "1080") || !strings.Contains(args, "1920") || strings.Contains(args, "720") {
					t.Error("conversion must retain up to 1080p")
				}
				if mode == "vaapi" && !strings.Contains(args, "-hwaccel vaapi -hwaccel_output_format vaapi") {
					t.Error("hardware conversion must also try hardware decoding")
				}
				if mode == "software" && (!strings.Contains(args, "-preset ultrafast") || !strings.Contains(args, "-threads 2")) {
					t.Error("software fallback must limit CPU cost")
				}
			}
		})
	}
}

func cachedPlaybackServer(t *testing.T) (*Server, string) {
	t.Helper()
	library := testLibrary(t)
	s := newTestServer(t, Config{Library: library})
	t.Cleanup(s.Close)
	item := library.AllItems()[0]
	stat, err := os.Stat(item.Path)
	if err != nil {
		t.Fatal(err)
	}
	s.playback.ffprobe = "/not-used-for-cached-metadata"
	s.playback.probes[item.Path] = probedVideo{size: stat.Size(), modTime: stat.ModTime(), info: playbackInfo{
		Duration: 120, DirectType: `video/mp4; codecs="avc1.640029,mp4a.40.2"`,
		Video: videoStream{Codec: "h264", PixelFormat: "yuv420p"},
	}}
	return s, item.ID
}

func TestBrowserPlaybackRoutes(t *testing.T) {
	for _, test := range []struct {
		name, method, target, body string
		status                     int
	}{
		{name: "watch page", method: "GET", target: "/ui/watch?id=ITEM", status: 200},
		{name: "video metadata", method: "GET", target: "/ui/playback?id=ITEM", status: 200},
		{name: "unknown video", method: "GET", target: "/ui/playback?id=missing", status: 404},
		{name: "metadata method", method: "POST", target: "/ui/playback?id=ITEM", status: 405},
		{name: "start method", method: "GET", target: "/ui/playback/start", status: 405},
		{name: "malformed request", method: "POST", target: "/ui/playback/start", body: "{", status: 400},
		{name: "unknown start video", method: "POST", target: "/ui/playback/start", body: `{"id":"missing"}`, status: 404},
		{name: "negative seek", method: "POST", target: "/ui/playback/start", body: `{"id":"ITEM","start":-1}`, status: 400},
		{name: "seek past end", method: "POST", target: "/ui/playback/start", body: `{"id":"ITEM","start":120}`, status: 400},
		{name: "missing ffmpeg", method: "POST", target: "/ui/playback/start", body: `{"id":"ITEM"}`, status: 503},
		{name: "stop missing session is idempotent", method: "POST", target: "/ui/playback/stop?token=gone", status: 204},
		{name: "expired session", method: "GET", target: "/playback/gone/index.m3u8", status: 410},
		{name: "private buffer files inaccessible", method: "GET", target: "/playback/gone/ffmpeg.log", status: 404},
		{name: "nested paths inaccessible", method: "GET", target: "/playback/gone/nested/init.mp4", status: 404},
		{name: "player script", method: "GET", target: "/ui/assets/player.js", status: 200},
		{name: "player styles", method: "GET", target: "/ui/assets/player.css", status: 200},
		{name: "local HLS bundle", method: "GET", target: "/ui/assets/hls.light.min.js", status: 200},
		{name: "private asset", method: "GET", target: "/ui/assets/README.md", status: 404},
	} {
		t.Run(test.name, func(t *testing.T) {
			s, id := cachedPlaybackServer(t)
			s.playback.ffmpeg = ""
			r := httptest.NewRequest(test.method, strings.ReplaceAll(test.target, "ITEM", id), strings.NewReader(strings.ReplaceAll(test.body, "ITEM", id)))
			w := httptest.NewRecorder()
			s.mux.ServeHTTP(w, r)
			if w.Code != test.status {
				t.Fatalf("status = %d, want %d: %s", w.Code, test.status, w.Body.String())
			}
			if test.name == "watch page" {
				for _, want := range []string{`data-id="` + id + `"`, `id="timeline"`, `id="fullscreen"`, "/ui/assets/player.js"} {
					if !strings.Contains(w.Body.String(), want) {
						t.Errorf("watch page missing %q", want)
					}
				}
				if strings.Contains(w.Body.String(), "requestFullscreen") {
					t.Error("watch page must not force fullscreen on load")
				}
			}
			if test.name == "video metadata" {
				var data struct {
					Duration   float64
					DirectURL  string
					CanConvert bool
				}
				if err := json.Unmarshal(w.Body.Bytes(), &data); err != nil {
					t.Fatal(err)
				}
				if data.Duration != 120 || data.DirectURL != "/files/"+id || data.CanConvert {
					t.Errorf("unexpected metadata: %+v", data)
				}
			}
		})
	}
}

func TestPlaybackAdmission(t *testing.T) {
	for _, name := range []string{"negative start", "NaN start", "infinite start", "unknown duration", "already converting", "closed"} {
		t.Run(name, func(t *testing.T) {
			m := &browserPlayback{ffmpeg: "/not-executed", sessions: make(map[string]*playbackSession)}
			info, start := playbackInfo{Duration: 120}, 0.0
			switch name {
			case "negative start":
				start = -1
			case "NaN start":
				start = math.NaN()
			case "infinite start":
				start = math.Inf(1)
			case "unknown duration":
				info.Duration = 0
			case "already converting":
				m.sessions["active"] = &playbackSession{copy: false}
			case "closed":
				m.closed = true
			}
			_, err := m.start(context.Background(), "video.mp4", info, start, false)
			if err == nil {
				t.Fatal("expected admission failure")
			}
			if (name == "already converting" || name == "closed") && !errors.Is(err, errPlaybackBusy) {
				t.Errorf("expected busy error, got %v", err)
			}
		})
	}
}

func TestCloseBrowserPlayback(t *testing.T) {
	for _, name := range []string{"no sessions", "active buffer"} {
		t.Run(name, func(t *testing.T) {
			s := newTestServer(t, Config{})
			dir := ""
			cancelled := false
			if name == "active buffer" {
				dir = filepath.Join(t.TempDir(), "buffer")
				if err := os.Mkdir(dir, 0o700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(dir, playbackInitName), []byte("buffer"), 0o600); err != nil {
					t.Fatal(err)
				}
				done := make(chan struct{})
				close(done)
				s.playback.sessions["session"] = &playbackSession{dir: dir, done: done, cancel: func() { cancelled = true }}
			}
			s.Close()
			s.Close()
			if len(s.playback.sessions) != 0 || !s.playback.closed {
				t.Error("close must retire all playback sessions")
			}
			if dir != "" {
				if !cancelled {
					t.Error("close did not cancel conversion")
				}
				if _, err := os.Stat(dir); !os.IsNotExist(err) {
					t.Errorf("buffer still exists: %v", err)
				}
			}
		})
	}
}
