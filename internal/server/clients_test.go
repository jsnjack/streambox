package server

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestDetectClient(t *testing.T) {
	tests := []struct {
		name      string
		userAgent string
		want      clientKind
	}{
		{name: "2012 TV control request", userAgent: "SEC_HHP_[TV]UE40ES8000/1.0 DLNADOC/1.50", want: clientSamsung},
		{name: "2024 TV with an edited name", userAgent: `DLNADOC/1.50 SEC_HHP_55" OLED/1.0 UPnP/1.0`, want: clientSamsung},
		{name: "Samsung media request", userAgent: "samsung-agent/1.1", want: clientSamsung},
		{name: "other client", userAgent: "VLC/3.0.24 LibVLC/3.0.24", want: clientUnknown},
		{name: "no user agent", userAgent: "", want: clientUnknown},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := detectClient(test.userAgent); got != test.want {
				t.Errorf("detectClient(%q) = %q, want %q", test.userAgent, got, test.want)
			}
		})
	}
}

func TestClientRegistryIdentify(t *testing.T) {
	type request struct{ remoteAddr, userAgent string }
	samsungControl := request{remoteAddr: "192.0.2.10:40000", userAgent: "SEC_HHP_[TV]UE40ES8000/1.0 DLNADOC/1.50"}
	tests := []struct {
		name    string
		earlier []request
		request request
		want    clientKind
	}{
		{
			name:    "media request after a Samsung control request from the same IP",
			earlier: []request{samsungControl},
			request: request{remoteAddr: "192.0.2.10:40001"},
			want:    clientSamsung,
		},
		{
			name:    "request from another IP",
			earlier: []request{samsungControl},
			request: request{remoteAddr: "192.0.2.11:40000"},
			want:    clientUnknown,
		},
		{
			name:    "unknown client with no history",
			request: request{remoteAddr: "192.0.2.10:40000"},
			want:    clientUnknown,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			newRequest := func(req request) *http.Request {
				r := httptest.NewRequest("GET", "/files/x", nil)
				r.RemoteAddr = req.remoteAddr
				if req.userAgent != "" {
					r.Header.Set("User-Agent", req.userAgent)
				}
				return r
			}
			var registry clientRegistry
			for _, earlier := range test.earlier {
				registry.identify(newRequest(earlier))
			}
			if got := registry.identify(newRequest(test.request)); got != test.want {
				t.Errorf("identify = %q, want %q", got, test.want)
			}
		})
	}
}
