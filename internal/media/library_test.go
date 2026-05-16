package media

import "testing"

func TestCleanTitle(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"year cuts at year", "The.Dark.Knight.2008.1080p.BluRay.x264", "The Dark Knight"},
		{"episode keeps code", "Breaking.Bad.S03E07.720p", "Breaking Bad S03E07"},
		{"no year no episode", "some.movie.name", "Some Movie Name"},
		{"lowercase episode normalises", "show.s01e02.web", "Show S01E02"},
		{"trailing dot trimmed", "Movie.2010.", "Movie"},
		{"already clean", "Casablanca 1942", "Casablanca"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := cleanTitle(c.in)
			if got != c.want {
				t.Fatalf("cleanTitle(%q) = %q, want %q", c.in, got, c.want)
			}
		})
	}
}
