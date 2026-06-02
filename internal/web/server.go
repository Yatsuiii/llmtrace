package web

import (
	"context"
	"encoding/json"
	"fmt"
	"html/template"
	"net/http"
	"os"
	"sort"
	"time"

	"github.com/Yatsuiii/llmtrace/internal/agent"
	"github.com/Yatsuiii/llmtrace/internal/detect"
	"github.com/Yatsuiii/llmtrace/internal/ingest"
	"github.com/Yatsuiii/llmtrace/internal/proxy"
	"github.com/Yatsuiii/llmtrace/internal/storage"
	"github.com/Yatsuiii/llmtrace/internal/watcher"
)

func Serve(ctx context.Context, db *storage.DB, port int, w *watcher.Watcher) error {
	mux := http.NewServeMux()
	mux.HandleFunc("/", dashboardHandler(db, w))
	mux.HandleFunc("/investigate", investigateHandler(db))
	mux.HandleFunc("/chat", chatHandler(db))
	mux.HandleFunc("/vision", visionHandler(db))
	mux.HandleFunc("/events", eventsHandler(w))
	mux.HandleFunc("/ingest/call", ingest.CallHandler(db))
	mux.HandleFunc("/ingest/deploy", ingest.DeployHandler(db))
	mux.HandleFunc("/v1/messages", proxy.Handler(db))

	addr := fmt.Sprintf(":%d", port)
	fmt.Printf("llmtrace dashboard → http://localhost%s\n", addr)
	srv := &http.Server{Addr: addr, Handler: mux}
	go func() {
		<-ctx.Done()
		srv.Shutdown(context.Background())
	}()
	return srv.ListenAndServe()
}

func dashboardHandler(db *storage.DB, w *watcher.Watcher) http.HandlerFunc {
	return func(rw http.ResponseWriter, r *http.Request) {
		ctx := r.Context()
		since := time.Now().UTC().AddDate(0, 0, -30)

		if _, err := detect.Run(ctx, db, detect.DefaultConfig(), since); err != nil {
			http.Error(rw, err.Error(), http.StatusInternalServerError)
			return
		}
		anomalies, err := db.ListAnomalies(ctx, since)
		if err != nil {
			http.Error(rw, err.Error(), http.StatusInternalServerError)
			return
		}
		daily, err := db.DailyCostByKey(ctx, since)
		if err != nil {
			http.Error(rw, err.Error(), http.StatusInternalServerError)
			return
		}
		deploys, err := db.ListDeploys(ctx, since)
		if err != nil {
			http.Error(rw, err.Error(), http.StatusInternalServerError)
			return
		}
		actions, err := db.ListAgentActions(ctx, since)
		if err != nil {
			http.Error(rw, err.Error(), http.StatusInternalServerError)
			return
		}

		chartJSON, err := buildChartJSON(daily, deploys)
		if err != nil {
			http.Error(rw, err.Error(), http.StatusInternalServerError)
			return
		}

		var watcherStatus watcherStatusData
		if w != nil {
			watcherStatus.Active = true
			watcherStatus.LastScan = formatTime(w.LastScan)
			watcherStatus.NextScan = formatTime(w.NextScan)
			watcherStatus.ScanCount = w.ScanCount
		}

		var projection projectionData
		if len(anomalies) > 0 {
			projection = buildProjection(daily, anomalies[0].APIKeyID)
		}

		var totalSpend float64
		for _, d := range daily {
			totalSpend += d.CostUSD
		}

		data := dashboardData{
			Anomalies:     anomalies,
			AgentActions:  actions,
			Now:           time.Now().UTC().Format("2006-01-02 15:04 UTC"),
			ChartDataJSON: template.JS(chartJSON),
			Watcher:       watcherStatus,
			Projection:    projection,
			TotalSpend:    totalSpend,
		}
		rw.Header().Set("Content-Type", "text/html; charset=utf-8")
		if err := dashboardTmpl.Execute(rw, data); err != nil {
			http.Error(rw, err.Error(), http.StatusInternalServerError)
		}
	}
}

func eventsHandler(w *watcher.Watcher) http.HandlerFunc {
	return func(rw http.ResponseWriter, r *http.Request) {
		if w == nil {
			http.Error(rw, "autonomous mode not enabled", http.StatusServiceUnavailable)
			return
		}
		rw.Header().Set("Content-Type", "text/event-stream")
		rw.Header().Set("Cache-Control", "no-cache")
		rw.Header().Set("X-Accel-Buffering", "no")
		flusher, ok := rw.(http.Flusher)
		if !ok {
			http.Error(rw, "streaming not supported", http.StatusInternalServerError)
			return
		}

		ch := w.Subscribe()
		defer w.Unsubscribe(ch)

		fmt.Fprintf(rw, "data: [connected to autonomous watcher]\n\n")
		flusher.Flush()

		for {
			select {
			case <-r.Context().Done():
				return
			case msg := <-ch:
				fmt.Fprintf(rw, "data: %s\n\n", msg)
				flusher.Flush()
			}
		}
	}
}

func chatHandler(db *storage.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query().Get("q")
		if q == "" {
			http.Error(w, "missing q", http.StatusBadRequest)
			return
		}
		apiKey := os.Getenv("GEMINI_API_KEY")
		if apiKey == "" {
			http.Error(w, "GEMINI_API_KEY not set", http.StatusServiceUnavailable)
			return
		}
		ctx := r.Context()
		inv, err := agent.New(db, apiKey, os.Getenv("GEMINI_MODEL"))
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}

		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("X-Accel-Buffering", "no")
		flusher, ok := w.(http.Flusher)
		if !ok {
			http.Error(w, "streaming not supported", http.StatusInternalServerError)
			return
		}
		emit := func(msg string) {
			fmt.Fprintf(w, "data: %s\n\n", msg)
			flusher.Flush()
		}

		var note string
		if anomalies, err := db.ListAnomalies(ctx, time.Now().UTC().AddDate(0, 0, -30)); err == nil && len(anomalies) > 0 {
			note = fmt.Sprintf("%d spend anomaly(ies) detected in the last 30 days; the largest is on key %s, date %s.",
				len(anomalies), anomalies[0].APIKeyID, anomalies[0].Date)
		}
		if err := inv.Chat(ctx, q, note, emit); err != nil {
			emit(fmt.Sprintf("[error] %v", err))
		}
		emit("[[END]]")
	}
}

type watcherStatusData struct {
	Active    bool
	LastScan  string
	NextScan  string
	ScanCount int
}

type dashboardData struct {
	Anomalies     []storage.AnomalyRow
	AgentActions  []storage.AgentActionRow
	Now           string
	ChartDataJSON template.JS
	Watcher       watcherStatusData
	Projection    projectionData
	TotalSpend    float64
}

type projectionData struct {
	Show        bool
	Key         string
	MonthlyNow  float64
	MonthlyBase float64
	Overspend   float64
}

// buildProjection estimates monthly run-rate for a key from its daily series:
// baseline = mean of the earliest 7 days, current = mean of the latest 7 days.
func buildProjection(daily []storage.KeyDailyCost, key string) projectionData {
	var series []float64
	dates := map[string]float64{}
	for _, d := range daily {
		if d.APIKeyID == key {
			dates[d.Date] = d.CostUSD
		}
	}
	var keys []string
	for d := range dates {
		keys = append(keys, d)
	}
	sort.Strings(keys)
	for _, d := range keys {
		series = append(series, dates[d])
	}
	if len(series) < 14 {
		return projectionData{}
	}
	mean := func(xs []float64) float64 {
		var s float64
		for _, x := range xs {
			s += x
		}
		return s / float64(len(xs))
	}
	base := mean(series[:7])
	now := mean(series[len(series)-7:])
	p := projectionData{
		Key:         key,
		MonthlyNow:  now * 30,
		MonthlyBase: base * 30,
		Overspend:   (now - base) * 30,
	}
	p.Show = p.Overspend > 1
	return p
}

func formatTime(t time.Time) string {
	if t.IsZero() {
		return "—"
	}
	return t.Format("15:04:05 UTC")
}

var keyColors = map[string]string{
	"prod-frontend":   "#ff6b6b",
	"internal-tools":  "#9a86ff",
	"background-jobs": "#5b6178",
}

func buildChartJSON(daily []storage.KeyDailyCost, deploys []storage.DeployRow) (string, error) {
	dateSet := map[string]struct{}{}
	keyData := map[string]map[string]float64{}
	for _, d := range daily {
		dateSet[d.Date] = struct{}{}
		if keyData[d.APIKeyID] == nil {
			keyData[d.APIKeyID] = map[string]float64{}
		}
		keyData[d.APIKeyID][d.Date] = d.CostUSD
	}
	var labels []string
	for d := range dateSet {
		labels = append(labels, d)
	}
	sort.Strings(labels)

	var keys []string
	for k := range keyData {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool {
		if keys[i] == "prod-frontend" {
			return true
		}
		if keys[j] == "prod-frontend" {
			return false
		}
		return keys[i] < keys[j]
	})

	type dataset struct {
		Label           string    `json:"label"`
		Data            []float64 `json:"data"`
		BorderColor     string    `json:"borderColor"`
		BackgroundColor string    `json:"backgroundColor"`
		BorderWidth     int       `json:"borderWidth"`
		PointRadius     int       `json:"pointRadius"`
		Tension         float64   `json:"tension"`
	}
	var datasets []dataset
	for _, k := range keys {
		color, ok := keyColors[k]
		if !ok {
			color = "#64748b"
		}
		var pts []float64
		for _, lbl := range labels {
			pts = append(pts, keyData[k][lbl])
		}
		datasets = append(datasets, dataset{
			Label:           k,
			Data:            pts,
			BorderColor:     color,
			BackgroundColor: color + "22",
			BorderWidth:     2,
			PointRadius:     0,
			Tension:         0.3,
		})
	}

	type annotationLine struct {
		Type        string `json:"type"`
		XMin        string `json:"xMin"`
		XMax        string `json:"xMax"`
		BorderColor string `json:"borderColor"`
		BorderWidth int    `json:"borderWidth"`
		BorderDash  []int  `json:"borderDash"`
		Label       struct {
			Display         bool   `json:"display"`
			Content         string `json:"content"`
			BackgroundColor string `json:"backgroundColor"`
			Color           string `json:"color"`
			Font            struct {
				Size int `json:"size"`
			} `json:"font"`
			Position string `json:"position"`
		} `json:"label"`
	}
	// One marker per deploy day — multiple deploys on a day collapse to a count.
	deploysByDate := map[string]int{}
	for _, d := range deploys {
		deploysByDate[d.StartedAt.Format("2006-01-02")]++
	}
	annotations := map[string]annotationLine{}
	i := 0
	for date, n := range deploysByDate {
		var a annotationLine
		a.Type = "line"
		a.XMin = date
		a.XMax = date
		a.BorderColor = "#fbbf24"
		a.BorderWidth = 2
		a.BorderDash = []int{6, 4}
		a.Label.Display = true
		if n == 1 {
			a.Label.Content = "deploy"
		} else {
			a.Label.Content = fmt.Sprintf("%d deploys", n)
		}
		a.Label.BackgroundColor = "#fbbf2422"
		a.Label.Color = "#fbbf24"
		a.Label.Font.Size = 11
		a.Label.Position = "start"
		annotations[fmt.Sprintf("deploy%d", i)] = a
		i++
	}

	out, err := json.Marshal(map[string]any{
		"labels":      labels,
		"datasets":    datasets,
		"annotations": annotations,
	})
	return string(out), err
}

var dashboardTmpl = template.Must(template.New("dashboard").Parse(`<!DOCTYPE html>
<html lang="en">
<head>
<meta charset="UTF-8">
<meta name="viewport" content="width=device-width, initial-scale=1.0">
<title>llmtrace · autonomous LLM-cost agent</title>
<link rel="preconnect" href="https://fonts.googleapis.com">
<link rel="preconnect" href="https://fonts.gstatic.com" crossorigin>
<link href="https://fonts.googleapis.com/css2?family=Inter:wght@400;500;600;700;800&family=JetBrains+Mono:wght@400;500&display=swap" rel="stylesheet">
<style>
  :root {
    --bg: #0a0b10;
    --panel: #101219;
    --inset: #0a0b11;
    --border: rgba(255,255,255,0.07);
    --border-lit: rgba(255,255,255,0.13);
    --text: #e8eaf2;
    --muted: #9094a8;
    --dim: #585c70;
    --violet: #9a86ff;
    --indigo: #6366f1;
    --red: #ff6b6b;
    --green: #4ade80;
    --amber: #fbbf24;
    --radius: 10px;
    --mono: 'JetBrains Mono', 'Cascadia Code', monospace;
  }
  * { box-sizing: border-box; margin: 0; padding: 0; }
  html { scroll-behavior: smooth; }
  body {
    font-family: 'Inter', -apple-system, system-ui, sans-serif;
    background: var(--bg); color: var(--text); min-height: 100vh;
    -webkit-font-smoothing: antialiased; position: relative; overflow-x: hidden;
  }
  ::selection { background: rgba(154,134,255,0.32); }

  .header { display: flex; align-items: center; gap: 12px;
    max-width: 1140px; margin: 0 auto; padding: 24px 32px 0; }
  .logo { font-size: 1.18rem; font-weight: 800; letter-spacing: -0.03em; color: #fff; }
  .logo span { color: var(--violet); }
  .header .ts { margin-left: auto; font-size: 0.76rem; color: var(--dim);
    font-variant-numeric: tabular-nums; }

  .hero { max-width: 1140px; margin: 0 auto; padding: 34px 32px 6px; }
  .hero .eyebrow { font-size: 0.72rem; font-weight: 700; letter-spacing: 0.17em;
    text-transform: uppercase; color: var(--violet); margin-bottom: 15px; }
  .hero h1 { font-size: 2.6rem; font-weight: 800; letter-spacing: -0.038em; line-height: 1.07;
    color: #f4f5fa; }
  .hero p { margin-top: 15px; font-size: 1.02rem; color: var(--muted);
    max-width: 64ch; line-height: 1.62; }

  .kpi-row { max-width: 1140px; margin: 28px auto 4px; padding: 0 32px;
    display: grid; grid-template-columns: repeat(3,1fr); gap: 16px; }
  .kpi { background: var(--panel); border: 1px solid var(--border);
    border-radius: var(--radius); padding: 18px 22px; position: relative; overflow: hidden;
    transition: border-color 0.2s; }
  .kpi:hover { border-color: var(--border-lit); }
  .kpi::after { content: ''; position: absolute; left: 0; top: 0; bottom: 0; width: 3px; }
  .kpi.k-spend::after { background: var(--violet); }
  .kpi.k-anom::after { background: var(--red); }
  .kpi.k-act::after { background: var(--green); }
  .kpi .kpi-lbl { font-size: 0.69rem; font-weight: 700; letter-spacing: 0.09em;
    text-transform: uppercase; color: var(--dim); }
  .kpi .kpi-val { font-size: 2rem; font-weight: 800; margin-top: 7px;
    letter-spacing: -0.025em; font-variant-numeric: tabular-nums; color: #fff; }
  .kpi.k-anom .kpi-val { color: var(--red); }
  .kpi.k-act .kpi-val { color: var(--green); }
  .kpi .kpi-sub { font-size: 0.77rem; color: var(--muted); margin-top: 3px; }

  .container { max-width: 1140px; margin: 0 auto; padding: 26px 32px 64px; }
  .section-title { font-size: 0.72rem; font-weight: 700; text-transform: uppercase;
    letter-spacing: 0.13em; color: var(--dim); margin: 32px 0 14px;
    display: flex; align-items: center; gap: 9px; }
  .section-title::before { content: ''; width: 14px; height: 2px; border-radius: 2px;
    background: var(--dim); }

  .chart-wrap { background: var(--panel); border: 1px solid var(--border);
    border-radius: var(--radius); padding: 22px 24px; margin-bottom: 26px; }
  .chart-wrap canvas { max-height: 270px; }

  .anomaly-card { background: var(--panel); border: 1px solid rgba(255,107,107,0.22);
    border-left: 2px solid var(--red);
    border-radius: var(--radius); padding: 20px 22px; margin-bottom: 12px; }
  .anomaly-card .top { display: flex; align-items: center; gap: 10px; margin-bottom: 14px; }
  .badge { font-size: 0.62rem; font-weight: 700; padding: 3px 8px; border-radius: 5px;
    text-transform: uppercase; letter-spacing: 0.06em;
    background: rgba(255,107,107,0.14); color: #ffb0b0; border: 1px solid rgba(255,107,107,0.3); }
  .badge.critical { background: rgba(255,107,107,0.14); color: #ffb0b0;
    border-color: rgba(255,107,107,0.3); }
  .badge.high { background: rgba(251,191,36,0.14); color: #fcd575;
    border-color: rgba(251,191,36,0.3); }
  .badge.done { background: rgba(74,222,128,0.13); color: #9af0b8;
    border-color: rgba(74,222,128,0.32); }
  .badge.failed { background: rgba(216,180,254,0.13); color: #e2c8ff;
    border-color: rgba(216,180,254,0.3); }
  .badge.skipped { background: rgba(255,255,255,0.06); color: var(--muted);
    border-color: var(--border); }
  .key-label { font-size: 0.96rem; font-weight: 700; color: #fff; }
  .date-label { font-size: 0.82rem; color: var(--dim); margin-left: auto;
    font-variant-numeric: tabular-nums; }
  .metrics { display: flex; gap: 30px; flex-wrap: wrap; }
  .metric { display: flex; flex-direction: column; gap: 3px; }
  .metric .lbl { color: var(--dim); font-size: 0.66rem; font-weight: 600;
    text-transform: uppercase; letter-spacing: 0.06em; }
  .metric .val { color: #f1f3f9; font-weight: 700; font-size: 1.02rem;
    font-variant-numeric: tabular-nums; }
  .metric .val.hot { color: var(--red); }

  .btn { margin-top: 16px; padding: 8px 16px; font-size: 0.82rem; font-weight: 600;
    font-family: inherit; color: #fff; border: 1px solid var(--indigo); border-radius: 7px;
    cursor: pointer; background: var(--indigo); transition: background 0.15s, border-color 0.15s; }
  .btn:hover { background: #4f52d8; border-color: #4f52d8; }
  .btn:disabled { opacity: 0.45; cursor: default; }

  .stream-box, .chat-output, .live-feed {
    background: var(--inset); border: 1px solid var(--border); border-radius: 10px;
    padding: 14px 16px; font-family: var(--mono); font-size: 0.78rem;
    line-height: 1.7; overflow-y: auto; white-space: pre-wrap; }
  .stream-box { margin-top: 14px; color: var(--muted); max-height: 340px; display: none; }
  .stream-box.visible { display: block; }
  .no-anomalies { color: var(--dim); padding: 18px 0; }

  .projection { background: var(--panel);
    border: 1px solid rgba(251,191,36,0.28); border-left: 2px solid var(--amber);
    border-radius: var(--radius);
    padding: 16px 22px; margin-bottom: 26px; display: flex; align-items: baseline;
    gap: 22px; flex-wrap: wrap; }
  .projection .lbl { font-size: 0.66rem; font-weight: 700; letter-spacing: 0.09em;
    text-transform: uppercase; color: #c9a86a; }
  .projection .big { font-size: 1.65rem; font-weight: 800; color: var(--amber);
    letter-spacing: -0.02em; }
  .projection .sub { font-size: 0.86rem; color: #c8b48f; }

  .chat-box, .vision-panel { background: var(--panel); border: 1px solid var(--border);
    border-radius: var(--radius); padding: 20px 22px; margin-bottom: 26px; }
  .chat-input-row { display: flex; gap: 9px; }
  .chat-input-row input { flex: 1; background: var(--inset); border: 1px solid var(--border);
    border-radius: 8px; padding: 11px 14px; color: var(--text); font-size: 0.88rem;
    font-family: inherit; transition: border-color 0.15s; }
  .chat-input-row input:focus { outline: none; border-color: var(--violet);
    box-shadow: 0 0 0 3px rgba(154,134,255,0.12); }
  .chat-input-row input::placeholder { color: var(--dim); }
  .chat-output { margin-top: 14px; color: var(--muted); max-height: 320px; display: none; }
  .chat-output.visible { display: block; }
  .chat-suggestions { margin-top: 12px; display: flex; gap: 8px; flex-wrap: wrap; }
  .chat-suggestions button { background: rgba(255,255,255,0.05); color: var(--muted);
    border: 1px solid var(--border); border-radius: 20px; padding: 6px 13px;
    font-size: 0.74rem; font-family: inherit; cursor: pointer; transition: all 0.15s; }
  .chat-suggestions button:hover { background: rgba(154,134,255,0.12);
    color: var(--text); border-color: rgba(154,134,255,0.35); }

  .vision-drop { border: 1.5px dashed var(--border-lit); border-radius: 11px; padding: 28px;
    text-align: center; cursor: pointer; transition: border-color 0.15s, background 0.15s; }
  .vision-drop:hover { border-color: var(--violet); background: rgba(154,134,255,0.05); }
  .vision-drop.has-file { border-color: var(--green); background: rgba(74,222,128,0.05); }
  .vision-drop .hint { color: var(--muted); font-size: 0.88rem; line-height: 1.55; }
  .vision-drop .fname { color: #9af0b8; font-size: 0.88rem; font-weight: 600; }
  .vision-tag { font-size: 0.6rem; font-weight: 700; letter-spacing: 0.07em;
    text-transform: uppercase; background: rgba(154,134,255,0.16); color: #c3b6ff;
    border: 1px solid rgba(154,134,255,0.32); padding: 3px 8px; border-radius: 5px; margin-left: 8px; }

  .watcher-bar { background: var(--panel);
    border: 1px solid rgba(74,222,128,0.26); border-left: 2px solid var(--green);
    border-radius: var(--radius);
    padding: 16px 22px; margin-bottom: 26px; display: flex; align-items: center;
    gap: 16px; flex-wrap: wrap; }
  .watcher-bar .pulse { width: 8px; height: 8px; border-radius: 50%; background: var(--green);
    animation: pulse 2s infinite; flex-shrink: 0; }
  @keyframes pulse { 0%,100% { opacity:1; } 50% { opacity:0.35; } }
  .watcher-bar .label { font-size: 0.86rem; font-weight: 700; color: #9af0b8; }
  .watcher-bar .meta { font-size: 0.75rem; color: #6ee79a; opacity: 0.8;
    font-variant-numeric: tabular-nums; }
  .watcher-bar .live-btn { margin-left: auto; padding: 7px 14px; font-size: 0.76rem;
    font-weight: 600; font-family: inherit; background: rgba(74,222,128,0.13); color: #9af0b8;
    border: 1px solid rgba(74,222,128,0.34); border-radius: 8px; cursor: pointer;
    transition: background 0.15s; }
  .watcher-bar .live-btn:hover { background: rgba(74,222,128,0.22); }
  .live-feed { color: #6ee79a; max-height: 320px; margin-bottom: 26px; display: none; }
  .live-feed.visible { display: block; }
  .live-feed .dim { color: var(--dim); }
  .live-feed .tool { color: #79b8ff; }
  .live-feed .attr { color: var(--text); }
  .live-feed .action { color: var(--amber); }

  .actions-table { width: 100%; border-collapse: collapse; font-size: 0.83rem;
    margin-bottom: 26px; background: var(--panel); border: 1px solid var(--border);
    border-radius: var(--radius); overflow: hidden; }
  .actions-table th { text-align: left; color: var(--dim); font-size: 0.66rem; font-weight: 700;
    text-transform: uppercase; letter-spacing: 0.06em; padding: 12px 14px;
    border-bottom: 1px solid var(--border); background: rgba(255,255,255,0.02); }
  .actions-table td { padding: 12px 14px; border-bottom: 1px solid var(--border);
    vertical-align: top; }
  .actions-table tr:last-child td { border-bottom: none; }
  .actions-table tr:hover td { background: rgba(255,255,255,0.02); }
  .actions-table code { font-family: var(--mono); font-size: 0.78rem; }
  .action-result a { color: var(--violet); text-decoration: none; font-weight: 600; }
  .action-result a:hover { text-decoration: underline; }

  @media (max-width: 720px) {
    .kpi-row { grid-template-columns: 1fr; }
    .hero h1 { font-size: 2rem; }
  }
</style>
</head>
<body>
<div class="header">
  <div class="logo">llm<span>trace</span></div>
  <span class="ts">{{.Now}}</span>
</div>
<div class="hero">
  <div class="eyebrow">Autonomous LLM-cost agent</div>
  <h1>Which deploy spiked your LLM bill?</h1>
  <p>llmtrace proxies your LLM traffic, detects per-key spend anomalies, and runs a Gemini
     agent that traces every spike back to the exact pull request, then ships the fix.</p>
</div>
<div class="kpi-row">
  <div class="kpi k-spend">
    <div class="kpi-lbl">30-day spend</div>
    <div class="kpi-val">${{printf "%.2f" .TotalSpend}}</div>
    <div class="kpi-sub">across all tracked keys</div>
  </div>
  <div class="kpi k-anom">
    <div class="kpi-lbl">Anomalies flagged</div>
    <div class="kpi-val">{{len .Anomalies}}</div>
    <div class="kpi-sub">spend spikes detected</div>
  </div>
  <div class="kpi k-act">
    <div class="kpi-lbl">Agent actions</div>
    <div class="kpi-val">{{len .AgentActions}}</div>
    <div class="kpi-sub">PRs, issues &amp; rate-limits</div>
  </div>
</div>
<div class="container">

  {{if .Watcher.Active}}
  <div class="watcher-bar">
    <div class="pulse"></div>
    <div>
      <div class="label">Autonomous Agent · Active</div>
      <div class="meta">scans: {{.Watcher.ScanCount}} · last: {{.Watcher.LastScan}} · next: {{.Watcher.NextScan}}</div>
    </div>
    <button class="live-btn" onclick="toggleLiveFeed(this)">Live Feed ▾</button>
  </div>
  <div class="live-feed" id="liveFeed"></div>
  {{end}}

  {{if .Projection.Show}}
  <div class="projection">
    <div>
      <div class="lbl">PROJECTED MONTHLY SPEND · {{.Projection.Key}}</div>
      <div class="big">${{printf "%.0f" .Projection.MonthlyNow}}/mo</div>
    </div>
    <div class="sub">baseline run-rate ${{printf "%.0f" .Projection.MonthlyBase}}/mo. This regression adds
      <strong style="color:#f87171">+${{printf "%.0f" .Projection.Overspend}}/mo</strong> if left unfixed</div>
  </div>
  {{end}}

  <div class="section-title">Investigate Any Dashboard <span class="vision-tag">Gemini Vision</span></div>
  <div class="vision-panel">
    <div class="vision-drop" id="visionDrop" onclick="document.getElementById('visionFile').click()">
      <div class="hint" id="visionHint">Drop in a screenshot of <strong>any</strong> LLM billing dashboard: Anthropic, OpenAI, a Grafana panel.
        Gemini reads the spend off the image, then the agent finds which deploy caused the spike.</div>
    </div>
    <input type="file" id="visionFile" accept="image/*" style="display:none" onchange="visionPicked()">
    <button class="btn" style="margin-top:12px" onclick="runVision()">Read &amp; Investigate →</button>
    <div class="chat-output" id="visionOutput"></div>
  </div>

  <div class="section-title">Daily Spend · Last 30 Days</div>
  <div class="chart-wrap">
    <canvas id="costChart"></canvas>
  </div>

  <div class="section-title">Ask the Agent</div>
  <div class="chat-box">
    <div class="chat-input-row">
      <input id="chatInput" type="text" autocomplete="off"
        placeholder="Ask about your LLM spend, e.g. why did prod-frontend spike?"
        onkeydown="if(event.key==='Enter')sendChat()">
      <button class="btn" style="margin-top:0" onclick="sendChat()">Ask</button>
    </div>
    <div class="chat-suggestions">
      <button onclick="askThis('Why did prod-frontend spend spike?')">Why did prod-frontend spike?</button>
      <button onclick="askThis('How much extra will this cost per month if unfixed?')">Monthly cost impact?</button>
      <button onclick="askThis('Did the control keys change at all around the deploy?')">Did the controls change?</button>
    </div>
    <div class="chat-output" id="chatOutput"></div>
  </div>

  <div class="section-title">Spend Anomalies</div>
  {{if .Anomalies}}
  {{range .Anomalies}}
  <div class="anomaly-card">
    <div class="top">
      <span class="badge">anomaly</span>
      <span class="key-label">{{.APIKeyID}}</span>
      <span class="date-label">{{.Date}}</span>
    </div>
    <div class="metrics">
      <div class="metric"><span class="lbl">actual</span><span class="val hot">${{printf "%.2f" .ActualValue}}</span></div>
      <div class="metric"><span class="lbl">baseline 7d</span><span class="val">${{printf "%.2f" .BaselineValue}}</span></div>
      <div class="metric"><span class="lbl">delta</span><span class="val hot">+${{printf "%.2f" .Delta}}</span></div>
      <div class="metric"><span class="lbl">sigma</span><span class="val hot">{{printf "%.1f" .Sigma}}σ</span></div>
      <div class="metric"><span class="lbl">calls</span><span class="val">{{.SampleSize}}</span></div>
    </div>
    <button class="btn" onclick="investigate({{.ID}}, this)">Investigate →</button>
    <div class="stream-box" id="stream-{{.ID}}"></div>
  </div>
  {{end}}
  {{else}}
  <div class="no-anomalies">No anomalies detected in the last 30 days.</div>
  {{end}}

  {{if .AgentActions}}
  <div class="section-title" style="margin-top:28px">Agent Actions Log</div>
  <table class="actions-table">
    <thead>
      <tr>
        <th>Time</th>
        <th>Action</th>
        <th>Anomaly</th>
        <th>Status</th>
        <th>Result</th>
      </tr>
    </thead>
    <tbody>
    {{range .AgentActions}}
    <tr>
      <td style="color:#475569;white-space:nowrap">{{.CreatedAt.Format "01-02 15:04"}}</td>
      <td><code style="color:#94a3b8">{{.ActionType}}</code></td>
      <td style="color:#64748b">#{{.AnomalyID}}</td>
      <td><span class="badge {{.Status}}">{{.Status}}</span></td>
      <td class="action-result" id="result-{{.ID}}">{{.Result}}</td>
    </tr>
    {{end}}
    </tbody>
  </table>
  {{end}}

</div>
<script src="https://cdn.jsdelivr.net/npm/chart.js@4.4.0/dist/chart.umd.min.js"></script>
<script src="https://cdn.jsdelivr.net/npm/chartjs-plugin-annotation@3.0.1/dist/chartjs-plugin-annotation.min.js"></script>
<script>
(function() {
  const d = {{.ChartDataJSON}};
  const ctx = document.getElementById('costChart').getContext('2d');
  new Chart(ctx, {
    type: 'line',
    data: { labels: d.labels, datasets: d.datasets },
    options: {
      responsive: true,
      maintainAspectRatio: true,
      interaction: { mode: 'index', intersect: false },
      scales: {
        x: {
          ticks: { color: '#8e92a6', maxTicksLimit: 10, font: { size: 11 } },
          grid: { color: 'rgba(255,255,255,0.05)' }
        },
        y: {
          ticks: { color: '#8e92a6', font: { size: 11 }, callback: v => '$' + v.toFixed(2) },
          grid: { color: 'rgba(255,255,255,0.05)' }
        }
      },
      plugins: {
        legend: { labels: { color: '#b6b9cf', font: { size: 12 }, boxWidth: 12, usePointStyle: true } },
        tooltip: {
          backgroundColor: '#14161f',
          borderColor: 'rgba(255,255,255,0.12)',
          borderWidth: 1,
          padding: 11,
          titleColor: '#e8eaf2',
          bodyColor: '#9094a8',
          callbacks: { label: ctx => ' ' + ctx.dataset.label + ': $' + ctx.parsed.y.toFixed(2) }
        },
        annotation: { annotations: d.annotations }
      }
    }
  });
})();

async function investigate(id, btn) {
  const box = document.getElementById('stream-' + id);
  box.textContent = '';
  box.classList.add('visible');
  btn.disabled = true;
  btn.textContent = 'Investigating…';

  const resp = await fetch('/investigate?id=' + id);
  const reader = resp.body.getReader();
  const dec = new TextDecoder();
  while (true) {
    const {done, value} = await reader.read();
    if (done) break;
    dec.decode(value).split('\n').forEach(line => {
      if (line.startsWith('data: ')) {
        box.textContent += line.slice(6) + '\n';
        box.scrollTop = box.scrollHeight;
      }
    });
  }
  btn.textContent = 'Re-investigate';
  btn.disabled = false;
}

let liveEs = null;
function toggleLiveFeed(btn) {
  const feed = document.getElementById('liveFeed');
  if (liveEs) {
    liveEs.close(); liveEs = null;
    feed.classList.remove('visible');
    btn.textContent = 'Live Feed ▾';
    return;
  }
  feed.classList.add('visible');
  btn.textContent = 'Live Feed ▴';
  liveEs = new EventSource('/events');
  liveEs.onmessage = e => {
    const msg = e.data;
    const line = document.createElement('div');
    if (msg.startsWith('[tool]')) line.className = 'tool';
    else if (msg.startsWith('── Attribution')) line.className = 'attr';
    else if (msg.startsWith('[watcher]')) line.className = 'action';
    else if (msg.startsWith('[')) line.className = 'dim';
    line.textContent = msg;
    feed.appendChild(line);
    feed.scrollTop = feed.scrollHeight;
  };
}

function visionPicked() {
  const f = document.getElementById('visionFile').files[0];
  if (!f) return;
  document.getElementById('visionDrop').classList.add('has-file');
  document.getElementById('visionHint').innerHTML =
    '<span class="fname">' + f.name + '</span> · ready to investigate';
}

async function runVision() {
  const inp = document.getElementById('visionFile');
  if (!inp.files.length) { alert('Pick a screenshot first.'); return; }
  const out = document.getElementById('visionOutput');
  out.textContent = '';
  out.classList.add('visible');
  const fd = new FormData();
  fd.append('image', inp.files[0]);
  let resp;
  try {
    resp = await fetch('/vision', { method: 'POST', body: fd });
  } catch (e) {
    out.textContent = 'request failed: ' + e;
    return;
  }
  const reader = resp.body.getReader();
  const dec = new TextDecoder();
  while (true) {
    const {done, value} = await reader.read();
    if (done) break;
    dec.decode(value).split('\n').forEach(line => {
      if (line.startsWith('data: ')) {
        const msg = line.slice(6);
        if (msg === '[[END]]') return;
        out.textContent += msg + '\n';
        out.scrollTop = out.scrollHeight;
      }
    });
  }
}

function askThis(q) {
  document.getElementById('chatInput').value = q;
  sendChat();
}

let chatEs = null;
function sendChat() {
  const inp = document.getElementById('chatInput');
  const q = inp.value.trim();
  if (!q) return;
  const out = document.getElementById('chatOutput');
  out.textContent = '';
  out.classList.add('visible');
  if (chatEs) chatEs.close();
  chatEs = new EventSource('/chat?q=' + encodeURIComponent(q));
  chatEs.onmessage = e => {
    if (e.data === '[[END]]') { chatEs.close(); chatEs = null; return; }
    out.textContent += e.data + '\n';
    out.scrollTop = out.scrollHeight;
  };
  chatEs.onerror = () => { if (chatEs) { chatEs.close(); chatEs = null; } };
}

// Prettify result JSON in actions table
document.querySelectorAll('.action-result').forEach(td => {
  try {
    const obj = JSON.parse(td.textContent);
    if (obj.html_url) {
      td.innerHTML = '<a href="' + obj.html_url + '" target="_blank">#' + obj.number + ' ↗</a>';
    } else if (obj.note) {
      td.textContent = obj.note;
    } else if (obj.error) {
      td.style.color = '#f87171';
      td.textContent = obj.error;
    } else if (obj.rpm) {
      td.textContent = obj.key + ' → ' + obj.rpm + ' rpm';
    }
  } catch(_) {}
});
</script>
</body>
</html>`))
