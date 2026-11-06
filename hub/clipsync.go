package main

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"time"
)

// Clip is one entry of the fleet-wide clipboard history.
type Clip struct {
	ID       int64  `json:"id"`
	Text     string `json:"text"`
	Device   string `json:"device"`
	DeviceID string `json:"deviceId"`
	Time     int64  `json:"time"`
}

const maxClipBytes = 256 << 10

// ClipSync keeps every PC and phone in the fleet on the same clipboard. Whatever is copied on one
// device is announced as a "clip.copied" event; events fan out across the mesh, and every hub puts
// the text on its own clipboard (so it lands in Win+V history) and on the phones it owns.
type ClipSync struct {
	s         *Server
	mu        sync.Mutex
	hist      []Clip
	nextID    int64
	lastLocal string // last text this PC copied or was given — never re-announced
}

func NewClipSync(s *Server) *ClipSync { return &ClipSync{s: s, nextID: 1} }

// watch notices local copies and announces them to the fleet.
func (c *ClipSync) watch() {
	seq := clipboardSeq()
	if t, err := clipboardGet(); err == nil {
		c.mu.Lock()
		c.lastLocal = t // don't broadcast whatever was on the clipboard at startup
		c.mu.Unlock()
	}
	tick := time.NewTicker(700 * time.Millisecond)
	defer tick.Stop()
	n := 0
	for range tick.C {
		n++
		if c.s.cfg.ClipSyncOff {
			continue
		}
		if seq != 0 || clipboardSeq() != 0 {
			ns := clipboardSeq()
			if ns == seq {
				continue
			}
			seq = ns
			time.Sleep(60 * time.Millisecond) // let the copying app finish writing all formats
		} else if n%3 != 0 { // no change counter (macOS/Linux): poll the text every ~2s
			continue
		}
		if clipboardPrivate() {
			continue
		}
		text, err := clipboardGet()
		if err != nil || strings.TrimSpace(text) == "" || len(text) > maxClipBytes {
			continue
		}
		c.mu.Lock()
		same := text == c.lastLocal
		c.lastLocal = text
		c.mu.Unlock()
		if same {
			continue
		}
		c.announce(text)
	}
}

// announce emits clip.copied from this PC; ownedEvent delivers it here and to every peer.
func (c *ClipSync) announce(text string) {
	params, _ := json.Marshal(map[string]any{"sid": "clip", "text": text})
	c.s.hub.ownedEvent(c.s.hub.local.Info().ID, Event{Event: "clip.copied", Params: params})
}

// handleEvent applies a clip copied anywhere in the fleet.
func (c *ClipSync) handleEvent(deviceID string, e Event) {
	if e.Event != "clip.copied" {
		return
	}
	var p struct {
		Text string `json:"text"`
	}
	if json.Unmarshal(e.Params, &p) != nil || p.Text == "" || len(p.Text) > maxClipBytes {
		return
	}
	name := deviceID
	if d, err := c.s.hub.Get(deviceID); err == nil {
		name = d.Info().Name
	}
	clip := c.add(p.Text, name, deviceID)
	c.s.notif.broadcastMap("clip.added", map[string]any{"id": clip.ID, "text": clip.Text, "device": clip.Device, "deviceId": clip.DeviceID, "time": clip.Time})
	if c.s.cfg.ClipSyncOff {
		return
	}
	localID := c.s.hub.local.Info().ID
	if deviceID != localID {
		c.mu.Lock()
		c.lastLocal = p.Text // set before writing so the watcher doesn't bounce it back
		c.mu.Unlock()
		clipboardSet(p.Text)
	}
	c.pushToPhones(p.Text, deviceID)
}

// pushToPhones copies the text to phones this hub owns (other hubs handle their own phones).
func (c *ClipSync) pushToPhones(text, exceptID string) {
	params, _ := json.Marshal(map[string]any{"text": text})
	for _, d := range c.s.hub.ownedDevices() {
		info := d.Info()
		if info.Local || info.ID == exceptID || !info.Online || !hasCap(info, "clipsync") {
			continue
		}
		go func(d Device) {
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			d.Call(ctx, "clipboard.set", params)
		}(d)
	}
}

func hasCap(info DeviceInfo, cap string) bool {
	for _, c := range info.Caps {
		if c == cap {
			return true
		}
	}
	return false
}

func (c *ClipSync) add(text, device, deviceID string) Clip {
	c.mu.Lock()
	defer c.mu.Unlock()
	for i, x := range c.hist { // re-copying an old item moves it to the top
		if x.Text == text {
			c.hist = append(c.hist[:i], c.hist[i+1:]...)
			break
		}
	}
	clip := Clip{ID: c.nextID, Text: text, Device: device, DeviceID: deviceID, Time: time.Now().UnixMilli()}
	c.nextID++
	c.hist = append(c.hist, clip)
	if len(c.hist) > 50 {
		c.hist = c.hist[len(c.hist)-50:]
	}
	return clip
}

// History returns clips newest first. Kept in memory only (clipboards hold secrets).
func (c *ClipSync) History() []Clip {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]Clip, 0, len(c.hist))
	for i := len(c.hist) - 1; i >= 0; i-- {
		out = append(out, c.hist[i])
	}
	return out
}

func (c *ClipSync) get(id int64) (Clip, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, x := range c.hist {
		if x.ID == id {
			return x, true
		}
	}
	return Clip{}, false
}

func (c *ClipSync) Clear() {
	c.mu.Lock()
	c.hist = nil
	c.mu.Unlock()
}
