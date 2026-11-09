package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// MCP server (JSON-RPC 2.0 over HTTP) so Claude can drive any linked device:
// run PowerShell/shell, browse/transfer files, screenshot, and move the mouse/keyboard.
// Bound to loopback by default; when AllowMCP is off it is disabled entirely.

type mcpReq struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params"`
}

func (s *Server) handleMCP(w http.ResponseWriter, r *http.Request) {
	if !s.cfg.AllowMCP {
		http.Error(w, "mcp disabled", 403)
		return
	}
	// MCP is loopback-only unless the caller presents the access key (for a trusted relay).
	if !isLoopback(r.RemoteAddr) && !s.keyOK(s.token(r)) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	if r.Method == "GET" {
		writeJSON(w, 200, map[string]any{"service": "bridgedesk-mcp", "transport": "http", "version": appVersion})
		return
	}
	body, _ := io.ReadAll(io.LimitReader(r.Body, 16<<20))
	var req mcpReq
	if err := json.Unmarshal(body, &req); err != nil {
		writeJSON(w, 200, mcpError(nil, -32700, "parse error"))
		return
	}
	resp := s.mcpDispatch(r.Context(), req)
	if resp != nil {
		writeJSON(w, 200, resp)
	} else {
		w.WriteHeader(202) // notification
	}
}

func mcpResult(id json.RawMessage, result any) map[string]any {
	return map[string]any{"jsonrpc": "2.0", "id": rawID(id), "result": result}
}
func mcpError(id json.RawMessage, code int, msg string) map[string]any {
	return map[string]any{"jsonrpc": "2.0", "id": rawID(id), "error": map[string]any{"code": code, "message": msg}}
}
func rawID(id json.RawMessage) any {
	if len(id) == 0 {
		return nil
	}
	return id
}

func (s *Server) mcpDispatch(ctx context.Context, req mcpReq) any {
	switch req.Method {
	case "initialize":
		return mcpResult(req.ID, map[string]any{
			"protocolVersion": "2024-11-05",
			"capabilities":    map[string]any{"tools": map[string]any{}},
			"serverInfo":      map[string]any{"name": "BridgeDesk", "version": appVersion},
		})
	case "notifications/initialized":
		return nil
	case "tools/list":
		return mcpResult(req.ID, map[string]any{"tools": mcpTools()})
	case "tools/call":
		return s.mcpCallTool(ctx, req)
	default:
		return mcpError(req.ID, -32601, "method not found: "+req.Method)
	}
}

func mcpTools() []map[string]any {
	dev := map[string]any{"type": "string", "description": "Target device id or name. Use 'local' or omit for this machine; call bridge_list_devices to see others."}
	return []map[string]any{
		{
			"name":        "bridge_list_devices",
			"description": "List all machines reachable through this hub (this PC plus any linked phones/PCs), with their id, name, OS and capabilities.",
			"inputSchema": map[string]any{"type": "object", "properties": map[string]any{}},
		},
		{
			"name":        "bridge_run",
			"description": "Run a shell command on a device and return stdout/stderr/exit code. On Windows the default shell is PowerShell (set shell='cmd' for cmd.exe); elsewhere it is bash.",
			"inputSchema": map[string]any{"type": "object", "required": []string{"command"}, "properties": map[string]any{
				"device":  dev,
				"command": map[string]any{"type": "string", "description": "Command line to execute."},
				"shell":   map[string]any{"type": "string", "enum": []string{"powershell", "pwsh", "cmd", "bash"}, "description": "Shell to use."},
				"cwd":     map[string]any{"type": "string", "description": "Working directory."},
				"timeout": map[string]any{"type": "integer", "description": "Timeout in seconds (default 60)."},
			}},
		},
		{
			"name":        "bridge_list_files",
			"description": "List a directory on a device. Returns entries with name, path, dir flag, size and mtime. With no path, lists the device's home directory.",
			"inputSchema": map[string]any{"type": "object", "properties": map[string]any{
				"device": dev,
				"path":   map[string]any{"type": "string"},
			}},
		},
		{
			"name":        "bridge_read_file",
			"description": "Read a (text) file from a device and return its contents. Best for files under ~1 MB.",
			"inputSchema": map[string]any{"type": "object", "required": []string{"path"}, "properties": map[string]any{
				"device": dev, "path": map[string]any{"type": "string"},
			}},
		},
		{
			"name":        "bridge_write_file",
			"description": "Create or overwrite a text file on a device with the given content.",
			"inputSchema": map[string]any{"type": "object", "required": []string{"path", "content"}, "properties": map[string]any{
				"device": dev, "path": map[string]any{"type": "string"}, "content": map[string]any{"type": "string"},
			}},
		},
		{
			"name":        "bridge_transfer",
			"description": "Copy a file from one device to another through the hub (e.g. pull a file off a phone onto the PC, or push one to it).",
			"inputSchema": map[string]any{"type": "object", "required": []string{"fromDevice", "fromPath", "toDevice", "toPath"}, "properties": map[string]any{
				"fromDevice": map[string]any{"type": "string"}, "fromPath": map[string]any{"type": "string"},
				"toDevice": map[string]any{"type": "string"}, "toPath": map[string]any{"type": "string"},
			}},
		},
		{
			"name":        "bridge_screenshot",
			"description": "Capture the current screen of a device and return it as an image.",
			"inputSchema": map[string]any{"type": "object", "properties": map[string]any{
				"device": dev, "maxWidth": map[string]any{"type": "integer", "description": "Downscale to this width (default 1280)."},
				"monitor": map[string]any{"type": "integer", "description": "Display to capture on multi-monitor PCs: 0 = primary (default), 1.. = others, -1 = all displays."},
			}},
		},
		{
			"name":        "bridge_input",
			"description": "Send mouse or keyboard input to a device for remote control. kind='move'|'click'|'dblclick'|'scroll' use x,y in 0..1 fractions of the captured screen (pass the same 'monitor' you screenshotted); kind='key' sends a key or combo like 'ctrl+shift+escape' or 'win+r'; kind='type' types text (newlines press Enter).",
			"inputSchema": map[string]any{"type": "object", "required": []string{"kind"}, "properties": map[string]any{
				"device": dev,
				"kind":   map[string]any{"type": "string", "enum": []string{"move", "click", "dblclick", "scroll", "key", "type"}},
				"x":      map[string]any{"type": "number"}, "y": map[string]any{"type": "number"},
				"button":  map[string]any{"type": "string", "enum": []string{"left", "right", "middle"}},
				"dy":      map[string]any{"type": "integer", "description": "Scroll amount for kind=scroll."},
				"key":     map[string]any{"type": "string", "description": "Key or combo for kind=key."},
				"text":    map[string]any{"type": "string", "description": "Text for kind=type."},
				"monitor": map[string]any{"type": "integer", "description": "Display the x,y refer to (0 = primary, -1 = all)."},
			}},
		},
		{
			"name":        "bridge_clipboard",
			"description": "Get or set a device's clipboard text. Omit 'text' to read; provide 'text' to set (device 'all' sets it on every PC and phone). history=true lists the fleet's recent clipboard items (newest first).",
			"inputSchema": map[string]any{"type": "object", "properties": map[string]any{
				"device": dev, "text": map[string]any{"type": "string"},
				"history": map[string]any{"type": "boolean", "description": "Return the shared clipboard history instead."},
			}},
		},
		{
			"name":        "bridge_phone",
			"description": "Use a linked Android phone's calls and texts. action: 'sms_threads' (list conversations), 'sms_read' (messages of a thread or number), 'sms_send' (text someone), 'whatsapp_send' (send a WhatsApp message to a number), 'call_dial' (place a call — audio stays on the phone), 'call_answer', 'call_end', 'call_log'. Always confirm the recipient and text with the user before sending or calling.",
			"inputSchema": map[string]any{"type": "object", "required": []string{"action"}, "properties": map[string]any{
				"device": map[string]any{"type": "string", "description": "Phone id or name (defaults to the first online phone)."},
				"action": map[string]any{"type": "string", "enum": []string{"sms_threads", "sms_read", "sms_send", "whatsapp_send", "call_dial", "call_answer", "call_end", "call_log"}},
				"to":     map[string]any{"type": "string", "description": "Phone number for sms_send / whatsapp_send / call_dial (include the country code for WhatsApp)."},
				"text":   map[string]any{"type": "string", "description": "Message text."},
				"thread": map[string]any{"type": "integer", "description": "SMS thread id for sms_read (from sms_threads)."},
				"limit":  map[string]any{"type": "integer"},
			}},
		},
		{
			"name":        "bridge_shares",
			"description": "List the SMB shares a device is exposing (Windows). Each has a name, folder path, and the UNC path others mount.",
			"inputSchema": map[string]any{"type": "object", "properties": map[string]any{"device": dev}},
		},
		{
			"name":        "bridge_share_create",
			"description": "Expose a folder on a device as an SMB share so other machines in the fleet can mount it as a network drive (Windows; needs admin). Returns the UNC path to mount elsewhere.",
			"inputSchema": map[string]any{"type": "object", "required": []string{"path", "name"}, "properties": map[string]any{
				"device":   dev,
				"name":     map[string]any{"type": "string", "description": "Share name (what others see, e.g. 'Assets')."},
				"path":     map[string]any{"type": "string", "description": "Local folder to share."},
				"readonly": map[string]any{"type": "boolean", "description": "Share read-only."},
			}},
		},
		{
			"name":        "bridge_share_mount",
			"description": "Mount a shared folder on a device: SMB (\\\\server\\share) or NFS (server:/export). mountAs is a drive letter on Windows (e.g. 'Z:') or a directory on Linux/macOS. Gives fleet machines shared storage.",
			"inputSchema": map[string]any{"type": "object", "required": []string{"source", "mountAs"}, "properties": map[string]any{
				"device":  dev,
				"source":  map[string]any{"type": "string", "description": "\\\\host\\share (SMB) or host:/export (NFS)."},
				"mountAs": map[string]any{"type": "string", "description": "Drive letter (Windows) or mount directory (Linux/macOS)."},
				"user":    map[string]any{"type": "string"},
				"pass":    map[string]any{"type": "string"},
			}},
		},
		{
			"name":        "bridge_notifications",
			"description": "List current notifications mirrored from a phone (messages, SMS codes, push prompts like Okta Verify). Each has a key, app, title, text and a list of action labels.",
			"inputSchema": map[string]any{"type": "object", "properties": map[string]any{"device": dev}},
		},
		{
			"name":        "bridge_notification_action",
			"description": "Act on a phone notification: open it (opens the app on the phone), tap one of its actions by index (e.g. 'Approve' on an Okta Verify prompt), send an inline reply to a messaging notification, or dismiss it. Use bridge_notifications first to get the key and the actions (each has a title and a 'reply' flag).",
			"inputSchema": map[string]any{"type": "object", "required": []string{"key"}, "properties": map[string]any{
				"device":  dev,
				"key":     map[string]any{"type": "string", "description": "Notification key from bridge_notifications."},
				"action":  map[string]any{"type": "integer", "description": "Index into the actions list to trigger."},
				"reply":   map[string]any{"type": "string", "description": "Text to send as an inline reply (use with the index of a reply action, e.g. WhatsApp 'Reply')."},
				"open":    map[string]any{"type": "boolean", "description": "If true, open the notification (launch its app on the phone)."},
				"dismiss": map[string]any{"type": "boolean", "description": "If true, dismiss the notification."},
			}},
		},
		{
			"name":        "bridge_notification_history",
			"description": "Search the hub's saved history of phone notifications from every linked phone (kept even after they were dismissed). Filter by text, app, or only those containing a one-time code.",
			"inputSchema": map[string]any{"type": "object", "properties": map[string]any{
				"query":   map[string]any{"type": "string", "description": "Text to search in app, title and body."},
				"app":     map[string]any{"type": "string", "description": "App name or package, e.g. 'WhatsApp' or 'com.google.android.apps.messaging'."},
				"codes":   map[string]any{"type": "boolean", "description": "Only notifications with a detected verification code."},
				"limit":   map[string]any{"type": "integer", "description": "Max results (default 30)."},
				"minutes": map[string]any{"type": "integer", "description": "Only the last N minutes."},
			}},
		},
		{
			"name":        "bridge_latest_code",
			"description": "Get the most recent one-time / 2FA verification code that arrived on a linked phone (SMS, email or app notification). Use right after triggering a login that sends a code.",
			"inputSchema": map[string]any{"type": "object", "properties": map[string]any{
				"app":     map[string]any{"type": "string", "description": "Optional app/sender filter, e.g. 'Messages' or 'Google'."},
				"minutes": map[string]any{"type": "integer", "description": "How far back to look (default 10)."},
				"wait":    map[string]any{"type": "integer", "description": "Seconds to wait for a new code to arrive if none is found yet (max 120)."},
			}},
		},
		{
			"name":        "email_accounts",
			"description": "List the mail accounts (Gmail / Outlook) Aria can read and send from. Use their 'email' as the 'account' argument of the other email_* tools.",
			"inputSchema": map[string]any{"type": "object", "properties": map[string]any{}},
		},
		{
			"name":        "email_list",
			"description": "List recent messages in a mailbox. Optional 'query' uses the provider's search (Gmail search syntax like 'is:unread from:bob', or Outlook keywords). Returns id, from, subject, date, snippet, unread.",
			"inputSchema": map[string]any{"type": "object", "properties": map[string]any{
				"account": map[string]any{"type": "string", "description": "Which mailbox (its email). Omit if only one is configured."},
				"query":   map[string]any{"type": "string", "description": "Search/filter, e.g. 'is:unread' or 'from:alice subject:invoice'."},
				"limit":   map[string]any{"type": "integer", "description": "Max messages (default 15, max 50)."},
			}},
		},
		{
			"name":        "email_read",
			"description": "Read one message in full (headers + plain-text body) by its id from email_list.",
			"inputSchema": map[string]any{"type": "object", "required": []string{"id"}, "properties": map[string]any{
				"account": map[string]any{"type": "string"},
				"id":      map[string]any{"type": "string", "description": "Message id from email_list."},
			}},
		},
		{
			"name":        "email_send",
			"description": "Send an email from one of the user's mailboxes. This CHANGES things — only call it once the user has approved the recipient, subject and body.",
			"inputSchema": map[string]any{"type": "object", "required": []string{"to", "subject", "body"}, "properties": map[string]any{
				"account": map[string]any{"type": "string", "description": "Which mailbox to send from (its email)."},
				"to":      map[string]any{"type": "string", "description": "Recipient(s), comma-separated."},
				"cc":      map[string]any{"type": "string", "description": "Optional Cc, comma-separated."},
				"subject": map[string]any{"type": "string"},
				"body":    map[string]any{"type": "string", "description": "Plain-text body."},
			}},
		},
	}
}

func (s *Server) mcpCallTool(ctx context.Context, req mcpReq) any {
	var call struct {
		Name string          `json:"name"`
		Args json.RawMessage `json:"arguments"`
	}
	json.Unmarshal(req.Params, &call)
	args := map[string]any{}
	json.Unmarshal(call.Args, &args)
	str := func(k string) string { v, _ := args[k].(string); return v }
	dev := str("device")

	textResult := func(s string) any {
		return mcpResult(req.ID, map[string]any{"content": []map[string]any{{"type": "text", "text": s}}})
	}
	jsonResult := func(v any) any {
		b, _ := json.MarshalIndent(v, "", "  ")
		return textResult(string(b))
	}
	fail := func(err error) any {
		return mcpResult(req.ID, map[string]any{"isError": true, "content": []map[string]any{{"type": "text", "text": "Error: " + err.Error()}}})
	}

	switch call.Name {
	case "bridge_list_devices":
		return jsonResult(s.hub.List())

	case "bridge_run":
		var out ExecResult
		err := s.hub.Call(ctx, dev, "exec", map[string]any{
			"command": str("command"), "shell": str("shell"), "cwd": str("cwd"),
			"timeout": intArg(args, "timeout"),
		}, &out)
		if err != nil {
			return fail(err)
		}
		var sb strings.Builder
		if out.Stdout != "" {
			sb.WriteString(out.Stdout)
		}
		if out.Stderr != "" {
			sb.WriteString("\n[stderr]\n" + out.Stderr)
		}
		fmt.Fprintf(&sb, "\n[exit %d, %dms]", out.ExitCode, out.DurationMs)
		if out.TimedOut {
			sb.WriteString(" [timed out]")
		}
		return textResult(sb.String())

	case "bridge_list_files":
		var entries []FileEntry
		if err := s.hub.Call(ctx, dev, "fs.list", map[string]any{"path": str("path")}, &entries); err != nil {
			return fail(err)
		}
		return jsonResult(entries)

	case "bridge_read_file":
		data, err := s.pullFile(ctx, dev, str("path"), 4<<20)
		if err != nil {
			return fail(err)
		}
		return textResult(string(data))

	case "bridge_write_file":
		err := s.hub.Call(ctx, dev, "fs.write", map[string]any{
			"path": str("path"), "offset": 0, "truncate": true, "data": b64enc([]byte(str("content"))),
		}, nil)
		if err != nil {
			return fail(err)
		}
		return textResult("wrote " + str("path"))

	case "bridge_transfer":
		data, err := s.pullFile(ctx, str("fromDevice"), str("fromPath"), 256<<20)
		if err != nil {
			return fail(err)
		}
		if err := s.hub.Call(ctx, str("toDevice"), "fs.write", map[string]any{
			"path": str("toPath"), "offset": 0, "truncate": true, "data": b64enc(data),
		}, nil); err != nil {
			return fail(err)
		}
		return textResult(fmt.Sprintf("copied %d bytes to %s", len(data), str("toPath")))

	case "bridge_screenshot":
		mw := intArg(args, "maxWidth")
		if mw == 0 {
			mw = 1280
		}
		var cap struct {
			Data string `json:"data"`
			W    int    `json:"w"`
			H    int    `json:"h"`
		}
		mon := intArg(args, "monitor")
		if err := s.hub.Call(ctx, dev, "screen.capture", map[string]any{"maxWidth": mw, "quality": 70, "monitor": mon}, &cap); err != nil {
			return fail(err)
		}
		note := fmt.Sprintf("screen %dx%d (monitor %d)", cap.W, cap.H, mon)
		var mons []Monitor
		if err := s.hub.Call(ctx, dev, "screen.monitors", nil, &mons); err == nil && len(mons) > 1 {
			note += fmt.Sprintf("; this device has %d displays — pass monitor=0..%d or -1 for all", len(mons), len(mons)-1)
		}
		return mcpResult(req.ID, map[string]any{"content": []map[string]any{
			{"type": "image", "data": cap.Data, "mimeType": "image/jpeg"},
			{"type": "text", "text": note},
		}})

	case "bridge_input":
		kind := str("kind")
		switch kind {
		case "key":
			err := s.hub.Call(ctx, dev, "input.key", map[string]any{"key": str("key"), "action": "press"}, nil)
			if err != nil {
				return fail(err)
			}
		case "type":
			err := s.hub.Call(ctx, dev, "input.text", map[string]any{"text": str("text")}, nil)
			if err != nil {
				return fail(err)
			}
		default:
			btn := str("button")
			if btn == "" {
				btn = "left"
			}
			err := s.hub.Call(ctx, dev, "input.mouse", map[string]any{
				"action": kind, "x": args["x"], "y": args["y"], "button": btn, "dy": intArg(args, "dy"),
				"monitor": intArg(args, "monitor"),
			}, nil)
			if err != nil {
				return fail(err)
			}
		}
		return textResult("ok")

	case "bridge_phone":
		phone := str("device")
		if phone == "" {
			for _, d := range s.hub.List() {
				if d.Kind == "android" && d.Online {
					phone = d.ID
					break
				}
			}
			if phone == "" {
				return fail(fmt.Errorf("no phone is online — open BridgeDesk on the phone and turn on Share this phone"))
			}
		}
		var method string
		params := map[string]any{"limit": intArg(args, "limit")}
		switch str("action") {
		case "sms_threads":
			method = "sms.threads"
		case "sms_read":
			method = "sms.messages"
			params["thread"] = intArg(args, "thread")
			params["address"] = str("to")
		case "sms_send":
			method, params = "sms.send", map[string]any{"to": str("to"), "text": str("text")}
		case "whatsapp_send":
			method, params = "whatsapp.send", map[string]any{"to": str("to"), "text": str("text")}
		case "call_dial":
			method, params = "call.dial", map[string]any{"number": str("to")}
		case "call_answer":
			method, params = "call.answer", nil
		case "call_end":
			method, params = "call.end", nil
		case "call_log":
			method = "call.log"
		default:
			return fail(fmt.Errorf("unknown action %q", str("action")))
		}
		var out json.RawMessage
		if err := s.hub.Call(ctx, phone, method, params, &out); err != nil {
			return fail(err)
		}
		return textResult(string(out))

	case "bridge_clipboard":
		if h, _ := args["history"].(bool); h {
			return jsonResult(s.clip.History())
		}
		if t, ok := args["text"].(string); ok && (dev == "all") {
			if err := s.clipTo(ctx, "all", t); err != nil {
				return fail(err)
			}
			return textResult("clipboard set on every device")
		}
		if t, ok := args["text"].(string); ok {
			if err := s.hub.Call(ctx, dev, "clipboard.set", map[string]any{"text": t}, nil); err != nil {
				return fail(err)
			}
			return textResult("clipboard set")
		}
		var out struct {
			Text string `json:"text"`
		}
		if err := s.hub.Call(ctx, dev, "clipboard.get", nil, &out); err != nil {
			return fail(err)
		}
		return textResult(out.Text)

	case "bridge_shares":
		var out []ShareInfo
		if err := s.hub.Call(ctx, dev, "share.list", nil, &out); err != nil {
			return fail(err)
		}
		return jsonResult(out)

	case "bridge_share_create":
		var out map[string]any
		ro, _ := args["readonly"].(bool)
		if err := s.hub.Call(ctx, dev, "share.create", map[string]any{"name": str("name"), "path": str("path"), "readonly": ro}, &out); err != nil {
			return fail(err)
		}
		return jsonResult(out)

	case "bridge_share_mount":
		var out map[string]any
		if err := s.hub.Call(ctx, dev, "share.mount", map[string]any{
			"source": str("source"), "mountAs": str("mountAs"), "user": str("user"), "pass": str("pass"),
		}, &out); err != nil {
			return fail(err)
		}
		return jsonResult(out)

	case "bridge_notifications":
		var out struct {
			Notifications json.RawMessage `json:"notifications"`
		}
		if err := s.hub.Call(ctx, dev, "notif.list", nil, &out); err != nil {
			return fail(err)
		}
		return textResult(string(out.Notifications))

	case "bridge_notification_history":
		limit := intArg(args, "limit")
		if limit <= 0 {
			limit = 30
		}
		f := NotifQuery{Q: str("query"), App: str("app"), Limit: limit}
		if c, _ := args["codes"].(bool); c {
			f.CodesOnly = true
		}
		if m := intArg(args, "minutes"); m > 0 {
			f.SinceMs = time.Now().Add(-time.Duration(m) * time.Minute).UnixMilli()
		}
		return jsonResult(s.notif.Query(s.cfg, f))

	case "bridge_latest_code":
		mins := intArg(args, "minutes")
		if mins <= 0 {
			mins = 10
		}
		wait := intArg(args, "wait")
		if wait > 120 {
			wait = 120
		}
		deadline := time.Now().Add(time.Duration(wait) * time.Second)
		for {
			f := NotifQuery{App: str("app"), CodesOnly: true, Limit: 1,
				SinceMs: time.Now().Add(-time.Duration(mins) * time.Minute).UnixMilli()}
			if list := s.notif.Query(s.cfg, f); len(list) > 0 {
				n := list[0]
				return jsonResult(map[string]any{"code": n.Code, "app": n.App, "title": n.Title, "text": n.Text,
					"device": n.Device, "ageSeconds": (time.Now().UnixMilli() - n.Received) / 1000})
			}
			if time.Now().After(deadline) {
				return textResult(fmt.Sprintf("no verification code in the last %d minutes", mins))
			}
			select {
			case <-ctx.Done():
				return fail(ctx.Err())
			case <-time.After(time.Second):
			}
		}

	case "bridge_notification_action":
		if dismiss, _ := args["dismiss"].(bool); dismiss {
			if err := s.hub.Call(ctx, dev, "notif.dismiss", map[string]any{"key": str("key")}, nil); err != nil {
				return fail(err)
			}
			return textResult("dismissed")
		}
		if open, _ := args["open"].(bool); open {
			if err := s.hub.Call(ctx, dev, "notif.open", map[string]any{"key": str("key")}, nil); err != nil {
				return fail(err)
			}
			return textResult("opened on phone")
		}
		action := 0
		if v, ok := args["action"]; ok {
			action = intArg(map[string]any{"a": v}, "a")
		}
		if reply, ok := args["reply"].(string); ok && reply != "" {
			if err := s.hub.Call(ctx, dev, "notif.reply", map[string]any{"key": str("key"), "action": action, "text": reply}, nil); err != nil {
				return fail(err)
			}
			return textResult("reply sent")
		}
		if err := s.hub.Call(ctx, dev, "notif.action", map[string]any{"key": str("key"), "action": action}, nil); err != nil {
			return fail(err)
		}
		return textResult("action sent")

	case "email_accounts":
		return jsonResult(s.emailAccountList())

	case "email_list":
		res, err := s.emailList(str("account"), str("query"), intArg(args, "limit"))
		if err != nil {
			return fail(err)
		}
		return jsonResult(res)

	case "email_read":
		res, err := s.emailRead(str("account"), str("id"))
		if err != nil {
			return fail(err)
		}
		return jsonResult(res)

	case "email_send":
		res, err := s.emailSend(str("account"), str("to"), str("subject"), str("body"), str("cc"))
		if err != nil {
			return fail(err)
		}
		return jsonResult(res)
	}
	return mcpError(req.ID, -32602, "unknown tool: "+call.Name)
}

func (s *Server) pullFile(ctx context.Context, device, path string, max int) ([]byte, error) {
	d, err := s.hub.Get(device)
	if err != nil {
		return nil, err
	}
	var buf []byte
	var offset int64
	for len(buf) < max {
		p, _ := json.Marshal(map[string]any{"path": path, "offset": offset, "length": 1 << 20})
		cctx, cancel := context.WithTimeout(ctx, 60*time.Second)
		raw, err := d.Call(cctx, "fs.read", p)
		cancel()
		if err != nil {
			return nil, err
		}
		var chunk struct {
			Data string `json:"data"`
			EOF  bool   `json:"eof"`
		}
		json.Unmarshal(raw, &chunk)
		b, _ := b64dec(chunk.Data)
		buf = append(buf, b...)
		offset += int64(len(b))
		if chunk.EOF || len(b) == 0 {
			break
		}
	}
	return buf, nil
}

func intArg(m map[string]any, k string) int {
	switch v := m[k].(type) {
	case float64:
		return int(v)
	case int:
		return v
	}
	return 0
}

func isLoopback(addr string) bool {
	return strings.HasPrefix(addr, "127.") || strings.HasPrefix(addr, "[::1]") || strings.HasPrefix(addr, "localhost")
}
