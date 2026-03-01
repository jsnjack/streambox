package media

import "sync"

const maxHistory = 10

// WatchedItem is a record of a played file.
type WatchedItem struct {
	ID    string
	Title string
	Path  string
}

// WatchHistory tracks the last N played files, most recent first.
type WatchHistory struct {
	mu    sync.Mutex
	items []WatchedItem
}

// Record adds an item to the front of the history, deduplicating by ID.
func (h *WatchHistory) Record(item *Item) {
	h.mu.Lock()
	defer h.mu.Unlock()
	// Remove existing entry for this ID.
	filtered := h.items[:0]
	for _, w := range h.items {
		if w.ID != item.ID {
			filtered = append(filtered, w)
		}
	}
	// Prepend and cap.
	h.items = append([]WatchedItem{{ID: item.ID, Title: item.Title, Path: item.Path}}, filtered...)
	if len(h.items) > maxHistory {
		h.items = h.items[:maxHistory]
	}
}

// List returns a snapshot of the watch history.
func (h *WatchHistory) List() []WatchedItem {
	h.mu.Lock()
	defer h.mu.Unlock()
	out := make([]WatchedItem, len(h.items))
	copy(out, h.items)
	return out
}

// Remove drops an item from the history by ID.
func (h *WatchHistory) Remove(id string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	filtered := h.items[:0]
	for _, w := range h.items {
		if w.ID != id {
			filtered = append(filtered, w)
		}
	}
	h.items = filtered
}
