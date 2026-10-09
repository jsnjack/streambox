//go:build integration

package server

import (
	"bytes"
	"context"
	"image/jpeg"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

func TestExtractThumbnail(t *testing.T) {
	ffmpeg, err := exec.LookPath("ffmpeg")
	if err != nil {
		t.Fatalf("FFmpeg is required for thumbnail integration tests: %v", err)
	}
	for _, test := range []struct {
		name     string
		size     string
		duration string
		width    int
		height   int
	}{
		{name: "short clip falls back to start", size: "320x180", duration: "1", width: 160, height: 90},
		{name: "portrait frame fits JPEG_TN", size: "240x400", duration: "6", width: 96, height: 160},
	} {
		t.Run(test.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "-video with spaces.mp4")
			ctx, cancel := context.WithTimeout(context.Background(), thumbnailTimeout)
			defer cancel()
			cmd := exec.CommandContext(ctx, ffmpeg, "-hide_banner", "-loglevel", "error",
				"-f", "lavfi", "-i", "testsrc2=size="+test.size+":rate=10",
				"-t", test.duration, "-threads", "1", "-c:v", "mpeg4", path)
			if output, err := cmd.CombinedOutput(); err != nil {
				t.Fatalf("create test video: %v: %s", err, output)
			}
			data, err := extractThumbnail(ctx, ffmpeg, path)
			if err != nil {
				t.Fatal(err)
			}
			frame, err := jpeg.Decode(bytes.NewReader(data))
			if err != nil {
				t.Fatalf("decode frame: %v", err)
			}
			if bounds := frame.Bounds(); bounds.Dx() != test.width || bounds.Dy() != test.height {
				t.Errorf("frame bounds = %v, want %dx%d", bounds, test.width, test.height)
			}
		})
	}
	for _, name := range []string{"corrupt video", "missing video", "cancelled generation"} {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "invalid.mp4")
			if name != "missing video" {
				if err := os.WriteFile(path, []byte("not a video"), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			if name == "cancelled generation" {
				cancel()
			}
			if _, err := extractThumbnail(ctx, ffmpeg, path); err == nil {
				t.Error("expected frame extraction to fail")
			}
		})
	}
}
