package cmd

import (
	"context"
	"io"
	"log/slog"
	"os"

	"streambox/internal/loglevel"
)

// LevelTrace is re-exported here for backward compatibility within the cmd
// package. New callers should reference loglevel.LevelTrace directly.
const LevelTrace = loglevel.LevelTrace

// L is the package-wide structured logger configured by initLogger.
var L *slog.Logger

// initLogger configures the default slog logger from the --debug / --trace
// flags. Returns a cleanup func that closes the trace file (if any).
//
// Behaviour:
//   - tracePath set:       write at LevelTrace to that file (truncated).
//   - level == "debug":    write at LevelDebug to stderr.
//   - otherwise (default): write info-and-above to stderr.
func initLogger(tracePath, level string) func() {
	var w io.Writer = os.Stderr
	cleanup := func() {}
	if tracePath != "" {
		f, err := os.OpenFile(tracePath, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
		if err == nil {
			w = f
			cleanup = func() {
				if cerr := f.Close(); cerr != nil {
					slog.Log(context.Background(), LevelTrace, "trace file close", "err", cerr)
				}
			}
		}
	}
	lvl := slog.LevelInfo
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
