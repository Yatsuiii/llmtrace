package detect

import (
	"context"
	"fmt"
	"math"
	"sort"
	"time"

	"github.com/Yatsuiii/llmtrace/internal/storage"
)

type Config struct {
	BaselineDays   int
	SigmaThreshold float64
	MinDeltaUSD    float64
}

func DefaultConfig() Config {
	return Config{BaselineDays: 7, SigmaThreshold: 2.5, MinDeltaUSD: 1.0}
}

// Run scans daily cost data for each key, flags anomalies against a rolling
// baseline, and persists new detections to the DB.
func Run(ctx context.Context, db *storage.DB, cfg Config, since time.Time) ([]storage.AnomalyRow, error) {
	rows, err := db.DailyCostByKey(ctx, since)
	if err != nil {
		return nil, fmt.Errorf("query daily costs: %w", err)
	}

	// Group by key.
	byKey := map[string][]storage.KeyDailyCost{}
	for _, r := range rows {
		byKey[r.APIKeyID] = append(byKey[r.APIKeyID], r)
	}

	now := time.Now().UTC()
	var detected []storage.AnomalyRow

	for keyID, daily := range byKey {
		sort.Slice(daily, func(i, j int) bool { return daily[i].Date < daily[j].Date })

		for i, d := range daily {
			if i < cfg.BaselineDays {
				continue // not enough history
			}
			window := daily[i-cfg.BaselineDays : i]
			mean, stddev := stats(window)
			if stddev == 0 {
				continue
			}
			sigma := (d.CostUSD - mean) / stddev
			delta := d.CostUSD - mean
			if sigma < cfg.SigmaThreshold || delta < cfg.MinDeltaUSD {
				continue
			}
			a := storage.AnomalyRow{
				DetectedAt:    now,
				APIKeyID:      keyID,
				Date:          d.Date,
				Metric:        "daily_cost",
				BaselineValue: mean,
				ActualValue:   d.CostUSD,
				Delta:         delta,
				Sigma:         sigma,
				SampleSize:    int64(d.Calls),
			}
			detected = append(detected, a)
		}
	}

	for i := range detected {
		if err := db.UpsertAnomaly(ctx, &detected[i]); err != nil {
			return nil, fmt.Errorf("upsert anomaly: %w", err)
		}
	}
	return detected, nil
}

func stats(window []storage.KeyDailyCost) (mean, stddev float64) {
	if len(window) == 0 {
		return 0, 0
	}
	for _, d := range window {
		mean += d.CostUSD
	}
	mean /= float64(len(window))
	for _, d := range window {
		diff := d.CostUSD - mean
		stddev += diff * diff
	}
	stddev = math.Sqrt(stddev / float64(len(window)))
	return mean, stddev
}
