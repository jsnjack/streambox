package server

import (
	"net"
	"net/http"
	"strings"
	"sync"
)

// clientKind is a family of DLNA clients that streambox recognises.
type clientKind string

const (
	clientUnknown clientKind = "unknown"
	clientSamsung clientKind = "samsung"
)

// samsungAgents are User-Agent fragments of Samsung TVs. Control requests
// carry "SEC_HHP_" followed by the owner's editable TV name; media requests
// often carry "samsung-agent/1.1" or no User-Agent at all.
var samsungAgents = []string{"SEC_HHP_", "samsung-agent", "Samsung UPnP SDK"}

// detectClient returns the client family named by a User-Agent header.
func detectClient(userAgent string) clientKind {
	for _, fragment := range samsungAgents {
		if strings.Contains(userAgent, fragment) {
			return clientSamsung
		}
	}
	return clientUnknown
}

// clientRegistry remembers each client's family by IP address, so requests
// without an identifying User-Agent are still attributed to it.
type clientRegistry struct {
	mu   sync.Mutex
	byIP map[string]clientKind
}

// identify returns the family of the client that sent r.
func (c *clientRegistry) identify(r *http.Request) clientKind {
	ip := remoteIP(r)
	kind := detectClient(r.Header.Get("User-Agent"))
	c.mu.Lock()
	defer c.mu.Unlock()
	if kind != clientUnknown {
		if c.byIP == nil {
			c.byIP = make(map[string]clientKind)
		}
		c.byIP[ip] = kind
		return kind
	}
	if known, ok := c.byIP[ip]; ok {
		return known
	}
	return clientUnknown
}

// remoteIP returns the IP address part of r.RemoteAddr.
func remoteIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}
