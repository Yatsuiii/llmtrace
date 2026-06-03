package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/Yatsuiii/llmtrace/internal/correlate"
	"github.com/Yatsuiii/llmtrace/internal/seed"
	"github.com/Yatsuiii/llmtrace/internal/storage"
)

// TestAttributionHandler verifies the SHA join: the commit that caused the
// seeded spend anomaly carries a model_change attribution, an innocent same-day
// deploy scores strictly below it, and an unknown commit is a clean miss.
func TestAttributionHandler(t *testing.T) {
	ctx := context.Background()
	db, err := storage.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer db.Close()
	if _, err := seed.Run(ctx, db); err != nil {
		t.Fatalf("seed: %v", err)
	}

	h := AttributionHandler(db)
	get := func(query string) attributionResponse {
		t.Helper()
		rec := httptest.NewRecorder()
		h(rec, httptest.NewRequest(http.MethodGet, "/api/attribution?"+query, nil))
		if rec.Code != http.StatusOK {
			t.Fatalf("%q -> status %d, want 200", query, rec.Code)
		}
		var resp attributionResponse
		if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
			t.Fatalf("decode %q: %v", query, err)
		}
		return resp
	}

	// PR #2 (the sonnet switch) caused the anomaly; a short SHA prefix must match.
	cause := get("sha=c4e2117")
	if !cause.Matched || cause.Deploy == nil || cause.Deploy.PRNumber != 2 {
		t.Fatalf("cause: matched=%v deploy=%+v, want PR #2", cause.Matched, cause.Deploy)
	}
	if cause.Attribution == nil {
		t.Fatal("cause: no attribution, want the spend anomaly")
	}
	if !hasEvidence(cause.Attribution.Evidence, "model_change") {
		t.Errorf("cause: missing model_change evidence: %+v", cause.Attribution.Evidence)
	}

	// An innocent same-day deploy matches but must score below the cause.
	innocent := get("sha=a1b2c3d4")
	if !innocent.Matched {
		t.Fatal("innocent: want matched")
	}
	innocentConf := 0.0
	if innocent.Attribution != nil {
		innocentConf = innocent.Attribution.Confidence
	}
	if innocentConf >= cause.Attribution.Confidence {
		t.Errorf("innocent confidence %.2f not below cause %.2f", innocentConf, cause.Attribution.Confidence)
	}

	// An unknown commit is a clean miss.
	if miss := get("sha=deadbeefdead"); miss.Matched {
		t.Errorf("unknown sha matched: %+v", miss)
	}

	// Missing sha is a 400.
	rec := httptest.NewRecorder()
	h(rec, httptest.NewRequest(http.MethodGet, "/api/attribution", nil))
	if rec.Code != http.StatusBadRequest {
		t.Errorf("no sha -> status %d, want 400", rec.Code)
	}
}

func hasEvidence(ev []correlate.Evidence, kind string) bool {
	for _, e := range ev {
		if e.Kind == kind {
			return true
		}
	}
	return false
}
