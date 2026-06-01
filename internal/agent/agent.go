package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"

	"google.golang.org/genai"

	"github.com/Yatsuiii/llmtrace/internal/actions"
	"github.com/Yatsuiii/llmtrace/internal/storage"
)

const defaultModel = "gemini-2.5-flash"

var retryAfterRe = regexp.MustCompile(`retry in (\d+)`)

// Result carries what an investigation produced beyond the streamed text.
type Result struct {
	Attribution string
	FixPRURL    string
	FixPRNumber int
}

type Investigator struct {
	db    *storage.DB
	model string
	gc    *genai.Client
	gh    actions.GitHubConfig
}

func New(db *storage.DB, apiKey, model string) (*Investigator, error) {
	ctx := context.Background()
	var cfg *genai.ClientConfig
	if proj := os.Getenv("GOOGLE_CLOUD_PROJECT"); proj != "" {
		loc := os.Getenv("GOOGLE_CLOUD_LOCATION")
		if loc == "" {
			loc = "us-central1"
		}
		cfg = &genai.ClientConfig{Backend: genai.BackendVertexAI, Project: proj, Location: loc}
	} else {
		cfg = &genai.ClientConfig{APIKey: apiKey}
	}
	gc, err := genai.NewClient(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("create genai client: %w", err)
	}
	if model == "" {
		model = defaultModel
	}
	return &Investigator{db: db, model: model, gc: gc, gh: actions.GitHubFromEnv()}, nil
}

// generateWithRetry calls GenerateContent, retrying up to 3 times on 429/503
// after the suggested delay (or a 20s fallback). Free-tier keys have low RPM.
func (inv *Investigator) generateWithRetry(ctx context.Context, history []*genai.Content, cfg *genai.GenerateContentConfig, emit func(string)) (*genai.GenerateContentResponse, error) {
	const maxRetries = 3
	for attempt := 0; attempt <= maxRetries; attempt++ {
		resp, err := inv.gc.Models.GenerateContent(ctx, inv.model, history, cfg)
		if err == nil {
			return resp, nil
		}
		errStr := err.Error()
		if (!strings.Contains(errStr, "429") && !strings.Contains(errStr, "503")) || attempt == maxRetries {
			return nil, err
		}
		wait := time.Duration(20+attempt*15) * time.Second
		if m := retryAfterRe.FindStringSubmatch(errStr); len(m) == 2 {
			if secs, e := strconv.Atoi(m[1]); e == nil {
				wait = time.Duration(secs+2) * time.Second
			}
		}
		emit(fmt.Sprintf("[rate limit — retrying in %s (attempt %d/%d)]", wait.Round(time.Second), attempt+1, maxRetries))
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(wait):
		}
	}
	return nil, fmt.Errorf("exhausted %d retries", maxRetries)
}

const systemPrompt = `You are an autonomous LLM-cost investigation agent. A spend
anomaly has been flagged on a production API key. Your mission: determine the
full root cause with high confidence and, if a code change is responsible,
ship a fix.

Every cost spike is the product of two factors:
    daily_cost = (number of calls) x (cost per call)
A complete attribution MUST quantify BOTH. If a model change explains a higher
per-call cost but call volume also rose, you are not finished until you have
accounted for the volume change as well. Decompose the dollar delta.

Your tools let you:
  - inspect the call ledger (daily cost, volume, model mix, per-prompt breakdown)
  - compare the affected key against unaffected control keys
  - find deploys near the anomaly
  - read the ACTUAL code diff of a suspect deploy from GitHub
  - read a current source file from GitHub
  - open a remediation pull request with a corrected file

Work like an investigator: form a hypothesis, test it with a tool, revise.
Do NOT follow a fixed checklist — choose each step from what you have learned.

Several deploys may land near the anomaly — most are unrelated. Read the diff
of EACH suspect deploy with fetch_pr_diff, and explicitly rule out the ones
with no cost impact (dependency bumps, documentation, styling) before naming a
cause. State why each innocent deploy is dismissed.

A cost incident has ONE root cause. Converge on the single deploy most
responsible — do not hedge or split blame across multiple deploys. A change
that touches no LLM call path (a version bump, a docs edit) cannot move spend
and must be dismissed outright, not listed as a contributing factor.

Once you have confirmed which deploy is responsible and read its real code:
if the change is a clear regression (an unintended switch to a costlier model,
a retry/duplication loop multiplying calls, etc.), then: fetch_file to get the
current source, write a corrected version that fixes the regression while
keeping intended behavior, and call open_fix_pr to ship it.

Finish with a concise attribution containing:
  - the deploy and PR responsible
  - the exact code lines at fault
  - the dollar delta split across price-per-call vs volume
  - a confidence score (0-1)
  - the remediation PR link, if you opened one`

const chatSystemPrompt = `You are the llmtrace cost-analysis assistant. Answer the
user's question about their LLM spend. Use the tools to query the call ledger,
compare keys, inspect deploys, and read code diffs as needed. Be concise and
specific — cite exact dollar amounts, dates, keys, and models. If the data does
not support an answer, say so plainly.`

// Investigate runs the autonomous investigation loop for a single anomaly.
func (inv *Investigator) Investigate(ctx context.Context, a storage.AnomalyRow, emit func(string)) (Result, error) {
	emit(fmt.Sprintf("anomaly: key=%s date=%s actual=$%.2f baseline=$%.2f delta=+$%.2f sigma=%.1fσ",
		a.APIKeyID, a.Date, a.ActualValue, a.BaselineValue, a.Delta, a.Sigma))
	emit("")

	var result Result
	cfg := &genai.GenerateContentConfig{
		SystemInstruction: genai.NewContentFromText(systemPrompt, genai.RoleUser),
		Tools:             []*genai.Tool{{FunctionDeclarations: investigationTools()}},
	}
	history := []*genai.Content{
		genai.NewContentFromText(fmt.Sprintf(
			"Investigate this spend anomaly and remediate it if a code regression is responsible.\n\n"+
				"Key: %s\nDate: %s\nActual daily cost: $%.2f\nBaseline (7d avg): $%.2f\nDelta: +$%.2f (%.1fσ)\n\n"+
				"You decide which tools to use and in what order.",
			a.APIKeyID, a.Date, a.ActualValue, a.BaselineValue, a.Delta, a.Sigma), genai.RoleUser),
	}

	for round := 0; round < 14; round++ {
		resp, err := inv.generateWithRetry(ctx, history, cfg, emit)
		if err != nil {
			return result, fmt.Errorf("generate round %d: %w", round, err)
		}
		if len(resp.Candidates) > 0 && resp.Candidates[0].Content != nil {
			history = append(history, resp.Candidates[0].Content)
		}
		fcs := resp.FunctionCalls()
		if len(fcs) == 0 {
			txt := resp.Text()
			emit("")
			emit("── Attribution " + strings.Repeat("─", 50))
			for _, line := range strings.Split(txt, "\n") {
				emit(line)
			}
			result.Attribution = txt
			return result, nil
		}
		var parts []*genai.Part
		for _, fc := range fcs {
			out, err := inv.dispatch(ctx, fc, emit, &result)
			if err != nil {
				out = map[string]any{"error": err.Error()}
			}
			parts = append(parts, genai.NewPartFromFunctionResponse(fc.Name, out))
		}
		history = append(history, genai.NewContentFromParts(parts, genai.RoleUser))
	}
	emit("[max reasoning rounds reached]")
	return result, nil
}

// Chat answers a free-form question using the read-only ledger tools.
func (inv *Investigator) Chat(ctx context.Context, question, contextNote string, emit func(string)) error {
	cfg := &genai.GenerateContentConfig{
		SystemInstruction: genai.NewContentFromText(chatSystemPrompt, genai.RoleUser),
		Tools:             []*genai.Tool{{FunctionDeclarations: chatTools()}},
	}
	prompt := question
	if contextNote != "" {
		prompt = "Context: " + contextNote + "\n\nQuestion: " + question
	}
	history := []*genai.Content{genai.NewContentFromText(prompt, genai.RoleUser)}

	for round := 0; round < 10; round++ {
		resp, err := inv.generateWithRetry(ctx, history, cfg, emit)
		if err != nil {
			return fmt.Errorf("chat round %d: %w", round, err)
		}
		if len(resp.Candidates) > 0 && resp.Candidates[0].Content != nil {
			history = append(history, resp.Candidates[0].Content)
		}
		fcs := resp.FunctionCalls()
		if len(fcs) == 0 {
			for _, line := range strings.Split(resp.Text(), "\n") {
				emit(line)
			}
			return nil
		}
		var parts []*genai.Part
		var discard Result
		for _, fc := range fcs {
			out, err := inv.dispatch(ctx, fc, emit, &discard)
			if err != nil {
				out = map[string]any{"error": err.Error()}
			}
			parts = append(parts, genai.NewPartFromFunctionResponse(fc.Name, out))
		}
		history = append(history, genai.NewContentFromParts(parts, genai.RoleUser))
	}
	return nil
}

func (inv *Investigator) dispatch(ctx context.Context, fc *genai.FunctionCall, emit func(string), result *Result) (map[string]any, error) {
	switch fc.Name {
	case "query_daily_cost":
		return inv.toolDailyCost(ctx, fc.Args, emit)
	case "query_model_distribution":
		return inv.toolModelDistribution(ctx, fc.Args, emit)
	case "compare_keys":
		return inv.toolCompareKeys(ctx, fc.Args, emit)
	case "get_deploys_in_window":
		return inv.toolDeploysInWindow(ctx, fc.Args, emit)
	case "diff_prompt_model_mix":
		return inv.toolPromptModelMix(ctx, fc.Args, emit)
	case "list_recent_prs":
		return inv.toolListRecentPRs(ctx, fc.Args, emit)
	case "fetch_pr_diff":
		return inv.toolFetchPRDiff(ctx, fc.Args, emit)
	case "fetch_file":
		return inv.toolFetchFile(ctx, fc.Args, emit)
	case "open_fix_pr":
		return inv.toolOpenFixPR(ctx, fc.Args, emit, result)
	default:
		return map[string]any{"error": "unknown tool: " + fc.Name}, nil
	}
}

func (inv *Investigator) toolDailyCost(ctx context.Context, args map[string]any, emit func(string)) (map[string]any, error) {
	apiKeyID, _ := args["api_key_id"].(string)
	startDate, _ := args["start_date"].(string)
	endDate, _ := args["end_date"].(string)
	emit(fmt.Sprintf("[tool] query_daily_cost key=%s %s → %s", apiKeyID, startDate, endDate))

	start, err := time.Parse("2006-01-02", startDate)
	if err != nil {
		return nil, fmt.Errorf("parse start_date: %w", err)
	}
	rows, err := inv.db.DailyCostByKey(ctx, start)
	if err != nil {
		return nil, err
	}
	type entry struct {
		Date    string  `json:"date"`
		CostUSD float64 `json:"cost_usd"`
		Calls   int64   `json:"calls"`
	}
	var entries []entry
	for _, r := range rows {
		if r.APIKeyID != apiKeyID {
			continue
		}
		if endDate != "" && r.Date > endDate {
			continue
		}
		entries = append(entries, entry{r.Date, r.CostUSD, r.Calls})
	}
	emit(fmt.Sprintf("       → %d day(s)", len(entries)))
	return map[string]any{"key": apiKeyID, "days": entries}, nil
}

func (inv *Investigator) toolCompareKeys(ctx context.Context, args map[string]any, emit func(string)) (map[string]any, error) {
	date, _ := args["date"].(string)
	emit(fmt.Sprintf("[tool] compare_keys date=%s", date))

	d, err := time.Parse("2006-01-02", date)
	if err != nil {
		return nil, fmt.Errorf("parse date: %w", err)
	}
	rows, err := inv.db.DailyCostByKey(ctx, d.AddDate(0, 0, -1))
	if err != nil {
		return nil, err
	}
	type entry struct {
		Key     string  `json:"key"`
		CostUSD float64 `json:"cost_usd"`
		Calls   int64   `json:"calls"`
	}
	var entries []entry
	for _, r := range rows {
		if r.Date == date {
			entries = append(entries, entry{r.APIKeyID, r.CostUSD, r.Calls})
		}
	}
	emit(fmt.Sprintf("       → %d key(s) on %s", len(entries), date))
	return map[string]any{"date": date, "keys": entries}, nil
}

func (inv *Investigator) toolModelDistribution(ctx context.Context, args map[string]any, emit func(string)) (map[string]any, error) {
	apiKeyID, _ := args["api_key_id"].(string)
	startDate, _ := args["start_date"].(string)
	endDate, _ := args["end_date"].(string)
	emit(fmt.Sprintf("[tool] query_model_distribution key=%s %s → %s", apiKeyID, startDate, endDate))

	start, err := time.Parse("2006-01-02", startDate)
	if err != nil {
		return nil, fmt.Errorf("parse start_date: %w", err)
	}
	end, err := time.Parse("2006-01-02", endDate)
	if err != nil {
		return nil, fmt.Errorf("parse end_date: %w", err)
	}
	end = end.Add(24 * time.Hour)

	rows, err := inv.db.ModelDistribution(ctx, apiKeyID, start, end)
	if err != nil {
		return nil, err
	}
	type entry struct {
		Model      string  `json:"model"`
		PromptHash string  `json:"prompt_hash"`
		Calls      int64   `json:"calls"`
		CostUSD    float64 `json:"cost_usd"`
	}
	var entries []entry
	var totalCalls int64
	for _, r := range rows {
		entries = append(entries, entry{r.Model, r.PromptHash, r.Calls, r.CostUSD})
		totalCalls += r.Calls
	}
	emit(fmt.Sprintf("       → %d rows, %d total calls", len(entries), totalCalls))
	return map[string]any{"rows": entries, "total_calls": totalCalls}, nil
}

func (inv *Investigator) toolDeploysInWindow(ctx context.Context, args map[string]any, emit func(string)) (map[string]any, error) {
	startStr, _ := args["start_time"].(string)
	endStr, _ := args["end_time"].(string)
	emit(fmt.Sprintf("[tool] get_deploys_in_window %s → %s", startStr, endStr))

	parse := func(s string) (time.Time, error) {
		for _, layout := range []string{time.RFC3339, "2006-01-02T15:04:05Z", "2006-01-02 15:04:05", "2006-01-02"} {
			if t, err := time.Parse(layout, s); err == nil {
				return t, nil
			}
		}
		return time.Time{}, fmt.Errorf("cannot parse time: %q", s)
	}
	start, err := parse(startStr)
	if err != nil {
		return nil, err
	}
	end, err := parse(endStr)
	if err != nil {
		return nil, err
	}
	deploys, err := inv.db.DeploysInWindow(ctx, start, end)
	if err != nil {
		return nil, err
	}
	type entry struct {
		ID        string `json:"id"`
		PRNumber  int    `json:"pr_number"`
		Title     string `json:"title"`
		Repo      string `json:"repo"`
		StartedAt string `json:"started_at"`
		CommitSHA string `json:"commit_sha"`
	}
	var entries []entry
	for _, d := range deploys {
		entries = append(entries, entry{d.ID, d.PRNumber, d.Title, d.Repo, d.StartedAt.Format(time.RFC3339), d.CommitSHA})
	}
	emit(fmt.Sprintf("       → %d deploys found", len(entries)))
	return map[string]any{"deploys": entries}, nil
}

func (inv *Investigator) toolPromptModelMix(ctx context.Context, args map[string]any, emit func(string)) (map[string]any, error) {
	promptHash, _ := args["prompt_hash"].(string)
	pivotStr, _ := args["pivot_time"].(string)
	emit(fmt.Sprintf("[tool] diff_prompt_model_mix prompt=%s pivot=%s", promptHash, pivotStr))

	pivot, err := time.Parse(time.RFC3339, pivotStr)
	if err != nil {
		pivot, err = time.Parse("2006-01-02T15:04:05Z", pivotStr)
		if err != nil {
			pivot, err = time.Parse("2006-01-02", pivotStr)
			if err != nil {
				return nil, fmt.Errorf("parse pivot_time: %w", err)
			}
		}
	}
	rows, err := inv.db.PromptModelMix(ctx, promptHash, pivot, 7*24*time.Hour)
	if err != nil {
		return nil, err
	}
	type entry struct {
		Model  string `json:"model"`
		Calls  int64  `json:"calls"`
		Period string `json:"period"`
	}
	var entries []entry
	for _, r := range rows {
		entries = append(entries, entry{r.Model, r.Calls, r.Period})
	}
	b, _ := json.Marshal(entries)
	emit(fmt.Sprintf("       → %s", string(b)))
	return map[string]any{"mix": entries}, nil
}

func (inv *Investigator) toolFetchPRDiff(ctx context.Context, args map[string]any, emit func(string)) (map[string]any, error) {
	prNumber := argInt(args["pr_number"])
	emit(fmt.Sprintf("[tool] fetch_pr_diff pr=#%d repo=%s", prNumber, inv.gh.Repo))
	if !inv.gh.Enabled() {
		emit("       → GitHub not configured (set GITHUB_TOKEN + GITHUB_REPO)")
		return map[string]any{"error": "github not configured"}, nil
	}
	diff, err := actions.FetchPRDiff(ctx, inv.gh, prNumber)
	if err != nil {
		emit(fmt.Sprintf("       → error: %v", err))
		return map[string]any{"error": err.Error()}, nil
	}
	emit(fmt.Sprintf("       → %d bytes of diff retrieved", len(diff)))
	return map[string]any{"pr_number": prNumber, "diff": diff}, nil
}

func (inv *Investigator) toolFetchFile(ctx context.Context, args map[string]any, emit func(string)) (map[string]any, error) {
	path, _ := args["path"].(string)
	emit(fmt.Sprintf("[tool] fetch_file path=%s repo=%s", path, inv.gh.Repo))
	if !inv.gh.Enabled() {
		emit("       → GitHub not configured")
		return map[string]any{"error": "github not configured"}, nil
	}
	fc, err := actions.FetchFile(ctx, inv.gh, path)
	if err != nil {
		emit(fmt.Sprintf("       → error: %v", err))
		return map[string]any{"error": err.Error()}, nil
	}
	emit(fmt.Sprintf("       → %d bytes retrieved", len(fc.Content)))
	return map[string]any{"path": fc.Path, "content": fc.Content}, nil
}

func (inv *Investigator) toolOpenFixPR(ctx context.Context, args map[string]any, emit func(string), result *Result) (map[string]any, error) {
	path, _ := args["file_path"].(string)
	content, _ := args["fixed_content"].(string)
	title, _ := args["pr_title"].(string)
	body, _ := args["pr_body"].(string)
	emit(fmt.Sprintf("[tool] open_fix_pr file=%s repo=%s", path, inv.gh.Repo))
	if !inv.gh.Enabled() {
		emit("       → GitHub not configured — fix PR skipped")
		return map[string]any{"error": "github not configured"}, nil
	}
	pr, err := actions.OpenFixPR(ctx, inv.gh, actions.FixPROptions{
		Path: path, NewContent: content, Title: title, Body: body,
	})
	if err != nil {
		emit(fmt.Sprintf("       → error: %v", err))
		return map[string]any{"error": err.Error()}, nil
	}
	result.FixPRURL = pr.URL
	result.FixPRNumber = pr.Number
	emit(fmt.Sprintf("       → ✓ remediation PR #%d opened: %s", pr.Number, pr.URL))
	return map[string]any{"pr_number": pr.Number, "pr_url": pr.URL}, nil
}

func argInt(v any) int {
	switch n := v.(type) {
	case float64:
		return int(n)
	case int:
		return n
	case string:
		i, _ := strconv.Atoi(n)
		return i
	default:
		return 0
	}
}

func investigationTools() []*genai.FunctionDeclaration {
	str := func(desc string) *genai.Schema { return &genai.Schema{Type: genai.TypeString, Description: desc} }
	obj := func(props map[string]*genai.Schema, req []string) *genai.Schema {
		return &genai.Schema{Type: genai.TypeObject, Properties: props, Required: req}
	}
	return []*genai.FunctionDeclaration{
		{
			Name:        "query_daily_cost",
			Description: "Returns the daily cost and call count timeline for one API key. Use to see the shape of the spike — when it started and whether volume or cost-per-call moved.",
			Parameters: obj(map[string]*genai.Schema{
				"api_key_id": str("API key ID, e.g. 'prod-frontend'"),
				"start_date": str("Start date inclusive, YYYY-MM-DD"),
				"end_date":   str("End date inclusive, YYYY-MM-DD"),
			}, []string{"api_key_id", "start_date", "end_date"}),
		},
		{
			Name:        "query_model_distribution",
			Description: "Returns per-model and per-prompt-hash call counts and costs for a key over a date range. Use to see which model and which prompt drove the cost.",
			Parameters: obj(map[string]*genai.Schema{
				"api_key_id": str("API key ID"),
				"start_date": str("Start date inclusive, YYYY-MM-DD"),
				"end_date":   str("End date inclusive, YYYY-MM-DD"),
			}, []string{"api_key_id", "start_date", "end_date"}),
		},
		{
			Name:        "compare_keys",
			Description: "Returns cost and call count for every API key on a single date. Use to confirm whether the spike is isolated to one key (a deploy) or system-wide (a price change).",
			Parameters: obj(map[string]*genai.Schema{
				"date": str("Date to compare, YYYY-MM-DD"),
			}, []string{"date"}),
		},
		{
			Name:        "get_deploys_in_window",
			Description: "Returns deploys that completed within a time window, including the PR number and repo. Use to find the deploy coinciding with the anomaly.",
			Parameters: obj(map[string]*genai.Schema{
				"start_time": str("Window start, RFC3339 or YYYY-MM-DD"),
				"end_time":   str("Window end, RFC3339 or YYYY-MM-DD"),
			}, []string{"start_time", "end_time"}),
		},
		{
			Name:        "diff_prompt_model_mix",
			Description: "Compares the model distribution for a prompt hash 7 days before vs 7 days after a pivot timestamp. Use to confirm a model change at a deploy.",
			Parameters: obj(map[string]*genai.Schema{
				"prompt_hash": str("The 12-char prompt fingerprint"),
				"pivot_time":  str("Pivot timestamp (RFC3339 or YYYY-MM-DD), usually the deploy time"),
			}, []string{"prompt_hash", "pivot_time"}),
		},
		{
			Name:        "fetch_pr_diff",
			Description: "Fetches the real unified code diff of a pull request from GitHub. Use this to read what a suspect deploy actually changed.",
			Parameters: obj(map[string]*genai.Schema{
				"pr_number": str("The pull request number"),
			}, []string{"pr_number"}),
		},
		{
			Name:        "fetch_file",
			Description: "Fetches the current full content of a source file from GitHub's default branch. Use before open_fix_pr so you can write a complete corrected file.",
			Parameters: obj(map[string]*genai.Schema{
				"path": str("Repo-relative file path, e.g. 'summarizer.py'"),
			}, []string{"path"}),
		},
		{
			Name:        "open_fix_pr",
			Description: "Opens a remediation pull request on GitHub with a corrected file. Call this only after you have read the diff and the current file and are confident the change fixes the regression.",
			Parameters: obj(map[string]*genai.Schema{
				"file_path":     str("Repo-relative path of the file to fix"),
				"fixed_content": str("The complete corrected content of the file"),
				"pr_title":      str("Pull request title"),
				"pr_body":       str("Pull request description — explain the regression and the fix"),
			}, []string{"file_path", "fixed_content", "pr_title", "pr_body"}),
		},
	}
}

// chatTools is the read-only subset exposed to the chat assistant.
func chatTools() []*genai.FunctionDeclaration {
	var out []*genai.FunctionDeclaration
	for _, t := range investigationTools() {
		if t.Name == "open_fix_pr" {
			continue
		}
		out = append(out, t)
	}
	return out
}
