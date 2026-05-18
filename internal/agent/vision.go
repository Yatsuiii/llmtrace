package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"google.golang.org/genai"

	"github.com/Yatsuiii/llmtrace/internal/actions"
)

// VisionDay is one day of spend read off a billing-dashboard screenshot.
type VisionDay struct {
	Date    string  `json:"date"`
	CostUSD float64 `json:"cost_usd"`
}

// VisionReport is the structured spend data Gemini extracts from an image.
type VisionReport struct {
	Days         []VisionDay `json:"days"`
	AnomalyDate  string      `json:"anomaly_date"`
	AnomalyCost  float64     `json:"anomaly_cost"`
	BaselineCost float64     `json:"baseline_cost"`
	Summary      string      `json:"summary"`
}

const visionExtractPrompt = `You are looking at a screenshot of an LLM API
billing or usage dashboard. Extract the daily spend data you can see.

Return ONLY a JSON object, no prose, no markdown fences:
{
  "days": [{"date": "YYYY-MM-DD", "cost_usd": 0.0}],
  "anomaly_date": "YYYY-MM-DD",
  "anomaly_cost": 0.0,
  "baseline_cost": 0.0,
  "summary": "one sentence describing what the dashboard shows"
}

Read every dated spend figure visible — prefer a table if one is present.
"anomaly_date" is the single day with the largest abnormal jump in spend.
"baseline_cost" is the typical daily spend before that jump.`

const visionInvestigateSystemPrompt = `You are an autonomous LLM-cost
investigation agent. A billing-dashboard screenshot has revealed a spend spike.
You do NOT have a call ledger — your evidence is the screenshot figures plus the
GitHub repository.

Investigate: call list_recent_prs with a window that STARTS at least 7 days
before the spike date and ENDS 2 days after it — deploys can land shortly
before or on the day the spend rises. Read the diff of each returned PR with
fetch_pr_diff. Rule out changes with no LLM-cost impact (dependency bumps,
docs, styling). A cost incident has ONE root cause — converge on it.

When you have found the responsible PR and it is a clear regression (a switch
to a costlier model, a retry/duplication loop, a raised token limit), use
fetch_file to read the current source, write a corrected version, and call
open_fix_pr to ship the fix.

Finish with an attribution: the PR responsible, the exact code at fault, the
likely cost driver (price-per-call vs volume), a confidence score (0-1), and
the remediation PR link if you opened one.`

// ExtractSpend sends a billing screenshot to Gemini and returns structured
// spend data — the multimodal entry point for Vision Import.
func (inv *Investigator) ExtractSpend(ctx context.Context, imageBytes []byte, mimeType string, emit func(string)) (VisionReport, error) {
	emit(fmt.Sprintf("[vision] reading screenshot with Gemini (%d KB, %s)...", len(imageBytes)/1024, mimeType))
	parts := []*genai.Part{
		genai.NewPartFromText(visionExtractPrompt),
		genai.NewPartFromBytes(imageBytes, mimeType),
	}
	history := []*genai.Content{genai.NewContentFromParts(parts, genai.RoleUser)}

	resp, err := inv.generateWithRetry(ctx, history, &genai.GenerateContentConfig{}, emit)
	if err != nil {
		return VisionReport{}, fmt.Errorf("vision extract: %w", err)
	}
	var report VisionReport
	if err := json.Unmarshal([]byte(stripFence(resp.Text())), &report); err != nil {
		return VisionReport{}, fmt.Errorf("parse vision output: %w", err)
	}
	return report, nil
}

// InvestigateFromVision runs a GitHub-only investigation for a spike that was
// identified from a screenshot (no call ledger available).
func (inv *Investigator) InvestigateFromVision(ctx context.Context, report VisionReport, emit func(string)) (Result, error) {
	var result Result
	cfg := &genai.GenerateContentConfig{
		SystemInstruction: genai.NewContentFromText(visionInvestigateSystemPrompt, genai.RoleUser),
		Tools:             []*genai.Tool{{FunctionDeclarations: visionInvestigationTools()}},
	}
	history := []*genai.Content{
		genai.NewContentFromText(fmt.Sprintf(
			"A billing screenshot shows an LLM spend anomaly.\n\n"+
				"Spike date: %s\nSpend that day: $%.2f\nNormal daily spend: $%.2f\n\n"+
				"Investigate the connected GitHub repository and determine which deploy caused it.",
			report.AnomalyDate, report.AnomalyCost, report.BaselineCost), genai.RoleUser),
	}

	for round := 0; round < 14; round++ {
		resp, err := inv.generateWithRetry(ctx, history, cfg, emit)
		if err != nil {
			return result, fmt.Errorf("vision investigate round %d: %w", round, err)
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

func (inv *Investigator) toolListRecentPRs(ctx context.Context, args map[string]any, emit func(string)) (map[string]any, error) {
	startStr, _ := args["start_date"].(string)
	endStr, _ := args["end_date"].(string)
	emit(fmt.Sprintf("[tool] list_recent_prs %s → %s repo=%s", startStr, endStr, inv.gh.Repo))
	if !inv.gh.Enabled() {
		return map[string]any{"error": "github not configured"}, nil
	}
	start, err := time.Parse("2006-01-02", startStr)
	if err != nil {
		return nil, fmt.Errorf("parse start_date: %w", err)
	}
	end, err := time.Parse("2006-01-02", endStr)
	if err != nil {
		return nil, fmt.Errorf("parse end_date: %w", err)
	}
	prs, err := actions.ListRecentPRs(ctx, inv.gh, start, end.Add(24*time.Hour))
	if err != nil {
		emit(fmt.Sprintf("       → error: %v", err))
		return map[string]any{"error": err.Error()}, nil
	}
	type entry struct {
		Number   int    `json:"pr_number"`
		Title    string `json:"title"`
		MergedAt string `json:"merged_at"`
	}
	var entries []entry
	for _, p := range prs {
		entries = append(entries, entry{p.Number, p.Title, p.MergedAt.Format(time.RFC3339)})
	}
	emit(fmt.Sprintf("       → %d merged PR(s) found", len(entries)))
	return map[string]any{"pull_requests": entries}, nil
}

func visionInvestigationTools() []*genai.FunctionDeclaration {
	str := func(desc string) *genai.Schema { return &genai.Schema{Type: genai.TypeString, Description: desc} }
	obj := func(props map[string]*genai.Schema, req []string) *genai.Schema {
		return &genai.Schema{Type: genai.TypeObject, Properties: props, Required: req}
	}
	listPRs := &genai.FunctionDeclaration{
		Name:        "list_recent_prs",
		Description: "Lists pull requests merged into the connected GitHub repo within a date range. Use to find deploys near the spike date.",
		Parameters: obj(map[string]*genai.Schema{
			"start_date": str("Window start, YYYY-MM-DD"),
			"end_date":   str("Window end, YYYY-MM-DD"),
		}, []string{"start_date", "end_date"}),
	}
	out := []*genai.FunctionDeclaration{listPRs}
	for _, t := range investigationTools() {
		switch t.Name {
		case "fetch_pr_diff", "fetch_file", "open_fix_pr":
			out = append(out, t)
		}
	}
	return out
}

// stripFence removes a leading ```json / ``` fence and trailing ``` if present.
func stripFence(s string) string {
	s = strings.TrimSpace(s)
	if strings.HasPrefix(s, "```") {
		if i := strings.IndexByte(s, '\n'); i >= 0 {
			s = s[i+1:]
		}
		s = strings.TrimSuffix(strings.TrimSpace(s), "```")
	}
	return strings.TrimSpace(s)
}
