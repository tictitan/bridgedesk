package main

import (
	"encoding/json"
	"os"
	"os/exec"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// TestApprovalSelftest verifies the permission-prompt-tool contract end to end: a change tool
// (bridge_run) is neither allowed nor denied, so the CLI must call our approval server; when we
// return behavior:"allow", the command must actually run. Guarded (one real Claude call) +
// needs a live hub MCP on :47811.
//
//	ARIA_AI_SELFTEST=1 go test -run ApprovalSelftest -v
func TestApprovalSelftest(t *testing.T) {
	if os.Getenv("ARIA_AI_SELFTEST") != "1" {
		t.Skip("set ARIA_AI_SELFTEST=1 to run the live approval-contract self-test")
	}
	var asked int32
	var askedTool atomic.Value
	askedTool.Store("")
	url, stop, err := startApprovalServer(func(tool string, input json.RawMessage) bool {
		atomic.AddInt32(&asked, 1)
		askedTool.Store(tool)
		return true // auto-allow
	})
	if err != nil {
		t.Fatal(err)
	}
	defer stop()

	mcpCfg, _ := json.Marshal(map[string]any{"mcpServers": map[string]any{
		"bridgedesk":  map[string]any{"type": "http", "url": "http://127.0.0.1:47811/mcp"},
		"ariaapprove": map[string]any{"type": "http", "url": url},
	}})
	// bridge_run is NOT pre-allowed, so it must be approved via our permission-prompt-tool.
	args := []string{
		"-p", "--output-format", "stream-json", "--verbose",
		"--append-system-prompt", "You are Aria. Use the bridge tools on the local PC.",
		"--strict-mcp-config", "--mcp-config", string(mcpCfg),
		"--permission-prompt-tool", "mcp__ariaapprove__approve",
		"--allowedTools", "mcp__bridgedesk__bridge_list_devices",
		"--disallowedTools", "Bash", "PowerShell", "Read", "Monitor", "Task",
	}
	cmd := exec.Command(findClaude(), args...)
	cmd.Dir = aiWorkDir()
	cmd.Env = os.Environ()
	cmd.Stdin = strings.NewReader("Run the shell command whoami on the local PC using the bridge tools and tell me the exact output.")
	out, _ := cmd.CombinedOutput()
	done := make(chan struct{})
	go func() { cmd.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(3 * time.Minute):
		cmd.Process.Kill()
	}

	text := string(out)
	t.Logf("asked=%d tool=%v", atomic.LoadInt32(&asked), askedTool.Load())
	if atomic.LoadInt32(&asked) == 0 {
		t.Fatal("permission-prompt-tool was never called — the CLI didn't route the change tool to our approval server")
	}
	if tl, _ := askedTool.Load().(string); !strings.Contains(tl, "bridge_run") {
		t.Errorf("expected approval request for bridge_run, got %q", tl)
	}
	if !strings.Contains(strings.ToLower(text), "whoami") && !strings.Contains(text, "\\") && !strings.Contains(strings.ToLower(text), "user") {
		t.Logf("output (trimmed): %s", text[:minI(len(text), 600)])
	}
}

func minI(a, b int) int {
	if a < b {
		return a
	}
	return b
}
