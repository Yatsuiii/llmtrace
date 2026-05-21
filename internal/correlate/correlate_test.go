package correlate

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/Yatsuiii/llmtrace/internal/detect"
	"github.com/Yatsuiii/llmtrace/internal/seed"
	"github.com/Yatsuiii/llmtrace/internal/storage"
)

// TestRunIsolatesCausingDeploy verifies the correlator attributes the spend
// anomaly to the deploy that actually flipped the model (the seeded scenario's
// PR #2), and not to the two innocent same-day deploys around it.
func TestRunIsolatesCausingDeploy(t *testing.T) {
	ctx := context.Background()
	db, err := storage.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	defer db.Close()

	if _, err := seed.Run(ctx, db); err != nil {
		t.Fatalf("seed: %v", err)
	}

	since := time.Date(2026, 4, 1, 0, 0, 0, 0, time.UTC)
	if _, err := detect.Run(ctx, db, detect.DefaultConfig(), since); err != nil {
		t.Fatalf("detect: %v", err)
	}
	results, err := Run(ctx, db, DefaultConfig(), since)
	if err != nil {
		t.Fatalf("correlate: %v", err)
	}
	if len(results) == 0 {
		t.Fatal("expected correlations, got none")
	}

	// Results are sorted by anomaly then descending confidence, so the first
	// entry is the strongest candidate for the (single) seeded anomaly.
	top := results[0]
	if top.Deploy.PRNumber != 2 {
		t.Errorf("top correlation = PR #%d (%q), want PR #2", top.Deploy.PRNumber, top.Deploy.Title)
	}
	if top.Confidence <= wTimeWindow {
		t.Errorf("top confidence = %.2f, want > %.2f (model_change should fire)", top.Confidence, wTimeWindow)
	}
	if !hasEvidence(top.Evidence, "model_change") {
		t.Errorf("top correlation missing model_change evidence: %+v", top.Evidence)
	}

	// Every other candidate for the same anomaly must score below the cause:
	// the innocent deploys should carry time-proximity only.
	for _, r := range results[1:] {
		if r.AnomalyID != top.AnomalyID {
			continue
		}
		if r.Confidence >= top.Confidence {
			t.Errorf("innocent deploy PR #%d scored %.2f, not below cause %.2f", r.Deploy.PRNumber, r.Confidence, top.Confidence)
		}
	}
}

func hasEvidence(ev []Evidence, kind string) bool {
	for _, e := range ev {
		if e.Kind == kind {
			return true
		}
	}
	return false
}
