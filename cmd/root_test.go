package cmd

import (
	"os"
	"path/filepath"
	"testing"
)

func TestExpandHome(t *testing.T) {
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatalf("UserHomeDir: %v", err)
	}
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"bare tilde", "~", home},
		{"tilde slash", "~/", home},
		{"tilde with subpath", "~/Videos", filepath.Join(home, "Videos")},
		{"absolute path untouched", "/var/data", "/var/data"},
		{"relative path untouched", "relative/path", "relative/path"},
		{"empty stays empty", "", ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := expandHome(c.in); got != c.want {
				t.Fatalf("expandHome(%q) = %q, want %q", c.in, got, c.want)
			}
		})
	}
}

func TestDataDirRespectsXDG(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", "/tmp/xdg-test")
	got, err := dataDir()
	if err != nil {
		t.Fatalf("dataDir: %v", err)
	}
	want := "/tmp/xdg-test/streambox"
	if got != want {
		t.Fatalf("dataDir() = %q, want %q", got, want)
	}
}

func TestDataDirFallsBackToHome(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", "")
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatalf("UserHomeDir: %v", err)
	}
	got, err := dataDir()
	if err != nil {
		t.Fatalf("dataDir: %v", err)
	}
	want := filepath.Join(home, ".local", "share", "streambox")
	if got != want {
		t.Fatalf("dataDir() = %q, want %q", got, want)
	}
}
