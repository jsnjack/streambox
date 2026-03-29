// Package server implements the UPnP/DLNA HTTP endpoints and file serving.
package server

import (
	"encoding/xml"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"streambox/internal/media"
)

const (
	contentDirNS = "urn:schemas-upnp-org:service:ContentDirectory:1"
	connMgrNS    = "urn:schemas-upnp-org:service:ConnectionManager:1"
)

// Config holds the server configuration.
type Config struct {
	Port               int
	Name               string
	UUID               string
	IP                 string
	Debug              bool
	Library            *media.Library
	History            *media.WatchHistory
	OnFileDelete       func()
	OnRefresh          func() // called on manual refresh; should send SSDP alive burst
	OnRestartService   func() // called to restart the systemd user service
	OnRegenUUID        func() // called to regenerate UUID and restart the service
}

// Server is the HTTP server for all UPnP/DLNA and file-serving endpoints.
type Server struct {
	cfg      Config
	mux      *http.ServeMux
	updateID atomic.Int64
	subs     subscriptions
	undoMu   sync.Mutex
	undoBuf  map[string]undoEntry
}

type undoEntry struct {
	item      media.WatchedItem
	expiresAt time.Time
}

// SetUpdateID sets the initial SystemUpdateID (e.g. loaded from disk).
func (s *Server) SetUpdateID(id int64) {
	s.updateID.Store(id)
}

// BumpUpdateID increments the SystemUpdateID and notifies all subscribers.
// It returns the new value so the caller can persist it.
func (s *Server) BumpUpdateID() int64 {
	id := s.updateID.Add(1)
	go s.subs.notify(id)
	return id
}

// New creates and configures the HTTP server.
func New(cfg Config) *Server {
	s := &Server{cfg: cfg, mux: http.NewServeMux()}
	s.mux.HandleFunc("/device.xml", s.deviceDesc)
	s.mux.HandleFunc("/contentdirectory.xml", s.contentDirSCPD)
	s.mux.HandleFunc("/connectionmanager.xml", s.connMgrSCPD)
	s.mux.HandleFunc("/contentdirectory/control", s.contentDirControl)
	s.mux.HandleFunc("/connectionmanager/control", s.connMgrControl)
	s.mux.HandleFunc("/contentdirectory/events", s.handleEvents)
	s.mux.HandleFunc("/connectionmanager/events", s.handleEvents)
	s.mux.HandleFunc("/files/", s.serveFile)
	s.mux.HandleFunc("/ui", s.serveUI)
	s.mux.HandleFunc("/ui/watch", s.serveWatch)
	s.mux.HandleFunc("/ui/delete", s.deleteFile)
	s.mux.HandleFunc("/ui/discard", s.discardFile)
	s.mux.HandleFunc("/ui/undiscard", s.undiscardFile)
	s.mux.HandleFunc("/ui/refresh", s.refreshLibrary)
	s.mux.HandleFunc("/ui/restart", s.restartService)
	s.mux.HandleFunc("/ui/regen-uuid", s.regenUUID)
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

func (s *Server) handleEvents(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case "SUBSCRIBE":
		callback := strings.Trim(r.Header.Get("CALLBACK"), "<>")
		if callback == "" {
			// Renewal — refresh timeout.
			s.subs.renew(r.Header.Get("SID"))
			w.Header().Set("SID", r.Header.Get("SID"))
			w.Header().Set("TIMEOUT", fmt.Sprintf("Second-%d", int(subTimeout.Seconds())))
			return
		}
		sid := s.subs.add(callback)
		w.Header().Set("SID", sid)
		w.Header().Set("TIMEOUT", fmt.Sprintf("Second-%d", int(subTimeout.Seconds())))
		// Send initial event with current SystemUpdateID.
		go s.subs.notifyOne(sid, s.updateID.Load())
	case "UNSUBSCRIBE":
		s.subs.remove(r.Header.Get("SID"))
		w.WriteHeader(http.StatusOK)
	default:
		w.WriteHeader(http.StatusMethodNotAllowed)
	}
}

// subscriptions tracks active UPnP event subscribers.
const subTimeout = 1800 * time.Second

var notifyClient = &http.Client{Timeout: 5 * time.Second}

type subscription struct {
	callback string
	expiry   time.Time
}

type subscriptions struct {
	mu   sync.Mutex
	subs map[string]subscription // sid → subscription
	seq  atomic.Int64
}

func (ss *subscriptions) add(callback string) string {
	sid := fmt.Sprintf("uuid:streambox-sub-%d", time.Now().UnixNano())
	ss.mu.Lock()
	if ss.subs == nil {
		ss.subs = make(map[string]subscription)
	}
	// Purge expired entries while we have the lock.
	now := time.Now()
	for id, s := range ss.subs {
		if now.After(s.expiry) {
			delete(ss.subs, id)
		}
	}
	ss.subs[sid] = subscription{callback: callback, expiry: now.Add(subTimeout)}
	ss.mu.Unlock()
	return sid
}

func (ss *subscriptions) renew(sid string) {
	ss.mu.Lock()
	if s, ok := ss.subs[sid]; ok {
		s.expiry = time.Now().Add(subTimeout)
		ss.subs[sid] = s
	}
	ss.mu.Unlock()
}

func (ss *subscriptions) remove(sid string) {
	ss.mu.Lock()
	delete(ss.subs, sid)
	ss.mu.Unlock()
}

func (ss *subscriptions) notify(updateID int64) {
	ss.mu.Lock()
	now := time.Now()
	active := make(map[string]string, len(ss.subs))
	for sid, s := range ss.subs {
		if now.Before(s.expiry) {
			active[sid] = s.callback
		} else {
			delete(ss.subs, sid)
		}
	}
	ss.mu.Unlock()
	for sid := range active {
		ss.notifyOne(sid, updateID)
	}
}

func (ss *subscriptions) notifyOne(sid string, updateID int64) {
	ss.mu.Lock()
	s, ok := ss.subs[sid]
	ss.mu.Unlock()
	if !ok {
		return
	}
	seq := ss.seq.Add(1) - 1
	body := fmt.Sprintf(
		`<?xml version="1.0"?>`+
			`<e:propertyset xmlns:e="urn:schemas-upnp-org:event-1-0">`+
			`<e:property><SystemUpdateID>%d</SystemUpdateID></e:property>`+
			`</e:propertyset>`,
		updateID,
	)
	req, err := http.NewRequest("NOTIFY", s.callback, strings.NewReader(body))
	if err != nil {
		log.Printf("event: bad callback URL %q: %v", s.callback, err)
		return
	}
	req.Header.Set("Content-Type", "text/xml")
	req.Header.Set("NT", "upnp:event")
	req.Header.Set("NTS", "upnp:propchange")
	req.Header.Set("SID", sid)
	req.Header.Set("SEQ", fmt.Sprintf("%d", seq))
	resp, err := notifyClient.Do(req)
	if err != nil {
		log.Printf("event: NOTIFY %s failed: %v", s.callback, err)
		return
	}
	resp.Body.Close()
	log.Printf("event: NOTIFY %s → %s", s.callback, resp.Status)
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
		"<Result>%s</Result><NumberReturned>%d</NumberReturned><TotalMatches>%d</TotalMatches><UpdateID>%d</UpdateID>",
		escXML(didl), len(objs), total, s.updateID.Load(),
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
				`<container id=%q parentID=%q restricted="1" childCount="%d" containerUpdateID="%d">`+
					`<dc:title>%s</dc:title>`+
					`<upnp:class>object.container.storageFolder</upnp:class>`+
					`</container>`,
				o.ID, parentCtx, n, s.updateID.Load(), escXML(o.Title),
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
	if s.cfg.History != nil {
		s.cfg.History.Record(item)
	}
	http.ServeContent(w, r, info.Name(), info.ModTime(), f)
}

// ----- Web UI -----

func (s *Server) serveUI(w http.ResponseWriter, r *http.Request) {
	watched := s.cfg.History.List()
	recent := s.cfg.Library.RecentItems()
	all := s.cfg.Library.AllItems()

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	fmt.Fprint(w, uiHeader)

	s.renderSection(w, "Recently Watched", watched2items(watched), true, true)
	s.renderSection(w, "Recent", recent, false, false)
	s.renderSection(w, "All", all, false, false)

	collapseAll := (len(watched) > 0 || len(recent) > 0) && len(all) > 0
	fmt.Fprintf(w, uiInitScript, collapseAll,
		r.URL.Query().Get("discarded"), r.URL.Query().Get("dtitle"))
	fmt.Fprint(w, `</body></html>`)
}

func (s *Server) renderSection(w http.ResponseWriter, title string, items []*media.Item, showEmpty bool, showDiscard bool) {
	slug := strings.ToLower(strings.ReplaceAll(title, " ", "-"))
	if len(items) == 0 {
		if showEmpty {
			fmt.Fprintf(w,
				`<div class="section" data-section="%s"><h2 onclick="toggleSection(this)">%s <span class="badge">0</span><span class="caret">▾</span></h2><p class="empty">Nothing yet.</p></div>`,
				slug, title)
		}
		return
	}
	fmt.Fprintf(w,
		`<div class="section" data-section="%s"><h2 onclick="toggleSection(this)">%s <span class="badge">%d</span><span class="caret">▾</span></h2><ul>`,
		slug, title, len(items))
	for _, item := range items {
		size := ""
		if item.Size > 0 {
			size = fmt.Sprintf(`<span class="size">%s</span>`, fmtSize(item.Size))
		}
		discard := ""
		if showDiscard {
			discard = fmt.Sprintf(`<a class="discard" href="/ui/discard?id=%s">Discard</a>`, item.ID)
		}
		fmt.Fprintf(w,
			`<li><div class="item-info"><a class="title" href="/ui/watch?id=%s">%s</a>%s</div>`+
				`<div class="actions">%s<a class="del" href="/ui/delete?id=%s" data-title="%s" onclick="return inlineDel(event,this)">Delete</a></div></li>`,
			item.ID, escXML(item.Title), size, discard, item.ID, escXML(item.Title))
	}
	fmt.Fprint(w, `</ul></div>`)
}

func (s *Server) serveWatch(w http.ResponseWriter, r *http.Request) {
	id := r.URL.Query().Get("id")
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
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	fmt.Fprintf(w, uiWatchPage, escXML(item.Title), item.ID, item.MIMEType, escXML(item.Title))
}

func (s *Server) refreshLibrary(w http.ResponseWriter, r *http.Request) {
	if s.cfg.OnFileDelete != nil {
		s.cfg.OnFileDelete()
	}
	if s.cfg.OnRefresh != nil {
		s.cfg.OnRefresh()
	}
	http.Redirect(w, r, "/ui", http.StatusSeeOther)
}

func (s *Server) restartService(w http.ResponseWriter, r *http.Request) {
	if s.cfg.OnRestartService != nil {
		go s.cfg.OnRestartService()
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	fmt.Fprint(w, uiRestartingPage)
}

func (s *Server) regenUUID(w http.ResponseWriter, r *http.Request) {
	if s.cfg.OnRegenUUID != nil {
		go s.cfg.OnRegenUUID()
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	fmt.Fprint(w, uiRestartingPage)
}

func (s *Server) deleteFile(w http.ResponseWriter, r *http.Request) {
	id := r.URL.Query().Get("id")
	if id == "" {
		http.Error(w, "missing id", http.StatusBadRequest)
		return
	}
	obj, ok := s.cfg.Library.Get(id)
	if !ok {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	item, ok := obj.(*media.Item)
	if !ok {
		http.Error(w, "not a file", http.StatusBadRequest)
		return
	}
	if err := os.Remove(item.Path); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if s.cfg.History != nil {
		s.cfg.History.Remove(id)
	}
	if s.cfg.OnFileDelete != nil {
		s.cfg.OnFileDelete()
	}
	http.Redirect(w, r, "/ui", http.StatusSeeOther)
}

func (s *Server) discardFile(w http.ResponseWriter, r *http.Request) {
	id := r.URL.Query().Get("id")
	if id == "" {
		http.Error(w, "missing id", http.StatusBadRequest)
		return
	}
	var saved media.WatchedItem
	if s.cfg.History != nil {
		saved, _ = s.cfg.History.Get(id)
		s.cfg.History.Remove(id)
	}
	if saved.ID != "" {
		s.undoMu.Lock()
		if s.undoBuf == nil {
			s.undoBuf = make(map[string]undoEntry)
		}
		s.undoBuf[id] = undoEntry{item: saved, expiresAt: time.Now().Add(30 * time.Second)}
		s.undoMu.Unlock()
	}
	v := url.Values{"discarded": {id}, "dtitle": {saved.Title}}
	http.Redirect(w, r, "/ui?"+v.Encode(), http.StatusSeeOther)
}

func (s *Server) undiscardFile(w http.ResponseWriter, r *http.Request) {
	id := r.URL.Query().Get("id")
	if id == "" {
		http.Error(w, "missing id", http.StatusBadRequest)
		return
	}
	s.undoMu.Lock()
	entry, ok := s.undoBuf[id]
	if ok {
		delete(s.undoBuf, id)
	}
	s.undoMu.Unlock()
	if ok && time.Now().Before(entry.expiresAt) && s.cfg.History != nil {
		s.cfg.History.Readd(entry.item)
	}
	http.Redirect(w, r, "/ui", http.StatusSeeOther)
}

func watched2items(ws []media.WatchedItem) []*media.Item {
	items := make([]*media.Item, len(ws))
	for i, w := range ws {
		items[i] = &media.Item{ID: w.ID, Title: w.Title, Path: w.Path}
	}
	return items
}

func fmtSize(b int64) string {
	const unit = 1024
	if b < unit {
		return fmt.Sprintf("%d B", b)
	}
	div, exp := int64(unit), 0
	for n := b / unit; n >= unit; n /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %cB", float64(b)/float64(div), "KMGTPE"[exp])
}

const uiHeader = `<!DOCTYPE html><html><head><meta charset="utf-8">
<meta name="viewport" content="width=device-width,initial-scale=1">
<title>StreamBox</title>
<style>
  body{font-family:sans-serif;max-width:700px;margin:2em auto;padding:0 1em;background:#111;color:#eee}
  .topbar{display:flex;align-items:stretch;gap:.6em;margin-bottom:.5em;flex-wrap:wrap}
  input#q{flex:1;min-width:120px;padding:.6em .8em;background:#222;border:1px solid #444;border-radius:4px;color:#eee;font-size:1em}
  input#q:focus{outline:none;border-color:#666}
  .btn{padding:.6em .9em;border-radius:4px;font-size:.85em;text-decoration:none;white-space:nowrap;display:flex;align-items:center;cursor:pointer;border:1px solid}
  .btn-refresh{background:#1a1a1a;border-color:#444;color:#aaa}
  .btn-refresh:hover{color:#fff;border-color:#888}
  .btn-restart{background:#1c1400;border-color:#6b4f00;color:#c8920a}
  .btn-restart:hover{color:#ffc12b;border-color:#c8920a}
  .btn-regen{background:#1c0000;border-color:#6b0000;color:#c84040}
  .btn-regen:hover{color:#ff7070;border-color:#c84040}
  .btn-advanced{background:#1a1a1a;border-color:#333;color:#666;font-size:.8em}
  .btn-advanced:hover{color:#aaa;border-color:#666}
  .advanced-panel{display:none;flex-wrap:wrap;gap:.6em;padding:.6em .2em;margin-bottom:1em}
  .advanced-panel.open{display:flex}
  h2{font-size:1.1em;margin:1.5em 0 .5em;color:#aaa;text-transform:uppercase;letter-spacing:.05em;cursor:pointer;user-select:none;display:flex;align-items:center;gap:.4em}
  h2:hover{color:#ccc}
  .caret{font-size:.75em;transition:transform .15s;display:inline-block;margin-left:auto}
  .section.collapsed .caret{transform:rotate(-90deg)}
  .section.collapsed ul,.section.collapsed p.empty{display:none}
  .badge{background:#252525;color:#777;font-size:.7em;padding:.1em .45em;border-radius:10px;font-weight:normal;letter-spacing:0;text-transform:none}
  ul{list-style:none;padding:0;margin:0}
  li{display:flex;align-items:center;padding:.6em 0;border-bottom:1px solid #222;gap:.6em;position:relative;overflow:hidden}
  .item-info{flex:1;min-width:0;display:flex;flex-direction:column;gap:.1em}
  a.title{overflow:hidden;text-overflow:ellipsis;white-space:nowrap;color:#eee;text-decoration:none}
  a.title:hover{color:#fff;text-decoration:underline}
  .size{color:#555;font-size:.75em}
  .actions{display:flex;align-items:center;gap:.5em;flex-shrink:0}
  a.del,a.discard{text-decoration:none;font-size:.85em;white-space:nowrap}
  a.del{color:#e55}
  a.del:hover{color:#f88}
  a.discard{color:#666}
  a.discard:hover{color:#bbb}
  .del-confirm{display:inline-flex;align-items:center;gap:.3em;font-size:.85em;white-space:nowrap}
  .del-confirm a.sure{color:#e55;text-decoration:none}
  .del-confirm a.sure:hover{color:#f88}
  .del-confirm a.cancel{color:#555;text-decoration:none}
  .del-confirm a.cancel:hover{color:#aaa}
  p.empty{color:#555;font-size:.9em}
  #toast{position:fixed;bottom:1.5em;left:50%;transform:translateX(-50%);background:#222;border:1px solid #444;border-radius:6px;padding:.6em 1em;display:flex;align-items:center;gap:.8em;font-size:.9em;box-shadow:0 4px 16px #0008;z-index:100;opacity:0;transition:opacity .2s;pointer-events:none;white-space:nowrap}
  #toast.show{opacity:1;pointer-events:auto}
  #toast a.undo{color:#7bf;text-decoration:none}
  #toast a.undo:hover{text-decoration:underline}
  #toast a.close-t{color:#555;text-decoration:none;font-size:1.1em}
  #toast a.close-t:hover{color:#aaa}
  @media(pointer:coarse){
    .actions{transform:translateX(110%);position:absolute;right:0;top:0;bottom:0;background:#1d1d1d;padding:0 .8em;border-left:1px solid #2a2a2a;transition:transform .2s;z-index:1}
    li.swiped .actions{transform:translateX(0)}
    li::after{content:'';position:absolute;right:0;top:20%;bottom:20%;width:3px;background:#2e2e2e;border-radius:2px;pointer-events:none}
    li.swiped::after{display:none}
  }
</style></head><body>
<div id="toast"><span id="toast-msg"></span><a class="undo" id="toast-undo" href="#">Undo</a><a class="close-t" id="toast-close" href="#">&#x2715;</a></div>
<div class="topbar">
<input id="q" type="search" placeholder="Filter&#x2026; (press / to focus)" autocomplete="off" oninput="filter(this.value)">
<button class="btn btn-advanced" id="adv-btn" onclick="toggleAdvanced()" type="button">&#x2699; Advanced &#x25be;</button>
</div>
<div class="advanced-panel" id="adv-panel">
<a class="btn btn-refresh" href="/ui/refresh">Rescan Library</a>
<a class="btn btn-restart" href="/ui/restart">Restart Service</a>
<a class="btn btn-regen" href="/ui/regen-uuid">Regenerate UUID</a>
</div>
<script>
var q=document.getElementById('q');
function filter(v){
  v=v.toLowerCase();
  document.querySelectorAll('.section').forEach(function(sec){
    var n=0;
    sec.querySelectorAll('li').forEach(function(li){
      var m=li.querySelector('.title').textContent.toLowerCase().includes(v);
      li.style.display=m?'':'none';
      if(m)n++;
    });
    sec.style.display=(v&&!n)?'none':'';
  });
}
function toggleSection(h2){
  var sec=h2.closest('.section');
  sec.classList.toggle('collapsed');
  try{sessionStorage.setItem('sec-'+sec.dataset.section,sec.classList.contains('collapsed')?'1':'0');}catch(e){}
}
function toggleAdvanced(){
  var p=document.getElementById('adv-panel'),b=document.getElementById('adv-btn');
  p.classList.toggle('open');
  b.innerHTML=p.classList.contains('open')?'&#x2699; Advanced &#x25b4;':'&#x2699; Advanced &#x25be;';
}
function inlineDel(e,a){
  e.preventDefault();
  var href=a.href,title=a.dataset.title||'this file';
  var conf=document.createElement('span');
  conf.className='del-confirm';
  var msg=document.createTextNode('Delete "'+title+'"?\u00a0');
  var yes=document.createElement('a');yes.href=href;yes.className='sure';yes.textContent='Yes';
  var sep=document.createTextNode('\u00a0');
  var no=document.createElement('a');no.href='#';no.className='cancel';no.textContent='No';
  no.addEventListener('click',function(e2){e2.preventDefault();conf.replaceWith(a);});
  conf.append(msg,yes,sep,no);
  a.replaceWith(conf);
  return false;
}
var _toastTimer;
function showToast(id,title){
  var t=document.getElementById('toast');
  document.getElementById('toast-msg').textContent='Discarded: '+title+'\u00a0';
  document.getElementById('toast-undo').href='/ui/undiscard?id='+encodeURIComponent(id);
  t.classList.add('show');
  clearTimeout(_toastTimer);
  _toastTimer=setTimeout(function(){t.classList.remove('show');},8000);
}
document.getElementById('toast-close').addEventListener('click',function(e){
  e.preventDefault();
  document.getElementById('toast').classList.remove('show');
  clearTimeout(_toastTimer);
});
document.addEventListener('keydown',function(e){
  if(e.key==='/'&&document.activeElement!==q&&e.target.tagName!=='INPUT'&&e.target.tagName!=='TEXTAREA'){
    e.preventDefault();q.focus();
  }else if(e.key==='Escape'){q.value='';filter('');q.blur();}
});
(function(){
  var sx,sy,el;
  document.addEventListener('touchstart',function(e){
    var li=e.target.closest('li');
    if(!li)return;
    sx=e.touches[0].clientX;sy=e.touches[0].clientY;el=li;
  },{passive:true});
  document.addEventListener('touchend',function(e){
    if(!el)return;
    var dx=e.changedTouches[0].clientX-sx,dy=Math.abs(e.changedTouches[0].clientY-sy);
    if(dx<-55&&dy<35)el.classList.toggle('swiped');
    else if(dx>20)el.classList.remove('swiped');
    el=null;
  },{passive:true});
})();
</script>`

const uiInitScript = `<script>(function(){
  var collapseAll=%v,discardedID=%q,discardedTitle=%q;
  document.querySelectorAll('.section').forEach(function(sec){
    var stored=null;
    try{stored=sessionStorage.getItem('sec-'+sec.dataset.section);}catch(e){}
    if(stored==='1')sec.classList.add('collapsed');
    else if(stored===null&&collapseAll&&sec.dataset.section==='all')sec.classList.add('collapsed');
  });
  if(discardedID){showToast(discardedID,discardedTitle);history.replaceState(null,'','/ui');}
})();</script>`

const uiWatchPage = `<!DOCTYPE html><html><head><meta charset="utf-8">
<meta name="viewport" content="width=device-width,initial-scale=1">
<title>%s</title>
<style>
  *{margin:0;padding:0;box-sizing:border-box}
  body{background:#000;width:100vw;height:100vh;overflow:hidden;display:flex;flex-direction:column}
  video{flex:1;width:100%%;min-height:0}
  footer{display:flex;align-items:center;justify-content:space-between;padding:.4em .8em;background:#111}
  footer h1{color:#eee;font-family:sans-serif;font-size:.9em;overflow:hidden;text-overflow:ellipsis;white-space:nowrap;flex:1;margin-right:1em}
  footer a{color:#aaa;font-family:sans-serif;font-size:.85em;text-decoration:none;white-space:nowrap}
  footer a:hover{color:#fff}
</style></head><body>
<video controls autoplay playsinline>
  <source src="/files/%s" type="%s">
</video>
<footer>
  <h1>%s</h1>
  <a href="/ui">← Back</a>
</footer>
<script>
document.querySelector('video').addEventListener('loadedmetadata',function(){
  this.requestFullscreen&&this.requestFullscreen();
});
</script>
</body></html>`

// ----- Helpers -----

const uiRestartingPage = `<!DOCTYPE html><html><head><meta charset="utf-8">
<meta name="viewport" content="width=device-width,initial-scale=1">
<title>Restarting…</title>
<style>
  *{margin:0;padding:0;box-sizing:border-box}
  body{font-family:sans-serif;background:#111;color:#eee;display:flex;flex-direction:column;align-items:center;justify-content:center;height:100vh;gap:1em}
  .spinner{font-size:2.5em;animation:spin 1.2s linear infinite}
  @keyframes spin{to{transform:rotate(360deg)}}
  .msg{color:#aaa;font-size:1em}
</style></head><body>
<div class="spinner">↻</div>
<div class="msg">Restarting service, please wait…</div>
<script>
function tryRedirect(){
  fetch('/ui',{method:'GET',cache:'no-store'})
    .then(function(r){if(r.ok){window.location='/ui';}else{setTimeout(tryRedirect,2000);}})
    .catch(function(){setTimeout(tryRedirect,2000);});
}
setTimeout(tryRedirect,3000);
</script>
</body></html>`

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
