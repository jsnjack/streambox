package server

import (
	"bytes"
	"io"
	"log/slog"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	"streambox/internal/loglevel"
)

const (
	// traceBodyLimit caps how much of a SOAP request or reply the trace log keeps.
	traceBodyLimit = 16 << 10

	// chunkingThreshold is the reply size above which net/http switches to
	// chunked transfer encoding when the handler sets no Content-Length.
	chunkingThreshold = 2048

	// rootProbeLimit root BrowseMetadata requests within rootProbeWindow,
	// with no BrowseDirectChildren in between, trigger a warning. Samsung TVs
	// repeat that request about once a second when they cannot parse the reply.
	rootProbeLimit  = 4
	rootProbeWindow = 10 * time.Second
)

// logRequests logs every request at debug level. When trace logging is on, it
// also logs request headers, SOAP bodies in both directions, and the reply
// status, size and transfer encoding.
func (s *Server) logRequests(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx := r.Context()
		client := s.clients.identify(r)
		slog.Debug("http",
			slog.String("method", r.Method),
			slog.String("path", r.URL.Path),
			slog.String("remote", r.RemoteAddr),
			slog.String("client", string(client)))
		if !slog.Default().Enabled(ctx, loglevel.LevelTrace) {
			next.ServeHTTP(w, r)
			return
		}

		soap := r.Method == http.MethodPost
		var requestBody []byte
		if soap {
			requestBody = peekBody(r)
		}
		slog.Log(ctx, loglevel.LevelTrace, "http request",
			slog.String("method", r.Method),
			slog.String("uri", r.RequestURI),
			slog.String("proto", r.Proto),
			slog.String("host", r.Host),
			slog.String("remote", r.RemoteAddr),
			slog.String("client", string(client)),
			slog.String("headers", formatHeaders(r.Header)),
			slog.String("body", string(requestBody)))

		rec := &recordingWriter{ResponseWriter: w}
		if soap {
			rec.capture = &bytes.Buffer{}
		}
		start := time.Now()
		next.ServeHTTP(rec, r)

		replyBody := ""
		if rec.capture != nil {
			replyBody = rec.capture.String()
		}
		slog.Log(ctx, loglevel.LevelTrace, "http response",
			slog.String("method", r.Method),
			slog.String("uri", r.RequestURI),
			slog.String("remote", r.RemoteAddr),
			slog.Int("status", rec.statusCode()),
			slog.Int64("bytes", rec.written),
			slog.Bool("chunked", rec.chunked(r)),
			slog.Duration("duration", time.Since(start)),
			slog.String("body", replyBody))
	})
}

// peekBody reads up to traceBodyLimit bytes of the request body and puts them
// back, so the handler still reads the whole body.
func peekBody(r *http.Request) []byte {
	head, err := io.ReadAll(io.LimitReader(r.Body, traceBodyLimit))
	if err != nil {
		slog.Log(r.Context(), loglevel.LevelTrace, "trace: read request body", "err", err)
	}
	r.Body = struct {
		io.Reader
		io.Closer
	}{io.MultiReader(bytes.NewReader(head), r.Body), r.Body}
	return head
}

// formatHeaders renders headers as "Name: value" pairs in a stable order.
func formatHeaders(h http.Header) string {
	names := make([]string, 0, len(h))
	for name := range h {
		names = append(names, name)
	}
	sort.Strings(names)
	parts := make([]string, len(names))
	for i, name := range names {
		parts[i] = name + ": " + strings.Join(h[name], ", ")
	}
	return strings.Join(parts, "; ")
}

// recordingWriter records the status and size of a reply and, when capture
// is set, its first traceBodyLimit bytes.
type recordingWriter struct {
	http.ResponseWriter
	status  int
	written int64
	capture *bytes.Buffer
}

func (w *recordingWriter) WriteHeader(status int) {
	if w.status == 0 {
		w.status = status
	}
	w.ResponseWriter.WriteHeader(status)
}

func (w *recordingWriter) Write(p []byte) (int, error) {
	n, err := w.ResponseWriter.Write(p)
	w.written += int64(n)
	if w.capture != nil && w.capture.Len() < traceBodyLimit {
		w.capture.Write(p[:min(n, traceBodyLimit-w.capture.Len())])
	}
	return n, err
}

// ReadFrom keeps net/http's sendfile path for file replies.
func (w *recordingWriter) ReadFrom(src io.Reader) (int64, error) {
	if rf, ok := w.ResponseWriter.(io.ReaderFrom); ok && w.capture == nil {
		n, err := rf.ReadFrom(src)
		w.written += n
		return n, err
	}
	return io.Copy(writerOnly{w}, src)
}

// Unwrap lets http.ResponseController reach the underlying writer.
func (w *recordingWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

func (w *recordingWriter) statusCode() int {
	if w.status == 0 {
		return http.StatusOK
	}
	return w.status
}

// chunked reports whether net/http sent the reply with chunked encoding.
func (w *recordingWriter) chunked(r *http.Request) bool {
	return r.ProtoAtLeast(1, 1) &&
		r.Method != http.MethodHead &&
		w.Header().Get("Content-Length") == "" &&
		w.written > chunkingThreshold
}

// writerOnly hides a writer's ReadFrom method so io.Copy falls back to Write.
type writerOnly struct{ io.Writer }

// rootProbes detects clients that keep requesting the root container's
// metadata without ever browsing its children.
type rootProbes struct {
	mu   sync.Mutex
	byIP map[string]*rootProbe
}

type rootProbe struct {
	first  time.Time
	count  int
	warned bool
}

// observe records a Browse request from ip and reports whether that client
// has just crossed rootProbeLimit. It reports each client once until the
// client browses children.
func (p *rootProbes) observe(ip, browseFlag, objectID string, now time.Time) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	if browseFlag == browseDirectChildren {
		delete(p.byIP, ip)
		return false
	}
	if browseFlag != browseMetadata || objectID != rootObjectID {
		return false
	}
	if p.byIP == nil {
		p.byIP = make(map[string]*rootProbe)
	}
	probe := p.byIP[ip]
	if probe == nil {
		probe = &rootProbe{first: now}
		p.byIP[ip] = probe
	}
	if now.Sub(probe.first) > rootProbeWindow {
		probe.first = now
		probe.count = 0
	}
	probe.count++
	if probe.count < rootProbeLimit || probe.warned {
		return false
	}
	probe.warned = true
	return true
}
