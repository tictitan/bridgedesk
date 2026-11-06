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

func (p *PeerHub) setRoster(r []DeviceInfo) {
	p.rmu.Lock()
	p.roster = r
	p.rmu.Unlock()
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

// relayCall sends an RPC to a device owned by the peer hub and waits for the result.
func (p *PeerHub) relayCall(ctx context.Context, target, method string, params json.RawMessage) (json.RawMessage, error) {
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
	if err := p.write(peerFrame{Kind: "relay", RID: rid, Target: target, Method: method, Params: params}); err != nil {
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
		p.setRoster(f.Roster)
	case "book":
		p.s.mergeBook(f.Book)
	case "relay":
		go func() {
			dev := p.s.hub.getOwned(f.Target)
			if dev == nil {
				p.write(peerFrame{Kind: "relayResult", RID: f.RID, Error: "device not reachable from its hub"})
				return
			}
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Hour) // long UE builds/cooks
			defer cancel()
			res, err := dev.Call(ctx, f.Method, f.Params)
			if err != nil {
				p.write(peerFrame{Kind: "relayResult", RID: f.RID, Error: err.Error()})
				return
			}
			p.write(peerFrame{Kind: "relayResult", RID: f.RID, Result: res})
		}()
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

// RelayDevice makes a device owned by a peer hub look like a local Device, routing calls over the mesh.
type RelayDevice struct {
	peer *PeerHub
	info DeviceInfo
}

func (r *RelayDevice) Info() DeviceInfo { return r.info }
func (r *RelayDevice) Call(ctx context.Context, method string, params json.RawMessage) (json.RawMessage, error) {
	return r.peer.relayCall(ctx, r.info.ID, method, params)
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

	// initial exchange: our owned devices + our address book (for gossip)
	p.sendRoster(s.hub.ownedRoster())
	p.sendBook(s.bookSnapshot())

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
