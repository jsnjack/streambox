package server

import (
	"net/http/httptest"
	"testing"
	"time"

	"streambox/internal/media"
)

func TestWatchTrackerAdd(t *testing.T) {
	type delivery struct {
		client string
		bytes  int64
		after  time.Duration
		want   bool
	}
	const mib = 1 << 20
	tests := []struct {
		name       string
		size       int64
		deliveries []delivery
	}{
		{
			name: "reaching the threshold records once",
			size: 4096 * mib,
			deliveries: []delivery{
				{client: "a", bytes: 16 * mib, want: false},
				{client: "a", bytes: 16 * mib, want: true},
				{client: "a", bytes: 16 * mib, want: false},
			},
		},
		{
			name: "a preview below the threshold does not record",
			size: 4096 * mib,
			deliveries: []delivery{
				{client: "a", bytes: 12 * mib, want: false},
			},
		},
		{
			name: "small files record after half is sent",
			size: 10,
			deliveries: []delivery{
				{client: "a", bytes: 4, want: false},
				{client: "a", bytes: 1, want: true},
			},
		},
		{
			name: "clients count separately",
			size: 4096 * mib,
			deliveries: []delivery{
				{client: "a", bytes: 20 * mib, want: false},
				{client: "b", bytes: 20 * mib, want: false},
			},
		},
		{
			name: "an idle gap restarts the count",
			size: 4096 * mib,
			deliveries: []delivery{
				{client: "a", bytes: 20 * mib, want: false},
				{client: "a", bytes: 20 * mib, after: watchWindow + time.Second, want: false},
			},
		},
		{
			name: "playing again after the window records again",
			size: 4096 * mib,
			deliveries: []delivery{
				{client: "a", bytes: 40 * mib, want: true},
				{client: "a", bytes: 40 * mib, after: watchWindow + time.Second, want: true},
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var tracker watchTracker
			now := time.Unix(1_700_000_000, 0)
			for i, d := range test.deliveries {
				now = now.Add(d.after)
				if got := tracker.add(d.client, "item", test.size, d.bytes, now); got != d.want {
					t.Errorf("delivery %d: watched = %v, want %v", i, got, d.want)
				}
			}
		})
	}
}

func TestServeFileWatchHistory(t *testing.T) {
	type request struct{ method, rangeHeader string }
	tests := []struct {
		name     string
		requests []request
		want     int
	}{
		{name: "HEAD does not count", requests: []request{{method: "HEAD"}}, want: 0},
		{name: "a tail range does not count", requests: []request{{method: "GET", rangeHeader: "bytes=4-"}}, want: 0},
		{name: "ranges from one client add up", requests: []request{
			{method: "GET", rangeHeader: "bytes=0-0"},
			{method: "GET", rangeHeader: "bytes=1-1"},
		}, want: 1},
		{name: "a full GET counts", requests: []request{{method: "GET"}}, want: 1},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			library := testLibrary(t)
			history := &media.WatchHistory{}
			s := newTestServer(t, Config{Library: library, History: history})
			item := library.AllItems()[0]
			for _, req := range test.requests {
				r := httptest.NewRequest(req.method, "/files/"+item.ID, nil)
				if req.rangeHeader != "" {
					r.Header.Set("Range", req.rangeHeader)
				}
				s.mux.ServeHTTP(httptest.NewRecorder(), r)
			}
			if got := len(history.List()); got != test.want {
				t.Errorf("history entries = %d, want %d", got, test.want)
			}
		})
	}
}
