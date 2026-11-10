package main

// assistant_relay.go — when the PC is running the full Aria desktop assistant, the hub relays the
// phone's prompt to it instead of spawning a stripped-down `claude -p`. Aria's relay runs the FULL
// agent (every aria-* helper, browser, orchestrator, business) and streams back text, tool-activity
// and real per-action approval requests. Enabled by two env vars Aria sets on the hub process:
//
//	ARIA_ASSISTANT_URL    e.g. http://127.0.0.1:PORT
//	ARIA_ASSISTANT_TOKEN  bearer token
//
// When ARIA_ASSISTANT_URL is unset (a hub with no Aria attached), ai.ask falls back to runAI.

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"strings"
)

// runAssistantTurn picks the full-Aria relay when configured, else the local claude -p relay.
func (s *Server) runAssistantTurn(ctx context.Context, req aiReq, emit func(string, any), setCmd func(*exec.Cmd), approvals *approvalRegistry) (string, string, error) {
	if os.Getenv("ARIA_ASSISTANT_URL") != "" {
		return s.runAssistantRelay(ctx, req, emit, approvals)
	}
	return s.runAI(ctx, req, emit, setCmd, approvals)
}

// relayEvent is one newline-delimited JSON event from Aria's assistant relay.
type relayEvent struct {
	Type    string `json:"type"`    // text | tool | approval | done
	Text    string `json:"text"`    // text/tool/done
	ID      int64  `json:"id"`      // approval
	Label   string `json:"label"`   // approval: short description
	Command string `json:"command"` // approval: the exact action (command/recipient/…)
	Error   string `json:"error"`   // done
	Session string `json:"session"` // done (optional)
}

func (s *Server) runAssistantRelay(ctx context.Context, req aiReq, emit func(string, any), approvals *approvalRegistry) (string, string, error) {
	base := strings.TrimRight(os.Getenv("ARIA_ASSISTANT_URL"), "/")
	token := os.Getenv("ARIA_ASSISTANT_TOKEN")

	body, _ := json.Marshal(map[string]any{"prompt": req.Prompt, "session": req.Session})
	httpReq, err := http.NewRequestWithContext(ctx, "POST", base+"/ask", bytes.NewReader(body))
	if err != nil {
		return req.Session, "", err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	if token != "" {
		httpReq.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := (&http.Client{}).Do(httpReq) // no timeout: this is a stream
	if err != nil {
		return req.Session, "", fmt.Errorf("aria assistant unreachable: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		return req.Session, "", fmt.Errorf("aria assistant: HTTP %d", resp.StatusCode)
	}

	session := req.Session
	var finalText, finalErr string
	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 0, 1<<20), 16<<20)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || line[0] != '{' {
			continue
		}
		var ev relayEvent
		if json.Unmarshal([]byte(line), &ev) != nil {
			continue
		}
		switch ev.Type {
		case "text":
			if ev.Text != "" {
				emit("ai.delta", map[string]any{"text": ev.Text})
			}
		case "tool":
			emit("ai.tool", map[string]any{"name": ev.Text})
		case "approval":
			// Route Aria's approval through the same phone card flow as the claude-p path:
			// show ai.approval, wait for the phone's ai.approve, then POST the decision back.
			s.relayApproval(ctx, base, token, ev, emit, approvals)
		case "done":
			finalText = ev.Text
			finalErr = ev.Error
			if ev.Session != "" {
				session = ev.Session
			}
		}
	}
	if finalErr != "" {
		return session, finalText, fmt.Errorf("%s", finalErr)
	}
	return session, finalText, nil
}

// relayApproval shows the phone an Allow/Deny card and, when answered, POSTs the decision to Aria.
// It waits inline so later tool calls don't race ahead of a pending approval.
func (s *Server) relayApproval(ctx context.Context, base, token string, ev relayEvent, emit func(string, any), approvals *approvalRegistry) {
	if approvals == nil {
		s.postApprove(ctx, base, token, ev.ID, false) // no channel to ask → deny safely
		return
	}
	id := fmt.Sprintf("rly-%d", ev.ID)
	ch := approvals.register(id)
	emit("ai.approval", map[string]any{
		"id": id, "tool": ev.Label, "input": map[string]any{"command": ev.Command},
	})
	var ok bool
	select {
	case ok = <-ch:
	case <-ctx.Done():
		approvals.drop(id)
	}
	s.postApprove(ctx, base, token, ev.ID, ok)
}

func (s *Server) postApprove(ctx context.Context, base, token string, id int64, ok bool) {
	body, _ := json.Marshal(map[string]any{"id": id, "ok": ok})
	req, err := http.NewRequestWithContext(ctx, "POST", base+"/approve", bytes.NewReader(body))
	if err != nil {
		return
	}
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	if resp, err := (&http.Client{}).Do(req); err == nil {
		resp.Body.Close()
	}
}
