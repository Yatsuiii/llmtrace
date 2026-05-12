package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"

	"google.golang.org/genai"

	"github.com/Yatsuiii/llmtrace/internal/storage"
)

var retryAfterRe = regexp.MustCompile(`retry in (\d+)`)

// generateWithRetry calls GenerateContent, retrying up to 3 times on 429/503
// after the suggested delay (or 20s fallback). Free-tier keys have low RPM.
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

const defaultModel = "gemini-2.5-pro"

const systemPrompt = `You are an AI FinOps investigation agent for LLM spend.
Your job: given a spend anomaly on a production API key, find the root cause by
querying the call ledger and deploy history.

Workflow:
1. Call query_model_distribution to see what models were used around the anomaly date.
2. Call get_deploys_in_window with a ±4h window around the anomaly date to find nearby deploys.
3. If you found a suspicious deploy AND a prompt hash that dominates anomalous traffic,
   call diff_prompt_model_mix to compare model mix before vs after the deploy.
4. Synthesize your findings into a clear attribution: which deploy caused it, what changed,
   confidence score (0–1), and a one-sentence recommendation.

Be concise. Use dollar signs and percentages. Emit a confidence score at the end.`

type Investigator struct {
	db    *storage.DB
	model string
	gc    *genai.Client
}

func New(db *storage.DB, apiKey, model string) (*Investigator, error) {
	ctx := context.Background()
	gc, err := genai.NewClient(ctx, &genai.ClientConfig{APIKey: apiKey})
	if err != nil {
		return nil, fmt.Errorf("create genai client: %w", err)
	}
	if model == "" {
		model = defaultModel
	}
	return &Investigator{db: db, model: model, gc: gc}, nil
}

func (inv *Investigator) Investigate(ctx context.Context, a storage.AnomalyRow, emit func(string)) error {
	emit(fmt.Sprintf("anomaly: key=%s date=%s actual=$%.2f baseline=$%.2f delta=+$%.2f sigma=%.1fσ",
		a.APIKeyID, a.Date, a.ActualValue, a.BaselineValue, a.Delta, a.Sigma))
	emit("")

	tools := []*genai.Tool{{FunctionDeclarations: toolDeclarations()}}
	cfg := &genai.GenerateContentConfig{
		SystemInstruction: genai.NewContentFromText(systemPrompt, genai.RoleUser),
		Tools:             tools,
	}

	history := []*genai.Content{
		genai.NewContentFromText(
			fmt.Sprintf("Investigate this spend anomaly:\nKey: %s\nDate: %s\nActual: $%.2f\nBaseline (7d avg): $%.2f\nDelta: +$%.2f (%.1fσ)",
				a.APIKeyID, a.Date, a.ActualValue, a.BaselineValue, a.Delta, a.Sigma),
			genai.RoleUser,
		),
	}

	// Agentic loop — max 6 tool rounds before forcing final answer.
	for round := 0; round < 6; round++ {
		resp, err := inv.generateWithRetry(ctx, history, cfg, emit)
		if err != nil {
			return fmt.Errorf("generate content round %d: %w", round, err)
		}

		// Append model turn to history.
		if len(resp.Candidates) > 0 && resp.Candidates[0].Content != nil {
			history = append(history, resp.Candidates[0].Content)
		}

		fcs := resp.FunctionCalls()
		if len(fcs) == 0 {
			// Model returned text directly — emit it without another API call.
			txt := resp.Text()
			emit("")
			emit("── Attribution " + strings.Repeat("─", 50))
			for _, line := range strings.Split(txt, "\n") {
				emit(line)
			}
			return nil
		}

		// Execute tools and collect responses.
		var responseParts []*genai.Part
		for _, fc := range fcs {
			result, err := inv.dispatch(ctx, fc, emit)
			if err != nil {
				result = map[string]any{"error": err.Error()}
			}
			responseParts = append(responseParts, genai.NewPartFromFunctionResponse(fc.Name, result))
		}
		history = append(history, genai.NewContentFromParts(responseParts, genai.RoleUser))
	}

	// Fallback if max rounds hit without a text response.
	emit("[max tool rounds reached — requesting final answer]")
	return inv.streamFinal(ctx, history, cfg, emit)
}

func (inv *Investigator) streamFinal(ctx context.Context, history []*genai.Content, cfg *genai.GenerateContentConfig, emit func(string)) error {
	// Disable tools so the model produces the attribution text.
	finalCfg := &genai.GenerateContentConfig{
		SystemInstruction: cfg.SystemInstruction,
	}
	resp, err := inv.generateWithRetry(ctx, history, finalCfg, emit)
	if err != nil {
		return fmt.Errorf("final response: %w", err)
	}
	txt := resp.Text()
	emit("")
	emit("── Attribution " + strings.Repeat("─", 50))
	for _, line := range strings.Split(txt, "\n") {
		emit(line)
	}
	return nil
}

func (inv *Investigator) dispatch(ctx context.Context, fc *genai.FunctionCall, emit func(string)) (map[string]any, error) {
	switch fc.Name {
	case "query_model_distribution":
		return inv.toolModelDistribution(ctx, fc.Args, emit)
	case "get_deploys_in_window":
		return inv.toolDeploysInWindow(ctx, fc.Args, emit)
	case "diff_prompt_model_mix":
		return inv.toolPromptModelMix(ctx, fc.Args, emit)
	default:
		return map[string]any{"error": "unknown tool: " + fc.Name}, nil
	}
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
	end = end.Add(24 * time.Hour) // end date inclusive

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
		StartedAt string `json:"started_at"`
		CommitSHA string `json:"commit_sha"`
	}
	var entries []entry
	for _, d := range deploys {
		entries = append(entries, entry{d.ID, d.PRNumber, d.Title, d.StartedAt.Format(time.RFC3339), d.CommitSHA})
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

func toolDeclarations() []*genai.FunctionDeclaration {
	str := func(desc string) *genai.Schema { return &genai.Schema{Type: genai.TypeString, Description: desc} }
	obj := func(desc string, props map[string]*genai.Schema, req []string) *genai.Schema {
		return &genai.Schema{Type: genai.TypeObject, Description: desc, Properties: props, Required: req}
	}
	return []*genai.FunctionDeclaration{
		{
			Name:        "query_model_distribution",
			Description: "Returns per-model and per-prompt-hash call counts and costs for a given API key and date range. Use this first to understand what changed around the anomaly date.",
			Parameters: obj("", map[string]*genai.Schema{
				"api_key_id": str("The API key ID to query (e.g. 'prod-frontend')"),
				"start_date": str("Start date inclusive, YYYY-MM-DD"),
				"end_date":   str("End date inclusive, YYYY-MM-DD"),
			}, []string{"api_key_id", "start_date", "end_date"}),
		},
		{
			Name:        "get_deploys_in_window",
			Description: "Returns deploys that completed within a time window. Use to find the deploy that coincides with the anomaly.",
			Parameters: obj("", map[string]*genai.Schema{
				"start_time": str("Window start, RFC3339 or YYYY-MM-DD"),
				"end_time":   str("Window end, RFC3339 or YYYY-MM-DD"),
			}, []string{"start_time", "end_time"}),
		},
		{
			Name:        "diff_prompt_model_mix",
			Description: "Compares the model distribution for a specific prompt hash 7 days before vs 7 days after a pivot timestamp. Use to confirm a model change caused the cost spike.",
			Parameters: obj("", map[string]*genai.Schema{
				"prompt_hash": str("The 12-char prompt fingerprint to analyse"),
				"pivot_time":  str("The pivot timestamp (RFC3339 or YYYY-MM-DD), typically the deploy time"),
			}, []string{"prompt_hash", "pivot_time"}),
		},
	}
}
