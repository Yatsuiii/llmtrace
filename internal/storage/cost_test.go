package storage

import (
	"context"
	"path/filepath"
	"testing"
	"time"
)

// TestCostFor verifies the cost aggregation filters by session id, by time
// window, and by both together — the three query shapes the /api/cost endpoint
// (and the re_gent integration) relies on.
func TestCostFor(t *testing.T) {
	ctx := context.Background()
	db, err := Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	defer db.Close()

	base := time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC)
	mk := func(session, model string, at time.Time, cost float64) CallRow {
		return CallRow{
			Timestamp: at, APIKeyID: "k", Provider: "anthropic", Model: model,
			Endpoint: "/v1/messages", InputTokens: 100, OutputTokens: 10,
			CostUSD: cost, LatencyMs: 100, Status: 200, SessionID: session,
		}
	}
	calls := []CallRow{
		mk("sess-a", "claude-sonnet-4-6", base, 0.30),
		mk("sess-a", "claude-haiku-4-5", base.Add(time.Minute), 0.05),
		mk("sess-b", "claude-sonnet-4-6", base.Add(time.Hour), 0.30),
	}
	if err := db.InsertCalls(ctx, calls); err != nil {
		t.Fatalf("insert: %v", err)
	}

	// By session: sess-a is two calls totalling $0.35 across two models.
	got, err := db.CostFor(ctx, "sess-a", time.Time{}, time.Time{})
	if err != nil {
		t.Fatalf("cost by session: %v", err)
	}
	if got.Calls != 2 {
		t.Errorf("sess-a calls = %d, want 2", got.Calls)
	}
	if !approx(got.CostUSD, 0.35) {
		t.Errorf("sess-a cost = %.4f, want 0.35", got.CostUSD)
	}
	if len(got.ByModel) != 2 {
		t.Fatalf("sess-a by_model = %d models, want 2", len(got.ByModel))
	}
	if got.ByModel[0].Model != "claude-sonnet-4-6" {
		t.Errorf("sess-a top model = %q, want sonnet (sorted by cost desc)", got.ByModel[0].Model)
	}

	// By window: only the first two calls fall inside a 5-minute window.
	got, err = db.CostFor(ctx, "", base.Add(-time.Minute), base.Add(5*time.Minute))
	if err != nil {
		t.Fatalf("cost by window: %v", err)
	}
	if got.Calls != 2 || !approx(got.CostUSD, 0.35) {
		t.Errorf("window = %d calls $%.4f, want 2 calls $0.35", got.Calls, got.CostUSD)
	}

	// Session and window combined: sess-b sits outside the early window.
	got, err = db.CostFor(ctx, "sess-b", base.Add(-time.Minute), base.Add(5*time.Minute))
	if err != nil {
		t.Fatalf("cost session+window: %v", err)
	}
	if got.Calls != 0 {
		t.Errorf("sess-b in early window = %d calls, want 0", got.Calls)
	}
}

func approx(a, b float64) bool {
	d := a - b
	if d < 0 {
		d = -d
	}
	return d < 1e-9
}
