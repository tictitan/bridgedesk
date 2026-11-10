package main

// ai.go — the "Aria" brain relay for the phone.
//
// The phone has no engine of its own in phase 1: it sends a prompt over the normal
// dashboard WebSocket (/ws) with action "ai.ask", and the hub runs the user's local
// Claude CLI (`claude -p`, the same one the Aria desktop app drives) right here on the
// PC, streaming the reply back to the phone as it arrives. The CLI is given the
// BridgeDesk MCP server (bridge_* tools) so it can see and drive the whole fleet.
//
// Safety mirrors Aria: read-only fleet tools run freely; anything that CHANGES a
// device (run a command, write a file, remote input, send SMS, mount/transfer…) is
// only offered to the model when the phone turned on "Allow changes" for that turn.
// The CLI runs headless, so a disallowed tool is simply denied and the model explains
// what it would have done. Passwords and 2FA codes are never requested or handled.

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"
)

// aiReq is the params object of an "ai.ask" message from the phone.
type aiReq struct {
	Prompt       string `json:"prompt"`
	Session      string `json:"session"`      // claude --resume id; "" starts a new chat
	AllowChanges bool   `json:"allowChanges"` // opt-in to change/write tools this turn
	Model        string `json:"model"`        // optional model override
}

// Fleet tools that only read — always available to the phone's assistant.
var aiReadTools = []string{
	"mcp__bridgedesk__bridge_list_devices",
	"mcp__bridgedesk__bridge_list_files",
	"mcp__bridgedesk__bridge_read_file",
	"mcp__bridgedesk__bridge_screenshot",
	"mcp__bridgedesk__bridge_notifications",
	"mcp__bridgedesk__bridge_notification_history",
	"mcp__bridgedesk__bridge_latest_code",
	"mcp__bridgedesk__bridge_shares",
	"mcp__bridgedesk__email_accounts",
	"mcp__bridgedesk__email_list",
	"mcp__bridgedesk__email_read",
}

// Fleet tools that change a device — only offered when the phone allows changes.
var aiChangeTools = []string{
	"mcp__bridgedesk__bridge_run",
	"mcp__bridgedesk__bridge_write_file",
	"mcp__bridgedesk__bridge_transfer",
	"mcp__bridgedesk__bridge_input",
	"mcp__bridgedesk__bridge_clipboard",
	"mcp__bridgedesk__bridge_phone",
	"mcp__bridgedesk__bridge_share_create",
	"mcp__bridgedesk__bridge_share_mount",
	"mcp__bridgedesk__bridge_notification_action",
	"mcp__bridgedesk__email_send",
}

// Built-in Claude Code tools the phone assistant must NEVER use. The CLI otherwise
// inherits the user's global ~/.claude permission rules (which may auto-approve Bash,
// Read, etc.), so we deny every tool that could run code on the PC, touch its files,
// reach the network, or escape — leaving only the bridge_* fleet tools (plus ToolSearch,
// which the CLI needs to load the deferred MCP tools). deny overrides allow in the CLI.
var aiBuiltinDeny = []string{
	"Bash", "PowerShell", "Write", "Edit", "NotebookEdit", "Read", "Glob", "Grep",
	"WebFetch", "WebSearch", "Task", "Workflow", "Monitor", "RemoteTrigger",
	"ScheduleWakeup", "CronCreate", "CronDelete", "CronList", "SendMessage", "Skill",
	"Artifact", "ArtifactComments", "ArtifactData", "DesignSync", "EnterWorktree",
	"ExitWorktree", "PushNotification", "TaskStop", "ListAgents", "ReportFindings",
}

const ariaMobilePersona = `You are Aria, the user's personal assistant, reached from their phone.
You run on their PC and can see and drive their whole BridgeDesk fleet — every PC and this
phone — through the bridge_* tools. Keep replies short and skimmable — this is a small phone screen.

Working across the fleet:
- To "check the fleet": call bridge_list_devices, then for each ONLINE device read what needs
  attention (notifications, pending 2FA codes, anything waiting). Give a short per-device
  summary with the device name as a heading, then note what you could do next.
- When told to operate on one device, target it by name/id in every bridge_* call and leave the
  others alone.

Email: the user may have several mailboxes (Gmail, Outlook). Use email_accounts to see them,
email_list/email_read to go through mail (free), and email_send to send (a change — draft it, show
the user the recipient/subject/body, and only send after they approve). Name the account when more
than one exists.

Safety:
- Reading is free (list devices/files, read a file, screenshot, notifications, 2FA codes).
- Anything that CHANGES something — run a command, write/move a file, remote tap/type,
  send an SMS or WhatsApp, dial a call, mount or transfer, act on a notification — must be
  confirmed by the user. If a change tool is not available this turn, do NOT keep trying it:
  say plainly what you would do and tell the user to turn on "Allow changes" and resend.
- Never ask for, store, or type passwords, PINs, or 2FA/OTP codes. You may read a 2FA code to
  tell the user, but never enter one anywhere yourself.
Prefer naming the device you act on. When unsure which device, list devices first.`

// findClaude locates the Claude CLI the same way the Aria desktop app does.
func findClaude() string {
	if p := os.Getenv("ARIA_CLAUDE_BIN"); p != "" {
		return p
	}
	home, _ := os.UserHomeDir()
	cands := []string{
		filepath.Join(home, ".local", "bin", "claude"),
		filepath.Join(home, ".claude", "local", "claude"),
		filepath.Join(home, ".npm-global", "bin", "claude"),
		"/opt/homebrew/bin/claude",
		"/usr/local/bin/claude",
	}
	if runtime.GOOS == "windows" {
		cands = append([]string{
			filepath.Join(home, ".local", "bin", "claude.exe"),
			filepath.Join(home, ".local", "bin", "claude.cmd"),
			filepath.Join(home, "AppData", "Roaming", "npm", "claude.cmd"),
		}, cands...)
	}
	for _, c := range cands {
		if st, err := os.Stat(c); err == nil && !st.IsDir() {
			return c
		}
	}
	if p, err := exec.LookPath("claude"); err == nil {
		return p
	}
	return "claude" // last resort: rely on PATH
}

// aiWorkDir is a stable project folder so `claude --resume <id>` finds prior chat state.
func aiWorkDir() string {
	home, _ := os.UserHomeDir()
	d := filepath.Join(home, ".bridgedesk", "aria-mobile")
	_ = os.MkdirAll(d, 0o755)
	return d
}

// runAI drives the local Claude CLI for one phone turn, streaming events through emit.
// It returns the session id to resume next turn and the final assistant text.
func (s *Server) runAI(ctx context.Context, req aiReq, emit func(event string, params any), setCmd func(*exec.Cmd), approvals *approvalRegistry) (string, string, error) {
	if strings.TrimSpace(req.Prompt) == "" {
		return req.Session, "", fmt.Errorf("empty prompt")
	}

	allowed := append([]string{}, aiReadTools...)
	disallowed := append([]string{}, aiBuiltinDeny...)
	if req.AllowChanges {
		// Auto-approve every change this turn (no per-action card).
		allowed = append(allowed, aiChangeTools...)
	} else if approvals == nil {
		// No per-action approval channel (e.g. fleet workers): deny change tools outright so
		// the user's global config can't silently auto-approve one.
		disallowed = append(disallowed, aiChangeTools...)
	}
	// else: a phone approval channel is available and changes weren't blanket-approved, so change
	// tools are left neither allowed nor denied — the CLI routes each to the permission-prompt-tool,
	// which shows an Allow/Deny card on the phone. Built-in shell etc. stay hard-denied (no card).

	servers := map[string]any{
		"bridgedesk": map[string]any{"type": "http", "url": fmt.Sprintf("http://127.0.0.1:%d/mcp", s.cfg.LocalPort)},
	}
	apprOn := false
	if approvals != nil {
		url, stop, err := startApprovalServer(func(tool string, input json.RawMessage) bool {
			id, ch := approvals.create()
			emit("ai.approval", map[string]any{"id": id, "tool": strings.TrimPrefix(tool, "mcp__bridgedesk__"), "input": input})
			select {
			case allow := <-ch:
				return allow
			case <-ctx.Done():
				approvals.drop(id)
				return false
			case <-time.After(5 * time.Minute):
				approvals.drop(id)
				return false
			}
		})
		if err == nil {
			servers["ariaapprove"] = map[string]any{"type": "http", "url": url}
			apprOn = true
			defer stop()
		}
	}
	mcpCfg, _ := json.Marshal(map[string]any{"mcpServers": servers})

	args := []string{
		"-p",
		"--output-format", "stream-json",
		"--verbose", // required for stream-json in print mode
		"--append-system-prompt", ariaMobilePersona,
		"--strict-mcp-config",
		"--mcp-config", string(mcpCfg),
	}
	if apprOn {
		args = append(args, "--permission-prompt-tool", "mcp__ariaapprove__approve")
	}
	if req.Model != "" {
		args = append(args, "--model", req.Model)
	}
	if req.Session != "" {
		args = append(args, "--resume", req.Session)
	}
	// Variadic allow/deny lists go last (each ends at the next --flag); the prompt is fed
	// on stdin so it can't be swallowed into a tool list and has no command-line length cap.
	args = append(args, "--allowedTools")
	args = append(args, allowed...)
	args = append(args, "--disallowedTools")
	args = append(args, disallowed...)

	cmd := exec.CommandContext(ctx, findClaude(), args...)
	cmd.Dir = aiWorkDir()
	cmd.Env = os.Environ() // same login / Bedrock config as when the user runs claude
	cmd.Stdin = strings.NewReader(req.Prompt)
	hideWindow(cmd)

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return req.Session, "", err
	}
	var stderr strings.Builder
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		return req.Session, "", fmt.Errorf("could not start claude: %w", err)
	}
	setCmd(cmd)

	session := req.Session
	var finalText string

	sc := bufio.NewScanner(stdout)
	sc.Buffer(make([]byte, 0, 1<<20), 16<<20) // allow long lines (base64 screenshots etc.)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || line[0] != '{' {
			continue
		}
		var evt map[string]json.RawMessage
		if json.Unmarshal([]byte(line), &evt) != nil {
			continue
		}
		var typ string
		json.Unmarshal(evt["type"], &typ)
		if sid := rawStr(evt["session_id"]); sid != "" {
			session = sid
		}
		switch typ {
		case "assistant":
			for _, text := range assistantParts(evt["message"], emit) {
				if text != "" {
					emit("ai.delta", map[string]any{"text": text})
				}
			}
		case "user":
			// tool_result coming back from a tool the model ran — surface a short snippet
			// so the phone can show what happened ("Checking email… → 3 unread"), not just spin.
			if s := toolResultSnippet(evt["message"]); s != "" {
				emit("ai.tool.result", map[string]any{"text": s})
			}
		case "result":
			finalText = rawStr(evt["result"])
			var isErr bool
			json.Unmarshal(evt["is_error"], &isErr)
			if isErr && finalText == "" {
				finalText = rawStr(evt["error"])
			}
		}
	}
	waitErr := cmd.Wait()
	setCmd(nil)

	if ctx.Err() == context.Canceled {
		return session, finalText, fmt.Errorf("cancelled")
	}
	if waitErr != nil && finalText == "" {
		msg := strings.TrimSpace(stderr.String())
		if msg == "" {
			msg = waitErr.Error()
		}
		return session, "", fmt.Errorf("claude: %s", aiFirstLine(msg))
	}
	return session, finalText, nil
}

// assistantParts pulls text out of an assistant message and emits an ai.tool event for
// each tool the model is about to call, so the phone can show "running bridge_screenshot…".
func assistantParts(msg json.RawMessage, emit func(string, any)) []string {
	var m struct {
		Content []struct {
			Type  string          `json:"type"`
			Text  string          `json:"text"`
			Name  string          `json:"name"`
			Input json.RawMessage `json:"input"`
		} `json:"content"`
	}
	if json.Unmarshal(msg, &m) != nil {
		return nil
	}
	var texts []string
	for _, c := range m.Content {
		switch c.Type {
		case "text":
			texts = append(texts, c.Text)
		case "tool_use":
			name := strings.TrimPrefix(c.Name, "mcp__bridgedesk__")
			emit("ai.tool", map[string]any{"name": name, "input": c.Input})
		}
	}
	return texts
}

// toolResultSnippet extracts a short, human-readable snippet from a tool_result message.
func toolResultSnippet(msg json.RawMessage) string {
	var m struct {
		Content []struct {
			Type    string          `json:"type"`
			Content json.RawMessage `json:"content"`
			Text    string          `json:"text"`
		} `json:"content"`
	}
	if json.Unmarshal(msg, &m) != nil {
		return ""
	}
	for _, c := range m.Content {
		if c.Type != "tool_result" {
			continue
		}
		// content may be a string, or an array of {type:text,text:...} blocks
		var s string
		if json.Unmarshal(c.Content, &s) != nil {
			var blocks []struct {
				Type, Text string
			}
			if json.Unmarshal(c.Content, &blocks) == nil {
				for _, b := range blocks {
					if b.Type == "text" {
						s += b.Text
					}
				}
			}
		}
		s = strings.TrimSpace(strings.ReplaceAll(s, "\n", " "))
		if len(s) > 160 {
			s = s[:160] + "…"
		}
		return s
	}
	return ""
}

func rawStr(r json.RawMessage) string {
	if len(r) == 0 {
		return ""
	}
	var s string
	if json.Unmarshal(r, &s) == nil {
		return s
	}
	return ""
}

func aiFirstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}

// aiSession tracks the in-flight work for a single phone connection: the current CLI
// process (single ask) and/or a cancel func for a whole-fleet run.
type aiSession struct {
	mu       sync.Mutex
	cmd      *exec.Cmd
	cancelFn context.CancelFunc
}

func (a *aiSession) set(c *exec.Cmd)                { a.mu.Lock(); a.cmd = c; a.mu.Unlock() }
func (a *aiSession) setCancel(fn context.CancelFunc) { a.mu.Lock(); a.cancelFn = fn; a.mu.Unlock() }
func (a *aiSession) cancel() {
	a.mu.Lock()
	c, fn := a.cmd, a.cancelFn
	a.mu.Unlock()
	if c != nil && c.Process != nil {
		_ = c.Process.Kill()
	}
	if fn != nil {
		fn()
	}
}

// aiFleetReq is the params of an "ai.fleet" message: run one Claude turn per device.
type aiFleetReq struct {
	Prompt       string   `json:"prompt"`
	Devices      []string `json:"devices"` // ids or names; empty = all online devices
	AllowChanges bool     `json:"allowChanges"`
	Model        string   `json:"model"`
}

// runFleet runs the same task on each target device as its own Claude turn, in parallel
// (capped), each scoped to that one device. Events are tagged with the device name so the
// phone can show a section per machine. Returns a {device,text[,error]} summary per device.
func (s *Server) runFleet(ctx context.Context, req aiFleetReq, emit func(event string, params any)) ([]map[string]string, error) {
	if strings.TrimSpace(req.Prompt) == "" {
		return nil, fmt.Errorf("empty task")
	}
	want := map[string]bool{}
	for _, d := range req.Devices {
		want[strings.ToLower(strings.TrimSpace(d))] = true
	}
	var targets []DeviceInfo
	for _, d := range s.hub.List() {
		if !d.Online {
			continue
		}
		if len(want) > 0 && !want[strings.ToLower(d.ID)] && !want[strings.ToLower(d.Name)] {
			continue
		}
		targets = append(targets, d)
	}
	if len(targets) == 0 {
		return nil, fmt.Errorf("no matching online devices")
	}

	type res struct {
		name, text, err string
	}
	out := make([]res, len(targets))
	sem := make(chan struct{}, 4) // bound concurrency (and cost)
	var wg sync.WaitGroup
	for i, d := range targets {
		wg.Add(1)
		go func(i int, d DeviceInfo) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			emit("ai.fleet.start", map[string]any{"device": d.Name})
			scoped := fmt.Sprintf("You are operating on ONE device only: %q (id %s, %s). Use the bridge_* "+
				"tools targeting that device for everything; do not touch other devices.\n\nTask:\n%s",
				d.Name, d.ID, d.OS, req.Prompt)
			devEmit := func(ev string, p any) {
				if m, ok := p.(map[string]any); ok {
					m["device"] = d.Name
				}
				emit(ev, p)
			}
			_, text, err := s.runAI(ctx, aiReq{Prompt: scoped, AllowChanges: req.AllowChanges, Model: req.Model}, devEmit, func(*exec.Cmd) {}, nil)
			r := res{name: d.Name, text: text}
			done := map[string]any{"device": d.Name, "ok": err == nil}
			if err != nil {
				r.err = err.Error()
				done["error"] = r.err
			} else {
				done["text"] = text
			}
			out[i] = r
			emit("ai.fleet.done", done)
		}(i, d)
	}
	wg.Wait()

	summary := make([]map[string]string, 0, len(out))
	for _, r := range out {
		m := map[string]string{"device": r.name, "text": r.text}
		if r.err != "" {
			m["error"] = r.err
		}
		summary = append(summary, m)
	}
	return summary, nil
}
