package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// TestAssistantRelay verifies the relay client offline: it maps the stream events to phone events,
// shows an approval card, and POSTs the phone's decision back to /approve. No Claude / no Aria.
func TestAssistantRelay(t *testing.T) {
	var approvedID int64 = -1
	var approvedOK int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer tok" {
			w.WriteHeader(401)
			return
		}
		switch r.URL.Path {
		case "/ask":
			fl, _ := w.(http.Flusher)
			write := func(v any) { b, _ := json.Marshal(v); w.Write(append(b, '\n')); if fl != nil { fl.Flush() } }
			write(map[string]any{"type": "tool", "text": "Checking AWS"})
			write(map[string]any{"type": "text", "text": "Looking into it."})
			write(map[string]any{"type": "approval", "id": 7, "label": "Run command", "command": "terraform apply"})
			write(map[string]any{"type": "done", "text": "All set.", "session": "sess-1"})
		case "/approve":
			var d struct {
				ID int64 `json:"id"`
				OK bool  `json:"ok"`
			}
			json.NewDecoder(r.Body).Decode(&d)
			atomic.StoreInt64(&approvedID, d.ID)
			if d.OK {
				atomic.StoreInt32(&approvedOK, 1)
			}
			io.WriteString(w, "{}")
		}
	}))
	defer srv.Close()

	t.Setenv("ARIA_ASSISTANT_URL", srv.URL)
	t.Setenv("ARIA_ASSISTANT_TOKEN", "tok")

	s := &Server{cfg: &Config{}}
	approvals := newApprovalRegistry()

	var mu sync.Mutex
	var deltas, tools []string
	var gotApprovalCmd string
	emit := func(ev string, p any) {
		m, _ := p.(map[string]any)
		mu.Lock()
		switch ev {
		case "ai.delta":
			deltas = append(deltas, m["text"].(string))
		case "ai.tool":
			tools = append(tools, m["name"].(string))
		case "ai.approval":
			if in, ok := m["input"].(map[string]any); ok {
				gotApprovalCmd, _ = in["command"].(string)
			}
			id, _ := m["id"].(string)
			mu.Unlock()
			approvals.resolve(id, true) // user taps Allow
			mu.Lock()
		}
		mu.Unlock()
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	session, text, err := s.runAssistantRelay(ctx, aiReq{Prompt: "ship it"}, emit, approvals)
	if err != nil {
		t.Fatalf("relay error: %v", err)
	}
	if text != "All set." || session != "sess-1" {
		t.Errorf("done mishandled: text=%q session=%q", text, session)
	}
	if len(tools) != 1 || tools[0] != "Checking AWS" {
		t.Errorf("tool activity not forwarded: %v", tools)
	}
	if len(deltas) != 1 || deltas[0] != "Looking into it." {
		t.Errorf("text not forwarded: %v", deltas)
	}
	if gotApprovalCmd != "terraform apply" {
		t.Errorf("approval command not surfaced: %q", gotApprovalCmd)
	}
	if atomic.LoadInt64(&approvedID) != 7 || atomic.LoadInt32(&approvedOK) != 1 {
		t.Errorf("decision not POSTed to /approve: id=%d ok=%d", approvedID, approvedOK)
	}
	_ = fmt.Sprint
}
