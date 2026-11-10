package main

import (
	"context"
	"crypto/subtle"
	"crypto/tls"
	"embed"
	"encoding/json"
	"fmt"
	"io/fs"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"
	"golang.org/x/net/webdav"
)

//go:embed web
var webFS embed.FS

type Server struct {
	cfg   *Config
	hub   *Hub
	cert  tls.Certificate
	fp    string
	up    websocket.Upgrader
	mux   *http.ServeMux
	wdav  *webdav.Handler
	start time.Time
	notif *NotifLog
	clip  *ClipSync

	bookMu    sync.Mutex
	book      map[string]LinkTarget      // fleet address book: url -> {key,fp}
	dialStops map[string]chan struct{}   // url -> stop dialing
	dialConns map[string]*websocket.Conn // url -> active outbound connection (for force-close on remove)
	selfURLs  map[string]bool            // urls that turned out to be ourselves
	removed   map[string]bool            // tombstones: urls the user removed (don't re-add via gossip)
}

func NewServer(cfg *Config, hub *Hub, cert tls.Certificate, fp string) *Server {
	s := &Server{
		cfg: cfg, hub: hub, cert: cert, fp: fp, start: time.Now(),
		up: websocket.Upgrader{
			ReadBufferSize:  32 << 10,
			WriteBufferSize: 32 << 10,
			CheckOrigin:     func(r *http.Request) bool { return true }, // token-authenticated, not cookie-based
		},
	}
	os.MkdirAll(cfg.InboxDir, 0o755)
	s.wdav = &webdav.Handler{
		FileSystem: webdav.Dir(cfg.InboxDir),
		LockSystem: webdav.NewMemLS(),
		Prefix:     "/dav",
	}
	s.notif = NewNotifLog(desktopToast)
	s.book = map[string]LinkTarget{}
	s.dialStops = map[string]chan struct{}{}
	s.dialConns = map[string]*websocket.Conn{}
	s.selfURLs = map[string]bool{}
	s.removed = map[string]bool{}
	for _, l := range cfg.Peers { // seed the book from persisted fleet members
		s.book[l.URL] = l
	}
	for _, l := range cfg.Links {
		s.book[l.URL] = l
	}
	s.clip = NewClipSync(s)
	hub.OnEvent = func(deviceID string, e Event) {
		s.notif.handleEvent(s, deviceID, e)
		s.clip.handleEvent(deviceID, e)
	}
	s.routes()
	return s
}

// bookSnapshot returns the fleet address book (shared with peers for gossip).
func (s *Server) bookSnapshot() []LinkTarget {
	s.bookMu.Lock()
	defer s.bookMu.Unlock()
	out := make([]LinkTarget, 0, len(s.book))
	for _, l := range s.book {
		out = append(out, l)
	}
	return out
}

// mergeBook learns new fleet members from a peer and dials any we don't yet know (gossip → full mesh).
func (s *Server) mergeBook(entries []LinkTarget) {
	added := false
	for _, e := range entries {
		if e.URL == "" {
			continue
		}
		s.bookMu.Lock()
		_, known := s.book[e.URL]
		skip := s.selfURLs[e.URL] || s.removed[e.URL]
		if !known && !skip {
			s.book[e.URL] = e
		}
		s.bookMu.Unlock()
		if !known && !skip {
			s.startPeerLink(e)
			added = true
		}
	}
	if added {
		s.persistBook()
	}
}

// startPeerLink begins dialing a fleet member (idempotent per URL).
func (s *Server) startPeerLink(link LinkTarget) {
	s.bookMu.Lock()
	if _, ok := s.dialStops[link.URL]; ok || s.selfURLs[link.URL] || s.removed[link.URL] {
		s.bookMu.Unlock()
		return
	}
	stop := make(chan struct{})
	s.dialStops[link.URL] = stop
	s.bookMu.Unlock()
	go s.dialPeer(link, stop)
}

func (s *Server) stopPeerLink(url string) {
	s.bookMu.Lock()
	if stop, ok := s.dialStops[url]; ok {
		close(stop)
		delete(s.dialStops, url)
	}
	conn := s.dialConns[url]
	delete(s.dialConns, url)
	delete(s.book, url)
	s.removed[url] = true // tombstone so gossip from other hubs won't re-add it
	s.bookMu.Unlock()
	if conn != nil {
		conn.Close()
	}
}

func (s *Server) routes() {
	m := http.NewServeMux()
	// Static dashboard (also the Android WebView UI).
	sub, _ := fs.Sub(webFS, "web")
	fileServer := http.FileServer(http.FS(sub))
	index, _ := fs.ReadFile(sub, "index.html")
	m.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/" || r.URL.Path == "/index.html" {
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			w.Write(index)
			return
		}
		fileServer.ServeHTTP(w, r)
	})

	// Public (no key): lets a phone confirm identity/fingerprint before trusting.
	m.HandleFunc("/api/hello", s.handleHello)

	// Authenticated REST + streaming.
	m.HandleFunc("/api/devices", s.auth(s.handleDevices))
	m.HandleFunc("/api/rpc", s.auth(s.handleRPC))
	m.HandleFunc("/api/download", s.auth(s.handleDownload))
	m.HandleFunc("/api/upload", s.auth(s.handleUpload))
	m.HandleFunc("/api/settings", s.auth(s.handleSettings))
	m.HandleFunc("/api/models", s.auth(s.handleModels))
	m.HandleFunc("/api/notifications", s.auth(s.handleNotifications))
	m.HandleFunc("/api/clipboard", s.auth(s.handleClipboard))
	m.HandleFunc("/api/links", s.auth(s.handleLinks))
	m.HandleFunc("/api/fleet-shares", s.auth(s.handleFleetShares))
	m.HandleFunc("/ws", s.auth(s.handleWS)) // dashboard event/RPC stream

	// Agents (phones / other PCs) link here.
	m.HandleFunc("/link", s.handleLink)

	// WebDAV share of the Inbox (mountable as a network drive / SMB-like share).
	m.HandleFunc("/dav", s.davAuth(s.handleDav))
	m.HandleFunc("/dav/", s.davAuth(s.handleDav))

	// MCP endpoint for Claude (loopback only; see handleMCP).
	m.HandleFunc("/mcp", s.handleMCP)
	m.HandleFunc("/mcp/", s.handleMCP)

	s.mux = m
}

// ---- auth ----

func (s *Server) token(r *http.Request) string {
	if h := r.Header.Get("Authorization"); strings.HasPrefix(h, "Bearer ") {
		return strings.TrimPrefix(h, "Bearer ")
	}
	if k := r.Header.Get("X-BridgeDesk-Key"); k != "" {
		return k
	}
	// WebDAV / "Map network drive" clients send the key as the Basic-auth password (or username).
	if user, pass, ok := r.BasicAuth(); ok {
		if pass != "" {
			return pass
		}
		return user
	}
	return r.URL.Query().Get("key")
}

func (s *Server) keyOK(k string) bool {
	return subtle.ConstantTimeCompare([]byte(strings.TrimSpace(k)), []byte(s.cfg.AccessKey)) == 1
}

func (s *Server) auth(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !s.keyOK(s.token(r)) {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		// Loopback (the user's own PC: dashboard/MCP on 127.0.0.1) is never subject to the
		// Google gate — the key + being local already prove it, and we must not lock the owner out.
		if s.cfg.RequireGoogle && !isLoopbackReq(r) {
			if _, err := s.googleGate(googleToken(r)); err != nil {
				http.Error(w, "google sign-in required: "+err.Error(), http.StatusUnauthorized)
				return
			}
			// Let the dashboard WebView's later same-origin requests carry the token.
			if tok := r.URL.Query().Get("gid"); tok != "" {
				http.SetCookie(w, &http.Cookie{Name: "aria_gid", Value: tok, Path: "/", HttpOnly: true, SameSite: http.SameSiteStrictMode})
			}
		}
		h(w, r)
	}
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(v)
}

// ---- handlers ----

func (s *Server) handleHello(w http.ResponseWriter, r *http.Request) {
	info := s.hub.local.Info()
	writeJSON(w, 200, map[string]any{
		"name":        info.Name,
		"kind":        info.Kind,
		"os":          info.OS,
		"fingerprint": s.fp,
		"version":     appVersion,
		"needsKey":    true,
	})
}

func (s *Server) handleDevices(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, 200, map[string]any{"devices": s.hub.List(), "self": s.hub.local.Info().ID})
}

type rpcRequest struct {
	Device string          `json:"device"`
	Method string          `json:"method"`
	Params json.RawMessage `json:"params"`
}

func (s *Server) handleRPC(w http.ResponseWriter, r *http.Request) {
	var req rpcRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, err.Error(), 400)
		return
	}
	dev, err := s.hub.Get(req.Device)
	if err != nil {
		writeJSON(w, 404, map[string]string{"error": err.Error()})
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 3*time.Hour)
	defer cancel()
	res, err := dev.Call(ctx, req.Method, req.Params)
	if err != nil {
		writeJSON(w, 200, map[string]any{"error": err.Error()})
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Write([]byte(`{"result":`))
	w.Write(res)
	w.Write([]byte(`}`))
}

// handleDownload streams a file from any device by chunked fs.read (so big files on a phone work too).
func (s *Server) handleDownload(w http.ResponseWriter, r *http.Request) {
	device := r.URL.Query().Get("device")
	path := r.URL.Query().Get("path")
	dev, err := s.hub.Get(device)
	if err != nil {
		http.Error(w, err.Error(), 404)
		return
	}
	name := path
	if i := strings.LastIndexAny(path, `/\`); i >= 0 {
		name = path[i+1:]
	}
	w.Header().Set("Content-Disposition", "attachment; filename=\""+name+"\"")
	w.Header().Set("Content-Type", "application/octet-stream")
	var offset int64
	for {
		p, _ := json.Marshal(map[string]any{"path": path, "offset": offset, "length": 1 << 20})
		raw, err := dev.Call(r.Context(), "fs.read", p)
		if err != nil {
			return
		}
		var chunk struct {
			Data string `json:"data"`
			EOF  bool   `json:"eof"`
		}
		json.Unmarshal(raw, &chunk)
		b := decodeB64(chunk.Data)
		w.Write(b)
		offset += int64(len(b))
		if chunk.EOF || len(b) == 0 {
			return
		}
	}
}

func (s *Server) handleUpload(w http.ResponseWriter, r *http.Request) {
	device := r.URL.Query().Get("device")
	dir := r.URL.Query().Get("dir")
	name := r.URL.Query().Get("name")
	dev, err := s.hub.Get(device)
	if err != nil {
		http.Error(w, err.Error(), 404)
		return
	}
	sep := "/"
	if dev.Info().Kind == "windows" {
		sep = "\\"
	}
	dest := strings.TrimRight(dir, `/\`) + sep + name
	buf := make([]byte, 1<<20)
	var offset int64
	first := true
	for {
		n, rerr := r.Body.Read(buf)
		if n > 0 {
			p, _ := json.Marshal(map[string]any{"path": dest, "offset": offset, "data": encodeB64(buf[:n]), "truncate": first})
			first = false
			if _, err := dev.Call(r.Context(), "fs.write", p); err != nil {
				http.Error(w, err.Error(), 500)
				return
			}
			offset += int64(n)
		}
		if rerr != nil {
			break
		}
	}
	writeJSON(w, 200, map[string]any{"path": dest, "size": offset})
}

// handleNotifications serves the persistent history (GET, filterable) and manages it (POST).
//
//	GET  /api/notifications?q=&app=&codes=1&unread=1&limit=100
//	POST /api/notifications {"op":"read","key":""} | {"op":"clear"} | {"op":"mute"|"unmute","app":"WhatsApp"}
func (s *Server) handleNotifications(w http.ResponseWriter, r *http.Request) {
	if r.Method == "POST" {
		var in struct {
			Op  string `json:"op"`
			Key string `json:"key"`
			App string `json:"app"`
		}
		json.NewDecoder(r.Body).Decode(&in)
		switch in.Op {
		case "read":
			s.notif.MarkRead(in.Key)
		case "clear":
			s.notif.Clear()
		case "mute", "unmute":
			app := strings.TrimSpace(in.App)
			if app == "" {
				http.Error(w, "app required", 400)
				return
			}
			var keep []string
			for _, m := range s.cfg.MutedApps {
				if !strings.EqualFold(m, app) {
					keep = append(keep, m)
				}
			}
			if in.Op == "mute" {
				keep = append(keep, app)
			}
			s.cfg.MutedApps = keep
			s.cfg.Save()
		default:
			http.Error(w, "unknown op", 400)
			return
		}
		writeJSON(w, 200, map[string]any{"ok": true, "summary": s.notif.Summary(s.cfg)})
		return
	}
	q := r.URL.Query()
	limit, _ := strconv.Atoi(q.Get("limit"))
	if limit <= 0 {
		limit = 200
	}
	list := s.notif.Query(s.cfg, NotifQuery{Q: q.Get("q"), App: q.Get("app"), CodesOnly: q.Get("codes") == "1",
		Unread: q.Get("unread") == "1", Limit: limit})
	writeJSON(w, 200, map[string]any{"notifications": list, "summary": s.notif.Summary(s.cfg)})
}

// handleFleetShares auto-discovers SMB shares offered by every online device in the fleet,
// so the dashboard can show them with one-click mount.
func (s *Server) handleFleetShares(w http.ResponseWriter, r *http.Request) {
	type row struct {
		DeviceID string `json:"deviceId"`
		Device   string `json:"device"`
		Kind     string `json:"kind"`
		Name     string `json:"name"`
		Path     string `json:"path"`
		UNC      string `json:"unc"`
		User     string `json:"user,omitempty"`
		Pass     string `json:"pass,omitempty"`
	}
	self := s.hub.local.Info().ID
	var mu sync.Mutex
	var rows []row
	var wg sync.WaitGroup
	for _, d := range s.hub.List() {
		if !d.Online {
			continue
		}
		d := d
		wg.Add(1)
		go func() {
			defer wg.Done()
			dev, err := s.hub.Get(d.ID)
			if err != nil {
				return
			}
			ctx, cancel := context.WithTimeout(r.Context(), 8*time.Second)
			raw, err := dev.Call(ctx, "share.list", nil)
			cancel()
			if err != nil {
				return
			}
			var shares []ShareInfo
			json.Unmarshal(raw, &shares)
			mu.Lock()
			for _, sh := range shares {
				rows = append(rows, row{d.ID, d.Name, d.Kind, sh.Name, sh.Path, sh.UNC, sh.User, sh.Pass})
			}
			mu.Unlock()
		}()
	}
	wg.Wait()
	writeJSON(w, 200, map[string]any{"shares": rows, "self": self})
}

// handleLinks manages the fleet of hubs this one meshes with.
// POST {host, port, key, fingerprint?} — join another hub into the fleet (fingerprint auto-resolved).
// DELETE ?url=... — forget a fleet member. GET — list known members with live status.
func (s *Server) handleLinks(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case "POST":
		var in struct {
			Host, Key, Fingerprint string
			Port                   int
		}
		if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
			http.Error(w, err.Error(), 400)
			return
		}
		in.Host = strings.TrimSpace(in.Host)
		if in.Host == "" || in.Key == "" {
			writeJSON(w, 400, map[string]string{"error": "address and access key are required"})
			return
		}
		if in.Port == 0 {
			in.Port = defaultLANPort
		}
		fp := strings.ReplaceAll(in.Fingerprint, " ", "")
		if fp == "" {
			got, err := fetchFingerprint(in.Host, in.Port)
			if err != nil {
				writeJSON(w, 200, map[string]string{"error": "could not reach that computer at " + in.Host + ": " + err.Error()})
				return
			}
			fp = got
		}
		entry := LinkTarget{URL: fmt.Sprintf("https://%s:%d", in.Host, in.Port), Key: in.Key, Fingerprint: fp}
		s.bookMu.Lock()
		delete(s.removed, entry.URL) // explicit add clears any tombstone
		s.book[entry.URL] = entry
		s.bookMu.Unlock()
		s.persistBook()
		s.startPeerLink(entry) // dial it; the mesh is symmetric + gossips, so the whole fleet converges
		writeJSON(w, 200, map[string]any{"ok": true, "url": entry.URL, "fingerprint": prettyFP(fp)})

	case "DELETE":
		url := r.URL.Query().Get("url")
		s.stopPeerLink(url)
		s.persistBook()
		writeJSON(w, 200, map[string]any{"ok": true})

	default:
		type linkView struct {
			URL         string `json:"url"`
			Fingerprint string `json:"fingerprint"`
		}
		links := []linkView{}
		for _, l := range s.bookSnapshot() {
			links = append(links, linkView{URL: l.URL, Fingerprint: prettyFP(l.Fingerprint)})
		}
		writeJSON(w, 200, map[string]any{"links": links})
	}
}

// persistBook writes the current fleet address book to the config so members reconnect after a restart.
func (s *Server) persistBook() {
	entries := s.bookSnapshot()
	s.cfg.Peers = entries
	s.cfg.Links = nil
	s.cfg.Save()
}

func (s *Server) handleSettings(w http.ResponseWriter, r *http.Request) {
	if r.Method == "POST" {
		var in map[string]any
		json.NewDecoder(r.Body).Decode(&in)
		applySettings(s.cfg, in)
		s.cfg.Save()
	}
	ips := localIPs()
	host := "<this-PC-IP>"
	if len(ips) > 0 {
		host = ips[0]
	}
	clipSupported, clipOn := winClipHistory()
	writeJSON(w, 200, map[string]any{
		"name": s.cfg.Name, "lanPort": s.cfg.LANPort, "listenLan": s.cfg.ListenLAN,
		"discovery": s.cfg.Discovery, "allowShell": s.cfg.AllowShell, "allowScreen": s.cfg.AllowScreen,
		"allowFiles": s.cfg.AllowFiles, "allowMcp": s.cfg.AllowMCP, "mirrorNotif": s.cfg.MirrorNotif, "inboxDir": s.cfg.InboxDir,
		"clipSync": !s.cfg.ClipSyncOff, "winClipHistorySupported": clipSupported, "winClipHistory": clipOn,
		"autoCopyCodes": s.cfg.AutoCopyCodes, "notifWebhook": s.cfg.NotifWebhook, "mutedApps": s.cfg.MutedApps,
		"fingerprint": s.fp, "autostart": autostartEnabled(), "accessKey": s.cfg.AccessKey,
		"requireGoogle": s.cfg.RequireGoogle, "googleEmails": s.cfg.GoogleEmails, "googleClients": s.cfg.GoogleClients,
		"addresses": ips, "primaryHost": host,
		"elevated": isElevated(), "os": s.hub.local.Info().Kind,
		"davUrl": fmt.Sprintf("https://%s:%d/dav/", host, s.cfg.LANPort),
	})
}

func applySettings(c *Config, in map[string]any) {
	b := func(k string, cur *bool) {
		if v, ok := in[k].(bool); ok {
			*cur = v
		}
	}
	b("listenLan", &c.ListenLAN)
	b("discovery", &c.Discovery)
	b("allowShell", &c.AllowShell)
	b("allowScreen", &c.AllowScreen)
	b("allowFiles", &c.AllowFiles)
	b("allowMcp", &c.AllowMCP)
	b("mirrorNotif", &c.MirrorNotif)
	b("autoCopyCodes", &c.AutoCopyCodes)
	if v, ok := in["clipSync"].(bool); ok {
		c.ClipSyncOff = !v
	}
	if v, ok := in["winClipHistory"].(bool); ok {
		setWinClipHistory(v)
	}
	if v, ok := in["notifWebhook"].(string); ok {
		v = strings.TrimSpace(v)
		if v == "" || strings.HasPrefix(v, "https://") || strings.HasPrefix(v, "http://") {
			c.NotifWebhook = v
		}
	}
	if v, ok := in["name"].(string); ok && v != "" {
		c.Name = v
	}
	if v, ok := in["inboxDir"].(string); ok && v != "" {
		c.InboxDir = v
	}
	if v, ok := in["autostart"].(bool); ok {
		setAutostart(v)
	}
	if v, ok := in["rotateKey"].(bool); ok && v {
		c.AccessKey = randomKey(16)
	}
	b("requireGoogle", &c.RequireGoogle)
	if list, ok := strList(in["googleEmails"]); ok {
		c.GoogleEmails = list
	}
	if list, ok := strList(in["googleClients"]); ok {
		c.GoogleClients = list
	}
}

// strList accepts a JSON array of strings or a comma/space/newline-separated string.
func strList(v any) ([]string, bool) {
	switch x := v.(type) {
	case []any:
		out := []string{}
		for _, e := range x {
			if s, ok := e.(string); ok && strings.TrimSpace(s) != "" {
				out = append(out, strings.TrimSpace(s))
			}
		}
		return out, true
	case string:
		out := []string{}
		for _, s := range strings.FieldsFunc(x, func(r rune) bool { return r == ',' || r == ' ' || r == '\n' || r == '\t' || r == ';' }) {
			if strings.TrimSpace(s) != "" {
				out = append(out, strings.TrimSpace(s))
			}
		}
		return out, true
	}
	return nil, false
}

func (s *Server) handleDav(w http.ResponseWriter, r *http.Request) {
	if !s.cfg.AllowFiles {
		http.Error(w, "file sharing disabled", 403)
		return
	}
	s.wdav.ServeHTTP(w, r)
}

// davAuth is like auth but issues a Basic challenge so the OS prompts for the key when mapping a drive.
func (s *Server) davAuth(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !s.keyOK(s.token(r)) {
			w.Header().Set("WWW-Authenticate", `Basic realm="BridgeDesk"`)
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		h(w, r)
	}
}

func (s *Server) handleLink(w http.ResponseWriter, r *http.Request) {
	if !s.keyOK(s.token(r)) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	if s.cfg.RequireGoogle && !isLoopbackReq(r) {
		if _, err := s.googleGate(googleToken(r)); err != nil {
			http.Error(w, "google sign-in required: "+err.Error(), http.StatusUnauthorized)
			return
		}
	}
	conn, err := s.up.Upgrade(w, r, nil)
	if err != nil {
		return
	}
	s.serveLinked(conn, r.RemoteAddr)
}

// handleWS is the dashboard's live channel: forwards RPCs and streams events (terminal data, screen frames).
func (s *Server) handleWS(w http.ResponseWriter, r *http.Request) {
	conn, err := s.up.Upgrade(w, r, nil)
	if err != nil {
		return
	}
	defer conn.Close()
	var wmu sync.Mutex
	send := func(v any) {
		wmu.Lock()
		conn.SetWriteDeadline(time.Now().Add(20 * time.Second))
		conn.WriteJSON(v)
		wmu.Unlock()
	}
	var subs []func()
	defer func() {
		for _, u := range subs {
			u()
		}
	}()
	// Every dashboard gets fleet-wide notification pushes (badge, browser alerts, live list).
	subs = append(subs, s.notif.Listen(func(event string, n any) {
		send(map[string]any{"event": event, "params": n})
	}))

	// One Aria (claude CLI) turn may be in flight per connection; ai.cancel kills it.
	ai := &aiSession{}
	defer ai.cancel()
	// Per-connection approval channel: the phone answers ai.approval cards with ai.approve.
	approvals := newApprovalRegistry()

	for {
		var msg struct {
			ID     int64           `json:"id"`
			Action string          `json:"action"`
			Device string          `json:"device"`
			Method string          `json:"method"`
			Sid    string          `json:"sid"`
			Params json.RawMessage `json:"params"`
		}
		if err := conn.ReadJSON(&msg); err != nil {
			return
		}
		switch msg.Action {
		case "rpc":
			go func() {
				dev, err := s.hub.Get(msg.Device)
				if err != nil {
					send(map[string]any{"id": msg.ID, "error": err.Error()})
					return
				}
				ctx, cancel := context.WithTimeout(context.Background(), 3*time.Hour)
				defer cancel()
				res, err := dev.Call(ctx, msg.Method, msg.Params)
				if err != nil {
					send(map[string]any{"id": msg.ID, "error": err.Error()})
					return
				}
				send(map[string]any{"id": msg.ID, "result": json.RawMessage(res)})
			}()
		case "subscribe":
			unsub := s.hub.Subscribe(deviceID(s.hub, msg.Device), msg.Sid, func(e Event) {
				send(map[string]any{"event": e.Event, "params": json.RawMessage(e.Params)})
			})
			subs = append(subs, unsub)
			send(map[string]any{"id": msg.ID, "result": "subscribed"})
		case "ai.ask":
			// Relay a phone prompt to the local Claude CLI and stream the reply back.
			var req aiReq
			json.Unmarshal(msg.Params, &req)
			id := msg.ID
			go func() {
				ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
				defer cancel()
				ai.setCancel(cancel)
				session, text, err := s.runAssistantTurn(ctx, req, func(event string, p any) {
					send(map[string]any{"event": event, "params": p})
				}, ai.set, approvals)
				if err != nil {
					send(map[string]any{"id": id, "error": err.Error(), "result": map[string]any{"session": session}})
					return
				}
				send(map[string]any{"id": id, "result": map[string]any{"session": session, "text": text}})
			}()
		case "ai.fleet":
			// Run the same task on every (or selected) online device as its own Claude turn.
			var freq aiFleetReq
			json.Unmarshal(msg.Params, &freq)
			id := msg.ID
			go func() {
				ctx, cancel := context.WithTimeout(context.Background(), 45*time.Minute)
				defer cancel()
				ai.setCancel(cancel)
				devices, err := s.runFleet(ctx, freq, func(event string, p any) {
					send(map[string]any{"event": event, "params": p})
				})
				if err != nil {
					send(map[string]any{"id": id, "error": err.Error()})
					return
				}
				send(map[string]any{"id": id, "result": map[string]any{"devices": devices}})
			}()
		case "ai.cancel":
			ai.cancel()
			send(map[string]any{"id": msg.ID, "result": "cancelled"})
		case "ai.approve":
			// Phone's answer to an ai.approval card: {id, allow}.
			var d struct {
				ID    string `json:"id"`
				Allow bool   `json:"allow"`
			}
			json.Unmarshal(msg.Params, &d)
			approvals.resolve(d.ID, d.Allow)
		}
	}
}

func deviceID(h *Hub, name string) string {
	if d, err := h.Get(name); err == nil {
		return d.Info().ID
	}
	return name
}

// ---- listeners ----

func (s *Server) Run() error {
	go s.clip.watch()
	tlsCfg := &tls.Config{Certificates: []tls.Certificate{s.cert}, MinVersion: tls.VersionTLS12}

	// Loopback HTTP: dashboard + MCP, never exposed off-box.
	// Retry briefly so a hand-off (e.g. relaunching elevated) can take the port from the exiting copy.
	localAddr := net.JoinHostPort("127.0.0.1", strconv.Itoa(s.cfg.LocalPort))
	ln, err := listenRetry("tcp", localAddr, nil)
	if err != nil {
		return fmt.Errorf("local listen: %w (is another BridgeDesk already running?)", err)
	}
	go http.Serve(ln, s.mux)
	log.Printf("dashboard:   http://127.0.0.1:%d/?key=%s", s.cfg.LocalPort, s.cfg.AccessKey)

	if s.cfg.ListenLAN {
		lanAddr := net.JoinHostPort("0.0.0.0", strconv.Itoa(s.cfg.LANPort))
		tln, err := listenRetry("tcp", lanAddr, tlsCfg)
		if err != nil {
			return fmt.Errorf("lan listen: %w", err)
		}
		go http.Serve(tln, s.mux)
		for _, ip := range localIPs() {
			log.Printf("LAN (phone): https://%s:%d/  key=%s", ip, s.cfg.LANPort, s.cfg.AccessKey)
		}
		if s.cfg.Discovery {
			go s.runDiscovery()
		}
	}

	// Dial every known fleet member; the mesh gossips so the whole fleet reconnects.
	for _, link := range s.bookSnapshot() {
		s.startPeerLink(link)
	}
	return nil
}

// listenRetry opens a listener, retrying for a few seconds if the port is briefly still held
// (used during the non-elevated → elevated hand-off). If tlsCfg is set it returns a TLS listener.
func listenRetry(network, addr string, tlsCfg *tls.Config) (net.Listener, error) {
	var lastErr error
	for i := 0; i < 20; i++ {
		var ln net.Listener
		if tlsCfg != nil {
			ln, lastErr = tls.Listen(network, addr, tlsCfg)
		} else {
			ln, lastErr = net.Listen(network, addr)
		}
		if lastErr == nil {
			return ln, nil
		}
		time.Sleep(300 * time.Millisecond)
	}
	return nil, lastErr
}

// primaryOutboundIP returns the IP of the interface with the default route — the real LAN IP,
// not a virtual/link-local one. Nothing is actually sent (UDP connect just selects a route).
func primaryOutboundIP() string {
	c, err := net.Dial("udp", "8.8.8.8:80")
	if err != nil {
		return ""
	}
	defer c.Close()
	if ua, ok := c.LocalAddr().(*net.UDPAddr); ok {
		return ua.IP.String()
	}
	return ""
}

func isPrivateIP(ip net.IP) bool {
	if ip4 := ip.To4(); ip4 != nil {
		switch {
		case ip4[0] == 10:
			return true
		case ip4[0] == 172 && ip4[1] >= 16 && ip4[1] <= 31:
			return true
		case ip4[0] == 192 && ip4[1] == 168:
			return true
		}
	}
	return false
}

// localIPs lists this machine's usable IPv4 addresses, best first: the default-route IP,
// then other private-LAN addresses, with loopback and link-local (169.254.x) junk filtered out.
func localIPs() []string {
	primary := primaryOutboundIP()
	addrs, _ := net.InterfaceAddrs()
	var lan, other []string
	seen := map[string]bool{}
	for _, a := range addrs {
		ipn, ok := a.(*net.IPNet)
		if !ok {
			continue
		}
		ip := ipn.IP.To4()
		if ip == nil || ip.IsLoopback() || ip.IsLinkLocalUnicast() {
			continue // drop 127.* and 169.254.* (Windows APIPA)
		}
		s := ip.String()
		if seen[s] || s == primary {
			continue
		}
		seen[s] = true
		if isPrivateIP(ip) {
			lan = append(lan, s)
		} else {
			other = append(other, s)
		}
	}
	out := []string{}
	if primary != "" {
		out = append(out, primary)
	}
	out = append(out, lan...)
	out = append(out, other...)
	return out
}

// isLoopbackReq reports whether the request came from this machine (127.0.0.1 / ::1).
func isLoopbackReq(r *http.Request) bool {
	ip := net.ParseIP(sourceIP(r))
	return ip != nil && ip.IsLoopback()
}

// sourceIP returns the client IP of an incoming request (no port).
func sourceIP(r *http.Request) string {
	host := r.RemoteAddr
	if i := strings.LastIndex(host, ":"); i >= 0 {
		host = host[:i]
	}
	return strings.Trim(host, "[]")
}

func decodeB64(s string) []byte { b, _ := b64dec(s); return b }
func encodeB64(b []byte) string { return b64enc(b) }

func (s *Server) handleDownloadStatic(w http.ResponseWriter, r *http.Request) {}

var _ = filepath.Join

// handleClipboard serves the fleet clipboard history.
//
//	GET  /api/clipboard
//	POST /api/clipboard {"op":"use","id":3,"target":"all"|"<deviceId>"} | {"op":"send","text":"…","target":…} | {"op":"clear"}
func (s *Server) handleClipboard(w http.ResponseWriter, r *http.Request) {
	if r.Method == "POST" {
		var in struct {
			Op     string `json:"op"`
			ID     int64  `json:"id"`
			Text   string `json:"text"`
			Target string `json:"target"`
		}
		json.NewDecoder(r.Body).Decode(&in)
		switch in.Op {
		case "clear":
			s.clip.Clear()
		case "use", "send":
			text := in.Text
			if in.Op == "use" {
				c, ok := s.clip.get(in.ID)
				if !ok {
					http.Error(w, "clip not found", 404)
					return
				}
				text = c.Text
			}
			if text == "" {
				http.Error(w, "empty", 400)
				return
			}
			if err := s.clipTo(r.Context(), in.Target, text); err != nil {
				http.Error(w, err.Error(), 502)
				return
			}
		default:
			http.Error(w, "unknown op", 400)
			return
		}
		writeJSON(w, 200, map[string]any{"ok": true})
		return
	}
	writeJSON(w, 200, map[string]any{"clips": s.clip.History(), "sync": !s.cfg.ClipSyncOff})
}

// clipTo puts text on one device's clipboard, or on every device ("all" / "").
func (s *Server) clipTo(ctx context.Context, target, text string) error {
	if target == "" || target == "all" {
		s.clip.mu.Lock()
		s.clip.lastLocal = text
		s.clip.mu.Unlock()
		clipboardSet(text)
		s.clip.announce(text) // every hub and phone picks it up
		return nil
	}
	return s.hub.Call(ctx, target, "clipboard.set", map[string]any{"text": text}, nil)
}
