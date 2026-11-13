package server

import (
	"bytes"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"streambox/internal/loglevel"
)

func TestRootProbesObserve(t *testing.T) {
	type browse struct {
		flag     string
		objectID string
		after    time.Duration
	}
	rootMetadata := browse{flag: browseMetadata, objectID: rootObjectID, after: time.Second}
	tests := []struct {
		name     string
		requests []browse
		wantWarn []bool
	}{
		{
			name:     "repeated root metadata warns once",
			requests: []browse{rootMetadata, rootMetadata, rootMetadata, rootMetadata, rootMetadata},
			wantWarn: []bool{false, false, false, true, false},
		},
		{
			name: "browsing children resets the count",
			requests: []browse{rootMetadata, rootMetadata, rootMetadata,
				{flag: browseDirectChildren, objectID: rootObjectID, after: time.Second},
				rootMetadata},
			wantWarn: []bool{false, false, false, false, false},
		},
		{
			name: "requests spread beyond the window do not warn",
			requests: []browse{rootMetadata, rootMetadata, rootMetadata,
				{flag: browseMetadata, objectID: rootObjectID, after: rootProbeWindow}},
			wantWarn: []bool{false, false, false, false},
		},
		{
			name: "metadata of other objects does not count",
			requests: []browse{
				{flag: browseMetadata, objectID: "1"}, {flag: browseMetadata, objectID: "1"},
				{flag: browseMetadata, objectID: "1"}, {flag: browseMetadata, objectID: "1"}},
			wantWarn: []bool{false, false, false, false},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var probes rootProbes
			now := time.Unix(1_700_000_000, 0)
			for i, request := range test.requests {
				now = now.Add(request.after)
				if got := probes.observe("192.0.2.10", request.flag, request.objectID, now); got != test.wantWarn[i] {
					t.Errorf("request %d: warn = %v, want %v", i, got, test.wantWarn[i])
				}
			}
		})
	}
}

func TestLogRequests(t *testing.T) {
	tests := []struct {
		name        string
		level       slog.Level
		requestBody string
		replySize   int
		wantLogged  []string
		wantMissing []string
	}{
		{
			name:        "trace logs SOAP bodies and a chunked reply",
			level:       loglevel.LevelTrace,
			requestBody: "<s:Envelope>browse</s:Envelope>",
			replySize:   chunkingThreshold + 1,
			wantLogged:  []string{`msg="http request"`, `body=<s:Envelope>browse</s:Envelope>`, "Soapaction: Browse", "status=200", "chunked=true"},
		},
		{
			name:        "trace passes a body larger than the log limit through whole",
			level:       loglevel.LevelTrace,
			requestBody: strings.Repeat("x", traceBodyLimit+10),
			replySize:   10,
			wantLogged:  []string{"chunked=false", "bytes=10"},
		},
		{
			name:        "debug logs only the request line",
			level:       slog.LevelDebug,
			requestBody: "<s:Envelope>browse</s:Envelope>",
			replySize:   10,
			wantLogged:  []string{`msg=http`, "client=unknown"},
			wantMissing: []string{`msg="http request"`, "<s:Envelope>"},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var logs bytes.Buffer
			previous := slog.Default()
			slog.SetDefault(slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: test.level})))
			t.Cleanup(func() { slog.SetDefault(previous) })

			var received string
			handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				body, err := io.ReadAll(r.Body)
				if err != nil {
					t.Errorf("read body: %v", err)
				}
				received = string(body)
				if _, err := w.Write(bytes.Repeat([]byte("r"), test.replySize)); err != nil {
					t.Errorf("write reply: %v", err)
				}
			})
			s := newTestServer(t, Config{})
			request := httptest.NewRequest("POST", "/contentdirectory/control", strings.NewReader(test.requestBody))
			request.Header.Set("SOAPACTION", "Browse")
			s.logRequests(handler).ServeHTTP(httptest.NewRecorder(), request)

			if received != test.requestBody {
				t.Errorf("handler received %d bytes, want %d", len(received), len(test.requestBody))
			}
			for _, want := range test.wantLogged {
				if !strings.Contains(logs.String(), want) {
					t.Errorf("log does not contain %q:\n%s", want, logs.String())
				}
			}
			for _, unwanted := range test.wantMissing {
				if strings.Contains(logs.String(), unwanted) {
					t.Errorf("log contains %q:\n%s", unwanted, logs.String())
				}
			}
		})
	}
}
