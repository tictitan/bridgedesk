package main

import (
	"context"
	"encoding/json"
	"errors"
	"log"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gorilla/websocket"
)

// peerFrame is the wire format between two meshed hubs.
type peerFrame struct {
	Kind   string          `json:"k"`
	Hello  *DeviceInfo     `json:"hello,omitempty"`
	Peer   bool            `json:"peer,omitempty"`
	Roster []DeviceInfo    `json:"roster,omitempty"`
	Book   []LinkTarget    `json:"book,omitempty"`
	RID    int64           `json:"rid,omitempty"`
	Target string          `json:"target,omitempty"`
	Method string          `json:"method,omitempty"`
	Params json.RawMessage `json:"params,omitempty"`
	Result json.RawMessage `json:"result,omitempty"`
	Error  string          `json:"error,omitempty"`
	Event  string          `json:"event,omitempty"`
	TTL    int             `json:"ttl,omitempty"`  // remaining forward hops for a relay (loop/length guard)
	Path   []string        `json:"path,omitempty"` // hub ids already on this relay's path (so it never loops back)
}

// PeerHub is a live mesh session with another hub. It advertises that hub's roster to us and
// relays our control to the devices that hub owns (and vice-versa — the session is symmetric).
type PeerHub struct {
	s    *Server
	conn *websocket.Conn
	id   string
	name string

	wmu     sync.Mutex
	rmu     sync.Mutex
	roster  []DeviceInfo
	nextRID atomic.Int64
	pmu     sync.Mutex
	pending map[int64]chan peerFrame
	closed  chan struct{}
}

func (p *PeerHub) write(f peerFrame) error {
	p.wmu.Lock()
	defer p.wmu.Unlock()
	p.conn.SetWriteDeadline(time.Now().Add(30 * time.Second))
	return p.conn.WriteJSON(f)
}

// setRoster stores a peer's advertised devices; returns true if it actually changed (so we only re-broadcast then).
func (p *PeerHub) setRoster(r []DeviceInfo) bool {
	p.rmu.Lock()
	defer p.rmu.Unlock()
	if sameRoster(p.roster, r) {
		return false
	}
	p.roster = r
	return true
}

func sameRoster(a, b []DeviceInfo) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i].ID != b[i].ID || a[i].Hops != b[i].Hops || a[i].Online != b[i].Online || a[i].Name != b[i].Name {
			return false
		}
	}
	return true
}

func (p *PeerHub) rosterCopy() []DeviceInfo {
	p.rmu.Lock()
	defer p.rmu.Unlock()
	out := make([]DeviceInfo, len(p.roster))
	copy(out, p.roster)
	return out
}

func (p *PeerHub) has(id string) (DeviceInfo, bool) {
	p.rmu.Lock()
	defer p.rmu.Unlock()
	for _, info := range p.roster {
		if info.ID == id || equalFold(info.Name, id) {
			return info, true
		}
	}
	return DeviceInfo{}, false
}

func (p *PeerHub) sendRoster(r []DeviceInfo) { p.write(peerFrame{Kind: "roster", Roster: r}) }
func (p *PeerHub) sendBook(b []LinkTarget)   { p.write(peerFrame{Kind: "book", Book: b}) }
func (p *PeerHub) sendEvent(target string, e Event) {
	p.write(peerFrame{Kind: "relayEvent", Target: target, Event: e.Event, Params: e.Params})
}

// relayCall sends an RPC toward a device reachable through this peer and waits for the result. ttl/path travel with
// the frame so the owning hub (or an intermediate one) can forward it another hop without looping.
func (p *PeerHub) relayCall(ctx context.Context, target, method string, params json.RawMessage, ttl int, path []string) (json.RawMessage, error) {
	rid := p.nextRID.Add(1)
	ch := make(chan peerFrame, 1)
	p.pmu.Lock()
	p.pending[rid] = ch
	p.pmu.Unlock()
	defer func() {
		p.pmu.Lock()
		delete(p.pending, rid)
		p.pmu.Unlock()
	}()
	if err := p.write(peerFrame{Kind: "relay", RID: rid, Target: target, Method: method, Params: params, TTL: ttl, Path: path}); err != nil {
		return nil, err
	}
	timeout := 120 * time.Second
	if _, ok := ctx.Deadline(); ok {
		timeout = 24 * time.Hour
	}
	select {
	case f := <-ch:
		if f.Error != "" {
			return nil, errors.New(f.Error)
		}
		return f.Result, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-p.closed:
		return nil, errors.New("hub disconnected")
	case <-time.After(timeout):
		return nil, errors.New("device did not answer in time")
	}
}

func (p *PeerHub) handle(f peerFrame) {
	switch f.Kind {
	case "roster":
		if p.setRoster(f.Roster) {
			p.s.hub.broadcastRoster() // reachability changed: propagate it one hop further out
		}
	case "book":
		p.s.mergeBook(f.Book)
	case "relay":
		go p.serveRelay(f)
	case "relayResult":
		p.pmu.Lock()
		ch := p.pending[f.RID]
		p.pmu.Unlock()
		if ch != nil {
			ch <- f
		}
	case "relayEvent":
		p.s.hub.localDispatch(f.Target, Event{Event: f.Event, Params: f.Params})
	}
}

// serveRelay answers a relay for a device we own, or forwards it another hop toward a device we can reach but the
// sender can't. The frame's path (hub ids already traversed) and ttl keep forwarding loop-free and bounded.
func (p *PeerHub) serveRelay(f peerFrame) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Hour) // long UE builds/cooks
	defer cancel()
	reply := func(res json.RawMessage, err error) {
		if err != nil {
			p.write(peerFrame{Kind: "relayResult", RID: f.RID, Error: err.Error()})
		} else {
			p.write(peerFrame{Kind: "relayResult", RID: f.RID, Result: res})
		}
	}
	if dev := p.s.hub.getOwned(f.Target); dev != nil {
		reply(dev.Call(ctx, f.Method, f.Params))
		return
	}
	// not ours: forward through the best next hop that isn't already on the path (and not the sender)
	ttl := f.TTL
	if ttl <= 0 {
		ttl = maxRelayHops
	}
	if ttl <= 1 {
		reply(nil, errors.New("no route to device (hop limit reached)"))
		return
	}
	avoid := map[string]bool{p.id: true}
	for _, id := range f.Path {
		avoid[id] = true
	}
	np, _, ok := p.s.hub.route(f.Target, avoid)
	if !ok {
		reply(nil, errors.New("device not reachable from this hub"))
		return
	}
	path := append(append([]string{}, f.Path...), p.s.hub.local.Info().ID)
	reply(np.relayCall(ctx, f.Target, f.Method, f.Params, ttl-1, path))
}

// RelayDevice makes a device reachable through a peer hub look like a local Device, routing calls over the mesh
// (and, if that hub doesn't own the device either, letting it forward another hop).
type RelayDevice struct {
	peer *PeerHub
	info DeviceInfo
	ttl  int
}

func (r *RelayDevice) Info() DeviceInfo { return r.info }
func (r *RelayDevice) Call(ctx context.Context, method string, params json.RawMessage) (json.RawMessage, error) {
	ttl := r.ttl
	if ttl <= 0 {
		ttl = maxRelayHops
	}
	return r.peer.relayCall(ctx, r.info.ID, method, params, ttl, []string{r.peer.s.hub.local.Info().ID})
}

// runPeerSession runs a symmetric mesh session after the hello handshake. Blocks until disconnect.
func (s *Server) runPeerSession(conn *websocket.Conn, peerInfo DeviceInfo, dialer bool) {
	if peerInfo.ID == s.hub.local.Info().ID {
		conn.Close()
		return // connected to ourselves
	}
	p := &PeerHub{s: s, conn: conn, id: peerInfo.ID, name: peerInfo.Name, pending: map[int64]chan peerFrame{}, closed: make(chan struct{})}
	if !s.hub.addPeer(p) {
		conn.Close()
		return // already meshed with this hub over another connection
	}
	log.Printf("hub meshed: %s (%s)", p.name, peerInfo.Kind)

	// initial exchange: everything we can reach (split-horizon for this peer) + our address book (for gossip)
	p.sendRoster(s.hub.perPeerRoster(p.id))
	p.sendBook(s.bookSnapshot())
	// tell our other peers this new hub's devices are now reachable through us (after it sends its roster)
	defer s.hub.broadcastRoster()

	conn.SetReadLimit(64 << 20)
	conn.SetPongHandler(func(string) error { conn.SetReadDeadline(time.Now().Add(90 * time.Second)); return nil })
	go func() {
		t := time.NewTicker(30 * time.Second)
		defer t.Stop()
		for {
			select {
			case <-t.C:
				p.wmu.Lock()
				err := conn.WriteControl(websocket.PingMessage, nil, time.Now().Add(10*time.Second))
				p.wmu.Unlock()
				if err != nil {
					return
				}
			case <-p.closed:
				return
			}
		}
	}()

	for {
		conn.SetReadDeadline(time.Now().Add(90 * time.Second))
		var f peerFrame
		if err := conn.ReadJSON(&f); err != nil {
			break
		}
		p.handle(f)
	}
	close(p.closed)
	conn.Close()
	s.hub.removePeer(p.id, p)
	log.Printf("hub unmeshed: %s", p.name)
}
