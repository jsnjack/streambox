// Package server implements the UPnP/DLNA HTTP endpoints and file serving.
package server

import (
	"encoding/xml"
	"fmt"
	"log"
	"net/http"
	"os"
	"strings"
	"sync/atomic"

	"streambox/internal/media"
)

const (
	contentDirNS = "urn:schemas-upnp-org:service:ContentDirectory:1"
	connMgrNS    = "urn:schemas-upnp-org:service:ConnectionManager:1"
)

// Config holds the server configuration.
type Config struct {
	Port    int
	Name    string
	UUID    string
	IP      string
	Debug   bool
	Library *media.Library
}

// Server is the HTTP server for all UPnP/DLNA and file-serving endpoints.
type Server struct {
	cfg      Config
	mux      *http.ServeMux
	updateID atomic.Int64
}

// BumpUpdateID increments the SystemUpdateID, signalling content has changed.
func (s *Server) BumpUpdateID() { s.updateID.Add(1) }

// New creates and configures the HTTP server.
func New(cfg Config) *Server {
	s := &Server{cfg: cfg, mux: http.NewServeMux()}
	s.mux.HandleFunc("/device.xml", s.deviceDesc)
	s.mux.HandleFunc("/contentdirectory.xml", s.contentDirSCPD)
	s.mux.HandleFunc("/connectionmanager.xml", s.connMgrSCPD)
	s.mux.HandleFunc("/contentdirectory/control", s.contentDirControl)
	s.mux.HandleFunc("/connectionmanager/control", s.connMgrControl)
	s.mux.HandleFunc("/contentdirectory/events", handleEvents)
	s.mux.HandleFunc("/connectionmanager/events", handleEvents)
	s.mux.HandleFunc("/files/", s.serveFile)
	return s
}

// ListenAndServe starts the HTTP server.
func (s *Server) ListenAndServe() error {
	var h http.Handler = s.mux
	if s.cfg.Debug {
		h = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			log.Printf("HTTP %s %s [%s]", r.Method, r.URL.Path, r.RemoteAddr)
			s.mux.ServeHTTP(w, r)
		})
	}
	return http.ListenAndServe(fmt.Sprintf(":%d", s.cfg.Port), h)
}

// ----- UPnP device + service descriptors -----

func (s *Server) deviceDesc(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/xml; charset=utf-8")
	fmt.Fprintf(w, deviceDescXML, s.cfg.Name, s.cfg.UUID)
}

func (s *Server) contentDirSCPD(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/xml; charset=utf-8")
	fmt.Fprint(w, contentDirSCPDXML)
}

func (s *Server) connMgrSCPD(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/xml; charset=utf-8")
	fmt.Fprint(w, connMgrSCPDXML)
}

func handleEvents(w http.ResponseWriter, r *http.Request) {
	// UPnP eventing: return required SID + TIMEOUT so TV doesn't retry/error.
	w.Header().Set("SID", "uuid:streambox-events-00000000-0000-0000-0000-000000000000")
	w.Header().Set("TIMEOUT", "Second-1800")
	w.WriteHeader(http.StatusOK)
}

// ----- ContentDirectory:1 SOAP control -----

func (s *Server) contentDirControl(w http.ResponseWriter, r *http.Request) {
	switch soapAction(r) {
	case "Browse":
		s.browse(w, r)
	case "GetSystemUpdateID":
		soapResp(w, "GetSystemUpdateID", contentDirNS, fmt.Sprintf("<Id>%d</Id>", s.updateID.Load()))
	case "GetSortCapabilities":
		soapResp(w, "GetSortCapabilities", contentDirNS, "<SortCaps></SortCaps>")
	case "GetSearchCapabilities":
		soapResp(w, "GetSearchCapabilities", contentDirNS, "<SearchCaps></SearchCaps>")
	default:
		log.Printf("ContentDir: unknown action %q", soapAction(r))
		soapFault(w, 401, "Invalid Action")
	}
}

func (s *Server) browse(w http.ResponseWriter, r *http.Request) {
	var env struct {
		Body struct {
			Browse struct {
				ObjectID       string `xml:"ObjectID"`
				BrowseFlag     string `xml:"BrowseFlag"`
				StartingIndex  int    `xml:"StartingIndex"`
				RequestedCount int    `xml:"RequestedCount"`
			} `xml:"Browse"`
		} `xml:"Body"`
	}
	if err := xml.NewDecoder(r.Body).Decode(&env); err != nil {
		soapFault(w, 402, "Invalid Args")
		return
	}
	req := env.Body.Browse

	var objs []media.Object
	var parentCtx string
	total := 0

	if req.BrowseFlag == "BrowseMetadata" {
		obj, ok := s.cfg.Library.Get(req.ObjectID)
		if !ok {
			soapFault(w, 701, "No Such Object")
			return
		}
		objs = []media.Object{obj}
		parentCtx = obj.GetParentID()
		total = 1
	} else { // BrowseDirectChildren
		all := s.cfg.Library.Children(req.ObjectID)
		if all == nil {
			soapFault(w, 701, "No Such Object")
			return
		}
		total = len(all)
		start := req.StartingIndex
		if start > total {
			start = total
		}
		end := total
		if req.RequestedCount > 0 && start+req.RequestedCount < end {
			end = start + req.RequestedCount
		}
		objs = all[start:end]
		parentCtx = req.ObjectID
	}

	didl := s.buildDIDL(objs, parentCtx)
	body := fmt.Sprintf(
		"<Result>%s</Result><NumberReturned>%d</NumberReturned><TotalMatches>%d</TotalMatches><UpdateID>1</UpdateID>",
		escXML(didl), len(objs), total,
	)
	soapResp(w, "Browse", contentDirNS, body)
}

// buildDIDL generates a DIDL-Lite XML fragment for the given objects.
// parentCtx is the ID of the container being browsed; it is used as the
// parentID attribute so that TV back-navigation works correctly even for
// virtual containers like "Recent" whose items live elsewhere in the tree.
func (s *Server) buildDIDL(objs []media.Object, parentCtx string) string {
	var sb strings.Builder
	sb.WriteString(`<DIDL-Lite xmlns="urn:schemas-upnp-org:metadata-1-0/DIDL-Lite/" xmlns:dc="http://purl.org/dc/elements/1.1/" xmlns:upnp="urn:schemas-upnp-org:metadata-1-0/upnp/">`)
	for _, obj := range objs {
		switch o := obj.(type) {
		case *media.Container:
			n := len(s.cfg.Library.Children(o.ID))
			fmt.Fprintf(&sb,
				`<container id=%q parentID=%q restricted="1" childCount="%d">`+
					`<dc:title>%s</dc:title>`+
					`<upnp:class>object.container.storageFolder</upnp:class>`+
					`</container>`,
				o.ID, parentCtx, n, escXML(o.Title),
			)
		case *media.Item:
			url := fmt.Sprintf("http://%s:%d/files/%s", s.cfg.IP, s.cfg.Port, o.ID)
			proto := fmt.Sprintf("http-get:*:%s:DLNA.ORG_OP=01;DLNA.ORG_FLAGS=01700000000000000000000000000000", o.MIMEType)
			fmt.Fprintf(&sb,
				`<item id=%q parentID=%q restricted="1">`+
					`<dc:title>%s</dc:title>`+
					`<upnp:class>object.item.videoItem</upnp:class>`+
					`<res protocolInfo=%q size="%d">%s</res>`+
					`</item>`,
				o.ID, parentCtx, escXML(o.Title), proto, o.Size, url,
			)
		}
	}
	sb.WriteString(`</DIDL-Lite>`)
	return sb.String()
}

// ----- ConnectionManager:1 SOAP control -----

func (s *Server) connMgrControl(w http.ResponseWriter, r *http.Request) {
	switch soapAction(r) {
	case "GetProtocolInfo":
		src := protocolInfos()
		soapResp(w, "GetProtocolInfo", connMgrNS,
			fmt.Sprintf("<Source>%s</Source><Sink></Sink>", escXML(src)))
	case "GetCurrentConnectionIDs":
		soapResp(w, "GetCurrentConnectionIDs", connMgrNS, "<ConnectionIDs>0</ConnectionIDs>")
	case "GetCurrentConnectionInfo":
		soapResp(w, "GetCurrentConnectionInfo", connMgrNS,
			"<RcsID>-1</RcsID><AVTransportID>-1</AVTransportID>"+
				"<ProtocolInfo></ProtocolInfo><PeerConnectionManager>/</PeerConnectionManager>"+
				"<PeerConnectionID>-1</PeerConnectionID><Direction>Output</Direction><Status>OK</Status>")
	default:
		log.Printf("ConnMgr: unknown action %q", soapAction(r))
		soapFault(w, 401, "Invalid Action")
	}
}

// ----- File serving -----

func (s *Server) serveFile(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimPrefix(r.URL.Path, "/files/")
	obj, ok := s.cfg.Library.Get(id)
	if !ok {
		http.NotFound(w, r)
		return
	}
	item, ok := obj.(*media.Item)
	if !ok {
		http.NotFound(w, r)
		return
	}
	f, err := os.Open(item.Path)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", item.MIMEType)
	w.Header().Set("transferMode.dlna.org", "Streaming")
	w.Header().Set("contentFeatures.dlna.org", "DLNA.ORG_OP=01;DLNA.ORG_FLAGS=01700000000000000000000000000000")
	http.ServeContent(w, r, info.Name(), info.ModTime(), f)
}

// ----- Helpers -----

func soapAction(r *http.Request) string {
	h := strings.Trim(r.Header.Get("SOAPACTION"), `"`)
	if idx := strings.LastIndex(h, "#"); idx >= 0 {
		return h[idx+1:]
	}
	return ""
}

func soapResp(w http.ResponseWriter, action, ns, body string) {
	w.Header().Set("Content-Type", `text/xml; charset="utf-8"`)
	w.Header().Set("EXT", "")
	fmt.Fprintf(w,
		`<?xml version="1.0" encoding="utf-8"?>`+
			`<s:Envelope xmlns:s="http://schemas.xmlsoap.org/soap/envelope/" s:encodingStyle="http://schemas.xmlsoap.org/soap/encoding/">`+
			`<s:Body><u:%sResponse xmlns:u="%s">%s</u:%sResponse></s:Body>`+
			`</s:Envelope>`,
		action, ns, body, action,
	)
}

func soapFault(w http.ResponseWriter, code int, desc string) {
	w.Header().Set("Content-Type", `text/xml; charset="utf-8"`)
	w.WriteHeader(http.StatusInternalServerError)
	fmt.Fprintf(w,
		`<?xml version="1.0"?>`+
			`<s:Envelope xmlns:s="http://schemas.xmlsoap.org/soap/envelope/">`+
			`<s:Body><s:Fault><faultcode>s:Client</faultcode><faultstring>UPnPError</faultstring>`+
			`<detail><UPnPError xmlns="urn:schemas-upnp-org:control-1-0">`+
			`<errorCode>%d</errorCode><errorDescription>%s</errorDescription>`+
			`</UPnPError></detail></s:Fault></s:Body></s:Envelope>`,
		code, desc,
	)
}

func escXML(s string) string {
	var b strings.Builder
	xml.EscapeText(&b, []byte(s))
	return b.String()
}

func protocolInfos() string {
	mimes := []string{
		"video/mp4", "video/x-matroska", "video/avi", "video/quicktime",
		"video/x-ms-wmv", "video/mpeg", "video/x-flv", "video/webm",
		"video/ogg", "video/3gpp",
	}
	parts := make([]string, len(mimes))
	for i, m := range mimes {
		parts[i] = "http-get:*:" + m + ":*"
	}
	return strings.Join(parts, ",")
}

// ----- Embedded XML constants -----

const deviceDescXML = `<?xml version="1.0"?>
<root xmlns="urn:schemas-upnp-org:device-1-0">
  <specVersion><major>1</major><minor>0</minor></specVersion>
  <device>
    <deviceType>urn:schemas-upnp-org:device:MediaServer:1</deviceType>
    <friendlyName>%s</friendlyName>
    <manufacturer>StreamBox</manufacturer>
    <modelName>StreamBox</modelName>
    <UDN>uuid:%s</UDN>
    <dlna:X_DLNADOC xmlns:dlna="urn:schemas-dlna-org:device-1-0">DMS-1.50</dlna:X_DLNADOC>
    <serviceList>
      <service>
        <serviceType>urn:schemas-upnp-org:service:ContentDirectory:1</serviceType>
        <serviceId>urn:upnp-org:serviceId:ContentDirectory</serviceId>
        <SCPDURL>/contentdirectory.xml</SCPDURL>
        <controlURL>/contentdirectory/control</controlURL>
        <eventSubURL>/contentdirectory/events</eventSubURL>
      </service>
      <service>
        <serviceType>urn:schemas-upnp-org:service:ConnectionManager:1</serviceType>
        <serviceId>urn:upnp-org:serviceId:ConnectionManager</serviceId>
        <SCPDURL>/connectionmanager.xml</SCPDURL>
        <controlURL>/connectionmanager/control</controlURL>
        <eventSubURL>/connectionmanager/events</eventSubURL>
      </service>
    </serviceList>
  </device>
</root>`

const contentDirSCPDXML = `<?xml version="1.0"?>
<scpd xmlns="urn:schemas-upnp-org:service-1-0">
  <specVersion><major>1</major><minor>0</minor></specVersion>
  <actionList>
    <action><name>Browse</name><argumentList>
      <argument><name>ObjectID</name><direction>in</direction><relatedStateVariable>A_ARG_TYPE_ObjectID</relatedStateVariable></argument>
      <argument><name>BrowseFlag</name><direction>in</direction><relatedStateVariable>A_ARG_TYPE_BrowseFlag</relatedStateVariable></argument>
      <argument><name>Filter</name><direction>in</direction><relatedStateVariable>A_ARG_TYPE_Filter</relatedStateVariable></argument>
      <argument><name>StartingIndex</name><direction>in</direction><relatedStateVariable>A_ARG_TYPE_Index</relatedStateVariable></argument>
      <argument><name>RequestedCount</name><direction>in</direction><relatedStateVariable>A_ARG_TYPE_Count</relatedStateVariable></argument>
      <argument><name>SortCriteria</name><direction>in</direction><relatedStateVariable>A_ARG_TYPE_SortCriteria</relatedStateVariable></argument>
      <argument><name>Result</name><direction>out</direction><relatedStateVariable>A_ARG_TYPE_Result</relatedStateVariable></argument>
      <argument><name>NumberReturned</name><direction>out</direction><relatedStateVariable>A_ARG_TYPE_Count</relatedStateVariable></argument>
      <argument><name>TotalMatches</name><direction>out</direction><relatedStateVariable>A_ARG_TYPE_Count</relatedStateVariable></argument>
      <argument><name>UpdateID</name><direction>out</direction><relatedStateVariable>A_ARG_TYPE_UpdateID</relatedStateVariable></argument>
    </argumentList></action>
    <action><name>GetSystemUpdateID</name><argumentList>
      <argument><name>Id</name><direction>out</direction><relatedStateVariable>SystemUpdateID</relatedStateVariable></argument>
    </argumentList></action>
    <action><name>GetSortCapabilities</name><argumentList>
      <argument><name>SortCaps</name><direction>out</direction><relatedStateVariable>SortCapabilities</relatedStateVariable></argument>
    </argumentList></action>
    <action><name>GetSearchCapabilities</name><argumentList>
      <argument><name>SearchCaps</name><direction>out</direction><relatedStateVariable>SearchCapabilities</relatedStateVariable></argument>
    </argumentList></action>
  </actionList>
  <serviceStateTable>
    <stateVariable sendEvents="no"><name>A_ARG_TYPE_ObjectID</name><dataType>string</dataType></stateVariable>
    <stateVariable sendEvents="no"><name>A_ARG_TYPE_Result</name><dataType>string</dataType></stateVariable>
    <stateVariable sendEvents="no"><name>A_ARG_TYPE_BrowseFlag</name><dataType>string</dataType>
      <allowedValueList><allowedValue>BrowseMetadata</allowedValue><allowedValue>BrowseDirectChildren</allowedValue></allowedValueList>
    </stateVariable>
    <stateVariable sendEvents="no"><name>A_ARG_TYPE_Filter</name><dataType>string</dataType></stateVariable>
    <stateVariable sendEvents="no"><name>A_ARG_TYPE_SortCriteria</name><dataType>string</dataType></stateVariable>
    <stateVariable sendEvents="no"><name>A_ARG_TYPE_Index</name><dataType>ui4</dataType></stateVariable>
    <stateVariable sendEvents="no"><name>A_ARG_TYPE_Count</name><dataType>ui4</dataType></stateVariable>
    <stateVariable sendEvents="no"><name>A_ARG_TYPE_UpdateID</name><dataType>ui4</dataType></stateVariable>
    <stateVariable sendEvents="yes"><name>SystemUpdateID</name><dataType>ui4</dataType></stateVariable>
    <stateVariable sendEvents="no"><name>SortCapabilities</name><dataType>string</dataType></stateVariable>
    <stateVariable sendEvents="no"><name>SearchCapabilities</name><dataType>string</dataType></stateVariable>
  </serviceStateTable>
</scpd>`

const connMgrSCPDXML = `<?xml version="1.0"?>
<scpd xmlns="urn:schemas-upnp-org:service-1-0">
  <specVersion><major>1</major><minor>0</minor></specVersion>
  <actionList>
    <action><name>GetProtocolInfo</name><argumentList>
      <argument><name>Source</name><direction>out</direction><relatedStateVariable>SourceProtocolInfo</relatedStateVariable></argument>
      <argument><name>Sink</name><direction>out</direction><relatedStateVariable>SinkProtocolInfo</relatedStateVariable></argument>
    </argumentList></action>
    <action><name>GetCurrentConnectionIDs</name><argumentList>
      <argument><name>ConnectionIDs</name><direction>out</direction><relatedStateVariable>CurrentConnectionIDs</relatedStateVariable></argument>
    </argumentList></action>
    <action><name>GetCurrentConnectionInfo</name><argumentList>
      <argument><name>ConnectionID</name><direction>in</direction><relatedStateVariable>A_ARG_TYPE_ConnectionID</relatedStateVariable></argument>
      <argument><name>RcsID</name><direction>out</direction><relatedStateVariable>A_ARG_TYPE_RcsID</relatedStateVariable></argument>
      <argument><name>AVTransportID</name><direction>out</direction><relatedStateVariable>A_ARG_TYPE_AVTransportID</relatedStateVariable></argument>
      <argument><name>ProtocolInfo</name><direction>out</direction><relatedStateVariable>A_ARG_TYPE_ProtocolInfo</relatedStateVariable></argument>
      <argument><name>PeerConnectionManager</name><direction>out</direction><relatedStateVariable>A_ARG_TYPE_ConnectionManager</relatedStateVariable></argument>
      <argument><name>PeerConnectionID</name><direction>out</direction><relatedStateVariable>A_ARG_TYPE_ConnectionID</relatedStateVariable></argument>
      <argument><name>Direction</name><direction>out</direction><relatedStateVariable>A_ARG_TYPE_Direction</relatedStateVariable></argument>
      <argument><name>Status</name><direction>out</direction><relatedStateVariable>A_ARG_TYPE_ConnectionStatus</relatedStateVariable></argument>
    </argumentList></action>
  </actionList>
  <serviceStateTable>
    <stateVariable sendEvents="yes"><name>SourceProtocolInfo</name><dataType>string</dataType></stateVariable>
    <stateVariable sendEvents="yes"><name>SinkProtocolInfo</name><dataType>string</dataType></stateVariable>
    <stateVariable sendEvents="yes"><name>CurrentConnectionIDs</name><dataType>string</dataType></stateVariable>
    <stateVariable sendEvents="no"><name>A_ARG_TYPE_ConnectionID</name><dataType>i4</dataType></stateVariable>
    <stateVariable sendEvents="no"><name>A_ARG_TYPE_ConnectionManager</name><dataType>string</dataType></stateVariable>
    <stateVariable sendEvents="no"><name>A_ARG_TYPE_Direction</name><dataType>string</dataType>
      <allowedValueList><allowedValue>Input</allowedValue><allowedValue>Output</allowedValue></allowedValueList>
    </stateVariable>
    <stateVariable sendEvents="no"><name>A_ARG_TYPE_ProtocolInfo</name><dataType>string</dataType></stateVariable>
    <stateVariable sendEvents="no"><name>A_ARG_TYPE_RcsID</name><dataType>i4</dataType></stateVariable>
    <stateVariable sendEvents="no"><name>A_ARG_TYPE_AVTransportID</name><dataType>i4</dataType></stateVariable>
    <stateVariable sendEvents="no"><name>A_ARG_TYPE_ConnectionStatus</name><dataType>string</dataType>
      <allowedValueList>
        <allowedValue>OK</allowedValue><allowedValue>ContentFormatMismatch</allowedValue>
        <allowedValue>InsufficientBandwidth</allowedValue><allowedValue>UnreliableChannel</allowedValue>
        <allowedValue>Unknown</allowedValue>
      </allowedValueList>
    </stateVariable>
  </serviceStateTable>
</scpd>`
