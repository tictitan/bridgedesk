package main

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"sync"
)

// DeviceInfo describes one machine reachable through the fleet.
type DeviceInfo struct {
	ID       string   `json:"id"`
	Name     string   `json:"name"`
	Kind     string   `json:"kind"` // windows | android | linux | darwin
	OS       string   `json:"os"`
	Caps     []string `json:"caps"`
	Online   bool     `json:"online"`
	Local    bool     `json:"local"`
	Addr     string   `json:"addr,omitempty"`
	Since    int64    `json:"since,omitempty"`
	Extra    any      `json:"extra,omitempty"`
	HomeDir  string   `json:"home,omitempty"`
	Platform string   `json:"platform,omitempty"`
	Via      string   `json:"via,omitempty"` // name of the hub that owns this device (for relayed devices)
}

// Event is pushed asynchronously by a device (terminal output, screen frames, notifications).
type Event struct {
	Event  string          `json:"event"`
	Params json.RawMessage `json:"params"`
}

type Device interface {
	Info() DeviceInfo
	Call(ctx context.Context, method string, params json.RawMessage) (json.RawMessage, error)
}

// ---------- Hub ----------
//
// A hub owns its local device plus any agents (phones) that dialed into it, and it meshes with
// peer hubs. Each hub advertises the devices it owns to every peer, and relays control to them,
// so every console in the fleet sees — and can drive — every device.

type Hub struct {
	mu         sync.RWMutex
	local      *LocalDevice
	agents     map[string]*LinkedDevice // phones / single agents dialed into THIS hub (owned)
	agentOrder []string
	peers      map[string]*PeerHub // peerHubID -> peer session

	subMu sync.Mutex
	subs  map[string]func(Event)

	OnEvent func(deviceID string, e Event) // hub-wide hook (e.g. PC toast on phone notification)
}

func NewHub(local *LocalDevice) *Hub {
	h := &Hub{
		local:  local,
		agents: map[string]*LinkedDevice{},
		peers:  map[string]*PeerHub{},
		subs:   map[string]func(Event){},
	}
	local.emit = func(e Event) { h.ownedEvent(local.Info().ID, e) }
	return h
}

// ---- agents (phones) ----

func (h *Hub) addAgent(d *LinkedDevice) {
	h.mu.Lock()
	id := d.Info().ID
	if _, ok := h.agents[id]; !ok {
		h.agentOrder = append(h.agentOrder, id)
	}
	h.agents[id] = d
	h.mu.Unlock()
	h.broadcastRoster()
}

func (h *Hub) removeAgent(id string, d *LinkedDevice) {
	h.mu.Lock()
	if cur, ok := h.agents[id]; ok && cur == d {
		delete(h.agents, id)
		for i, x := range h.agentOrder {
			if x == id {
				h.agentOrder = append(h.agentOrder[:i], h.agentOrder[i+1:]...)
				break
			}
		}
	}
	h.mu.Unlock()
	h.broadcastRoster()
}

// ---- peers (other hubs) ----

func (h *Hub) addPeer(p *PeerHub) bool {
	h.mu.Lock()
	if _, exists := h.peers[p.id]; exists {
		h.mu.Unlock()
		return false // already meshed with this hub
	}
	h.peers[p.id] = p
	h.mu.Unlock()
	return true
}

func (h *Hub) removePeer(id string, p *PeerHub) {
	h.mu.Lock()
	if cur, ok := h.peers[id]; ok && cur == p {
		delete(h.peers, id)
	}
	h.mu.Unlock()
}

func (h *Hub) peerList() []*PeerHub {
	h.mu.RLock()
	defer h.mu.RUnlock()
	out := make([]*PeerHub, 0, len(h.peers))
	for _, p := range h.peers {
		out = append(out, p)
	}
	return out
}

// ownedDevices returns the devices this hub serves directly (local + agents).
func (h *Hub) ownedDevices() []Device {
	h.mu.RLock()
	defer h.mu.RUnlock()
	out := []Device{h.local}
	for _, id := range h.agentOrder {
		out = append(out, h.agents[id])
	}
	return out
}

func (h *Hub) ownedRoster() []DeviceInfo {
	devs := h.ownedDevices()
	out := make([]DeviceInfo, 0, len(devs))
	for _, d := range devs {
		out = append(out, d.Info())
	}
	return out
}

// getOwned returns a device this hub serves directly, or nil (used when answering a relay).
func (h *Hub) getOwned(id string) Device {
	if id == "" || id == "local" || id == "this" || id == h.local.Info().ID {
		return h.local
	}
	h.mu.RLock()
	defer h.mu.RUnlock()
	if d, ok := h.agents[id]; ok {
		return d
	}
	for _, d := range h.agents {
		if equalFold(d.Info().Name, id) {
			return d
		}
	}
	if equalFold(h.local.Info().Name, id) {
		return h.local
	}
	return nil
}

// Get resolves any device in the fleet: owned first, else a relay through the peer that owns it.
func (h *Hub) Get(id string) (Device, error) {
	if d := h.getOwned(id); d != nil {
		return d, nil
	}
	h.mu.RLock()
	defer h.mu.RUnlock()
	for _, p := range h.peers {
		if info, ok := p.has(id); ok {
			return &RelayDevice{peer: p, info: info}, nil
		}
	}
	return nil, fmt.Errorf("device %q not found or offline", id)
}

// List returns every device in the fleet (owned + all peers' rosters), de-duplicated by ID.
func (h *Hub) List() []DeviceInfo {
	seen := map[string]bool{}
	var out []DeviceInfo
	for _, info := range h.ownedRoster() {
		seen[info.ID] = true
		out = append(out, info)
	}
	for _, p := range h.peerList() {
		for _, info := range p.rosterCopy() {
			if seen[info.ID] {
				continue
			}
			seen[info.ID] = true
			info.Local = false
			if info.Via == "" {
				info.Via = p.name
			}
			out = append(out, info)
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Local && !out[j].Local })
	return out
}

// ---- events ----

func (h *Hub) Subscribe(deviceID, sid string, fn func(Event)) func() {
	k := deviceID + "/" + sid
	h.subMu.Lock()
	h.subs[k] = fn
	h.subMu.Unlock()
	return func() {
		h.subMu.Lock()
		delete(h.subs, k)
		h.subMu.Unlock()
	}
}

// localDispatch routes an event to this hub's own subscribers (dashboard WS) and the OnEvent hook.
func (h *Hub) localDispatch(deviceID string, e Event) {
	var p struct {
		Sid string `json:"sid"`
	}
	json.Unmarshal(e.Params, &p)
	h.subMu.Lock()
	fn := h.subs[deviceID+"/"+p.Sid]
	h.subMu.Unlock()
	if fn != nil {
		fn(e)
	}
	if h.OnEvent != nil {
		h.OnEvent(deviceID, e)
	}
}

// ownedEvent handles an event from a device THIS hub owns: deliver locally and fan out to peers.
func (h *Hub) ownedEvent(deviceID string, e Event) {
	h.localDispatch(deviceID, e)
	for _, p := range h.peerList() {
		p.sendEvent(deviceID, e)
	}
}

func (h *Hub) broadcastRoster() {
	roster := h.ownedRoster()
	for _, p := range h.peerList() {
		p.sendRoster(roster)
	}
}

// Call is a convenience wrapper that marshals params and unmarshals the result.
func (h *Hub) Call(ctx context.Context, deviceID, method string, params any, out any) error {
	d, err := h.Get(deviceID)
	if err != nil {
		return err
	}
	raw, _ := json.Marshal(params)
	res, err := d.Call(ctx, method, raw)
	if err != nil {
		return err
	}
	if out != nil {
		return json.Unmarshal(res, out)
	}
	return nil
}

func equalFold(a, b string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := 0; i < len(a); i++ {
		x, y := a[i], b[i]
		if 'A' <= x && x <= 'Z' {
			x += 32
		}
		if 'A' <= y && y <= 'Z' {
			y += 32
		}
		if x != y {
			return false
		}
	}
	return true
}
