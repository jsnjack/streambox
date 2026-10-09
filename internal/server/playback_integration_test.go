//go:build integration

package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"streambox/internal/media"
)

func playbackFixture(t *testing.T, codec, audio, extension, size, duration string) string {
	t.Helper()
	ffmpeg, err := exec.LookPath("ffmpeg")
	if err != nil {
		t.Fatalf("FFmpeg is required for playback integration tests: %v", err)
	}
	path := filepath.Join(t.TempDir(), "video with spaces."+extension)
	args := []string{"-hide_banner", "-loglevel", "error", "-f", "lavfi", "-i", "testsrc2=size=" + size + ":rate=10",
		"-f", "lavfi", "-i", "sine=frequency=440:sample_rate=48000", "-t", duration,
		"-c:v", codec, "-pix_fmt", "yuv420p", "-threads", "2", "-g", "20", "-c:a", audio}
	if codec == "libx264" {
		args = append(args, "-preset", "ultrafast", "-tune", "zerolatency")
	}
	args = append(args, path)
	ctx, cancel := context.WithTimeout(context.Background(), playbackStartTimeout)
	defer cancel()
	if output, err := exec.CommandContext(ctx, ffmpeg, args...).CombinedOutput(); err != nil {
		t.Fatalf("create playback video: %v: %s", err, output)
	}
	return path
}

func TestPlaybackConversion(t *testing.T) {
	for _, test := range []struct {
		name, codec, audio, size string
		start                    float64
		force, copy              bool
	}{
		{name: "H264 and AAC remux without encoding", codec: "libx264", audio: "aac", size: "320x180", copy: true},
		{name: "H264 and AC3 only convert audio", codec: "libx264", audio: "ac3", size: "320x180", copy: true},
		{name: "seeking preserves remux with accurate preroll", codec: "libx264", audio: "aac", size: "320x180", start: 3.25, copy: true},
		{name: "hardware failure falls back at 1080p", codec: "mpeg4", audio: "ac3", size: "2560x1440"},
		{name: "browser decode failure forces video encoding", codec: "libx264", audio: "aac", size: "320x180", force: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			path := playbackFixture(t, test.codec, test.audio, "mkv", test.size, "8")
			m := newBrowserPlayback()
			m.device = "/nonexistent-render-device"
			t.Cleanup(m.close)
			info, err := m.inspect(context.Background(), path)
			if err != nil {
				t.Fatal(err)
			}
			session, err := m.start(context.Background(), path, info, test.start, test.force)
			if err != nil {
				t.Fatal(err)
			}
			if session.copy != test.copy {
				t.Errorf("copy video = %t, want %t", session.copy, test.copy)
			}
			if test.start > 0 && (session.start != 2 || session.start > test.start) {
				t.Errorf("keyframe position = %f, want 2", session.start)
			}
			playlist, err := os.ReadFile(filepath.Join(session.dir, playbackPlaylistName))
			if err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(string(playlist), `#EXT-X-MAP:URI="init.mp4"`) {
				t.Errorf("missing fMP4 initialization: %s", playlist)
			}
			init, err := os.ReadFile(filepath.Join(session.dir, playbackInitName))
			if err != nil {
				t.Fatal(err)
			}
			segment, err := os.ReadFile(filepath.Join(session.dir, "segment000000.m4s"))
			if err != nil {
				t.Fatal(err)
			}
			fragment := filepath.Join(t.TempDir(), "fragment.mp4")
			if err := os.WriteFile(fragment, append(init, segment...), 0o600); err != nil {
				t.Fatal(err)
			}
			output, err := m.inspect(context.Background(), fragment)
			if err != nil {
				t.Fatal(err)
			}
			if output.Video.Codec != "h264" || output.Audio == nil || output.Audio.Codec != "aac" {
				t.Errorf("incompatible output: %+v", output)
			}
			if test.size == "2560x1440" && (output.Video.Width != 1920 || output.Video.Height != 1080) {
				t.Errorf("output = %dx%d, want 1920x1080", output.Video.Width, output.Video.Height)
			}
			if !session.copy {
				if _, err := m.start(context.Background(), path, info, 0, true); err != errPlaybackBusy {
					t.Errorf("second video encode must be rejected: %v", err)
				}
			}
			m.stop(session.token)
			if _, err := os.Stat(session.dir); !os.IsNotExist(err) {
				t.Errorf("buffer not removed: %v", err)
			}
			if _, ok := m.get(session.token); ok {
				t.Error("stopped session remains accessible")
			}
		})
	}
}

func TestPlaybackHTTPConversion(t *testing.T) {
	t.Run("start serve and stop", func(t *testing.T) {
		path := playbackFixture(t, "mpeg4", "ac3", "mkv", "320x180", "8")
		library, err := media.NewLibrary(filepath.Dir(path), 0, 1)
		if err != nil {
			t.Fatal(err)
		}
		history := &media.WatchHistory{}
		s := newTestServer(t, Config{Library: library, History: history})
		s.playback.device = ""
		t.Cleanup(s.Close)
		item := library.AllItems()[0]
		request := httptest.NewRequest(http.MethodPost, "/ui/playback/start", strings.NewReader(`{"id":"`+item.ID+`","start":0}`))
		response := httptest.NewRecorder()
		s.mux.ServeHTTP(response, request)
		if response.Code != http.StatusOK {
			t.Fatalf("start status = %d: %s", response.Code, response.Body.String())
		}
		var result struct{ Token, URL string }
		if err := json.Unmarshal(response.Body.Bytes(), &result); err != nil {
			t.Fatal(err)
		}
		for _, name := range []string{playbackPlaylistName, playbackInitName, "segment000000.m4s"} {
			w := httptest.NewRecorder()
			s.mux.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/playback/"+result.Token+"/"+name, nil))
			if w.Code != http.StatusOK || w.Body.Len() == 0 {
				t.Errorf("%s status = %d, bytes = %d", name, w.Code, w.Body.Len())
			}
		}
		if got := len(history.List()); got != 1 {
			t.Errorf("history = %d, want 1 entry for the video", got)
		}
		w := httptest.NewRecorder()
		s.mux.ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/ui/playback/stop?token="+result.Token, nil))
		if w.Code != http.StatusNoContent {
			t.Errorf("stop status = %d", w.Code)
		}
		w = httptest.NewRecorder()
		s.mux.ServeHTTP(w, httptest.NewRequest(http.MethodGet, result.URL, nil))
		if w.Code != http.StatusGone {
			t.Errorf("stopped stream status = %d", w.Code)
		}
	})
	t.Run("cancelled startup cleans buffer", func(t *testing.T) {
		path := playbackFixture(t, "mpeg4", "ac3", "mkv", "320x180", "8")
		m := newBrowserPlayback()
		t.Cleanup(m.close)
		info, err := m.inspect(context.Background(), path)
		if err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		if _, err := m.start(ctx, path, info, 0, false); err == nil {
			t.Error("cancelled startup should fail")
		}
		if len(m.sessions) != 0 {
			t.Error("cancelled startup left an active session")
		}
	})
}
