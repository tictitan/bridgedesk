package main

// approvals.go — per-action approval on the phone. Instead of the coarse per-turn "Allow changes"
// toggle, the relay runs the Claude CLI with --permission-prompt-tool pointing at a tiny per-turn
// MCP server started here. When the model wants a tool that isn't pre-allowed, the CLI calls that
// server's `approve` tool; the hub shows an Allow/Deny card on the phone and blocks until the user
// decides, then returns the decision to the CLI. Built-in shell etc. stay hard-denied (no card).

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"sync"
	"time"
)

// approvalRegistry correlates a phone's ai.approve reply with the blocked approval request.
type approvalRegistry struct {
	mu   sync.Mutex
	seq  int64
	wait map[string]chan bool
}

func newApprovalRegistry() *approvalRegistry { return &approvalRegistry{wait: map[string]chan bool{}} }

func (a *approvalRegistry) create() (string, chan bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.seq++
	id := fmt.Sprintf("apr-%d", a.seq)
	ch := make(chan bool, 1)
	a.wait[id] = ch
	return id, ch
}

func (a *approvalRegistry) resolve(id string, allow bool) {
	a.mu.Lock()
	ch := a.wait[id]
	delete(a.wait, id)
	a.mu.Unlock()
	if ch != nil {
		select {
		case ch <- allow:
		default:
		}
	}
}

func (a *approvalRegistry) drop(id string) {
	a.mu.Lock()
	delete(a.wait, id)
	a.mu.Unlock()
}

// startApprovalServer brings up a loopback MCP server exposing one tool, `approve`. onAsk is
// called with the pending tool name + input and returns the decision (true = allow). Returns the
// server's MCP URL and a stop func.
func startApprovalServer(onAsk func(toolName string, input json.RawMessage) bool) (string, func(), error) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return "", nil, err
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/mcp", func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "GET" {
			writeJSON(w, 200, map[string]any{"service": "aria-approve", "transport": "http"})
			return
		}
		var req mcpReq
		if json.NewDecoder(r.Body).Decode(&req) != nil {
			writeJSON(w, 200, mcpError(nil, -32700, "parse error"))
			return
		}
		switch req.Method {
		case "initialize":
			writeJSON(w, 200, mcpResult(req.ID, map[string]any{
				"protocolVersion": "2024-11-05",
				"capabilities":    map[string]any{"tools": map[string]any{}},
				"serverInfo":      map[string]any{"name": "aria-approve", "version": appVersion},
			}))
		case "notifications/initialized":
			w.WriteHeader(202)
		case "tools/list":
			writeJSON(w, 200, mcpResult(req.ID, map[string]any{"tools": []map[string]any{{
				"name":        "approve",
				"description": "Ask the user to allow or deny a tool call.",
				"inputSchema": map[string]any{"type": "object", "properties": map[string]any{
					"tool_name": map[string]any{"type": "string"},
					"input":     map[string]any{"type": "object"},
				}},
			}}}))
		case "tools/call":
			var call struct {
				Name string `json:"name"`
				Args struct {
					ToolName string          `json:"tool_name"`
					Input    json.RawMessage `json:"input"`
				} `json:"arguments"`
			}
			json.Unmarshal(req.Params, &call)
			allow := onAsk(call.Args.ToolName, call.Args.Input)
			// Claude Code's permission-prompt-tool contract: return a single text block whose
			// text is JSON {behavior:"allow",updatedInput} | {behavior:"deny",message}.
			var decision map[string]any
			if allow {
				in := call.Args.Input
				if len(in) == 0 {
					in = json.RawMessage(`{}`)
				}
				decision = map[string]any{"behavior": "allow", "updatedInput": in}
			} else {
				decision = map[string]any{"behavior": "deny", "message": "Denied on the phone."}
			}
			db, _ := json.Marshal(decision)
			writeJSON(w, 200, mcpResult(req.ID, map[string]any{
				"content": []map[string]any{{"type": "text", "text": string(db)}},
			}))
		default:
			writeJSON(w, 200, mcpError(req.ID, -32601, "method not found"))
		}
	})
	srv := &http.Server{Handler: mux}
	go srv.Serve(ln)
	url := fmt.Sprintf("http://127.0.0.1:%d/mcp", ln.Addr().(*net.TCPAddr).Port)
	stop := func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		srv.Shutdown(ctx)
	}
	return url, stop, nil
}
