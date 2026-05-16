package cmd

import (
	"context"
	"log/slog"
	"os"

	"streambox/internal/loglevel"
)

// LevelTrace is re-exported here for backward compatibility within the cmd
// package. New callers should reference loglevel.LevelTrace directly.
const LevelTrace = loglevel.LevelTrace

// L is the package-wide structured logger configured by initLogger.
var L *slog.Logger

// initLogger wires up a fan-out logger:
//   - stderr always receives INFO+ (or DEBUG+ when debugStderr is true).
//   - If tracePath is non-empty, an additional sink at LevelTrace writes to
//     that file (truncated each run).
//
// --debug and --trace are independent: both can be enabled together.
// Returns a cleanup func that closes the trace file (if any).
func initLogger(tracePath string, debugStderr bool) func() {
	stderrLevel := slog.LevelInfo
	if debugStderr {
		stderrLevel = slog.LevelDebug
	}
	handlers := []slog.Handler{
		slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: stderrLevel}),
	}

	cleanup := func() {}
	if tracePath != "" {
		f, err := os.OpenFile(tracePath, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
		if err == nil {
			handlers = append(handlers, slog.NewTextHandler(f, &slog.HandlerOptions{Level: LevelTrace}))
			cleanup = func() {
				if cerr := f.Close(); cerr != nil {
					slog.Log(context.Background(), LevelTrace, "trace file close", "err", cerr)
				}
			}
		}
	}

	h := handlers[0]
	if len(handlers) > 1 {
		h = &fanoutHandler{handlers: handlers}
	}
	L = slog.New(h)
	slog.SetDefault(L)
	return cleanup
}

// fanoutHandler dispatches each record to every wrapped handler that accepts
// the record's level. Each child enforces its own level threshold, so stderr
// can run at INFO/DEBUG while a trace file runs at LevelTrace.
type fanoutHandler struct {
	handlers []slog.Handler
}

func (m *fanoutHandler) Enabled(ctx context.Context, level slog.Level) bool {
	for _, h := range m.handlers {
		if h.Enabled(ctx, level) {
			return true
		}
	}
	return false
}

func (m *fanoutHandler) Handle(ctx context.Context, r slog.Record) error {
	for _, h := range m.handlers {
		if !h.Enabled(ctx, r.Level) {
			continue
		}
		if err := h.Handle(ctx, r.Clone()); err != nil {
			return err
		}
	}
	return nil
}

func (m *fanoutHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	nh := make([]slog.Handler, len(m.handlers))
	for i, h := range m.handlers {
		nh[i] = h.WithAttrs(attrs)
	}
	return &fanoutHandler{handlers: nh}
}

func (m *fanoutHandler) WithGroup(name string) slog.Handler {
	nh := make([]slog.Handler, len(m.handlers))
	for i, h := range m.handlers {
		nh[i] = h.WithGroup(name)
	}
	return &fanoutHandler{handlers: nh}
}
