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

// InsertDeploy upserts a deploy by ID, so re-ingesting the same workflow run
// updates it in place rather than failing on the primary key.
func (db *DB) InsertDeploy(ctx context.Context, d DeployRow) error {
	_, err := db.conn.ExecContext(ctx,
		`INSERT OR REPLACE INTO deploys (id, repo, branch, commit_sha, pr_number, title, started_at, completed_at, status)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		d.ID, d.Repo, d.Branch, d.CommitSHA, d.PRNumber, d.Title,
		d.StartedAt.UnixMilli(), d.CompletedAt.UnixMilli(), d.Status,
	)
	if err != nil {
		return fmt.Errorf("insert deploy %s: %w", d.ID, err)
	}
	return nil
}

// GetCursor returns a stored sync cursor value, or "" if it has never been set.
func (db *DB) GetCursor(ctx context.Context, name string) (string, error) {
	var v string
	err := db.conn.QueryRowContext(ctx,
		`SELECT value FROM sync_cursors WHERE name = ?`, name).Scan(&v)
	if err == sql.ErrNoRows {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("get cursor %s: %w", name, err)
	}
	return v, nil
}

// SetCursor stores a sync cursor value, overwriting any previous one.
func (db *DB) SetCursor(ctx context.Context, name, value string) error {
	_, err := db.conn.ExecContext(ctx,
		`INSERT OR REPLACE INTO sync_cursors (name, value) VALUES (?, ?)`, name, value)
	if err != nil {
		return fmt.Errorf("set cursor %s: %w", name, err)
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

// ErrorRate returns total calls and errored calls (non-empty error_class) for a
// key in [start, end). Used by correlation to detect post-deploy error spikes.
func (db *DB) ErrorRate(ctx context.Context, apiKeyID string, start, end time.Time) (total, errors int64, err error) {
	row := db.conn.QueryRowContext(ctx,
		`SELECT COUNT(*), COALESCE(SUM(CASE WHEN error_class != '' THEN 1 ELSE 0 END), 0)
		 FROM calls
		 WHERE api_key_id = ? AND timestamp >= ? AND timestamp < ?`,
		apiKeyID, start.UnixMilli(), end.UnixMilli(),
	)
	if err := row.Scan(&total, &errors); err != nil {
		return 0, 0, fmt.Errorf("error rate for %s: %w", apiKeyID, err)
	}
	return total, errors, nil
}

type CorrelationRow struct {
	AnomalyID  int64
	DeployID   string
	Confidence float64
	Evidence   string // JSON-encoded []Evidence
}

// UpsertCorrelation stores a scored anomaly-to-deploy correlation, replacing any
// prior score for the same pair.
func (db *DB) UpsertCorrelation(ctx context.Context, c CorrelationRow) error {
	_, err := db.conn.ExecContext(ctx,
		`INSERT OR REPLACE INTO correlations (anomaly_id, deploy_id, confidence, evidence)
		 VALUES (?, ?, ?, ?)`,
		c.AnomalyID, c.DeployID, c.Confidence, c.Evidence,
	)
	if err != nil {
		return fmt.Errorf("upsert correlation %d/%s: %w", c.AnomalyID, c.DeployID, err)
	}
	return nil
}

// ListCorrelations returns stored correlations for an anomaly, highest confidence first.
func (db *DB) ListCorrelations(ctx context.Context, anomalyID int64) ([]CorrelationRow, error) {
	rows, err := db.conn.QueryContext(ctx,
		`SELECT anomaly_id, deploy_id, confidence, evidence
		 FROM correlations WHERE anomaly_id = ? ORDER BY confidence DESC`,
		anomalyID,
	)
	if err != nil {
		return nil, fmt.Errorf("list correlations: %w", err)
	}
	defer rows.Close()
	var out []CorrelationRow
	for rows.Next() {
		var c CorrelationRow
		if err := rows.Scan(&c.AnomalyID, &c.DeployID, &c.Confidence, &c.Evidence); err != nil {
			return nil, fmt.Errorf("scan correlation: %w", err)
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

type AgentActionRow struct {
	ID          int64
	AnomalyID   int64
	ActionType  string
	Status      string
	Payload     string
	Result      string
	Attribution string
	CreatedAt   time.Time
}

func (db *DB) InsertAgentAction(ctx context.Context, a AgentActionRow) error {
	_, err := db.conn.ExecContext(ctx,
		`INSERT OR IGNORE INTO agent_actions
		 (anomaly_id, action_type, status, payload, result, attribution, created_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?)`,
		a.AnomalyID, a.ActionType, a.Status, a.Payload, a.Result, a.Attribution,
		a.CreatedAt.UnixMilli(),
	)
	if err != nil {
		return fmt.Errorf("insert agent_action: %w", err)
	}
	return nil
}

func (db *DB) HasAgentAction(ctx context.Context, anomalyID int64) (bool, error) {
	var n int
	err := db.conn.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM agent_actions WHERE anomaly_id = ?`, anomalyID,
	).Scan(&n)
	return n > 0, err
}

func (db *DB) ListAgentActions(ctx context.Context, since time.Time) ([]AgentActionRow, error) {
	rows, err := db.conn.QueryContext(ctx,
		`SELECT id, anomaly_id, action_type, status, payload, result, attribution, created_at
		 FROM agent_actions WHERE created_at >= ? ORDER BY created_at DESC`,
		since.UnixMilli(),
	)
	if err != nil {
		return nil, fmt.Errorf("list agent_actions: %w", err)
	}
	defer rows.Close()
	var out []AgentActionRow
	for rows.Next() {
		var a AgentActionRow
		var createdMs int64
		if err := rows.Scan(&a.ID, &a.AnomalyID, &a.ActionType, &a.Status,
			&a.Payload, &a.Result, &a.Attribution, &createdMs); err != nil {
			return nil, fmt.Errorf("scan agent_action: %w", err)
		}
		a.CreatedAt = time.UnixMilli(createdMs).UTC()
		out = append(out, a)
	}
	return out, rows.Err()
}

func (db *DB) SetAPIKeyRateLimit(ctx context.Context, keyID string, rpm int) error {
	_, err := db.conn.ExecContext(ctx,
		`UPDATE api_keys SET rate_limit_rpm = ? WHERE id = ?`, rpm, keyID,
	)
	return err
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

func (db *DB) ListAPIKeys(ctx context.Context) ([]APIKeyRow, error) {
	rows, err := db.conn.QueryContext(ctx,
		`SELECT id, hashed_key, label, budget_usd, rate_limit_rpm, active, created_at FROM api_keys ORDER BY created_at`)
	if err != nil {
		return nil, fmt.Errorf("list api_keys: %w", err)
	}
	defer rows.Close()
	var out []APIKeyRow
	for rows.Next() {
		var k APIKeyRow
		var active int
		var createdMs int64
		if err := rows.Scan(&k.ID, &k.HashedKey, &k.Label, &k.BudgetUSD, &k.RateLimitRPM, &active, &createdMs); err != nil {
			return nil, fmt.Errorf("scan api_key: %w", err)
		}
		k.Active = active == 1
		k.CreatedAt = time.UnixMilli(createdMs).UTC()
		out = append(out, k)
	}
	return out, rows.Err()
}

func (db *DB) SetAPIKeyActive(ctx context.Context, keyID string, active bool) error {
	v := 0
	if active {
		v = 1
	}
	_, err := db.conn.ExecContext(ctx, `UPDATE api_keys SET active = ? WHERE id = ?`, v, keyID)
	return err
}

func (db *DB) GetAnomaly(ctx context.Context, id int64) (AnomalyRow, error) {
	var a AnomalyRow
	var detectedMs int64
	err := db.conn.QueryRowContext(ctx,
		`SELECT id, detected_at, api_key_id, date, metric, baseline_value, actual_value, delta, sigma, sample_size
		 FROM anomalies WHERE id = ?`, id,
	).Scan(&a.ID, &detectedMs, &a.APIKeyID, &a.Date, &a.Metric,
		&a.BaselineValue, &a.ActualValue, &a.Delta, &a.Sigma, &a.SampleSize)
	if err != nil {
		return a, fmt.Errorf("get anomaly %d: %w", id, err)
	}
	a.DetectedAt = time.UnixMilli(detectedMs).UTC()
	return a, nil
}

func (db *DB) CallSummary(ctx context.Context, since time.Time) ([]CallSummaryRow, error) {
	rows, err := db.conn.QueryContext(ctx,
		`SELECT api_key_id, model, COUNT(*) AS calls, SUM(input_tokens) AS input_tokens,
		        SUM(output_tokens) AS output_tokens, SUM(cost_usd) AS cost_usd,
		        AVG(latency_ms) AS avg_latency_ms
		 FROM calls WHERE timestamp >= ?
		 GROUP BY api_key_id, model ORDER BY cost_usd DESC`,
		since.UnixMilli(),
	)
	if err != nil {
		return nil, fmt.Errorf("call summary: %w", err)
	}
	defer rows.Close()
	var out []CallSummaryRow
	for rows.Next() {
		var r CallSummaryRow
		if err := rows.Scan(&r.APIKeyID, &r.Model, &r.Calls, &r.InputTokens, &r.OutputTokens, &r.CostUSD, &r.AvgLatencyMs); err != nil {
			return nil, fmt.Errorf("scan summary: %w", err)
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

func (db *DB) RecentCalls(ctx context.Context, limit int) ([]CallRow, error) {
	rows, err := db.conn.QueryContext(ctx,
		`SELECT id, timestamp, api_key_id, provider, model, endpoint,
		        input_tokens, output_tokens, cost_usd, latency_ms, status, error_class, prompt_hash, session_id
		 FROM calls ORDER BY timestamp DESC LIMIT ?`, limit,
	)
	if err != nil {
		return nil, fmt.Errorf("recent calls: %w", err)
	}
	defer rows.Close()
	var out []CallRow
	for rows.Next() {
		var c CallRow
		var tsMs int64
		if err := rows.Scan(&c.ID, &tsMs, &c.APIKeyID, &c.Provider, &c.Model, &c.Endpoint,
			&c.InputTokens, &c.OutputTokens, &c.CostUSD, &c.LatencyMs, &c.Status, &c.ErrorClass,
			&c.PromptHash, &c.SessionID); err != nil {
			return nil, fmt.Errorf("scan call: %w", err)
		}
		c.Timestamp = time.UnixMilli(tsMs).UTC()
		out = append(out, c)
	}
	return out, rows.Err()
}

type CallSummaryRow struct {
	APIKeyID     string
	Model        string
	Calls        int64
	InputTokens  int64
	OutputTokens int64
	CostUSD      float64
	AvgLatencyMs float64
}

// CostByModel is one model's slice of a CostResult.
type CostByModel struct {
	Model   string  `json:"model"`
	Calls   int64   `json:"calls"`
	CostUSD float64 `json:"cost_usd"`
}

// CostResult aggregates call telemetry for a session and/or time window.
type CostResult struct {
	Calls        int64
	InputTokens  int64
	OutputTokens int64
	CostUSD      float64
	FirstCall    time.Time
	LastCall     time.Time
	ByModel      []CostByModel
}

// CostFor aggregates call cost filtered by an optional session id and an
// optional [from, to] window. An empty session or a zero time skips that
// filter. This is the read side of integrations like re_gent: join an external
// unit of work to its LLM cost by session id (exact) or timestamp (per-step).
func (db *DB) CostFor(ctx context.Context, session string, from, to time.Time) (CostResult, error) {
	where := "WHERE 1=1"
	var args []any
	if session != "" {
		where += " AND session_id = ?"
		args = append(args, session)
	}
	if !from.IsZero() {
		where += " AND timestamp >= ?"
		args = append(args, from.UnixMilli())
	}
	if !to.IsZero() {
		where += " AND timestamp <= ?"
		args = append(args, to.UnixMilli())
	}

	var res CostResult
	var firstMs, lastMs int64
	err := db.conn.QueryRowContext(ctx,
		`SELECT COUNT(*), COALESCE(SUM(input_tokens),0), COALESCE(SUM(output_tokens),0),
		        COALESCE(SUM(cost_usd),0), COALESCE(MIN(timestamp),0), COALESCE(MAX(timestamp),0)
		 FROM calls `+where, args...,
	).Scan(&res.Calls, &res.InputTokens, &res.OutputTokens, &res.CostUSD, &firstMs, &lastMs)
	if err != nil {
		return res, fmt.Errorf("cost aggregate: %w", err)
	}
	if firstMs > 0 {
		res.FirstCall = time.UnixMilli(firstMs).UTC()
		res.LastCall = time.UnixMilli(lastMs).UTC()
	}

	rows, err := db.conn.QueryContext(ctx,
		`SELECT model, COUNT(*), COALESCE(SUM(cost_usd),0)
		 FROM calls `+where+` GROUP BY model ORDER BY SUM(cost_usd) DESC`, args...,
	)
	if err != nil {
		return res, fmt.Errorf("cost by model: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var m CostByModel
		if err := rows.Scan(&m.Model, &m.Calls, &m.CostUSD); err != nil {
			return res, fmt.Errorf("scan cost by model: %w", err)
		}
		res.ByModel = append(res.ByModel, m)
	}
	return res, rows.Err()
}
