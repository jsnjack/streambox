// Package ssdp implements a minimal UPnP/SSDP discovery server.
package ssdp

import (
	"context"
	"fmt"
	"log"
	"net"
	"strings"
	"time"

	"golang.org/x/net/ipv4"
)

const (
	ssdpIP   = "239.255.255.250"
	ssdpPort = 1900
)

type entry struct{ nt, usn string }

// Server handles SSDP multicast discovery for the media server device.
type Server struct {
	uuid     string
	location string
	iface    *net.Interface
	debug    bool
	aliveCh  chan struct{}
}

// New creates a new SSDP server. iface may be nil for auto-detection.
func New(uuid, location string, iface *net.Interface, debug bool) *Server {
	return &Server{uuid: uuid, location: location, iface: iface, debug: debug, aliveCh: make(chan struct{}, 1)}
}

// SendAlive triggers an immediate ssdp:alive NOTIFY burst.
func (s *Server) SendAlive() {
	select {
	case s.aliveCh <- struct{}{}:
	default: // already pending, no need to queue another
	}
}

func (s *Server) entries() []entry {
	u := s.uuid
	return []entry{
		{"upnp:rootdevice", fmt.Sprintf("uuid:%s::upnp:rootdevice", u)},
		{fmt.Sprintf("uuid:%s", u), fmt.Sprintf("uuid:%s", u)},
		{"urn:schemas-upnp-org:device:MediaServer:1", fmt.Sprintf("uuid:%s::urn:schemas-upnp-org:device:MediaServer:1", u)},
		{"urn:schemas-upnp-org:service:ContentDirectory:1", fmt.Sprintf("uuid:%s::urn:schemas-upnp-org:service:ContentDirectory:1", u)},
		{"urn:schemas-upnp-org:service:ConnectionManager:1", fmt.Sprintf("uuid:%s::urn:schemas-upnp-org:service:ConnectionManager:1", u)},
	}
}

// Start listens for M-SEARCH requests and sends NOTIFY announcements until ctx is done.
func (s *Server) Start(ctx context.Context) error {
	conn, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4zero, Port: ssdpPort})
	if err != nil {
		return fmt.Errorf("listen udp4 :%d: %w", ssdpPort, err)
	}
	defer conn.Close()
	if s.debug {
		log.Printf("ssdp: listening on :%d, advertising %s", ssdpPort, s.location)
	}

	pc := ipv4.NewPacketConn(conn)
	_ = pc.SetMulticastTTL(4)

	group := &net.UDPAddr{IP: net.ParseIP(ssdpIP)}
	joined := 0
	if s.iface != nil {
		if err := pc.JoinGroup(s.iface, group); err == nil {
			joined++
			if s.debug {
				log.Printf("ssdp: joined multicast group on %s", s.iface.Name)
			}
		} else {
			log.Printf("ssdp: JoinGroup %s: %v", s.iface.Name, err)
		}
	} else {
		ifaces, _ := net.Interfaces()
		for _, iface := range ifaces {
			if iface.Flags&net.FlagMulticast != 0 && iface.Flags&net.FlagUp != 0 {
				if err := pc.JoinGroup(&iface, group); err == nil {
					joined++
					if s.debug {
						log.Printf("ssdp: joined multicast group on %s", iface.Name)
					}
				}
			}
		}
	}
	if joined == 0 {
		log.Printf("ssdp: WARNING: failed to join multicast group on any interface — discovery will not work")
	}

	// Send 3 initial NOTIFYs spaced 200ms apart (standard DLNA practice).
	go func() {
		for i := 0; i < 3; i++ {
			s.notify(conn, true)
			time.Sleep(200 * time.Millisecond)
		}
	}()

	go func() {
		ticker := time.NewTicker(30 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				s.notify(conn, false)
				return
			case <-s.aliveCh:
				s.notify(conn, true)
			case <-ticker.C:
				s.notify(conn, true)
			}
		}
	}()

	buf := make([]byte, 2048)
	for {
		_ = conn.SetReadDeadline(time.Now().Add(time.Second))
		n, src, err := conn.ReadFromUDP(buf)
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			if netErr, ok := err.(net.Error); ok && netErr.Timeout() {
				continue
			}
			log.Printf("ssdp read: %v", err)
			continue
		}
		go s.handle(conn, src, string(buf[:n]))
	}
}

func (s *Server) handle(conn *net.UDPConn, src *net.UDPAddr, msg string) {
	if !strings.HasPrefix(msg, "M-SEARCH") {
		return
	}
	st := ssdpHeader(msg, "ST")
	if s.debug {
		log.Printf("ssdp: M-SEARCH from %s ST=%q", src, st)
	}
	if st == "" {
		return
	}

	for _, e := range s.entries() {
		if st != "ssdp:all" && st != e.nt {
			continue
		}
		resp := fmt.Sprintf(
			"HTTP/1.1 200 OK\r\n"+
				"CACHE-CONTROL: max-age=1800\r\n"+
				"EXT:\r\n"+
				"LOCATION: %s\r\n"+
				"SERVER: Linux/1.0 UPnP/1.0 StreamBox/1.0\r\n"+
				"ST: %s\r\n"+
				"USN: %s\r\n"+
				"\r\n",
			s.location, e.nt, e.usn,
		)
		if _, err := conn.WriteToUDP([]byte(resp), src); err != nil {
			log.Printf("ssdp: response to %s: %v", src, err)
		} else if s.debug {
			log.Printf("ssdp: responded to %s with ST=%s", src, e.nt)
		}
	}
}

func (s *Server) notify(conn *net.UDPConn, alive bool) {
	dst := &net.UDPAddr{IP: net.ParseIP(ssdpIP), Port: ssdpPort}
	for _, e := range s.entries() {
		var msg string
		if alive {
			msg = fmt.Sprintf(
				"NOTIFY * HTTP/1.1\r\n"+
					"HOST: %s:%d\r\n"+
					"CACHE-CONTROL: max-age=1800\r\n"+
					"LOCATION: %s\r\n"+
					"NT: %s\r\n"+
					"NTS: ssdp:alive\r\n"+
					"SERVER: Linux/1.0 UPnP/1.0 StreamBox/1.0\r\n"+
					"USN: %s\r\n"+
					"\r\n",
				ssdpIP, ssdpPort, s.location, e.nt, e.usn,
			)
		} else {
			msg = fmt.Sprintf(
				"NOTIFY * HTTP/1.1\r\n"+
					"HOST: %s:%d\r\n"+
					"NT: %s\r\n"+
					"NTS: ssdp:byebye\r\n"+
					"USN: %s\r\n"+
					"\r\n",
				ssdpIP, ssdpPort, e.nt, e.usn,
			)
		}
		if _, err := conn.WriteToUDP([]byte(msg), dst); err != nil {
			log.Printf("ssdp notify: %v", err)
		}
	}
}

func ssdpHeader(msg, key string) string {
	prefix := strings.ToUpper(key) + ":"
	for _, line := range strings.Split(msg, "\r\n") {
		if strings.HasPrefix(strings.ToUpper(line), prefix) {
			return strings.TrimSpace(line[len(prefix):])
		}
	}
	return ""
}
