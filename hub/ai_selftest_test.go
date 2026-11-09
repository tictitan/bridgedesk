package main

import (
	"context"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"
)

// TestAISelftest exercises the real runAI relay against a live hub MCP on LocalPort.
// It makes a real Claude CLI call, so it is skipped unless ARIA_AI_SELFTEST=1 (keeps CI green).
//
//	ARIA_AI_SELFTEST=1 ARIA_AI_PORT=47811 go test -run AISelftest -v
func TestAISelftest(t *testing.T) {
	if os.Getenv("ARIA_AI_SELFTEST") != "1" {
		t.Skip("set ARIA_AI_SELFTEST=1 to run the live Claude relay self-test")
	}
	port := 47811
	s := &Server{cfg: &Config{LocalPort: port}}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	var deltas []string
	session, text, err := s.runAI(ctx, aiReq{Prompt: "Reply with exactly: OK"}, func(ev string, p any) {
		t.Logf("event %s: %v", ev, p)
		if ev == "ai.delta" {
			if m, ok := p.(map[string]any); ok {
				deltas = append(deltas, m["text"].(string))
			}
		}
	}, func(*exec.Cmd) {})
	if err != nil {
		t.Fatalf("runAI error: %v", err)
	}
	t.Logf("session=%s finalText=%q deltas=%v", session, text, deltas)
	if session == "" {
		t.Error("expected a resume session id")
	}
	if !strings.Contains(strings.ToLower(text+strings.Join(deltas, " ")), "ok") {
		t.Errorf("expected the model to say OK, got %q / %v", text, deltas)
	}
}
