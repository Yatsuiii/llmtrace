// Package watcher runs the autonomous anomaly-detection and remediation loop.
// Every Interval it: detects anomalies → investigates with the Gemini agent →
// decides an action based on sigma severity → executes the action (GitHub issue,
// rate limit, or log-only) → records the action in the DB.
package watcher

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/Yatsuiii/llmtrace/internal/actions"
	"github.com/Yatsuiii/llmtrace/internal/agent"
	"github.com/Yatsuiii/llmtrace/internal/correlate"
	"github.com/Yatsuiii/llmtrace/internal/deploys"
	"github.com/Yatsuiii/llmtrace/internal/detect"
	"github.com/Yatsuiii/llmtrace/internal/storage"
)

type Config struct {
	Interval      time.Duration
	LookbackDays  int
	DeployPattern string
}

func DefaultConfig() Config {
	return Config{
		Interval:      15 * time.Minute,
		LookbackDays:  30,
		DeployPattern: deploys.DefaultPattern,
	}
}

type Watcher struct {
	db  *storage.DB
	inv *agent.Investigator
	gh  actions.GitHubConfig
	cfg Config

	mu   sync.Mutex
	subs []chan string

	LastScan  time.Time
	NextScan  time.Time
	ScanCount int
}

func New(db *storage.DB, inv *agent.Investigator, gh actions.GitHubConfig, cfg Config) *Watcher {
	return &Watcher{db: db, inv: inv, gh: gh, cfg: cfg}
}

// Subscribe returns a buffered channel that receives broadcast event lines.
// The caller must call Unsubscribe when done to avoid leaking goroutines.
func (w *Watcher) Subscribe() chan string {
	ch := make(chan string, 64)
	w.mu.Lock()
	w.subs = append(w.subs, ch)
	w.mu.Unlock()
	return ch
}

func (w *Watcher) Unsubscribe(ch chan string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	out := w.subs[:0]
	for _, s := range w.subs {
		if s != ch {
			out = append(out, s)
		}
	}
	w.subs = out
}

func (w *Watcher) emit(msg string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	for _, ch := range w.subs {
		select {
		case ch <- msg:
		default:
		}
	}
}

// Run starts the autonomous loop. It blocks until ctx is cancelled.
func (w *Watcher) Run(ctx context.Context) error {
	w.emit("[watcher] autonomous mode started")
	w.tick(ctx)

	ticker := time.NewTicker(w.cfg.Interval)
	defer ticker.Stop()
	for {
		w.mu.Lock()
		w.NextScan = time.Now().UTC().Add(w.cfg.Interval)
		w.mu.Unlock()
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
			w.tick(ctx)
		}
	}
}

func (w *Watcher) tick(ctx context.Context) {
	now := time.Now().UTC()
	w.mu.Lock()
	w.LastScan = now
	w.ScanCount++
	w.mu.Unlock()

	w.emit(fmt.Sprintf("[watcher] scan #%d started — %s", w.ScanCount, now.Format("15:04:05 UTC")))

	// Refresh deploy events from GitHub Actions so correlation has live data.
	if w.gh.Enabled() {
		lookback := time.Duration(w.cfg.LookbackDays) * 24 * time.Hour
		if n, err := deploys.Sync(ctx, w.db, w.gh, w.cfg.DeployPattern, lookback); err != nil {
			w.emit(fmt.Sprintf("[watcher] deploy sync error: %v", err))
		} else if n > 0 {
			w.emit(fmt.Sprintf("[watcher] synced %d deploy(s) from GitHub", n))
		}
	}

	since := now.AddDate(0, 0, -w.cfg.LookbackDays)
	anomalies, err := detect.Run(ctx, w.db, detect.DefaultConfig(), since)
	if err != nil {
		w.emit(fmt.Sprintf("[watcher] detection error: %v", err))
		return
	}

	// Use real IDs from DB (UpsertAnomaly returns 0 on conflict path).
	listed, err := w.db.ListAnomalies(ctx, since)
	if err != nil {
		w.emit(fmt.Sprintf("[watcher] list anomalies error: %v", err))
		return
	}
	idMap := map[string]int64{} // "keyID|date" → id
	for _, a := range listed {
		idMap[a.APIKeyID+"|"+a.Date] = a.ID
	}
	for i := range anomalies {
		if id, ok := idMap[anomalies[i].APIKeyID+"|"+anomalies[i].Date]; ok {
			anomalies[i].ID = id
		}
	}

	// Persist deterministic anomaly→deploy correlations for the dashboard and
	// to ground the agent's narrative in a scored lineage.
	if _, err := correlate.Run(ctx, w.db, correlate.DefaultConfig(), since); err != nil {
		w.emit(fmt.Sprintf("[watcher] correlation error: %v", err))
	}

	if len(anomalies) == 0 {
		w.emit("[watcher] no anomalies — system nominal")
		return
	}
	w.emit(fmt.Sprintf("[watcher] %d anomaly(ies) detected", len(anomalies)))

	for _, a := range anomalies {
		if a.ID == 0 {
			continue
		}
		done, _ := w.db.HasAgentAction(ctx, a.ID)
		if done {
			w.emit(fmt.Sprintf("[watcher] anomaly #%d (%s %s) already handled — skipping", a.ID, a.APIKeyID, a.Date))
			continue
		}
		w.handle(ctx, a)
	}
}

func (w *Watcher) handle(ctx context.Context, a storage.AnomalyRow) {
	w.emit(fmt.Sprintf("[watcher] anomaly #%d: %s %s  +$%.2f  %.1fσ — investigating...",
		a.ID, a.APIKeyID, a.Date, a.Delta, a.Sigma))

	// Collect attribution text from the investigation agent.
	var lines []string
	emit := func(msg string) {
		w.emit(msg)
		lines = append(lines, msg)
	}

	res, err := w.inv.Investigate(ctx, a, emit)
	if err != nil {
		w.emit(fmt.Sprintf("[watcher] investigation error: %v", err))
		w.recordAction(ctx, a.ID, "log", "failed", "{}", "{}", strings.Join(lines, "\n"))
		return
	}
	attribution := res.Attribution
	if attribution == "" {
		attribution = strings.Join(lines, "\n")
	}

	// The agent may have shipped a remediation PR on its own.
	if res.FixPRURL != "" {
		w.emit(fmt.Sprintf("[watcher] agent shipped a remediation PR — #%d", res.FixPRNumber))
		resJSON, _ := json.Marshal(map[string]any{"html_url": res.FixPRURL, "number": res.FixPRNumber})
		w.recordAction(ctx, a.ID, "fix_pr", "done", "{}", string(resJSON), attribution)
	}

	// Decide follow-up action based on sigma severity.
	severity := severityOf(a.Sigma)
	w.emit(fmt.Sprintf("[watcher] sigma=%.1f → severity=%s", a.Sigma, severity))

	switch severity {
	case "critical", "high":
		w.actGitHubIssue(ctx, a, attribution, severity)
		if severity == "critical" {
			w.actRateLimit(ctx, a)
		}
	default:
		w.emit("[watcher] low severity — logging only")
		w.recordAction(ctx, a.ID, "log", "done", "{}", "{}", attribution)
	}
}

func (w *Watcher) actGitHubIssue(ctx context.Context, a storage.AnomalyRow, attribution, severity string) {
	title := fmt.Sprintf("LLM cost anomaly: %s +$%.2f (%.1fσ) on %s", a.APIKeyID, a.Delta, a.Sigma, a.Date)
	body := fmt.Sprintf("## LLM Cost Anomaly Detected\n\n"+
		"**Key:** `%s`  \n**Date:** %s  \n**Actual:** $%.2f  \n**Baseline:** $%.2f  \n**Delta:** +$%.2f  \n**Sigma:** %.1fσ  \n**Severity:** %s\n\n"+
		"---\n\n## Agent Investigation\n\n```\n%s\n```\n\n"+
		"*Auto-generated by [llmtrace](https://github.com/Yatsuiii/llmtrace) autonomous agent*",
		a.APIKeyID, a.Date, a.ActualValue, a.BaselineValue, a.Delta, a.Sigma, severity, attribution)

	labels := []string{"llm-cost-incident", severity}

	payloadJSON, _ := json.Marshal(map[string]any{"title": title, "labels": labels})

	if !w.gh.Enabled() {
		w.emit("[watcher] GITHUB_TOKEN/GITHUB_REPO not set — skipping issue creation (would create: " + title + ")")
		w.recordAction(ctx, a.ID, "github_issue", "skipped", string(payloadJSON), `{"note":"no github config"}`, attribution)
		return
	}

	w.emit(fmt.Sprintf("[watcher] creating GitHub issue: %q", title))
	result, err := actions.CreateIssue(ctx, w.gh, title, body, labels)
	if err != nil {
		w.emit(fmt.Sprintf("[watcher] GitHub issue failed: %v", err))
		w.recordAction(ctx, a.ID, "github_issue", "failed", string(payloadJSON), fmt.Sprintf(`{"error":%q}`, err.Error()), attribution)
		return
	}
	resultJSON, _ := json.Marshal(result)
	w.emit(fmt.Sprintf("[watcher] GitHub issue #%d created: %s", result.Number, result.URL))
	w.recordAction(ctx, a.ID, "github_issue", "done", string(payloadJSON), string(resultJSON), attribution)
}

func (w *Watcher) actRateLimit(ctx context.Context, a storage.AnomalyRow) {
	const criticalRPM = 20
	w.emit(fmt.Sprintf("[watcher] enforcing rate limit on %s → %d rpm", a.APIKeyID, criticalRPM))
	payloadJSON, _ := json.Marshal(map[string]any{"key": a.APIKeyID, "rpm": criticalRPM})
	if err := w.db.SetAPIKeyRateLimit(ctx, a.APIKeyID, criticalRPM); err != nil {
		w.emit(fmt.Sprintf("[watcher] rate limit update failed: %v", err))
		w.recordAction(ctx, a.ID, "rate_limit", "failed", string(payloadJSON), fmt.Sprintf(`{"error":%q}`, err.Error()), "")
		return
	}
	w.emit(fmt.Sprintf("[watcher] rate limit set: %s → %d rpm", a.APIKeyID, criticalRPM))
	resultJSON, _ := json.Marshal(map[string]any{"key": a.APIKeyID, "rpm": criticalRPM})
	w.recordAction(ctx, a.ID, "rate_limit", "done", string(payloadJSON), string(resultJSON), "")
}

func (w *Watcher) recordAction(ctx context.Context, anomalyID int64, actionType, status, payload, result, attribution string) {
	_ = w.db.InsertAgentAction(ctx, storage.AgentActionRow{
		AnomalyID:   anomalyID,
		ActionType:  actionType,
		Status:      status,
		Payload:     payload,
		Result:      result,
		Attribution: attribution,
		CreatedAt:   time.Now().UTC(),
	})
}

func severityOf(sigma float64) string {
	switch {
	case sigma >= 10:
		return "critical"
	case sigma >= 5:
		return "high"
	default:
		return "low"
	}
}
