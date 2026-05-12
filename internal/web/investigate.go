package web

import (
	"fmt"
	"net/http"
	"os"
	"strconv"
	"time"

	"github.com/Yatsuiii/llmtrace/internal/agent"
	"github.com/Yatsuiii/llmtrace/internal/storage"
)

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

		apiKey := os.Getenv("GEMINI_API_KEY")
		if apiKey == "" {
			http.Error(w, "GEMINI_API_KEY not set", http.StatusServiceUnavailable)
			return
		}
		model := os.Getenv("GEMINI_MODEL")

		inv, err := agent.New(db, apiKey, model)
		if err != nil {
			http.Error(w, fmt.Sprintf("init agent: %v", err), http.StatusInternalServerError)
			return
		}

		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("X-Accel-Buffering", "no")
		flusher := w.(http.Flusher)

		emit := func(msg string) {
			fmt.Fprintf(w, "data: %s\n\n", msg)
			flusher.Flush()
		}

		if err := inv.Investigate(ctx, *target, emit); err != nil {
			emit(fmt.Sprintf("[error] %v", err))
		}
	}
}
