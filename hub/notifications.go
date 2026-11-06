package main

import (
	"bytes"
	"encoding/json"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"
)

// Notification is one mirrored phone notification. The hub keeps a persistent history so the
// dashboard and MCP can search it later, even after the phone dismissed it or the hub restarted.
type Notification struct {
	Sid      string          `json:"sid,omitempty"` // set only on live dashboard pushes
	DeviceID string          `json:"deviceId"`
	Device   string          `json:"device"`
	Key      string          `json:"key"`
	Pkg      string          `json:"pkg"`
	App      string          `json:"app"`
	Title    string          `json:"title"`
	Text     string          `json:"text"`
	Sub      string          `json:"sub"`
	Category string          `json:"category,omitempty"` // Android category: msg, call, email, alarm…
	Time     int64           `json:"time"`
	Actions  json.RawMessage `json:"actions,omitempty"`
	CanOpen  bool            `json:"canOpen,omitempty"`
	Code     string          `json:"code,omitempty"`  // detected one-time / verification code
	Active   bool            `json:"active"`          // still showing on the phone
	Read     bool            `json:"read"`            // seen in the dashboard
	Muted    bool            `json:"muted,omitempty"` // computed from the muted-apps list when served
	Received int64           `json:"received"`        // when the hub got it (ms)
}

type NotifLog struct {
	mu        sync.Mutex
	ring      []Notification
	max       int
	path      string
	saveTimer *time.Timer
	toast     func(title, text string)
	listeners map[int]func(string, any)
	nextID    int
}

func NewNotifLog(toast func(string, string)) *NotifLog {
	nl := &NotifLog{max: 500, toast: toast, listeners: map[int]func(string, any){},
		path: filepath.Join(configDir(), "notifications.json")}
	if data, err := os.ReadFile(nl.path); err == nil {
		json.Unmarshal(data, &nl.ring)
		for i := range nl.ring {
			nl.ring[i].Active = false // the phone re-reports what is still active
		}
	}
	return nl
}

// ---- one-time code detection ----

var (
	// "Your code is 123456", "code de vérification : 123 456", "OTP: 9921"
	codeAfterKeyword = regexp.MustCompile(`(?i)(?:code|otp|pin|passcode|password|verif\w*|security|token|login|sign[- ]?in|authenticat\w*|código|codice|kode|kod)\D{0,40}?(\b\d{3}[- ]\d{3}\b|\b\d{4,8}\b)`)
	// "G-123456 is your Google verification code", "123456 est votre code"
	codeBeforeKeyword = regexp.MustCompile(`(?i)\b(?:[A-Z]-)?(\d{4,8})\b[^\n]{0,45}?(?:is your|est votre|es tu|ist dein|is the|code|otp|verif|security)`)
	digitsOnly        = regexp.MustCompile(`\D`)
)

func extractCode(title, text string) string {
	s := title + "\n" + text
	for _, re := range []*regexp.Regexp{codeAfterKeyword, codeBeforeKeyword} {
		if m := re.FindStringSubmatch(s); m != nil {
			c := digitsOnly.ReplaceAllString(m[1], "")
			if len(c) >= 4 && len(c) <= 8 && !looksLikeYear(c) {
				return c
			}
		}
	}
	return ""
}

func looksLikeYear(c string) bool {
	return len(c) == 4 && (strings.HasPrefix(c, "19") || strings.HasPrefix(c, "20"))
}

// ---- muting ----

func isMuted(cfg *Config, n *Notification) bool {
	for _, m := range cfg.MutedApps {
		m = strings.ToLower(strings.TrimSpace(m))
		if m != "" && (m == strings.ToLower(n.Pkg) || m == strings.ToLower(n.App)) {
			return true
		}
	}
	return false
}

// handleEvent is wired to Hub.OnEvent; it reacts to phone notif.posted/removed events.
func (nl *NotifLog) handleEvent(s *Server, deviceID string, e Event) {
	switch e.Event {
	case "notif.posted":
		var n Notification
		if err := json.Unmarshal(e.Params, &n); err != nil {
			return
		}
		n.Sid = ""
		n.DeviceID = deviceID
		if d, err := s.hub.Get(deviceID); err == nil {
			n.Device = d.Info().Name
		}
		n.Active = true
		n.Received = time.Now().UnixMilli()
		n.Code = extractCode(n.Title, n.Text)
		n.Muted = isMuted(s.cfg, &n)
		fresh := nl.add(n)
		if !fresh {
			return // same content re-posted (progress updates etc.): no new toast/forward
		}
		nl.broadcast("notif.posted", n)
		if n.Muted {
			return
		}
		if n.Code != "" && s.cfg.AutoCopyCodes {
			if err := clipboardSet(n.Code); err == nil && nl.toast != nil {
				nl.toast("Code "+n.Code+" copied", n.App+": "+firstLine(n.Title, n.Text))
			}
		} else if s.cfg.MirrorNotif && nl.toast != nil {
			title := n.App
			if n.Title != "" {
				title = n.App + ": " + n.Title
			}
			nl.toast(title, n.Text)
		}
		if s.cfg.NotifWebhook != "" && n.Code == "" { // codes are never forwarded off this PC
			go postWebhook(s.cfg.NotifWebhook, n)
		}
		log.Printf("notification [%s] %s: %s — %s", n.Device, n.App, n.Title, n.Text)
	case "call.state", "sms.received":
		var m map[string]any
		if json.Unmarshal(e.Params, &m) != nil {
			return
		}
		m["deviceId"] = deviceID
		if d, err := s.hub.Get(deviceID); err == nil {
			m["device"] = d.Info().Name
		}
		nl.broadcastMap(e.Event, m)
		who := strOf(m["name"])
		if who == "" {
			who = strOf(m["number"])
		}
		if e.Event == "call.state" && nl.toast != nil && s.cfg.MirrorNotif {
			switch {
			case strOf(m["state"]) == "ringing":
				nl.toast("📞 Incoming call", who+" — answer or decline from BridgeDesk")
			case m["missed"] == true:
				nl.toast("Missed call", who)
			}
		}
		log.Printf("%s [%v] %s", e.Event, m["device"], who)
	case "notif.removed":
		var p struct {
			Key string `json:"key"`
		}
		json.Unmarshal(e.Params, &p)
		nl.markInactive(p.Key)
		nl.broadcast("notif.removed", Notification{Key: p.Key, DeviceID: deviceID})
	}
}

func firstLine(a, b string) string {
	if a != "" {
		return a
	}
	if i := strings.IndexByte(b, '\n'); i >= 0 {
		return b[:i]
	}
	return b
}

// add stores n (replacing an earlier version with the same key) and reports whether it is new content.
func (nl *NotifLog) add(n Notification) bool {
	nl.mu.Lock()
	defer nl.mu.Unlock()
	for i, x := range nl.ring {
		if x.Key == n.Key && x.DeviceID == n.DeviceID {
			if x.Title == n.Title && x.Text == n.Text {
				nl.ring[i].Active = true
				nl.ring[i].Actions = n.Actions
				nl.scheduleSave()
				return false
			}
			nl.ring = append(nl.ring[:i], nl.ring[i+1:]...)
			break
		}
	}
	nl.ring = append(nl.ring, n)
	if len(nl.ring) > nl.max {
		nl.ring = nl.ring[len(nl.ring)-nl.max:]
	}
	nl.scheduleSave()
	return true
}

func (nl *NotifLog) markInactive(key string) {
	nl.mu.Lock()
	defer nl.mu.Unlock()
	for i := range nl.ring {
		if nl.ring[i].Key == key {
			nl.ring[i].Active = false
		}
	}
	nl.scheduleSave()
}

// MarkRead marks everything (key == "") or one notification as read.
func (nl *NotifLog) MarkRead(key string) {
	nl.mu.Lock()
	defer nl.mu.Unlock()
	for i := range nl.ring {
		if key == "" || nl.ring[i].Key == key {
			nl.ring[i].Read = true
		}
	}
	nl.scheduleSave()
}

// Clear drops history (inactive items only, so live prompts stay actionable).
func (nl *NotifLog) Clear() {
	nl.mu.Lock()
	defer nl.mu.Unlock()
	keep := nl.ring[:0]
	for _, n := range nl.ring {
		if n.Active {
			keep = append(keep, n)
		}
	}
	nl.ring = keep
	nl.scheduleSave()
}

func (nl *NotifLog) scheduleSave() {
	if nl.saveTimer != nil {
		nl.saveTimer.Stop()
	}
	nl.saveTimer = time.AfterFunc(2*time.Second, nl.save)
}

func (nl *NotifLog) save() {
	nl.mu.Lock()
	data, _ := json.Marshal(nl.ring)
	nl.mu.Unlock()
	tmp := nl.path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err == nil {
		os.Rename(tmp, nl.path)
	}
}

// NotifQuery filters history: q matches app/title/text, app matches pkg or app name.
type NotifQuery struct {
	Q         string
	App       string
	CodesOnly bool
	Unread    bool
	Limit     int
	SinceMs   int64
}

// Query returns matching notifications, newest first, with Muted computed from cfg.
func (nl *NotifLog) Query(cfg *Config, f NotifQuery) []Notification {
	nl.mu.Lock()
	defer nl.mu.Unlock()
	q := strings.ToLower(strings.TrimSpace(f.Q))
	app := strings.ToLower(strings.TrimSpace(f.App))
	out := []Notification{}
	for i := len(nl.ring) - 1; i >= 0; i-- {
		n := nl.ring[i]
		n.Muted = isMuted(cfg, &n)
		if f.CodesOnly && n.Code == "" {
			continue
		}
		if f.Unread && n.Read {
			continue
		}
		if f.SinceMs > 0 && n.Received < f.SinceMs && n.Time < f.SinceMs {
			continue
		}
		if app != "" && strings.ToLower(n.App) != app && strings.ToLower(n.Pkg) != app {
			continue
		}
		if q != "" && !strings.Contains(strings.ToLower(n.App+"\n"+n.Title+"\n"+n.Text+"\n"+n.Device), q) {
			continue
		}
		out = append(out, n)
		if f.Limit > 0 && len(out) >= f.Limit {
			break
		}
	}
	return out
}

// Summary gives counts for the dashboard badge and app filter.
func (nl *NotifLog) Summary(cfg *Config) map[string]any {
	nl.mu.Lock()
	defer nl.mu.Unlock()
	unread := 0
	apps := map[string]int{}
	for i := range nl.ring {
		n := nl.ring[i]
		muted := isMuted(cfg, &n)
		if !n.Read && !muted {
			unread++
		}
		apps[n.App]++
	}
	return map[string]any{"unread": unread, "total": len(nl.ring), "apps": apps, "muted": cfg.MutedApps}
}

// recent keeps the old API shape (newest first).
func (nl *NotifLog) recent() []Notification { return nl.Query(&Config{}, NotifQuery{}) }

// ---- live push to open dashboards ----

func (nl *NotifLog) Listen(fn func(event string, params any)) func() {
	nl.mu.Lock()
	id := nl.nextID
	nl.nextID++
	nl.listeners[id] = fn
	nl.mu.Unlock()
	return func() { nl.mu.Lock(); delete(nl.listeners, id); nl.mu.Unlock() }
}

func (nl *NotifLog) broadcast(event string, n Notification) {
	n.Sid = "hub.notif"
	nl.emit(event, n)
}

// broadcastMap pushes any other hub event (calls, texts, clipboard) to open dashboards.
func (nl *NotifLog) broadcastMap(event string, m map[string]any) {
	m["sid"] = "hub.notif"
	nl.emit(event, m)
}

func (nl *NotifLog) emit(event string, payload any) {
	nl.mu.Lock()
	fns := make([]func(string, any), 0, len(nl.listeners))
	for _, f := range nl.listeners {
		fns = append(fns, f)
	}
	nl.mu.Unlock()
	for _, f := range fns {
		f(event, payload)
	}
}

// ---- webhook forwarding (Slack / Discord / Teams / any JSON endpoint) ----

func postWebhook(url string, n Notification) {
	line := n.App
	if n.Title != "" {
		line += ": " + n.Title
	}
	if n.Text != "" {
		line += "\n" + n.Text
	}
	body, _ := json.Marshal(map[string]any{
		"text": line, "content": line, // Slack/Teams use "text", Discord uses "content"
		"app": n.App, "package": n.Pkg, "title": n.Title, "body": n.Text, "device": n.Device,
		"category": n.Category, "time": n.Time,
	})
	c := &http.Client{Timeout: 10 * time.Second}
	resp, err := c.Post(url, "application/json", bytes.NewReader(body))
	if err != nil {
		log.Printf("notification webhook: %v", err)
		return
	}
	resp.Body.Close()
	if resp.StatusCode >= 300 {
		log.Printf("notification webhook: HTTP %d", resp.StatusCode)
	}
}

func strOf(v any) string {
	if s, ok := v.(string); ok {
		return s
	}
	return ""
}
