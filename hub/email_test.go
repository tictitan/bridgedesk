package main

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

// callTool invokes the MCP dispatch for one tool and returns the flattened text content.
func callTool(t *testing.T, s *Server, name string, args map[string]any) (string, bool) {
	t.Helper()
	ab, _ := json.Marshal(args)
	params, _ := json.Marshal(map[string]any{"name": name, "arguments": json.RawMessage(ab)})
	res := s.mcpCallTool(context.Background(), mcpReq{ID: json.RawMessage(`1`), Params: params})
	m, ok := res.(map[string]any)
	if !ok {
		t.Fatalf("unexpected result type %T", res)
	}
	isErr, _ := m["result"].(map[string]any)["isError"].(bool)
	var sb strings.Builder
	if r, ok := m["result"].(map[string]any); ok {
		if content, ok := r["content"].([]map[string]any); ok {
			for _, c := range content {
				sb.WriteString(c["text"].(string))
			}
		}
	}
	return sb.String(), isErr
}

func TestEmailToolsWiring(t *testing.T) {
	// No accounts configured: email_accounts returns an empty list; the others explain how to add one.
	s := &Server{cfg: &Config{}}

	if txt, isErr := callTool(t, s, "email_accounts", nil); isErr || strings.TrimSpace(txt) != "[]" {
		t.Errorf("email_accounts with no accounts should be empty list, got isErr=%v %q", isErr, txt)
	}

	txt, isErr := callTool(t, s, "email_list", map[string]any{"query": "is:unread"})
	if !isErr || !strings.Contains(strings.ToLower(txt), "no email accounts") {
		t.Errorf("email_list with no accounts should error helpfully, got isErr=%v %q", isErr, txt)
	}

	// Two accounts, none named -> ambiguity error (no network hit).
	s.cfg.EmailAccounts = []EmailAccount{
		{Provider: "gmail", Email: "me@gmail.com"},
		{Provider: "outlook", Email: "me@outlook.com"},
	}
	if list, _ := callTool(t, s, "email_accounts", nil); !strings.Contains(list, "me@gmail.com") || !strings.Contains(list, "me@outlook.com") {
		t.Errorf("email_accounts should list both, got %q", list)
	}
	if txt, isErr := callTool(t, s, "email_read", map[string]any{"id": "x"}); !isErr || !strings.Contains(txt, "several accounts") {
		t.Errorf("ambiguous account should error, got isErr=%v %q", isErr, txt)
	}
}
