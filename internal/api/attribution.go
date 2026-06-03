package api

import (
	"encoding/json"
	"log"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/Yatsuiii/llmtrace/internal/correlate"
	"github.com/Yatsuiii/llmtrace/internal/detect"
	"github.com/Yatsuiii/llmtrace/internal/storage"
)

type deployInfo struct {
	ID          string `json:"id"`
	PRNumber    int    `json:"pr_number,omitempty"`
	Title       string `json:"title"`
	CommitSHA   string `json:"commit_sha"`
	Repo        string `json:"repo"`
	Branch      string `json:"branch"`
	CompletedAt string `json:"completed_at"`
}

type attributionInfo struct {
	AnomalyID  int64                `json:"anomaly_id"`
	Metric     string               `json:"metric"`
	Date       string               `json:"date"`
	Baseline   float64              `json:"baseline"`
	Actual     float64              `json:"actual"`
	Delta      float64              `json:"delta"`
	Sigma      float64              `json:"sigma"`
	Confidence float64              `json:"confidence"`
	Evidence   []correlate.Evidence `json:"evidence"`
}

type attributionResponse struct {
	SHA         string           `json:"sha"`
	Matched     bool             `json:"matched"`
	Deploy      *deployInfo      `json:"deploy,omitempty"`
	Attribution *attributionInfo `json:"attribution"`
	Note        string           `json:"note,omitempty"`
}

// AttributionHandler serves GET /api/attribution?sha=<commit-sha>. It answers
// the join re_gent needs: given the git HEAD a step produced, did the deploy of
// that commit move the bill, and by how much? It looks the SHA up in the deploy
// ledger and returns the best cost/latency anomaly the correlator attributes to
// it, with confidence and evidence. Accepts a short or full SHA (prefix match).
func AttributionHandler(db *storage.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, "GET only", http.StatusMethodNotAllowed)
			return
		}
		sha := strings.TrimSpace(r.URL.Query().Get("sha"))
		if sha == "" {
			http.Error(w, "provide ?sha=<commit-sha>", http.StatusBadRequest)
			return
		}
		days := 90
		if v := r.URL.Query().Get("days"); v != "" {
			n, err := strconv.Atoi(v)
			if err != nil || n <= 0 {
				http.Error(w, "bad days (want positive integer)", http.StatusBadRequest)
				return
			}
			days = n
		}

		ctx := r.Context()
		since := time.Now().UTC().AddDate(0, 0, -days)

		// Find the deploy with this SHA. A matched-but-innocent deploy is a real
		// answer ("shipped, didn't move the bill"), so look it up independently
		// of whether the correlator tied an anomaly to it.
		deploys, err := db.ListDeploys(ctx, since)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		var matched *storage.DeployRow
		for i := range deploys {
			if shaMatch(deploys[i].CommitSHA, sha) {
				matched = &deploys[i]
				break
			}
		}
		if matched == nil {
			writeJSON(w, attributionResponse{SHA: sha, Matched: false,
				Note: "no deploy with this commit found in the ledger"})
			return
		}

		resp := attributionResponse{SHA: sha, Matched: true, Deploy: &deployInfo{
			ID:          matched.ID,
			PRNumber:    matched.PRNumber,
			Title:       matched.Title,
			CommitSHA:   matched.CommitSHA,
			Repo:        matched.Repo,
			Branch:      matched.Branch,
			CompletedAt: matched.CompletedAt.Format(time.RFC3339),
		}}

		// Ensure anomalies and correlations are current, then pick the strongest
		// correlation that points at this deploy.
		if _, err := detect.Run(ctx, db, detect.DefaultConfig(), since); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		results, err := correlate.Run(ctx, db, correlate.DefaultConfig(), since)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		var best *correlate.Result
		for i := range results {
			if results[i].Deploy.ID == matched.ID {
				if best == nil || results[i].Confidence > best.Confidence {
					best = &results[i]
				}
			}
		}
		if best == nil {
			resp.Note = "deployed, no cost or latency anomaly correlated to this commit"
			writeJSON(w, resp)
			return
		}

		a, err := db.GetAnomaly(ctx, best.AnomalyID)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		resp.Attribution = &attributionInfo{
			AnomalyID:  a.ID,
			Metric:     a.Metric,
			Date:       a.Date,
			Baseline:   a.BaselineValue,
			Actual:     a.ActualValue,
			Delta:      a.Delta,
			Sigma:      a.Sigma,
			Confidence: best.Confidence,
			Evidence:   best.Evidence,
		}
		writeJSON(w, resp)
	}
}

// shaMatch reports whether two commit SHAs refer to the same commit, tolerating
// short vs full forms by matching on the shorter as a prefix of the longer.
func shaMatch(a, b string) bool {
	if a == "" || b == "" {
		return false
	}
	a, b = strings.ToLower(a), strings.ToLower(b)
	return strings.HasPrefix(a, b) || strings.HasPrefix(b, a)
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(v); err != nil {
		log.Printf("api: encode response: %v", err)
	}
}
