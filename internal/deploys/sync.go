// Package deploys ingests deploy events from GitHub Actions into the ledger so
// spend anomalies can be correlated against the deploys that may have caused them.
package deploys

import (
	"context"
	"fmt"
	"time"

	"github.com/Yatsuiii/llmtrace/internal/actions"
	"github.com/Yatsuiii/llmtrace/internal/storage"
)

// CursorName is the sync_cursors row tracking the last successful ingest time.
const CursorName = "deploys_last_sync"

// DefaultPattern matches workflow names treated as deploys when none is configured.
const DefaultPattern = "deploy.*"

// Sync pulls successful deploy-workflow runs from GitHub within the lookback
// window and upserts them into the deploys table. It returns the number ingested.
// Re-running is safe: deploys upsert by run ID.
func Sync(ctx context.Context, db *storage.DB, gh actions.GitHubConfig, pattern string, lookback time.Duration) (int, error) {
	if !gh.Enabled() {
		return 0, fmt.Errorf("github not configured: set GITHUB_TOKEN and GITHUB_REPO")
	}
	if pattern == "" {
		pattern = DefaultPattern
	}
	until := time.Now().UTC()
	since := until.Add(-lookback)

	runs, err := actions.ListDeployRuns(ctx, gh, pattern, since, until)
	if err != nil {
		return 0, err
	}

	n := 0
	for _, r := range runs {
		row := storage.DeployRow{
			ID:          fmt.Sprintf("gha-%d", r.ID),
			Repo:        gh.Repo,
			Branch:      r.Branch,
			CommitSHA:   r.CommitSHA,
			PRNumber:    r.PRNumber,
			Title:       r.Title,
			StartedAt:   r.StartedAt,
			CompletedAt: r.CompletedAt,
			Status:      r.Conclusion,
		}
		if err := db.InsertDeploy(ctx, row); err != nil {
			return n, err
		}
		n++
	}

	if err := db.SetCursor(ctx, CursorName, until.Format(time.RFC3339)); err != nil {
		return n, err
	}
	return n, nil
}
