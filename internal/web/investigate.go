package web

import (
	"fmt"
	"net/http"
	"strconv"
	"time"

	"github.com/Yatsuiii/llmtrace/internal/storage"
)

// investigateHandler streams a placeholder agent investigation for a given
// anomaly ID. The real Gemini agent loop replaces the placeholder on Day 3.
func investigateHandler(db *storage.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		idStr := r.URL.Query().Get("id")
		id, err := strconv.ParseInt(idStr, 10, 64)
		if err != nil {
			http.Error(w, "bad id", http.StatusBadRequest)
			return
		}

		ctx := r.Context()
		since := time.Now().UTC().AddDate(0, 0, -30)
		anomalies, err := db.ListAnomalies(ctx, since)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		var target *storage.AnomalyRow
		for i := range anomalies {
			if anomalies[i].ID == id {
				target = &anomalies[i]
				break
			}
		}
		if target == nil {
			http.Error(w, "anomaly not found", http.StatusNotFound)
			return
		}

		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("X-Accel-Buffering", "no")
		flusher := w.(http.Flusher)

		emit := func(msg string) {
			fmt.Fprintf(w, "data: %s\n\n", msg)
			flusher.Flush()
			time.Sleep(60 * time.Millisecond)
		}

		emit(fmt.Sprintf("🔍 Investigating anomaly #%d for key '%s' on %s", id, target.APIKeyID, target.Date))
		emit(fmt.Sprintf("   actual: $%.2f  baseline: $%.2f  delta: +$%.2f  (%.1fσ)", target.ActualValue, target.BaselineValue, target.Delta, target.Sigma))
		emit("")
		emit("⚙  [agent] querying ledger for model distribution around anomaly date…")
		emit("   [placeholder — Gemini agent loop wires in on Day 3]")
		emit("")
		emit("📋 Attribution: agent not yet connected.")
		emit("   Run: go run ./cmd/llmtrace serve  after Day 3 to see full reasoning.")
	}
}
