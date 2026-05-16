package media

import "testing"

func TestWatchHistoryRecord(t *testing.T) {
	cases := []struct {
		name      string
		actions   []*Item // items to Record, in order
		wantIDs   []string
		wantTitle string // expected title at front of list
	}{
		{
			name:      "single item",
			actions:   []*Item{{ID: "1", Title: "A"}},
			wantIDs:   []string{"1"},
			wantTitle: "A",
		},
		{
			name:      "two items most recent first",
			actions:   []*Item{{ID: "1", Title: "A"}, {ID: "2", Title: "B"}},
			wantIDs:   []string{"2", "1"},
			wantTitle: "B",
		},
		{
			name:      "duplicate id deduped to front",
			actions:   []*Item{{ID: "1", Title: "A"}, {ID: "2", Title: "B"}, {ID: "1", Title: "A"}},
			wantIDs:   []string{"1", "2"},
			wantTitle: "A",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			h := &WatchHistory{}
			for _, item := range c.actions {
				h.Record(item)
			}
			got := h.items // direct read — List() filters by file existence
			if len(got) != len(c.wantIDs) {
				t.Fatalf("len(items) = %d, want %d", len(got), len(c.wantIDs))
			}
			for i, want := range c.wantIDs {
				if got[i].ID != want {
					t.Fatalf("items[%d].ID = %q, want %q", i, got[i].ID, want)
				}
			}
			if got[0].Title != c.wantTitle {
				t.Fatalf("items[0].Title = %q, want %q", got[0].Title, c.wantTitle)
			}
		})
	}
}

func TestWatchHistoryCappedAtMax(t *testing.T) {
	h := &WatchHistory{}
	for i := 0; i < maxHistory+5; i++ {
		h.Record(&Item{ID: string(rune('a' + i)), Title: "x"})
	}
	if len(h.items) != maxHistory {
		t.Fatalf("len(items) = %d, want %d", len(h.items), maxHistory)
	}
}
