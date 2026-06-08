// Package proxy is a provider-agnostic reverse proxy. It forwards requests to
// an upstream LLM provider, records token usage, cost, and latency into the
// ledger, and returns the upstream response unchanged. The optional
// X-Llmtrace-Key header tags the call with an inbound API key ID (defaults to
// "prod-frontend"); X-Llmtrace-Session tags it with a caller-defined session id.
package proxy

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/Yatsuiii/llmtrace/internal/pricing"
	"github.com/Yatsuiii/llmtrace/internal/providers"
	"github.com/Yatsuiii/llmtrace/internal/storage"
)

// Handler returns an http.HandlerFunc that proxies the given provider. upstreamKey
// is the plaintext API key to forward; the handler returns 503 if it is empty.
func Handler(db *storage.DB, p providers.Provider, upstreamKey string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "POST only", http.StatusMethodNotAllowed)
			return
		}
		if upstreamKey == "" {
			http.Error(w, fmt.Sprintf("%s API key not configured on llmtrace", p.Name()), http.StatusServiceUnavailable)
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

		upstreamURL := p.UpstreamURL() + r.URL.Path
		if r.URL.RawQuery != "" {
			upstreamURL += "?" + r.URL.RawQuery
		}
		upstream, err := http.NewRequestWithContext(r.Context(), http.MethodPost, upstreamURL, bytes.NewReader(reqBody))
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		p.SetAuth(upstream, upstreamKey)
		upstream.Header.Set("content-type", "application/json")
		// Pass through provider-specific version headers from the caller.
		if v := r.Header.Get("anthropic-version"); v != "" {
			upstream.Header.Set("anthropic-version", v)
		} else if p.Name() == "anthropic" {
			upstream.Header.Set("anthropic-version", "2023-06-01")
		}

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

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(resp.StatusCode)
		w.Write(respBody)

		go recordCall(context.Background(), db, p, apiKeyID, sessionID, r.URL.Path, reqBody, respBody, resp.StatusCode, latency)
	}
}

func recordCall(ctx context.Context, db *storage.DB, p providers.Provider, apiKeyID, sessionID, endpoint string, reqBody, respBody []byte, status int, latency time.Duration) {
	usage, err := p.ParseUsage(nil, respBody)
	if err != nil {
		log.Printf("proxy: parse usage (%s): %v", p.Name(), err)
	}
	// Fall back to request body for model name if response didn't include it.
	if usage.Model == "" {
		var req struct {
			Model string `json:"model"`
		}
		if err := json.Unmarshal(reqBody, &req); err == nil {
			usage.Model = req.Model
		}
	}

	row := storage.CallRow{
		Timestamp:    time.Now().UTC(),
		APIKeyID:     apiKeyID,
		Provider:     p.Name(),
		Model:        usage.Model,
		Endpoint:     endpoint,
		InputTokens:  usage.InputTokens,
		OutputTokens: usage.OutputTokens,
		CostUSD:      pricing.Cost(usage.Model, usage.InputTokens, usage.OutputTokens),
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
// first user message — same call pattern produces the same fingerprint regardless
// of provider.
func promptFingerprint(reqBody []byte) string {
	// Handles both Anthropic (messages[].content string) and OpenAI (messages[].content string).
	var req struct {
		Model    string `json:"model"`
		System   any    `json:"system"`
		Messages []struct {
			Role    string `json:"role"`
			Content any    `json:"content"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(reqBody, &req); err != nil {
		return ""
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
