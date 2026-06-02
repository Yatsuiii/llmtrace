package web

import (
	"fmt"
	"io"
	"net/http"
	"os"

	"github.com/Yatsuiii/llmtrace/internal/agent"
	"github.com/Yatsuiii/llmtrace/internal/storage"
)

// visionHandler accepts a billing-dashboard screenshot, has Gemini read the
// spend off it, then runs a GitHub investigation of the spike it found.
func visionHandler(db *storage.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "POST only", http.StatusMethodNotAllowed)
			return
		}
		apiKey := os.Getenv("GEMINI_API_KEY")
		if apiKey == "" && os.Getenv("GOOGLE_CLOUD_PROJECT") == "" {
			http.Error(w, "neither GEMINI_API_KEY nor GOOGLE_CLOUD_PROJECT set", http.StatusServiceUnavailable)
			return
		}
		if err := r.ParseMultipartForm(12 << 20); err != nil {
			http.Error(w, "bad upload: "+err.Error(), http.StatusBadRequest)
			return
		}
		file, header, err := r.FormFile("image")
		if err != nil {
			http.Error(w, "missing image: "+err.Error(), http.StatusBadRequest)
			return
		}
		defer file.Close()
		imgBytes, err := io.ReadAll(file)
		if err != nil {
			http.Error(w, "read image: "+err.Error(), http.StatusBadRequest)
			return
		}
		mime := header.Header.Get("Content-Type")
		if mime == "" {
			mime = "image/png"
		}

		inv, err := agent.New(db, apiKey, os.Getenv("GEMINI_MODEL"))
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}

		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("X-Accel-Buffering", "no")
		flusher, ok := w.(http.Flusher)
		if !ok {
			http.Error(w, "streaming not supported", http.StatusInternalServerError)
			return
		}
		emit := func(msg string) {
			fmt.Fprintf(w, "data: %s\n\n", msg)
			flusher.Flush()
		}

		ctx := r.Context()
		report, err := inv.ExtractSpend(ctx, imgBytes, mime, emit)
		if err != nil {
			emit(fmt.Sprintf("[error] %v", err))
			emit("[[END]]")
			return
		}

		emit("")
		emit("── Gemini read your dashboard " + repeat("─", 36))
		for _, d := range report.Days {
			emit(fmt.Sprintf("  %s   $%.2f", d.Date, d.CostUSD))
		}
		emit("")
		emit(fmt.Sprintf("  anomaly: %s · $%.2f vs $%.2f baseline",
			report.AnomalyDate, report.AnomalyCost, report.BaselineCost))
		if report.Summary != "" {
			emit("  " + report.Summary)
		}
		emit("")

		if _, err := inv.InvestigateFromVision(ctx, report, emit); err != nil {
			emit(fmt.Sprintf("[error] %v", err))
		}
		emit("[[END]]")
	}
}

func repeat(s string, n int) string {
	out := ""
	for i := 0; i < n; i++ {
		out += s
	}
	return out
}
