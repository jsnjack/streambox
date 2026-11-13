package server

import (
	"net/http"
	"sync"
	"time"
)

const (
	// watchThreshold is how much of a file one client must receive before the
	// file counts as watched. Samsung TVs fetch the start of files to animate
	// previews while browsing, and TVs read file indexes with HEAD requests
	// and short ranges; those stay below it.
	watchThreshold = 32 << 20

	// watchWindow is how long a client's byte count for a file survives
	// between requests, so playback split across range requests adds up.
	watchWindow = 10 * time.Minute
)

// watchTracker counts the bytes each client receives of each file and
// reports when a file has been played far enough to record.
type watchTracker struct {
	mu     sync.Mutex
	counts map[watchKey]*watchCount
}

type watchKey struct{ client, itemID string }

type watchCount struct {
	bytes    int64
	last     time.Time
	recorded bool
}

// add credits n bytes of a file of the given size to a client and reports
// whether the file has just become watched by that client.
func (t *watchTracker) add(client, itemID string, size, n int64, now time.Time) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	key := watchKey{client: client, itemID: itemID}
	count := t.counts[key]
	if count == nil || now.Sub(count.last) > watchWindow {
		t.purge(now)
		if t.counts == nil {
			t.counts = make(map[watchKey]*watchCount)
		}
		count = &watchCount{}
		t.counts[key] = count
	}
	count.bytes += n
	count.last = now
	if count.recorded || count.bytes < watchedAfter(size) {
		return false
	}
	count.recorded = true
	return true
}

// purge drops counts that have been idle for longer than watchWindow.
func (t *watchTracker) purge(now time.Time) {
	for key, count := range t.counts {
		if now.Sub(count.last) > watchWindow {
			delete(t.counts, key)
		}
	}
}

// watchedAfter returns how many bytes of a file make it watched.
func watchedAfter(size int64) int64 {
	return max(1, min(watchThreshold, size/2))
}

// countingWriter reports the size of every reply body write to onWrite. It
// has no ReadFrom method, so file data goes through Write instead of
// sendfile; at video bitrates the extra copy costs little.
type countingWriter struct {
	http.ResponseWriter
	onWrite func(n int)
}

func (w *countingWriter) Write(p []byte) (int, error) {
	n, err := w.ResponseWriter.Write(p)
	w.onWrite(n)
	return n, err
}

// Unwrap lets http.ResponseController reach the underlying writer.
func (w *countingWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }
