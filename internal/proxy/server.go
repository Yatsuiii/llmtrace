// Package proxy is a reverse proxy for the Anthropic Messages API. It forwards
// requests upstream untouched and records real token usage, cost, and latency
// for every call into the ledger — this is how llmtrace gets real telemetry.
package proxy

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/Yatsuiii/llmtrace/internal/pricing"
	"github.com/Yatsuiii/llmtrace/internal/storage"
)

const anthropicUpstream = "https://api.anthropic.com/v1/messages"

// Handler returns the proxy endpoint for POST /v1/messages. Callers send a
// normal Anthropic request; the optional X-Llmtrace-Key header tags the call
// with an inbound API key ID (defaults to "prod-frontend"), and the optional
// X-Llmtrace-Session header tags it with a caller-defined session id so the
// call can later be joined back to a unit of work (e.g. a re_gent agent step).
func Handler(db *storage.DB) http.HandlerFunc {
	upstreamKey := os.Getenv("ANTHROPIC_API_KEY")
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "POST only", http.StatusMethodNotAllowed)
			return
		}
		if upstreamKey == "" {
			http.Error(w, "ANTHROPIC_API_KEY not configured on llmtrace", http.StatusServiceUnavailable)
			return
		}
		reqBody, err := io.ReadAll(r.Body)
		if err != nil {
			http.Error(w, "read body: "+err.Error(), http.StatusBadRequest)
			return
		}

		apiKeyID := r.Header.Get("X-Llmtrace-Key")
		if apiKeyID == "" {
			apiKeyID = "prod-frontend"
		}
		sessionID := r.Header.Get("X-Llmtrace-Session")

		upstream, err := http.NewRequestWithContext(r.Context(), http.MethodPost, anthropicUpstream, bytes.NewReader(reqBody))
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		upstream.Header.Set("x-api-key", upstreamKey)
		upstream.Header.Set("anthropic-version", "2023-06-01")
		upstream.Header.Set("content-type", "application/json")

		start := time.Now()
		resp, err := http.DefaultClient.Do(upstream)
		if err != nil {
			http.Error(w, "upstream: "+err.Error(), http.StatusBadGateway)
			return
		}
		defer resp.Body.Close()
		respBody, err := io.ReadAll(resp.Body)
		if err != nil {
			http.Error(w, "read upstream response: "+err.Error(), http.StatusBadGateway)
			return
		}
		latency := time.Since(start)

		// Return the upstream response unchanged.
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(resp.StatusCode)
		w.Write(respBody)

		recordCall(r.Context(), db, apiKeyID, sessionID, reqBody, respBody, resp.StatusCode, latency)
	}
}

func recordCall(ctx context.Context, db *storage.DB, apiKeyID, sessionID string, reqBody, respBody []byte, status int, latency time.Duration) {
	var parsed struct {
		Model string `json:"model"`
		Usage struct {
			InputTokens  int `json:"input_tokens"`
			OutputTokens int `json:"output_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(respBody, &parsed); err != nil {
		log.Printf("proxy: parse response body: %v", err)
	}

	model := parsed.Model
	if model == "" {
		var req struct {
			Model string `json:"model"`
		}
		if err := json.Unmarshal(reqBody, &req); err != nil {
			log.Printf("proxy: parse request body: %v", err)
		}
		model = req.Model
	}
	in, out := parsed.Usage.InputTokens, parsed.Usage.OutputTokens
	row := storage.CallRow{
		Timestamp:    time.Now().UTC(),
		APIKeyID:     apiKeyID,
		Provider:     "anthropic",
		Model:        model,
		Endpoint:     "/v1/messages",
		InputTokens:  in,
		OutputTokens: out,
		CostUSD:      pricing.Cost(model, in, out),
		LatencyMs:    latency.Milliseconds(),
		Status:       status,
		PromptHash:   promptFingerprint(reqBody),
		SessionID:    sessionID,
	}
	if err := db.InsertCalls(ctx, []storage.CallRow{row}); err != nil {
		log.Printf("proxy: insert call record: %v", err)
	}
}

// promptFingerprint derives a stable hash from the model, system prompt, and
// first user message — the same call pattern produces the same fingerprint.
func promptFingerprint(reqBody []byte) string {
	var req struct {
		Model    string `json:"model"`
		System   any    `json:"system"`
		Messages []struct {
			Role    string `json:"role"`
			Content any    `json:"content"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(reqBody, &req); err != nil {
		log.Printf("proxy: parse request for fingerprint: %v", err)
	}

	var sb strings.Builder
	sb.WriteString(req.Model)
	if s, ok := req.System.(string); ok {
		sb.WriteString(s)
	}
	if len(req.Messages) > 0 {
		if c, ok := req.Messages[0].Content.(string); ok {
			sb.WriteString(c)
		}
	}
	h := sha256.Sum256([]byte(sb.String()))
	return hex.EncodeToString(h[:])[:12]
}
