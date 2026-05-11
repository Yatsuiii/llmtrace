// Package seed plants the demo scenario into the ledger so the agent
// has something to investigate without a live proxy intercepting traffic.
//
// Scenario locked for the May 2026 hackathon demo:
//
//	Window: 2026-04-11 → 2026-05-11 (30 days).
//	Keys:   prod-frontend (affected), internal-tools (control), background-jobs (control).
//	Deploy: 2026-05-03 14:05 UTC — PR #129 "switch summary endpoint to claude-sonnet".
//	Pre-deploy:  /summary calls on prod-frontend run 90% Haiku / 10% Sonnet.
//	Post-deploy: 10% Haiku / 90% Sonnet AND volume rises 1.6× (the new prompt
//	             retries on malformed JSON).
//	Effect:      prod-frontend daily cost ≈ $5.60 → $23.60 (~4.2× spike).
//	Controls:    internal-tools and background-jobs stay flat across the window.
package seed

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"math/rand/v2"
	"time"

	"github.com/Yatsuiii/llmtrace/internal/pricing"
	"github.com/Yatsuiii/llmtrace/internal/storage"
)

const (
	scenarioDays        = 30
	demoSeed1    uint64 = 0x6c6c6d74 // "llmt"
	demoSeed2    uint64 = 0x72616365 // "race"

	modelHaiku  = "claude-haiku-4-5-20251001"
	modelSonnet = "claude-sonnet-4-6"

	endpointMessages = "/v1/messages"
	providerAnth     = "anthropic"
)

var (
	scenarioStart = time.Date(2026, 4, 11, 0, 0, 0, 0, time.UTC)
	deployAt      = time.Date(2026, 5, 3, 14, 5, 0, 0, time.UTC)

	fpSummary = fp("summary-endpoint-v1")
	fpChat    = fp("chat-endpoint-v1")
	fpAgent   = fp("agent-tool-call-v1")
)

func fp(s string) string {
	h := sha256.Sum256([]byte(s))
	return hex.EncodeToString(h[:])[:12]
}

func Run(ctx context.Context, db *storage.DB) (int, error) {
	if err := db.Wipe(ctx); err != nil {
		return 0, err
	}
	if err := seedAPIKeys(ctx, db); err != nil {
		return 0, err
	}
	if err := seedDeploy(ctx, db); err != nil {
		return 0, err
	}
	return seedCalls(ctx, db)
}

func seedAPIKeys(ctx context.Context, db *storage.DB) error {
	keys := []storage.APIKeyRow{
		{ID: "prod-frontend", HashedKey: "demo", Label: "Production frontend", BudgetUSD: 500, Active: true, CreatedAt: scenarioStart},
		{ID: "internal-tools", HashedKey: "demo", Label: "Internal tooling", BudgetUSD: 200, Active: true, CreatedAt: scenarioStart},
		{ID: "background-jobs", HashedKey: "demo", Label: "Background jobs", BudgetUSD: 300, Active: true, CreatedAt: scenarioStart},
	}
	for _, k := range keys {
		if err := db.InsertAPIKey(ctx, k); err != nil {
			return err
		}
	}
	return nil
}

func seedDeploy(ctx context.Context, db *storage.DB) error {
	return db.InsertDeploy(ctx, storage.DeployRow{
		ID:          "gha-129-summary-sonnet",
		Repo:        "acme/web",
		Branch:      "main",
		CommitSHA:   "c4e2117a8b1f",
		PRNumber:    129,
		Title:       "switch summary endpoint to claude-sonnet",
		StartedAt:   deployAt,
		CompletedAt: deployAt.Add(7 * time.Minute),
		Status:      "success",
	})
}

func seedCalls(ctx context.Context, db *storage.DB) (int, error) {
	rng := rand.New(rand.NewPCG(demoSeed1, demoSeed2))
	var rows []storage.CallRow
	for day := 0; day < scenarioDays; day++ {
		dayStart := scenarioStart.Add(time.Duration(day) * 24 * time.Hour)
		rows = append(rows, summaryCalls(rng, dayStart)...)
		rows = append(rows, chatCalls(rng, dayStart)...)
		rows = append(rows, agentCalls(rng, dayStart)...)
	}
	if err := db.InsertCalls(ctx, rows); err != nil {
		return 0, fmt.Errorf("insert calls: %w", err)
	}
	return len(rows), nil
}

// summaryCalls — the affected traffic. Pre-deploy ≈ 500 calls/day on Haiku.
// Post-deploy ≈ 800 calls/day on Sonnet (60% volume bump from retry loop).
func summaryCalls(rng *rand.Rand, dayStart time.Time) []storage.CallRow {
	post := !dayStart.Before(deployAt.Truncate(24 * time.Hour))
	base := 500
	if post {
		base = 800
	}
	count := base + rng.IntN(80) - 40 // ±40 jitter
	out := make([]storage.CallRow, 0, count)
	for i := 0; i < count; i++ {
		ts := dayStart.Add(time.Duration(rng.IntN(86400)) * time.Second)
		// On the deploy day, only calls after deployAt take the new path.
		isPostForCall := ts.After(deployAt)
		model := modelHaiku
		// Even post-deploy, 10% of traffic lingers on the old model (canary residue).
		if isPostForCall {
			if rng.Float64() < 0.9 {
				model = modelSonnet
			}
		} else if rng.Float64() < 0.1 {
			model = modelSonnet
		}
		inTok := 4800 + rng.IntN(400)
		outTok := 750 + rng.IntN(100)
		latency := int64(900 + rng.IntN(400))
		if model == modelSonnet {
			latency = int64(1500 + rng.IntN(600))
		}
		out = append(out, storage.CallRow{
			Timestamp:    ts,
			APIKeyID:     "prod-frontend",
			Provider:     providerAnth,
			Model:        model,
			Endpoint:     endpointMessages,
			InputTokens:  inTok,
			OutputTokens: outTok,
			CostUSD:      pricing.Cost(model, inTok, outTok),
			LatencyMs:    latency,
			Status:       200,
			PromptHash:   fpSummary,
			SessionID:    fmt.Sprintf("sess-%d", rng.IntN(50_000)),
		})
	}
	return out
}

// chatCalls — control traffic on internal-tools that doesn't move across deploy.
func chatCalls(rng *rand.Rand, dayStart time.Time) []storage.CallRow {
	count := 220 + rng.IntN(40) - 20
	out := make([]storage.CallRow, 0, count)
	for i := 0; i < count; i++ {
		ts := dayStart.Add(time.Duration(rng.IntN(86400)) * time.Second)
		inTok := 1200 + rng.IntN(300)
		outTok := 280 + rng.IntN(80)
		out = append(out, storage.CallRow{
			Timestamp:    ts,
			APIKeyID:     "internal-tools",
			Provider:     providerAnth,
			Model:        modelSonnet,
			Endpoint:     endpointMessages,
			InputTokens:  inTok,
			OutputTokens: outTok,
			CostUSD:      pricing.Cost(modelSonnet, inTok, outTok),
			LatencyMs:    int64(800 + rng.IntN(300)),
			Status:       200,
			PromptHash:   fpChat,
			SessionID:    fmt.Sprintf("chat-%d", rng.IntN(10_000)),
		})
	}
	return out
}

// agentCalls — control traffic for background-jobs.
func agentCalls(rng *rand.Rand, dayStart time.Time) []storage.CallRow {
	count := 140 + rng.IntN(30) - 15
	out := make([]storage.CallRow, 0, count)
	for i := 0; i < count; i++ {
		ts := dayStart.Add(time.Duration(rng.IntN(86400)) * time.Second)
		inTok := 2800 + rng.IntN(500)
		outTok := 420 + rng.IntN(120)
		out = append(out, storage.CallRow{
			Timestamp:    ts,
			APIKeyID:     "background-jobs",
			Provider:     providerAnth,
			Model:        modelSonnet,
			Endpoint:     endpointMessages,
			InputTokens:  inTok,
			OutputTokens: outTok,
			CostUSD:      pricing.Cost(modelSonnet, inTok, outTok),
			LatencyMs:    int64(1100 + rng.IntN(400)),
			Status:       200,
			PromptHash:   fpAgent,
			SessionID:    fmt.Sprintf("job-%d", rng.IntN(5_000)),
		})
	}
	return out
}
