package main

// models.go — a small read-only view of the PC's local Ollama models, surfaced to the Aria phone
// dashboard. The hub just proxies Ollama's loopback API (127.0.0.1:11434); if Ollama isn't
// running, it reports available:false rather than erroring.

import (
	"encoding/json"
	"net/http"
	"time"
)

const ollamaBase = "http://127.0.0.1:11434"

func (s *Server) handleModels(w http.ResponseWriter, r *http.Request) {
	client := &http.Client{Timeout: 2 * time.Second}

	var tags struct {
		Models []struct {
			Name     string `json:"name"`
			Size     int64  `json:"size"`
			Details  struct {
				Family        string `json:"family"`
				ParameterSize string `json:"parameter_size"`
			} `json:"details"`
		} `json:"models"`
	}
	resp, err := client.Get(ollamaBase + "/api/tags")
	if err != nil {
		writeJSON(w, 200, map[string]any{"available": false, "models": []any{}, "running": []any{}})
		return
	}
	json.NewDecoder(resp.Body).Decode(&tags)
	resp.Body.Close()

	var ps struct {
		Models []struct {
			Name      string `json:"name"`
			SizeVRAM  int64  `json:"size_vram"`
			ExpiresAt string `json:"expires_at"`
		} `json:"models"`
	}
	if r2, err := client.Get(ollamaBase + "/api/ps"); err == nil {
		json.NewDecoder(r2.Body).Decode(&ps)
		r2.Body.Close()
	}
	runningNames := map[string]bool{}
	running := []map[string]any{}
	for _, m := range ps.Models {
		runningNames[m.Name] = true
		running = append(running, map[string]any{"name": m.Name, "vram": m.SizeVRAM})
	}

	models := []map[string]any{}
	for _, m := range tags.Models {
		models = append(models, map[string]any{
			"name": m.Name, "size": m.Size,
			"params": m.Details.ParameterSize, "family": m.Details.Family,
			"running": runningNames[m.Name],
		})
	}
	writeJSON(w, 200, map[string]any{"available": true, "models": models, "running": running})
}
