// Package actions implements the real-world side effects the agent can take:
// reading code from GitHub and opening pull requests to remediate regressions.
package actions

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"
)

type GitHubConfig struct {
	Token string
	Repo  string // "owner/repo"
}

func GitHubFromEnv() GitHubConfig {
	return GitHubConfig{
		Token: os.Getenv("GITHUB_TOKEN"),
		Repo:  os.Getenv("GITHUB_REPO"),
	}
}

func (c GitHubConfig) Enabled() bool {
	return c.Token != "" && c.Repo != ""
}

func (c GitHubConfig) do(ctx context.Context, method, path string, body any) ([]byte, int, error) {
	var rdr io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rdr = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, "https://api.github.com"+path, rdr)
	if err != nil {
		return nil, 0, err
	}
	req.Header.Set("Authorization", "Bearer "+c.Token)
	req.Header.Set("Accept", "application/vnd.github+json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, 0, fmt.Errorf("github request: %w", err)
	}
	defer resp.Body.Close()
	rb, _ := io.ReadAll(resp.Body)
	return rb, resp.StatusCode, nil
}

type IssueResult struct {
	Number int    `json:"number"`
	URL    string `json:"html_url"`
}

func CreateIssue(ctx context.Context, cfg GitHubConfig, title, body string, labels []string) (IssueResult, error) {
	b, status, err := cfg.do(ctx, http.MethodPost,
		fmt.Sprintf("/repos/%s/issues", cfg.Repo),
		map[string]any{"title": title, "body": body, "labels": labels})
	if err != nil {
		return IssueResult{}, err
	}
	if status >= 300 {
		return IssueResult{}, fmt.Errorf("create issue %d: %s", status, string(b))
	}
	var r IssueResult
	if err := json.Unmarshal(b, &r); err != nil {
		return IssueResult{}, fmt.Errorf("parse issue response: %w", err)
	}
	return r, nil
}

// FetchPRDiff returns the unified diff of every file changed in a pull request.
func FetchPRDiff(ctx context.Context, cfg GitHubConfig, prNumber int) (string, error) {
	b, status, err := cfg.do(ctx, http.MethodGet,
		fmt.Sprintf("/repos/%s/pulls/%d/files", cfg.Repo, prNumber), nil)
	if err != nil {
		return "", err
	}
	if status >= 300 {
		return "", fmt.Errorf("fetch pr #%d files %d: %s", prNumber, status, string(b))
	}
	var files []struct {
		Filename  string `json:"filename"`
		Status    string `json:"status"`
		Additions int    `json:"additions"`
		Deletions int    `json:"deletions"`
		Patch     string `json:"patch"`
	}
	if err := json.Unmarshal(b, &files); err != nil {
		return "", fmt.Errorf("parse pr files: %w", err)
	}
	if len(files) == 0 {
		return "", fmt.Errorf("pr #%d has no changed files", prNumber)
	}
	var sb strings.Builder
	for _, f := range files {
		fmt.Fprintf(&sb, "FILE: %s (%s, +%d -%d)\n%s\n\n", f.Filename, f.Status, f.Additions, f.Deletions, f.Patch)
	}
	return sb.String(), nil
}

type FileContent struct {
	Path    string
	Content string
	SHA     string
}

// FetchFile reads a file's current content from the repository's default branch.
func FetchFile(ctx context.Context, cfg GitHubConfig, path string) (FileContent, error) {
	b, status, err := cfg.do(ctx, http.MethodGet,
		fmt.Sprintf("/repos/%s/contents/%s", cfg.Repo, path), nil)
	if err != nil {
		return FileContent{}, err
	}
	if status >= 300 {
		return FileContent{}, fmt.Errorf("fetch file %s %d: %s", path, status, string(b))
	}
	var resp struct {
		Content string `json:"content"`
		SHA     string `json:"sha"`
	}
	if err := json.Unmarshal(b, &resp); err != nil {
		return FileContent{}, fmt.Errorf("parse file response: %w", err)
	}
	decoded, err := base64.StdEncoding.DecodeString(strings.ReplaceAll(resp.Content, "\n", ""))
	if err != nil {
		return FileContent{}, fmt.Errorf("decode file content: %w", err)
	}
	return FileContent{Path: path, Content: string(decoded), SHA: resp.SHA}, nil
}

type PRSummary struct {
	Number   int       `json:"number"`
	Title    string    `json:"title"`
	MergedAt time.Time `json:"merged_at"`
}

// ListRecentPRs returns pull requests merged into the repo within [since, until].
func ListRecentPRs(ctx context.Context, cfg GitHubConfig, since, until time.Time) ([]PRSummary, error) {
	b, status, err := cfg.do(ctx, http.MethodGet,
		fmt.Sprintf("/repos/%s/pulls?state=closed&sort=updated&direction=desc&per_page=50", cfg.Repo), nil)
	if err != nil {
		return nil, err
	}
	if status >= 300 {
		return nil, fmt.Errorf("list prs %d: %s", status, string(b))
	}
	var raw []struct {
		Number   int     `json:"number"`
		Title    string  `json:"title"`
		MergedAt *string `json:"merged_at"`
	}
	if err := json.Unmarshal(b, &raw); err != nil {
		return nil, fmt.Errorf("parse pr list: %w", err)
	}
	var out []PRSummary
	for _, p := range raw {
		if p.MergedAt == nil {
			continue
		}
		t, err := time.Parse(time.RFC3339, *p.MergedAt)
		if err != nil || t.Before(since) || t.After(until) {
			continue
		}
		out = append(out, PRSummary{Number: p.Number, Title: p.Title, MergedAt: t})
	}
	return out, nil
}

type FixPROptions struct {
	Path       string
	NewContent string
	Title      string
	Body       string
}

// OpenFixPR creates a branch off the default branch, commits a corrected file
// onto it, and opens a pull request. Returns the new PR's number and URL.
func OpenFixPR(ctx context.Context, cfg GitHubConfig, opts FixPROptions) (IssueResult, error) {
	// 1. Resolve the default branch HEAD.
	b, status, err := cfg.do(ctx, http.MethodGet,
		fmt.Sprintf("/repos/%s/git/ref/heads/main", cfg.Repo), nil)
	if err != nil {
		return IssueResult{}, err
	}
	if status >= 300 {
		return IssueResult{}, fmt.Errorf("get main ref %d: %s", status, string(b))
	}
	var ref struct {
		Object struct {
			SHA string `json:"sha"`
		} `json:"object"`
	}
	if err := json.Unmarshal(b, &ref); err != nil || ref.Object.SHA == "" {
		return IssueResult{}, fmt.Errorf("resolve main sha: %v", err)
	}

	// 2. Create a fresh branch (timestamped so repeated runs don't collide).
	branch := fmt.Sprintf("llmtrace/fix-%d", time.Now().Unix())
	b, status, err = cfg.do(ctx, http.MethodPost,
		fmt.Sprintf("/repos/%s/git/refs", cfg.Repo),
		map[string]string{"ref": "refs/heads/" + branch, "sha": ref.Object.SHA})
	if err != nil {
		return IssueResult{}, err
	}
	if status >= 300 {
		return IssueResult{}, fmt.Errorf("create branch %d: %s", status, string(b))
	}

	// 3. Update the file on the new branch (needs the file's current blob SHA).
	cur, err := FetchFile(ctx, cfg, opts.Path)
	if err != nil {
		return IssueResult{}, fmt.Errorf("fetch current file: %w", err)
	}
	b, status, err = cfg.do(ctx, http.MethodPut,
		fmt.Sprintf("/repos/%s/contents/%s", cfg.Repo, opts.Path),
		map[string]string{
			"message": "fix: " + opts.Title,
			"content": base64.StdEncoding.EncodeToString([]byte(opts.NewContent)),
			"sha":     cur.SHA,
			"branch":  branch,
		})
	if err != nil {
		return IssueResult{}, err
	}
	if status >= 300 {
		return IssueResult{}, fmt.Errorf("commit fix %d: %s", status, string(b))
	}

	// 4. Open the pull request.
	b, status, err = cfg.do(ctx, http.MethodPost,
		fmt.Sprintf("/repos/%s/pulls", cfg.Repo),
		map[string]string{"title": opts.Title, "body": opts.Body, "head": branch, "base": "main"})
	if err != nil {
		return IssueResult{}, err
	}
	if status >= 300 {
		return IssueResult{}, fmt.Errorf("open pr %d: %s", status, string(b))
	}
	var pr IssueResult
	if err := json.Unmarshal(b, &pr); err != nil {
		return IssueResult{}, fmt.Errorf("parse pr response: %w", err)
	}
	return pr, nil
}
