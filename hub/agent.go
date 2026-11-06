package main

import (
	"context"
	"encoding/json"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

// serveAgent answers RPCs from a remote hub against the given local device, and forwards
// the device's async events (terminal data, screen frames) back over the same socket.
// This is the outbound half of linking: our machine becomes a device in the remote hub's list.
func serveAgent(conn *websocket.Conn, dev *LocalDevice) {
	var wmu sync.Mutex
	writeMsg := func(m rpcMsg) {
		wmu.Lock()
		conn.SetWriteDeadline(time.Now().Add(30 * time.Second))
		conn.WriteJSON(m)
		wmu.Unlock()
	}
	// Route this device's events out as event frames.
	prev := dev.emit
	dev.emit = func(e Event) { writeMsg(rpcMsg{Event: e.Event, Params: e.Params}) }
	defer func() { dev.emit = prev }()

	conn.SetReadLimit(64 << 20)
	conn.SetPongHandler(func(string) error { conn.SetReadDeadline(time.Now().Add(90 * time.Second)); return nil })
	go func() {
		t := time.NewTicker(30 * time.Second)
		defer t.Stop()
		for range t.C {
			wmu.Lock()
			err := conn.WriteControl(websocket.PingMessage, nil, time.Now().Add(10*time.Second))
			wmu.Unlock()
			if err != nil {
				return
			}
		}
	}()

	for {
		conn.SetReadDeadline(time.Now().Add(90 * time.Second))
		var m rpcMsg
		if err := conn.ReadJSON(&m); err != nil {
			return
		}
		if m.Method == "" {
			continue
		}
		go func(m rpcMsg) {
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
			defer cancel()
			res, err := dev.Call(ctx, m.Method, m.Params)
			if err != nil {
				writeMsg(rpcMsg{ID: m.ID, Error: err.Error()})
				return
			}
			writeMsg(rpcMsg{ID: m.ID, Result: json.RawMessage(res)})
		}(m)
	}
}
