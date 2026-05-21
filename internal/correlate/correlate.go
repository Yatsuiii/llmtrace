// Package correlate joins spend anomalies to the deploys that may have caused
// them. For each anomaly it finds deploys inside a time window, then scores each
// candidate with a deterministic, additive lineage rubric (model change, prompt
// change, error spike, time proximity) and persists the result.
package correlate

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"time"

	"github.com/Yatsuiii/llmtrace/internal/storage"
)

// Additive evidence weights. A deploy that lands in the window and flips the
// model in use is the strongest signal; bare time proximity is the weakest.
// Total possible is 1.00; confidence is capped below that to avoid claiming
// certainty.
const (
	wModelChange  = 0.50
	wPromptChange = 0.30
	wErrorSpike   = 0.15
	wTimeWindow   = 0.05
	maxConfidence = 0.95

	// A shift counts as material only if the new dominant share clears this and
	// the dominant identity actually changed.
	dominanceThreshold = 0.60
	// Post-deploy error rate must rise by at least this (fractional) margin.
	errorSpikeMargin = 0.05
)

type Evidence struct {
	Kind        string            `json:"kind"` // time_window | model_change | prompt_change | error_spike
	Description string            `json:"description"`
	Details     map[string]string `json:"details,omitempty"`
}

type Result struct {
	AnomalyID  int64
	Deploy     storage.DeployRow
	Confidence float64
	Evidence   []Evidence
}

type Config struct {
	WindowHours    int           // how close a deploy must be to the anomaly day
	AnalysisWindow time.Duration // pre/post comparison span for lineage signals
}

func DefaultConfig() Config {
	return Config{WindowHours: 4, AnalysisWindow: 48 * time.Hour}
}

// Run correlates every anomaly since the given time and persists the results.
// Results are returned sorted by anomaly, then descending confidence.
func Run(ctx context.Context, db *storage.DB, cfg Config, since time.Time) ([]Result, error) {
	anomalies, err := db.ListAnomalies(ctx, since)
	if err != nil {
		return nil, fmt.Errorf("list anomalies: %w", err)
	}

	var out []Result
	for _, a := range anomalies {
		if a.ID == 0 {
			continue
		}
		day, err := time.Parse("2006-01-02", a.Date)
		if err != nil {
			continue
		}
		day = day.UTC()
		winStart := day.Add(-time.Duration(cfg.WindowHours) * time.Hour)
		winEnd := day.Add(24 * time.Hour)

		deploys, err := db.DeploysInWindow(ctx, winStart, winEnd)
		if err != nil {
			return nil, fmt.Errorf("deploys in window: %w", err)
		}
		// Sort by completion so each deploy's before/after comparison can be
		// bounded by its neighbors. Without this, clustered same-day deploys all
		// "see" the same shift and score identically.
		sort.SliceStable(deploys, func(i, j int) bool {
			return deploys[i].CompletedAt.Before(deploys[j].CompletedAt)
		})
		for i, dep := range deploys {
			var prevBound, nextBound time.Time
			if i > 0 {
				prevBound = deploys[i-1].CompletedAt
			}
			if i < len(deploys)-1 {
				nextBound = deploys[i+1].CompletedAt
			}
			ev, conf, err := score(ctx, db, a, dep, cfg, prevBound, nextBound)
			if err != nil {
				return nil, err
			}
			evJSON, _ := json.Marshal(ev)
			if err := db.UpsertCorrelation(ctx, storage.CorrelationRow{
				AnomalyID:  a.ID,
				DeployID:   dep.ID,
				Confidence: conf,
				Evidence:   string(evJSON),
			}); err != nil {
				return nil, err
			}
			out = append(out, Result{AnomalyID: a.ID, Deploy: dep, Confidence: conf, Evidence: ev})
		}
	}

	sort.SliceStable(out, func(i, j int) bool {
		if out[i].AnomalyID != out[j].AnomalyID {
			return out[i].AnomalyID < out[j].AnomalyID
		}
		return out[i].Confidence > out[j].Confidence
	})
	return out, nil
}

// score rates one candidate deploy. prevBound/nextBound are the completion times
// of the adjacent deploys (zero if none); the before/after comparison windows are
// clamped to them so a shift is attributed only to the deploy at its boundary.
func score(ctx context.Context, db *storage.DB, a storage.AnomalyRow, dep storage.DeployRow, cfg Config, prevBound, nextBound time.Time) ([]Evidence, float64, error) {
	pivot := dep.CompletedAt
	preStart := pivot.Add(-cfg.AnalysisWindow)
	if !prevBound.IsZero() && prevBound.After(preStart) {
		preStart = prevBound
	}
	postEnd := pivot.Add(cfg.AnalysisWindow)
	if !nextBound.IsZero() && nextBound.Before(postEnd) {
		postEnd = nextBound
	}

	conf := 0.0
	ev := []Evidence{{
		Kind: "time_window",
		Description: fmt.Sprintf("deploy %q (PR #%d) completed %s, within the window of the %s spend anomaly",
			dep.Title, dep.PRNumber, pivot.Format(time.RFC3339), a.Date),
		Details: map[string]string{"deploy_completed": pivot.Format(time.RFC3339), "anomaly_date": a.Date},
	}}
	conf += wTimeWindow

	pre, err := db.ModelDistribution(ctx, a.APIKeyID, preStart, pivot)
	if err != nil {
		return nil, 0, err
	}
	post, err := db.ModelDistribution(ctx, a.APIKeyID, pivot, postEnd)
	if err != nil {
		return nil, 0, err
	}

	// model_change: dominant model flipped and the new one is a clear majority.
	preModel, _ := dominant(pre, func(r storage.ModelDistributionRow) string { return r.Model })
	postModel, postModelShare := dominant(post, func(r storage.ModelDistributionRow) string { return r.Model })
	if preModel != "" && postModel != "" && preModel != postModel && postModelShare >= dominanceThreshold {
		ev = append(ev, Evidence{
			Kind:        "model_change",
			Description: fmt.Sprintf("dominant model shifted from %s to %s (%.0f%% of post-deploy calls)", preModel, postModel, postModelShare*100),
			Details:     map[string]string{"before": preModel, "after": postModel},
		})
		conf += wModelChange
	}

	// prompt_change: dominant prompt fingerprint flipped post-deploy.
	prePrompt, _ := dominant(pre, func(r storage.ModelDistributionRow) string { return r.PromptHash })
	postPrompt, postPromptShare := dominant(post, func(r storage.ModelDistributionRow) string { return r.PromptHash })
	if prePrompt != "" && postPrompt != "" && prePrompt != postPrompt && postPromptShare >= dominanceThreshold {
		ev = append(ev, Evidence{
			Kind:        "prompt_change",
			Description: fmt.Sprintf("dominant prompt fingerprint changed from %s to %s after deploy", short(prePrompt), short(postPrompt)),
			Details:     map[string]string{"before": prePrompt, "after": postPrompt},
		})
		conf += wPromptChange
	}

	// error_spike: error rate rose materially post-deploy.
	preTot, preErr, err := db.ErrorRate(ctx, a.APIKeyID, preStart, pivot)
	if err != nil {
		return nil, 0, err
	}
	postTot, postErr, err := db.ErrorRate(ctx, a.APIKeyID, pivot, postEnd)
	if err != nil {
		return nil, 0, err
	}
	preRate, postRate := rate(preErr, preTot), rate(postErr, postTot)
	if postRate > preRate+errorSpikeMargin {
		ev = append(ev, Evidence{
			Kind:        "error_spike",
			Description: fmt.Sprintf("error rate rose from %.1f%% to %.1f%% after deploy", preRate*100, postRate*100),
			Details:     map[string]string{"before": fmt.Sprintf("%.3f", preRate), "after": fmt.Sprintf("%.3f", postRate)},
		})
		conf += wErrorSpike
	}

	if conf > maxConfidence {
		conf = maxConfidence
	}
	return ev, conf, nil
}

// dominant returns the key with the most calls and its share of the total.
func dominant(rows []storage.ModelDistributionRow, key func(storage.ModelDistributionRow) string) (string, float64) {
	totals := map[string]int64{}
	var sum int64
	for _, r := range rows {
		totals[key(r)] += r.Calls
		sum += r.Calls
	}
	if sum == 0 {
		return "", 0
	}
	var best string
	var bestN int64
	for k, n := range totals {
		if n > bestN {
			best, bestN = k, n
		}
	}
	return best, float64(bestN) / float64(sum)
}

func rate(n, total int64) float64 {
	if total == 0 {
		return 0
	}
	return float64(n) / float64(total)
}

func short(s string) string {
	if len(s) > 12 {
		return s[:12]
	}
	return s
}
