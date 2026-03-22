package config

import "github.com/BurntSushi/toml"

// DefaultConfig is the template written by `streambox config init`.
const DefaultConfig = `# streambox configuration
# Edit this file, then run: streambox serve --config ~/.config/streambox/config.toml

# Path to the directory containing your video files (required).
media_dir = "~/Videos"

# HTTP port the server listens on.
port = 8080

# Friendly name shown on the TV's media source list.
name = "StreamBox"

# Files modified within this many days appear in the "Recent" folder.
# Set to 0 to disable the Recent folder.
recent_days = 14

# Write log output to this file in addition to stderr.
# Leave empty to log to stderr only.
log_file = "/tmp/streambox.log"

# Enable verbose debug logging (HTTP requests, SSDP activity).
debug = false

# When enabled, streambox watches for new subfolders inside media_dir.
# Once a subfolder's contents are stable (no changes for 5 seconds),
# all video files are moved into media_dir and the subfolder is removed.
# Useful when a download client places each release in its own folder.
# flatten = false
`

// Config holds all streambox settings.
type Config struct {
	MediaDir   string `toml:"media_dir"`
	Port       int    `toml:"port"`
	Name       string `toml:"name"`
	RecentDays int    `toml:"recent_days"`
	Debug      bool   `toml:"debug"`
	LogFile    string `toml:"log_file"`
	// Flatten moves video files from newly-detected subfolders into the root
	// media_dir and removes the now-empty subfolder. Useful when a downloader
	// puts each release in its own directory. The folder is only processed once
	// its contents stop changing (5-second stability window).
	Flatten bool `toml:"flatten"`
}

// Defaults returns a Config populated with sensible defaults.
func Defaults() Config {
	return Config{
		MediaDir:   "~/Videos",
		Port:       8080,
		Name:       "StreamBox",
		RecentDays: 14,
		LogFile:    "/tmp/streambox.log",
	}
}

// Load reads a TOML file and returns the merged config (defaults + file values).
func Load(path string) (Config, error) {
	cfg := Defaults()
	if _, err := toml.DecodeFile(path, &cfg); err != nil {
		return cfg, err
	}
	return cfg, nil
}
