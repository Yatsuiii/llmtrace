// Package ingest exposes HTTP endpoints that let external services push call
// records and deploy events directly into the ledger. This is how the
// traffic-gen instance on Instance 2 feeds data to Instance 1.
package ingest

import (
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"github.com/Yatsuiii/llmtrace/internal/storage"
)

type callPayload struct {
	APIKeyID     string  `json:"api_key_id"`
	Provider     string  `json:"provider"`
	Model        string  `json:"model"`
	Endpoint     string  `json:"endpoint"`
	InputTokens  int     `json:"input_tokens"`
	OutputTokens int     `json:"output_tokens"`
	CostUSD      float64 `json:"cost_usd"`
	LatencyMs    int64   `json:"latency_ms"`
	Status       int     `json:"status"`
	PromptHash   string  `json:"prompt_hash"`
}

type deployPayload struct {
	ID       string `json:"id"`
	PRNumber int    `json:"pr_number"`
	Title    string `json:"title"`
	Repo     string `json:"repo"`
	Branch   string `json:"branch"`
}

func CallHandler(db *storage.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "POST only", http.StatusMethodNotAllowed)
			return
		}
		var p callPayload
		if err := json.NewDecoder(r.Body).Decode(&p); err != nil {
			http.Error(w, "bad json: "+err.Error(), http.StatusBadRequest)
			return
		}
		if p.Provider == "" {
			p.Provider = "anthropic"
		}
		if p.Endpoint == "" {
			p.Endpoint = "/v1/messages"
		}
		if p.Status == 0 {
			p.Status = 200
		}
		row := storage.CallRow{
			Timestamp:    time.Now().UTC(),
			APIKeyID:     p.APIKeyID,
			Provider:     p.Provider,
			Model:        p.Model,
			Endpoint:     p.Endpoint,
			InputTokens:  p.InputTokens,
			OutputTokens: p.OutputTokens,
			CostUSD:      p.CostUSD,
			LatencyMs:    p.LatencyMs,
			Status:       p.Status,
			PromptHash:   p.PromptHash,
		}
		if err := db.InsertCalls(r.Context(), []storage.CallRow{row}); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusCreated)
		fmt.Fprintln(w, `{"ok":true}`)
	}
}

func DeployHandler(db *storage.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "POST only", http.StatusMethodNotAllowed)
			return
		}
		var p deployPayload
		if err := json.NewDecoder(r.Body).Decode(&p); err != nil {
			http.Error(w, "bad json: "+err.Error(), http.StatusBadRequest)
			return
		}
		if p.ID == "" {
			p.ID = fmt.Sprintf("deploy-%d", time.Now().UnixMilli())
		}
		if p.Repo == "" {
			p.Repo = "org/app"
		}
		if p.Branch == "" {
			p.Branch = "main"
		}
		now := time.Now().UTC()
		row := storage.DeployRow{
			ID:          p.ID,
			Repo:        p.Repo,
			Branch:      p.Branch,
			CommitSHA:   fmt.Sprintf("%x", now.UnixNano())[:12],
			PRNumber:    p.PRNumber,
			Title:       p.Title,
			StartedAt:   now.Add(-2 * time.Minute),
			CompletedAt: now,
			Status:      "success",
		}
		if err := db.InsertDeploy(r.Context(), row); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusCreated)
		fmt.Fprintf(w, `{"ok":true,"deploy_id":%q}`+"\n", row.ID)
	}
}
