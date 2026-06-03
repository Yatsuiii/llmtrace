// Package api exposes llmtrace's read endpoints for external integrations.
// The cost endpoint lets another tool join its own unit of work (for example a
// re_gent agent step or session) to the LLM cost llmtrace recorded for it.
package api

import (
	"encoding/json"
	"log"
	"net/http"
	"time"

	"github.com/Yatsuiii/llmtrace/internal/storage"
)

type costResponse struct {
	Session      string                `json:"session,omitempty"`
	From         string                `json:"from,omitempty"`
	To           string                `json:"to,omitempty"`
	Calls        int64                 `json:"calls"`
	InputTokens  int64                 `json:"input_tokens"`
	OutputTokens int64                 `json:"output_tokens"`
	CostUSD      float64               `json:"cost_usd"`
	FirstCall    string                `json:"first_call,omitempty"`
	LastCall     string                `json:"last_call,omitempty"`
	ByModel      []storage.CostByModel `json:"by_model"`
}

// CostHandler serves GET /api/cost. Query by ?session=<id> for an exact match,
// ?from=<RFC3339>&to=<RFC3339> for a time window (per-step attribution), or
// both. Returns aggregated call count, tokens, and USD cost, broken out by
// model. This is the read side of the re_gent integration.
func CostHandler(db *storage.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, "GET only", http.StatusMethodNotAllowed)
			return
		}
		q := r.URL.Query()
		session := q.Get("session")

		var from, to time.Time
		if v := q.Get("from"); v != "" {
			t, err := time.Parse(time.RFC3339, v)
			if err != nil {
				http.Error(w, "bad from (want RFC3339): "+err.Error(), http.StatusBadRequest)
				return
			}
			from = t
		}
		if v := q.Get("to"); v != "" {
			t, err := time.Parse(time.RFC3339, v)
			if err != nil {
				http.Error(w, "bad to (want RFC3339): "+err.Error(), http.StatusBadRequest)
				return
			}
			to = t
		}
		if session == "" && from.IsZero() && to.IsZero() {
			http.Error(w, "provide ?session=<id> and/or ?from=<RFC3339>&to=<RFC3339>", http.StatusBadRequest)
			return
		}

		res, err := db.CostFor(r.Context(), session, from, to)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}

		out := costResponse{
			Session:      session,
			Calls:        res.Calls,
			InputTokens:  res.InputTokens,
			OutputTokens: res.OutputTokens,
			CostUSD:      res.CostUSD,
			ByModel:      res.ByModel,
		}
		if !from.IsZero() {
			out.From = from.UTC().Format(time.RFC3339)
		}
		if !to.IsZero() {
			out.To = to.UTC().Format(time.RFC3339)
		}
		if !res.FirstCall.IsZero() {
			out.FirstCall = res.FirstCall.Format(time.RFC3339)
			out.LastCall = res.LastCall.Format(time.RFC3339)
		}
		if out.ByModel == nil {
			out.ByModel = []storage.CostByModel{}
		}

		w.Header().Set("Content-Type", "application/json")
		if err := json.NewEncoder(w).Encode(out); err != nil {
			log.Printf("api: encode cost response: %v", err)
		}
	}
}
