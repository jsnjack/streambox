// Package loglevel exposes the custom slog level used for --trace output.
// Lives in its own package so any subsystem can log at trace level without
// importing cmd.
package loglevel

import "log/slog"

// LevelTrace is the slog level used for --trace output (max detail).
const LevelTrace = slog.Level(-8)
