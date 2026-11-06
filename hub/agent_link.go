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

type rpcMsg struct {
	ID     int64           `json:"id,omitempty"`
	Method string          `json:"method,omitempty"`
	Params json.RawMessage `json:"params,omitempty"`
	Result json.RawMessage `json:"result,omitempty"`
	Error  string          `json:"error,omitempty"`
	Event  string          `json:"event,omitempty"`
}

// LinkedDevice is a single agent (a phone) that dialed into this hub over a WebSocket.
type LinkedDevice struct {
	info    DeviceInfo
	conn    *websocket.Conn
	wmu     sync.Mutex
	nextID  atomic.Int64
	pmu     sync.Mutex
	pending map[int64]chan rpcMsg
	closed  chan struct{}
}

func (l *LinkedDevice) Info() DeviceInfo { return l.info }

func (l *LinkedDevice) write(m rpcMsg) error {
	l.wmu.Lock()
	defer l.wmu.Unlock()
	l.conn.SetWriteDeadline(time.Now().Add(30 * time.Second))
	return l.conn.WriteJSON(m)
}

func (l *LinkedDevice) Call(ctx context.Context, method string, params json.RawMessage) (json.RawMessage, error) {
	id := l.nextID.Add(1)
	ch := make(chan rpcMsg, 1)
	l.pmu.Lock()
	l.pending[id] = ch
	l.pmu.Unlock()
	defer func() {
		l.pmu.Lock()
		delete(l.pending, id)
		l.pmu.Unlock()
	}()
	if err := l.write(rpcMsg{ID: id, Method: method, Params: params}); err != nil {
		return nil, err
	}
	timeout := 120 * time.Second
	if _, ok := ctx.Deadline(); ok {
		timeout = 24 * time.Hour
	}
	select {
	case m := <-ch:
		if m.Error != "" {
			return nil, errors.New(m.Error)
		}
		return m.Result, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-l.closed:
		return nil, errors.New("device disconnected")
	case <-time.After(timeout):
		return nil, errors.New("device did not answer in time")
	}
}

// serveLinked runs the read loop for whoever dialed /link. A peer hub (peer:true) becomes a mesh
// session; anything else (a phone) becomes a single agent device owned by this hub.
func (s *Server) serveLinked(conn *websocket.Conn, addr string) {
	conn.SetReadLimit(64 << 20)
	conn.SetReadDeadline(time.Now().Add(20 * time.Second))
	var hello struct {
		Hello DeviceInfo `json:"hello"`
		Peer  bool       `json:"peer"`
	}
	if err := conn.ReadJSON(&hello); err != nil || hello.Hello.ID == "" {
		conn.Close()
		return
	}
	if hello.Peer {
		// Reply with our identity so the dialer can complete the handshake, then run the session.
		conn.WriteJSON(map[string]any{"hello": s.hub.local.Info(), "peer": true})
		s.runPeerSession(conn, hello.Hello, false)
		return
	}

	info := hello.Hello
	info.Online, info.Local, info.Addr, info.Since = true, false, addr, time.Now().Unix()
	if info.Name == "" {
		info.Name = info.ID
	}
	l := &LinkedDevice{info: info, conn: conn, pending: map[int64]chan rpcMsg{}, closed: make(chan struct{})}
	s.hub.addAgent(l)
	log.Printf("device linked: %s (%s) from %s", info.Name, info.Kind, addr)
	conn.WriteJSON(map[string]any{"welcome": s.hub.local.Info().Name})

	conn.SetPongHandler(func(string) error { conn.SetReadDeadline(time.Now().Add(90 * time.Second)); return nil })
	go func() {
		t := time.NewTicker(30 * time.Second)
		defer t.Stop()
		for {
			select {
			case <-t.C:
				l.wmu.Lock()
				err := conn.WriteControl(websocket.PingMessage, nil, time.Now().Add(10*time.Second))
				l.wmu.Unlock()
				if err != nil {
					return
				}
			case <-l.closed:
				return
			}
		}
	}()

	for {
		conn.SetReadDeadline(time.Now().Add(90 * time.Second))
		var m rpcMsg
		if err := conn.ReadJSON(&m); err != nil {
			break
		}
		if m.Event != "" {
			s.hub.ownedEvent(info.ID, Event{Event: m.Event, Params: m.Params})
			continue
		}
		if m.Method == "ping" {
			l.write(rpcMsg{ID: m.ID, Result: json.RawMessage(`"pong"`)})
			continue
		}
		l.pmu.Lock()
		ch := l.pending[m.ID]
		l.pmu.Unlock()
		if ch != nil {
			ch <- m
		}
	}
	close(l.closed)
	conn.Close()
	s.hub.removeAgent(info.ID, l)
	log.Printf("device unlinked: %s", info.Name)
}
