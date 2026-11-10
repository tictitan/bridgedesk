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
	}, func(*exec.Cmd) {}, nil)
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

// TestFleetSelftest exercises runFleet (device enumeration, per-device tagging, summary)
// against a hub holding just the local device. Same guard; one real Claude call.
func TestFleetSelftest(t *testing.T) {
	if os.Getenv("ARIA_AI_SELFTEST") != "1" {
		t.Skip("set ARIA_AI_SELFTEST=1 to run the live fleet relay self-test")
	}
	cfg := &Config{LocalPort: 47811, Name: "selftest"}
	s := &Server{cfg: cfg, hub: NewHub(NewLocalDevice(cfg))}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	var started, doneDevices []string
	summary, err := s.runFleet(ctx, aiFleetReq{Prompt: "Reply with exactly: OK"}, func(ev string, p any) {
		m, _ := p.(map[string]any)
		if ev == "ai.fleet.start" {
			started = append(started, m["device"].(string))
		} else if ev == "ai.fleet.done" {
			doneDevices = append(doneDevices, m["device"].(string))
		}
	})
	if err != nil {
		t.Fatalf("runFleet error: %v", err)
	}
	t.Logf("started=%v done=%v summary=%v", started, doneDevices, summary)
	if len(summary) == 0 {
		t.Fatal("expected at least the local device in the summary")
	}
	if len(started) != len(summary) || len(doneDevices) != len(summary) {
		t.Errorf("expected a start and done event per device: start=%d done=%d summary=%d",
			len(started), len(doneDevices), len(summary))
	}
}
