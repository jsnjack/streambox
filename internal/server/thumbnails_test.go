package server

import (
	"bytes"
	"container/list"
	"context"
	"encoding/xml"
	"errors"
	"fmt"
	"image"
	"image/jpeg"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"streambox/internal/media"
)

func testThumbnails(generate func(context.Context, string) ([]byte, error)) *thumbnailCache {
	return &thumbnailCache{
		entries: make(map[string]*list.Element),
		gate:    make(chan struct{}, 1), generate: generate,
	}
}

func testJPEG(t *testing.T) []byte {
	t.Helper()
	var output bytes.Buffer
	if err := jpeg.Encode(&output, image.NewRGBA(image.Rect(0, 0, 160, 90)), nil); err != nil {
		t.Fatalf("encode test thumbnail: %v", err)
	}
	return output.Bytes()
}

func TestServeThumbnail(t *testing.T) {
	for _, name := range []string{"GET", "HEAD", "range", "conditional", "unknown ID", "container", "deleted source", "disabled", "generation failure", "POST"} {
		t.Run(name, func(t *testing.T) {
			library := testLibrary(t)
			item := library.AllItems()[0]
			history := &media.WatchHistory{}
			s := newTestServer(t, Config{Library: library, History: history})
			jpegData := testJPEG(t)
			calls := 0
			s.thumbnails = testThumbnails(func(context.Context, string) ([]byte, error) {
				calls++
				if name == "generation failure" {
					return nil, errors.New("invalid video")
				}
				return jpegData, nil
			})
			method, id, wantStatus := http.MethodGet, item.ID, http.StatusOK
			switch name {
			case "HEAD":
				method = http.MethodHead
			case "range":
				wantStatus = http.StatusPartialContent
			case "conditional":
				wantStatus = http.StatusNotModified
			case "unknown ID":
				id, wantStatus = "unknown", http.StatusNotFound
			case "container":
				id, wantStatus = "0", http.StatusNotFound
			case "deleted source":
				if err := os.Remove(item.Path); err != nil {
					t.Fatal(err)
				}
				wantStatus = http.StatusNotFound
			case "disabled":
				s.thumbnails, wantStatus = nil, http.StatusNotFound
			case "generation failure":
				wantStatus = http.StatusServiceUnavailable
			case "POST":
				method, wantStatus = http.MethodPost, http.StatusMethodNotAllowed
			}
			request := httptest.NewRequest(method, "/thumbnails/"+id, nil)
			if name == "range" {
				request.Header.Set("Range", "bytes=0-9")
			}
			if name == "conditional" {
				first := httptest.NewRecorder()
				s.mux.ServeHTTP(first, httptest.NewRequest(http.MethodGet, request.URL.String(), nil))
				request.Header.Set("If-None-Match", first.Header().Get("ETag"))
			}
			response := httptest.NewRecorder()
			s.mux.ServeHTTP(response, request)
			if response.Code != wantStatus {
				t.Fatalf("status = %d, want %d: %s", response.Code, wantStatus, response.Body.String())
			}
			if name == "GET" && !bytes.Equal(response.Body.Bytes(), jpegData) {
				t.Error("response does not contain the generated JPEG")
			}
			if (name == "HEAD" || name == "conditional") && response.Body.Len() != 0 {
				t.Error("response must have no body")
			}
			if name == "range" && !bytes.Equal(response.Body.Bytes(), jpegData[:10]) {
				t.Error("response does not contain the requested range")
			}
			if wantStatus == http.StatusOK || wantStatus == http.StatusPartialContent {
				if got := response.Header().Get("Content-Type"); got != "image/jpeg" {
					t.Errorf("Content-Type = %q", got)
				}
				if got := response.Header().Get("contentFeatures.dlna.org"); !strings.Contains(got, "DLNA.ORG_PN=JPEG_TN") {
					t.Errorf("missing thumbnail profile: %q", got)
				}
			}
			if name == "conditional" && calls != 1 {
				t.Errorf("generated thumbnail %d times, want 1", calls)
			}
			if len(history.List()) != 0 {
				t.Error("thumbnail request recorded watch history")
			}
		})
	}
}

func TestThumbnailListings(t *testing.T) {
	for _, enabled := range []bool{true, false} {
		t.Run(fmt.Sprintf("enabled=%t", enabled), func(t *testing.T) {
			library := testLibrary(t)
			item := library.AllItems()[0]
			history := &media.WatchHistory{}
			history.Record(item)
			s := newTestServer(t, Config{Library: library, History: history, IP: "192.0.2.1", Port: 8080})
			s.thumbnails = nil
			if enabled {
				s.thumbnails = testThumbnails(func(context.Context, string) ([]byte, error) {
					t.Error("listing must not generate thumbnails")
					return nil, errors.New("unexpected generation")
				})
			}
			var didl struct {
				Items []struct {
					Parent string `xml:"parentID,attr"`
					Art    struct {
						URL     string `xml:",chardata"`
						Profile string `xml:"urn:schemas-dlna-org:metadata-1-0/ profileID,attr"`
					} `xml:"urn:schemas-upnp-org:metadata-1-0/upnp/ albumArtURI"`
				} `xml:"item"`
			}
			if err := xml.Unmarshal([]byte(s.buildDIDL([]media.Object{item}, "2")), &didl); err != nil {
				t.Fatal(err)
			}
			if len(didl.Items) != 1 || didl.Items[0].Parent != "2" {
				t.Fatalf("unexpected DIDL: %+v", didl)
			}
			art := didl.Items[0].Art
			if enabled && (art.URL != "http://192.0.2.1:8080/thumbnails/"+item.ID || art.Profile != "JPEG_TN") {
				t.Errorf("unexpected album art: %+v", art)
			}
			if !enabled && art.URL != "" {
				t.Error("disabled thumbnails advertised to DLNA")
			}
			response := httptest.NewRecorder()
			s.mux.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/ui", nil))
			got := strings.Count(response.Body.String(), `src="/thumbnails/`+item.ID+`"`)
			want := 0
			if enabled {
				want = 2 // All and Recently Watched share the same endpoint.
			}
			if got != want {
				t.Errorf("UI thumbnail count = %d, want %d", got, want)
			}
		})
	}
}

func TestThumbnailCache(t *testing.T) {
	for _, name := range []string{"reuses frame", "size change", "mtime change", "temporary failure", "failure retry", "concurrent requests", "cancelled wait", "bounded cache"} {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "video.mp4")
			if err := os.WriteFile(path, []byte("video"), 0o600); err != nil {
				t.Fatal(err)
			}
			info, err := os.Stat(path)
			if err != nil {
				t.Fatal(err)
			}
			var calls atomic.Int32
			c := testThumbnails(func(context.Context, string) ([]byte, error) {
				calls.Add(1)
				if strings.Contains(name, "failure") {
					return nil, errors.New("cannot decode")
				}
				return []byte("frame"), nil
			})
			get := func(ctx context.Context, path string, info os.FileInfo) {
				t.Helper()
				data, err := c.get(ctx, path, info)
				if strings.Contains(name, "failure") {
					if err == nil {
						t.Error("expected generation failure")
					}
				} else if err != nil || string(data) != "frame" {
					t.Errorf("get = %q, %v", data, err)
				}
			}
			ctx := context.Background()
			wantCalls := int32(1)
			switch name {
			case "cancelled wait":
				c.gate <- struct{}{}
				cancelCtx, cancel := context.WithCancel(ctx)
				cancel()
				if _, err := c.get(cancelCtx, path, info); !errors.Is(err, context.Canceled) {
					t.Errorf("expected cancellation, got %v", err)
				}
				<-c.gate
				wantCalls = 0
			case "concurrent requests":
				var wg sync.WaitGroup
				for i := 0; i < 20; i++ {
					wg.Add(1)
					go func() { defer wg.Done(); get(ctx, path, info) }()
				}
				wg.Wait()
			case "bounded cache":
				for i := 0; i <= thumbnailLimit; i++ {
					get(ctx, fmt.Sprint(i), info)
				}
				if len(c.entries) != thumbnailLimit {
					t.Errorf("cache size = %d", len(c.entries))
				}
				get(ctx, "1", info) // Recently used entries survive eviction.
				get(ctx, "0", info) // The least recently used frame is regenerated.
				wantCalls = thumbnailLimit + 2
			default:
				get(ctx, path, info)
				switch name {
				case "size change":
					if err := os.WriteFile(path, []byte("changed video"), 0o600); err != nil {
						t.Fatal(err)
					}
					if err := os.Chtimes(path, info.ModTime(), info.ModTime()); err != nil {
						t.Fatal(err)
					}
					wantCalls = 2
				case "mtime change":
					modified := info.ModTime().Add(time.Second)
					if err := os.Chtimes(path, modified, modified); err != nil {
						t.Fatal(err)
					}
					wantCalls = 2
				case "failure retry":
					entry := c.entries[path].Value.(thumbnailEntry)
					entry.retryAt = time.Now().Add(-time.Second)
					c.entries[path].Value = entry
					wantCalls = 2
				}
				info, err = os.Stat(path)
				if err != nil {
					t.Fatal(err)
				}
				get(ctx, path, info)
			}
			if calls.Load() != wantCalls {
				t.Errorf("generation calls = %d, want %d", calls.Load(), wantCalls)
			}
		})
	}
}
