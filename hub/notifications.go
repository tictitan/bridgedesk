package main

import (
	"encoding/json"
	"log"
	"sync"
	"time"
)

// Notification is one mirrored phone notification, kept in a small ring so the dashboard
// and MCP can show recent history even if they connect after it arrived.
type Notification struct {
	DeviceID string   `json:"deviceId"`
	Device   string   `json:"device"`
	Key      string   `json:"key"`
	Pkg      string   `json:"pkg"`
	App      string   `json:"app"`
	Title    string   `json:"title"`
	Text     string   `json:"text"`
	Sub      string          `json:"sub"`
	Time     int64           `json:"time"`
	Actions  json.RawMessage `json:"actions,omitempty"`
	CanOpen  bool            `json:"canOpen,omitempty"`
}

type NotifLog struct {
	mu    sync.Mutex
	ring  []Notification
	max   int
	toast func(title, text string)
}

func NewNotifLog(toast func(string, string)) *NotifLog {
	return &NotifLog{max: 200, toast: toast}
}

// handleEvent is wired to Hub.OnEvent; it reacts to phone notif.posted/removed events.
func (nl *NotifLog) handleEvent(s *Server, deviceID string, e Event) {
	switch e.Event {
	case "notif.posted":
		var n Notification
		if err := json.Unmarshal(e.Params, &n); err != nil {
			return
		}
		n.DeviceID = deviceID
		if d, err := s.hub.Get(deviceID); err == nil {
			n.Device = d.Info().Name
		}
		nl.add(n)
		if s.cfg.MirrorNotif && nl.toast != nil {
			title := n.App
			if n.Title != "" {
				title = n.App + ": " + n.Title
			}
			nl.toast(title, n.Text)
		}
		log.Printf("notification [%s] %s: %s — %s", n.Device, n.App, n.Title, n.Text)
	case "notif.removed":
		var p struct {
			Key string `json:"key"`
		}
		json.Unmarshal(e.Params, &p)
		nl.remove(p.Key)
	}
}

func (nl *NotifLog) add(n Notification) {
	nl.mu.Lock()
	defer nl.mu.Unlock()
	// de-dupe by key (updates replace)
	for i, x := range nl.ring {
		if x.Key == n.Key {
			nl.ring[i] = n
			return
		}
	}
	nl.ring = append(nl.ring, n)
	if len(nl.ring) > nl.max {
		nl.ring = nl.ring[len(nl.ring)-nl.max:]
	}
}

func (nl *NotifLog) remove(key string) {
	nl.mu.Lock()
	defer nl.mu.Unlock()
	for i, x := range nl.ring {
		if x.Key == key {
			nl.ring = append(nl.ring[:i], nl.ring[i+1:]...)
			return
		}
	}
}

func (nl *NotifLog) recent() []Notification {
	nl.mu.Lock()
	defer nl.mu.Unlock()
	out := make([]Notification, len(nl.ring))
	copy(out, nl.ring)
	// newest first
	for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 {
		out[i], out[j] = out[j], out[i]
	}
	return out
}

var _ = time.Now
