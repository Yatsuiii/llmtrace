package storage

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	_ "modernc.org/sqlite"
)

type DB struct {
	conn *sql.DB
}

func Open(path string) (*DB, error) {
	conn, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, fmt.Errorf("open sqlite at %s: %w", path, err)
	}
	if _, err := conn.ExecContext(context.Background(), schema); err != nil {
		conn.Close()
		return nil, fmt.Errorf("apply schema: %w", err)
	}
	return &DB{conn: conn}, nil
}

func (db *DB) Close() error {
	return db.conn.Close()
}

func (db *DB) Wipe(ctx context.Context) error {
	for _, t := range []string{"calls", "api_keys", "anomalies", "deploys", "correlations", "sync_cursors"} {
		if _, err := db.conn.ExecContext(ctx, "DELETE FROM "+t); err != nil {
			return fmt.Errorf("wipe %s: %w", t, err)
		}
	}
	return nil
}

func (db *DB) InsertAPIKey(ctx context.Context, k APIKeyRow) error {
	active := 0
	if k.Active {
		active = 1
	}
	_, err := db.conn.ExecContext(ctx,
		`INSERT INTO api_keys (id, hashed_key, label, budget_usd, rate_limit_rpm, active, created_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?)`,
		k.ID, k.HashedKey, k.Label, k.BudgetUSD, k.RateLimitRPM, active, k.CreatedAt.UnixMilli(),
	)
	if err != nil {
		return fmt.Errorf("insert api_key %s: %w", k.ID, err)
	}
	return nil
}

func (db *DB) InsertDeploy(ctx context.Context, d DeployRow) error {
	_, err := db.conn.ExecContext(ctx,
		`INSERT INTO deploys (id, repo, branch, commit_sha, pr_number, title, started_at, completed_at, status)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		d.ID, d.Repo, d.Branch, d.CommitSHA, d.PRNumber, d.Title,
		d.StartedAt.UnixMilli(), d.CompletedAt.UnixMilli(), d.Status,
	)
	if err != nil {
		return fmt.Errorf("insert deploy %s: %w", d.ID, err)
	}
	return nil
}

// InsertCalls bulk-inserts calls inside a single transaction. Required for any
// realistic synthetic-data volume — SQLite's default per-statement fsync makes
// row-at-a-time inserts unusably slow above a few hundred rows.
func (db *DB) InsertCalls(ctx context.Context, rows []CallRow) error {
	tx, err := db.conn.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}
	stmt, err := tx.PrepareContext(ctx,
		`INSERT INTO calls
		 (timestamp, api_key_id, provider, model, endpoint, input_tokens, output_tokens, cost_usd, latency_ms, status, error_class, prompt_hash, session_id)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`)
	if err != nil {
		tx.Rollback()
		return fmt.Errorf("prepare insert: %w", err)
	}
	defer stmt.Close()
	for _, c := range rows {
		if _, err := stmt.ExecContext(ctx,
			c.Timestamp.UnixMilli(), c.APIKeyID, c.Provider, c.Model, c.Endpoint,
			c.InputTokens, c.OutputTokens, c.CostUSD, c.LatencyMs, c.Status,
			c.ErrorClass, c.PromptHash, c.SessionID,
		); err != nil {
			tx.Rollback()
			return fmt.Errorf("insert call: %w", err)
		}
	}
	return tx.Commit()
}

type KeyDailyCost struct {
	APIKeyID string
	Date     string
	CostUSD  float64
	Calls    int64
}

func (db *DB) DailyCostByKey(ctx context.Context, since time.Time) ([]KeyDailyCost, error) {
	rows, err := db.conn.QueryContext(ctx,
		`SELECT api_key_id,
		        strftime('%Y-%m-%d', timestamp/1000, 'unixepoch') AS day,
		        SUM(cost_usd),
		        COUNT(*)
		 FROM calls
		 WHERE timestamp >= ?
		 GROUP BY api_key_id, day
		 ORDER BY day, api_key_id`,
		since.UnixMilli(),
	)
	if err != nil {
		return nil, fmt.Errorf("query daily cost: %w", err)
	}
	defer rows.Close()
	var out []KeyDailyCost
	for rows.Next() {
		var r KeyDailyCost
		if err := rows.Scan(&r.APIKeyID, &r.Date, &r.CostUSD, &r.Calls); err != nil {
			return nil, fmt.Errorf("scan daily cost: %w", err)
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

func (db *DB) UpsertAnomaly(ctx context.Context, a *AnomalyRow) error {
	res, err := db.conn.ExecContext(ctx,
		`INSERT INTO anomalies
		 (detected_at, api_key_id, date, metric, baseline_value, actual_value, delta, sigma, sample_size)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
		 ON CONFLICT(api_key_id, date, metric) DO UPDATE SET
		   detected_at=excluded.detected_at,
		   baseline_value=excluded.baseline_value,
		   actual_value=excluded.actual_value,
		   delta=excluded.delta,
		   sigma=excluded.sigma,
		   sample_size=excluded.sample_size`,
		a.DetectedAt.UnixMilli(), a.APIKeyID, a.Date, a.Metric,
		a.BaselineValue, a.ActualValue, a.Delta, a.Sigma, a.SampleSize,
	)
	if err != nil {
		return fmt.Errorf("upsert anomaly: %w", err)
	}
	id, _ := res.LastInsertId()
	a.ID = id
	return nil
}

func (db *DB) ListAnomalies(ctx context.Context, since time.Time) ([]AnomalyRow, error) {
	rows, err := db.conn.QueryContext(ctx,
		`SELECT id, detected_at, api_key_id, date, metric,
		        baseline_value, actual_value, delta, sigma, sample_size
		 FROM anomalies
		 WHERE date >= ?
		 ORDER BY date DESC, sigma DESC`,
		since.Format("2006-01-02"),
	)
	if err != nil {
		return nil, fmt.Errorf("list anomalies: %w", err)
	}
	defer rows.Close()
	var out []AnomalyRow
	for rows.Next() {
		var a AnomalyRow
		var detectedMs int64
		if err := rows.Scan(&a.ID, &detectedMs, &a.APIKeyID, &a.Date, &a.Metric,
			&a.BaselineValue, &a.ActualValue, &a.Delta, &a.Sigma, &a.SampleSize); err != nil {
			return nil, fmt.Errorf("scan anomaly: %w", err)
		}
		a.DetectedAt = time.UnixMilli(detectedMs).UTC()
		out = append(out, a)
	}
	return out, rows.Err()
}

// ModelDistribution returns per-model call counts, costs, and top prompt hashes
// for a given key and date window. Used by the agent investigation tools.
func (db *DB) ModelDistribution(ctx context.Context, apiKeyID string, start, end time.Time) ([]ModelDistributionRow, error) {
	rows, err := db.conn.QueryContext(ctx,
		`SELECT model, prompt_hash, COUNT(*) AS calls, SUM(cost_usd) AS cost_usd
		 FROM calls
		 WHERE api_key_id = ? AND timestamp >= ? AND timestamp < ?
		 GROUP BY model, prompt_hash
		 ORDER BY calls DESC`,
		apiKeyID, start.UnixMilli(), end.UnixMilli(),
	)
	if err != nil {
		return nil, fmt.Errorf("model distribution: %w", err)
	}
	defer rows.Close()
	var out []ModelDistributionRow
	for rows.Next() {
		var r ModelDistributionRow
		if err := rows.Scan(&r.Model, &r.PromptHash, &r.Calls, &r.CostUSD); err != nil {
			return nil, fmt.Errorf("scan distribution: %w", err)
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

type ModelDistributionRow struct {
	Model      string
	PromptHash string
	Calls      int64
	CostUSD    float64
}

func (db *DB) DeploysInWindow(ctx context.Context, start, end time.Time) ([]DeployRow, error) {
	rows, err := db.conn.QueryContext(ctx,
		`SELECT id, repo, branch, commit_sha, pr_number, title, started_at, completed_at, status
		 FROM deploys
		 WHERE started_at >= ? AND started_at <= ?
		 ORDER BY started_at`,
		start.UnixMilli(), end.UnixMilli(),
	)
	if err != nil {
		return nil, fmt.Errorf("deploys in window: %w", err)
	}
	defer rows.Close()
	var out []DeployRow
	for rows.Next() {
		var d DeployRow
		var startedMs, completedMs int64
		if err := rows.Scan(&d.ID, &d.Repo, &d.Branch, &d.CommitSHA, &d.PRNumber,
			&d.Title, &startedMs, &completedMs, &d.Status); err != nil {
			return nil, fmt.Errorf("scan deploy: %w", err)
		}
		d.StartedAt = time.UnixMilli(startedMs).UTC()
		d.CompletedAt = time.UnixMilli(completedMs).UTC()
		out = append(out, d)
	}
	return out, rows.Err()
}

type PromptModelMixRow struct {
	Model  string
	Calls  int64
	Period string // "before" | "after"
}

func (db *DB) PromptModelMix(ctx context.Context, promptHash string, pivot time.Time, window time.Duration) ([]PromptModelMixRow, error) {
	beforeStart := pivot.Add(-window)
	afterEnd := pivot.Add(window)
	rows, err := db.conn.QueryContext(ctx,
		`SELECT model, COUNT(*) AS calls, 'before' AS period
		 FROM calls
		 WHERE prompt_hash = ? AND timestamp >= ? AND timestamp < ?
		 GROUP BY model
		 UNION ALL
		 SELECT model, COUNT(*) AS calls, 'after' AS period
		 FROM calls
		 WHERE prompt_hash = ? AND timestamp >= ? AND timestamp < ?
		 GROUP BY model
		 ORDER BY period DESC, calls DESC`,
		promptHash, beforeStart.UnixMilli(), pivot.UnixMilli(),
		promptHash, pivot.UnixMilli(), afterEnd.UnixMilli(),
	)
	if err != nil {
		return nil, fmt.Errorf("prompt model mix: %w", err)
	}
	defer rows.Close()
	var out []PromptModelMixRow
	for rows.Next() {
		var r PromptModelMixRow
		if err := rows.Scan(&r.Model, &r.Calls, &r.Period); err != nil {
			return nil, fmt.Errorf("scan mix: %w", err)
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

func (db *DB) ListDeploys(ctx context.Context, since time.Time) ([]DeployRow, error) {
	rows, err := db.conn.QueryContext(ctx,
		`SELECT id, repo, branch, commit_sha, pr_number, title, started_at, completed_at, status
		 FROM deploys WHERE started_at >= ? ORDER BY started_at`,
		since.UnixMilli(),
	)
	if err != nil {
		return nil, fmt.Errorf("list deploys: %w", err)
	}
	defer rows.Close()
	var out []DeployRow
	for rows.Next() {
		var d DeployRow
		var startedMs, completedMs int64
		if err := rows.Scan(&d.ID, &d.Repo, &d.Branch, &d.CommitSHA, &d.PRNumber,
			&d.Title, &startedMs, &completedMs, &d.Status); err != nil {
			return nil, fmt.Errorf("scan deploy: %w", err)
		}
		d.StartedAt = time.UnixMilli(startedMs).UTC()
		d.CompletedAt = time.UnixMilli(completedMs).UTC()
		out = append(out, d)
	}
	return out, rows.Err()
}

func (db *DB) CountCalls(ctx context.Context) (int64, error) {
	var n int64
	if err := db.conn.QueryRowContext(ctx, "SELECT COUNT(*) FROM calls").Scan(&n); err != nil {
		return 0, fmt.Errorf("count calls: %w", err)
	}
	return n, nil
}

type CallRow struct {
	ID           int64
	Timestamp    time.Time
	APIKeyID     string
	Provider     string
	Model        string
	Endpoint     string
	InputTokens  int
	OutputTokens int
	CostUSD      float64
	LatencyMs    int64
	Status       int
	ErrorClass   string
	PromptHash   string
	SessionID    string
}

type APIKeyRow struct {
	ID           string
	HashedKey    string
	Label        string
	BudgetUSD    float64
	RateLimitRPM int
	Active       bool
	CreatedAt    time.Time
}

type AnomalyRow struct {
	ID            int64
	DetectedAt    time.Time
	APIKeyID      string
	Date          string
	Metric        string
	BaselineValue float64
	ActualValue   float64
	Delta         float64
	Sigma         float64
	SampleSize    int64
}

type DeployRow struct {
	ID          string
	Repo        string
	Branch      string
	CommitSHA   string
	PRNumber    int
	Title       string
	StartedAt   time.Time
	CompletedAt time.Time
	Status      string
}
