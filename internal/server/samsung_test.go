package server

import (
	"bufio"
	"encoding/xml"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"streambox/internal/media"
)

// trickyNames are file names whose titles need XML escaping.
var trickyNames = []string{`Ocean's "Eleven" & Co.mp4`, "Tom <and> Jerry.mkv"}

func TestEscXML(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  string
	}{
		{name: "predefined entities by name", input: `Ocean's "Eleven" & <Co>`, want: "Ocean&apos;s &quot;Eleven&quot; &amp; &lt;Co&gt;"},
		{name: "tabs and line breaks become spaces", input: "a\tb\nc\rd", want: "a b c d"},
		{name: "forbidden control character", input: "a\x01b", want: "a�b"},
		{name: "invalid UTF-8", input: "a\xffb", want: "a�b"},
		{name: "non-ASCII text unchanged", input: "Амели 🎬", want: "Амели 🎬"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := escXML(test.input); got != test.want {
				t.Errorf("escXML(%q) = %q, want %q", test.input, got, test.want)
			}
		})
	}
}

func TestSamsungCaptures(t *testing.T) {
	tests := []struct {
		capture        string
		wantContainers []string
	}{
		{capture: "samsung/browse-root-metadata.http", wantContainers: []string{rootObjectID}},
		{capture: "samsung/browse-empty-objectid.http", wantContainers: []string{"1", "2"}},
	}
	for _, test := range tests {
		t.Run(test.capture, func(t *testing.T) {
			s := newTestServer(t, Config{Library: libraryWithFiles(t, trickyNames...)})
			response := httptest.NewRecorder()
			s.mux.ServeHTTP(response, loadCapture(t, test.capture))

			if response.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200:\n%s", response.Code, response.Body.String())
			}
			assertNoNumericReferences(t, response.Body.String())
			listing := parseBrowseReply(t, response.Body.String())
			var got []string
			for _, container := range listing.Containers {
				got = append(got, container.ID)
			}
			if strings.Join(got, ",") != strings.Join(test.wantContainers, ",") {
				t.Errorf("containers = %v, want %v", got, test.wantContainers)
			}
		})
	}
}

func TestSamsungFeatureList(t *testing.T) {
	s := newTestServer(t, Config{Library: libraryWithFiles(t, trickyNames...)})
	response := httptest.NewRecorder()
	s.mux.ServeHTTP(response, loadCapture(t, "samsung/x-getfeaturelist.http"))

	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200:\n%s", response.Code, response.Body.String())
	}
	assertNoNumericReferences(t, response.Body.String())
	var envelope struct {
		FeatureList string `xml:"Body>X_GetFeatureListResponse>FeatureList"`
	}
	if err := xml.Unmarshal(response.Body.Bytes(), &envelope); err != nil {
		t.Fatalf("SOAP reply does not parse: %v", err)
	}
	var features struct {
		Feature struct {
			Name       string `xml:"name,attr"`
			Containers []struct {
				ID   string `xml:"id,attr"`
				Type string `xml:"type,attr"`
			} `xml:"container"`
		} `xml:"Feature"`
	}
	if err := xml.Unmarshal([]byte(envelope.FeatureList), &features); err != nil {
		t.Fatalf("FeatureList does not parse: %v\n%s", err, envelope.FeatureList)
	}
	if features.Feature.Name != "samsung.com_BASICVIEW" {
		t.Errorf("feature = %q, want samsung.com_BASICVIEW", features.Feature.Name)
	}
	for _, container := range features.Feature.Containers {
		t.Run(container.Type, func(t *testing.T) {
			if container.ID != rootObjectID {
				t.Errorf("container id = %q, want the root %q", container.ID, rootObjectID)
			}
		})
	}
}

func TestSamsungSetBookmark(t *testing.T) {
	s := newTestServer(t, Config{})
	response := httptest.NewRecorder()
	s.mux.ServeHTTP(response, soapRequest("X_SetBookmark",
		"<CategoryType>1</CategoryType><RID>0</RID><ObjectID>1</ObjectID><PosSecond>42</PosSecond>"))

	if response.Code != http.StatusOK {
		t.Errorf("status = %d, want 200:\n%s", response.Code, response.Body.String())
	}
	if !strings.Contains(response.Body.String(), "X_SetBookmarkResponse") {
		t.Errorf("reply is not an X_SetBookmarkResponse:\n%s", response.Body.String())
	}
}

func TestContentDirectorySCPD(t *testing.T) {
	var scpd struct {
		Actions []struct {
			Name      string   `xml:"name"`
			Variables []string `xml:"argumentList>argument>relatedStateVariable"`
		} `xml:"actionList>action"`
		Variables []string `xml:"serviceStateTable>stateVariable>name"`
	}
	if err := xml.Unmarshal([]byte(contentDirSCPDXML), &scpd); err != nil {
		t.Fatalf("SCPD does not parse: %v", err)
	}
	declared := map[string]bool{}
	for _, name := range scpd.Variables {
		declared[name] = true
	}
	actions := map[string]bool{}
	for _, action := range scpd.Actions {
		actions[action.Name] = true
		t.Run(action.Name, func(t *testing.T) {
			for _, variable := range action.Variables {
				if !declared[variable] {
					t.Errorf("argument refers to undeclared state variable %q", variable)
				}
			}
		})
	}
	for _, want := range []string{"Browse", "X_GetFeatureList", "X_SetBookmark"} {
		if !actions[want] {
			t.Errorf("SCPD does not declare %s", want)
		}
	}
}

func TestBrowseTitlesSurviveEscaping(t *testing.T) {
	library := libraryWithFiles(t, trickyNames...)
	s := newTestServer(t, Config{Library: library})
	response := httptest.NewRecorder()
	s.mux.ServeHTTP(response, soapRequest("Browse",
		"<ObjectID>1</ObjectID><BrowseFlag>BrowseDirectChildren</BrowseFlag>"+
			"<Filter>*</Filter><StartingIndex>0</StartingIndex><RequestedCount>0</RequestedCount>"))

	assertNoNumericReferences(t, response.Body.String())
	listing := parseBrowseReply(t, response.Body.String())
	got := map[string]bool{}
	for _, item := range listing.Items {
		got[item.Title] = true
	}
	for _, item := range library.AllItems() {
		t.Run(item.Title, func(t *testing.T) {
			if !got[item.Title] {
				t.Errorf("title %q missing from listing %v", item.Title, got)
			}
		})
	}
}

func TestDeviceDescriptionEscapesName(t *testing.T) {
	tests := []struct{ name string }{
		{name: `Living room "TV" & more`},
		{name: "Kid's <TV>"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			s := newTestServer(t, Config{Name: test.name})
			response := httptest.NewRecorder()
			s.mux.ServeHTTP(response, httptest.NewRequest("GET", "/device.xml", nil))

			assertNoNumericReferences(t, response.Body.String())
			var root struct {
				FriendlyName string `xml:"device>friendlyName"`
			}
			if err := xml.Unmarshal(response.Body.Bytes(), &root); err != nil {
				t.Fatalf("device.xml does not parse: %v", err)
			}
			if root.FriendlyName != test.name {
				t.Errorf("friendlyName = %q, want %q", root.FriendlyName, test.name)
			}
		})
	}
}

// didlListing is the part of a DIDL-Lite document the tests inspect.
type didlListing struct {
	Containers []struct {
		ID       string `xml:"id,attr"`
		ParentID string `xml:"parentID,attr"`
		Title    string `xml:"title"`
	} `xml:"container"`
	Items []struct {
		ID    string `xml:"id,attr"`
		Title string `xml:"title"`
	} `xml:"item"`
}

// parseBrowseReply extracts and parses the DIDL-Lite listing in a Browse reply.
func parseBrowseReply(t *testing.T, body string) didlListing {
	t.Helper()
	var envelope struct {
		Result string `xml:"Body>BrowseResponse>Result"`
	}
	if err := xml.Unmarshal([]byte(body), &envelope); err != nil {
		t.Fatalf("SOAP reply does not parse: %v\n%s", err, body)
	}
	var listing didlListing
	if err := xml.Unmarshal([]byte(envelope.Result), &listing); err != nil {
		t.Fatalf("DIDL-Lite does not parse: %v\n%s", err, envelope.Result)
	}
	return listing
}

// assertNoNumericReferences fails when text contains a numeric character
// reference such as &#34; at either escaping level. Samsung TVs show folders
// as empty when a Browse reply contains one.
func assertNoNumericReferences(t *testing.T, text string) {
	t.Helper()
	if i := strings.Index(text, "&#"); i >= 0 {
		t.Errorf("reply contains a numeric character reference near %q", text[max(0, i-40):min(len(text), i+20)])
	}
	if i := strings.Index(text, "&amp;#"); i >= 0 {
		t.Errorf("reply contains an escaped numeric reference near %q", text[max(0, i-40):min(len(text), i+20)])
	}
}

// soapRequest builds a ContentDirectory SOAP request for action.
func soapRequest(action, arguments string) *http.Request {
	body := `<?xml version="1.0" encoding="utf-8"?>` +
		`<s:Envelope xmlns:s="http://schemas.xmlsoap.org/soap/envelope/" s:encodingStyle="http://schemas.xmlsoap.org/soap/encoding/">` +
		`<s:Body><u:` + action + ` xmlns:u="` + contentDirNS + `">` + arguments + `</u:` + action + `></s:Body></s:Envelope>`
	r := httptest.NewRequest("POST", "/contentdirectory/control", strings.NewReader(body))
	r.Header.Set("Content-Type", `text/xml; charset="utf-8"`)
	r.Header.Set("SOAPACTION", `"`+contentDirNS+"#"+action+`"`)
	return r
}

// loadCapture reads a raw HTTP request captured from a real client. Captures
// use LF line endings; the body follows the first blank line.
func loadCapture(t *testing.T, name string) *http.Request {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatalf("read capture: %v", err)
	}
	head, body, found := strings.Cut(string(raw), "\n\n")
	if !found {
		t.Fatalf("capture %s has no blank line after its headers", name)
	}
	r, err := http.ReadRequest(bufio.NewReader(strings.NewReader(strings.ReplaceAll(head, "\n", "\r\n") + "\r\n\r\n")))
	if err != nil {
		t.Fatalf("parse capture %s: %v", name, err)
	}
	body = strings.TrimRight(body, "\n")
	r.Body = io.NopCloser(strings.NewReader(body))
	r.ContentLength = int64(len(body))
	r.RemoteAddr = "192.0.2.10:40000"
	return r
}

// libraryWithFiles builds a library from empty video files with the given names.
func libraryWithFiles(t *testing.T, names ...string) *media.Library {
	t.Helper()
	directory := t.TempDir()
	for _, name := range names {
		if err := os.WriteFile(filepath.Join(directory, name), []byte("video"), 0o600); err != nil {
			t.Fatalf("write test video: %v", err)
		}
	}
	library, err := media.NewLibrary(directory, 0, 1)
	if err != nil {
		t.Fatalf("build test library: %v", err)
	}
	return library
}
