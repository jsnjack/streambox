package config

import "github.com/BurntSushi/toml"

// Config holds all streambox settings.
type Config struct {
	MediaDir   string `toml:"media_dir"`
	Port       int    `toml:"port"`
	Name       string `toml:"name"`
	RecentDays int    `toml:"recent_days"`
	Debug      bool   `toml:"debug"`
	LogFile    string `toml:"log_file"`
}

// Defaults returns a Config populated with sensible defaults.
func Defaults() Config {
	return Config{
		Port:       8080,
		Name:       "StreamBox",
		RecentDays: 14,
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
