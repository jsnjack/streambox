package server

import (
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"streambox/internal/media"
)

func TestServeUIItemActions(t *testing.T) {
	library := testLibrary(t)
	history := &media.WatchHistory{}
	item := library.AllItems()[0]
	history.Record(item)
	server := newTestServer(t, Config{
		Library: library,
		History: history,
	})

	request := httptest.NewRequest("GET", "/ui", nil)
	response := httptest.NewRecorder()
	server.mux.ServeHTTP(response, request)

	wants := []string{
		`class="download" href="/files/` + item.ID + `?download=1" download>Download</a>`,
		`class="discard" href="/ui/discard?id=` + item.ID + `">Discard</a>`,
		`class="del" href="/ui/delete?id=` + item.ID,
		`.actions a.download{display:none}`,
		`document.addEventListener('touchstart'`,
	}
	for _, want := range wants {
		if !strings.Contains(response.Body.String(), want) {
			t.Errorf("UI does not contain %q", want)
		}
	}
}

func TestServeFileDownload(t *testing.T) {
	tests := []struct {
		name               string
		targetSuffix       string
		wantDisposition    string
		wantHistoryEntries int
	}{
		{
			name:               "playback records history",
			wantHistoryEntries: 1,
		},
		{
			name:               "download uses original filename without recording history",
			targetSuffix:       "?download=1",
			wantDisposition:    `attachment; filename="A Movie.mp4"`,
			wantHistoryEntries: 0,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			library := testLibrary(t)
			history := &media.WatchHistory{}
			server := newTestServer(t, Config{
				Library: library,
				History: history,
			})
			item := library.AllItems()[0]

			request := httptest.NewRequest("GET", "/files/"+item.ID+test.targetSuffix, nil)
			response := httptest.NewRecorder()
			server.mux.ServeHTTP(response, request)

			if response.Code != 200 {
				t.Fatalf("status = %d, want 200", response.Code)
			}
			if got := response.Header().Get("Content-Disposition"); got != test.wantDisposition {
				t.Errorf("Content-Disposition = %q, want %q", got, test.wantDisposition)
			}
			if got := len(history.List()); got != test.wantHistoryEntries {
				t.Errorf("history entries = %d, want %d", got, test.wantHistoryEntries)
			}
		})
	}
}

func testLibrary(t *testing.T) *media.Library {
	t.Helper()
	directory := t.TempDir()
	path := filepath.Join(directory, "A Movie.mp4")
	if err := os.WriteFile(path, []byte("video"), 0o600); err != nil {
		t.Fatalf("write test video: %v", err)
	}
	library, err := media.NewLibrary(directory, 0, 1)
	if err != nil {
		t.Fatalf("build test library: %v", err)
	}
	return library
}
