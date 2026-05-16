package cmd

import (
	"io"
	"log/slog"
	"os"
)

// LevelTrace is the slog level used for --trace output (max detail).
const LevelTrace = slog.Level(-8)

// L is the package-wide structured logger configured by initLogger.
var L *slog.Logger

// initLogger configures the default slog logger from the --debug / --trace
// flags. Returns a cleanup func that closes the trace file (if any).
//
// Behaviour:
//   - tracePath set:       write at LevelTrace to that file (truncated).
//   - level == "debug":    write at LevelDebug to stderr.
//   - otherwise (default): write warnings/errors to stderr.
func initLogger(tracePath, level string) func() {
	var w io.Writer = os.Stderr
	cleanup := func() {}
	if tracePath != "" {
		f, err := os.OpenFile(tracePath, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
		if err == nil {
			w = f
			cleanup = func() { _ = f.Close() }
		}
	}
	lvl := slog.LevelWarn
	switch level {
	case "debug":
		lvl = slog.LevelDebug
	case "trace":
		lvl = LevelTrace
	}
	h := slog.NewTextHandler(w, &slog.HandlerOptions{Level: lvl})
	L = slog.New(h)
	slog.SetDefault(L)
	return cleanup
}
