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

		data := dashboardData{
			Anomalies:     anomalies,
			AgentActions:  actions,
			Now:           time.Now().UTC().Format("2006-01-02 15:04 UTC"),
			ChartDataJSON: template.JS(chartJSON),
			Watcher:       watcherStatus,
			Projection:    projection,
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
		flusher := rw.(http.Flusher)

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
		flusher := w.(http.Flusher)
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
	"prod-frontend":   "#f87171",
	"internal-tools":  "#475569",
	"background-jobs": "#334155",
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
<title>llmtrace</title>
<style>
  * { box-sizing: border-box; margin: 0; padding: 0; }
  body { font-family: 'Segoe UI', system-ui, sans-serif; background: #0f1117; color: #e2e8f0; min-height: 100vh; }
  .header { padding: 20px 32px; border-bottom: 1px solid #1e2d3d; display: flex; align-items: center; gap: 12px; }
  .logo { font-size: 1.3rem; font-weight: 700; color: #fff; letter-spacing: -0.02em; }
  .logo span { color: #f87171; }
  .header .ts { font-size: 0.8rem; color: #475569; margin-left: auto; }
  .container { max-width: 1100px; margin: 0 auto; padding: 28px 32px; }
  .section-title { font-size: 0.7rem; font-weight: 700; text-transform: uppercase; letter-spacing: 0.1em; color: #475569; margin-bottom: 14px; }
  .chart-wrap { background: #151b27; border: 1px solid #1e2d3d; border-radius: 10px; padding: 20px 24px; margin-bottom: 28px; }
  .chart-wrap canvas { max-height: 260px; }
  .anomaly-card { background: #151b27; border: 1px solid #1e3a5f; border-radius: 8px; padding: 18px 22px; margin-bottom: 10px; }
  .anomaly-card .top { display: flex; align-items: center; gap: 10px; margin-bottom: 12px; }
  .badge { font-size: 0.65rem; font-weight: 700; padding: 2px 7px; border-radius: 4px; text-transform: uppercase; letter-spacing: 0.05em; background: #7f1d1d; color: #fca5a5; }
  .badge.autonomous { background: #14532d; color: #86efac; }
  .badge.critical { background: #7f1d1d; color: #fca5a5; }
  .badge.high { background: #78350f; color: #fcd34d; }
  .badge.done { background: #14532d; color: #86efac; }
  .badge.failed { background: #3b0764; color: #d8b4fe; }
  .badge.skipped { background: #1e293b; color: #94a3b8; }
  .key-label { font-size: 0.95rem; font-weight: 600; }
  .date-label { font-size: 0.82rem; color: #64748b; margin-left: auto; }
  .metrics { display: flex; gap: 28px; flex-wrap: wrap; }
  .metric { display: flex; flex-direction: column; gap: 2px; }
  .metric .lbl { color: #64748b; font-size: 0.7rem; text-transform: uppercase; letter-spacing: 0.05em; }
  .metric .val { color: #f1f5f9; font-weight: 600; font-size: 0.9rem; }
  .metric .val.hot { color: #f87171; }
  .btn { margin-top: 14px; padding: 6px 14px; font-size: 0.82rem; font-weight: 500;
    background: #1d4ed8; color: #fff; border: none; border-radius: 5px; cursor: pointer; }
  .btn:hover { background: #2563eb; }
  .btn:disabled { opacity: 0.5; cursor: default; }
  .stream-box { margin-top: 12px; background: #080c14; border: 1px solid #1e2d3d; border-radius: 6px;
    padding: 12px 14px; font-family: 'Cascadia Code', 'Fira Code', monospace; font-size: 0.76rem;
    color: #94a3b8; line-height: 1.65; max-height: 340px; overflow-y: auto; display: none; white-space: pre-wrap; }
  .stream-box.visible { display: block; }
  .no-anomalies { color: #475569; padding: 16px 0; }

  /* Cost projection banner */
  .projection { background: #1a1410; border: 1px solid #78350f; border-radius: 8px;
    padding: 14px 20px; margin-bottom: 28px; display: flex; align-items: baseline; gap: 20px; flex-wrap: wrap; }
  .projection .lbl { font-size: 0.68rem; font-weight: 700; letter-spacing: 0.08em; color: #92745a; }
  .projection .big { font-size: 1.5rem; font-weight: 700; color: #fbbf24; }
  .projection .sub { font-size: 0.84rem; color: #b08968; }

  /* Chat */
  .chat-box { background: #151b27; border: 1px solid #1e2d3d; border-radius: 10px; padding: 18px 20px; margin-bottom: 28px; }
  .chat-input-row { display: flex; gap: 8px; }
  .chat-input-row input { flex: 1; background: #0b0f17; border: 1px solid #1e2d3d; border-radius: 6px;
    padding: 9px 12px; color: #e2e8f0; font-size: 0.85rem; font-family: inherit; }
  .chat-input-row input:focus { outline: none; border-color: #2563eb; }
  .chat-output { margin-top: 12px; background: #080c14; border: 1px solid #1e2d3d; border-radius: 6px;
    padding: 12px 14px; font-family: 'Cascadia Code', 'Fira Code', monospace; font-size: 0.78rem;
    color: #94a3b8; line-height: 1.65; max-height: 300px; overflow-y: auto; display: none; white-space: pre-wrap; }
  .chat-output.visible { display: block; }
  .chat-suggestions { margin-top: 10px; display: flex; gap: 8px; flex-wrap: wrap; }
  .chat-suggestions button { background: #1e2d3d; color: #94a3b8; border: none; border-radius: 12px;
    padding: 4px 11px; font-size: 0.73rem; cursor: pointer; }
  .chat-suggestions button:hover { background: #2d3d4d; color: #e2e8f0; }

  /* Vision import */
  .vision-panel { background: #151b27; border: 1px solid #1e2d3d; border-radius: 10px; padding: 18px 20px; margin-bottom: 28px; }
  .vision-drop { border: 1.5px dashed #2d3d4d; border-radius: 8px; padding: 24px; text-align: center;
    cursor: pointer; transition: border-color 0.15s, background 0.15s; }
  .vision-drop:hover { border-color: #2563eb; background: #0d1320; }
  .vision-drop.has-file { border-color: #16a34a; }
  .vision-drop .hint { color: #64748b; font-size: 0.85rem; }
  .vision-drop .fname { color: #86efac; font-size: 0.85rem; font-weight: 600; }
  .vision-tag { font-size: 0.62rem; font-weight: 700; letter-spacing: 0.06em; text-transform: uppercase;
    background: #1e3a8a; color: #93c5fd; padding: 2px 7px; border-radius: 4px; margin-left: 8px; }

  /* Watcher status bar */
  .watcher-bar { background: #0d1f0d; border: 1px solid #14532d; border-radius: 8px;
    padding: 14px 20px; margin-bottom: 28px; display: flex; align-items: center; gap: 16px; flex-wrap: wrap; }
  .watcher-bar .pulse { width: 8px; height: 8px; border-radius: 50%; background: #22c55e;
    box-shadow: 0 0 6px #22c55e; animation: pulse 2s infinite; flex-shrink: 0; }
  @keyframes pulse { 0%,100% { opacity:1; } 50% { opacity:0.4; } }
  .watcher-bar .label { font-size: 0.82rem; font-weight: 600; color: #86efac; }
  .watcher-bar .meta { font-size: 0.75rem; color: #4ade80; opacity: 0.7; }
  .watcher-bar .live-btn { margin-left: auto; padding: 4px 12px; font-size: 0.75rem; font-weight: 500;
    background: #14532d; color: #86efac; border: 1px solid #16a34a; border-radius: 4px; cursor: pointer; }
  .watcher-bar .live-btn:hover { background: #166534; }
  .live-feed { background: #080c14; border: 1px solid #1e2d3d; border-radius: 6px;
    padding: 12px 14px; font-family: 'Cascadia Code', 'Fira Code', monospace; font-size: 0.76rem;
    color: #4ade80; line-height: 1.65; max-height: 300px; overflow-y: auto; margin-bottom: 28px;
    display: none; white-space: pre-wrap; }
  .live-feed.visible { display: block; }
  .live-feed .dim { color: #475569; }
  .live-feed .tool { color: #60a5fa; }
  .live-feed .attr { color: #e2e8f0; }
  .live-feed .action { color: #fbbf24; }

  /* Actions table */
  .actions-table { width: 100%; border-collapse: collapse; font-size: 0.82rem; margin-bottom: 28px; }
  .actions-table th { text-align: left; color: #475569; font-size: 0.7rem; text-transform: uppercase;
    letter-spacing: 0.05em; padding: 0 10px 8px 0; border-bottom: 1px solid #1e2d3d; }
  .actions-table td { padding: 10px 10px 10px 0; border-bottom: 1px solid #1a2332; vertical-align: top; }
  .actions-table tr:last-child td { border-bottom: none; }
  .action-result a { color: #60a5fa; text-decoration: none; }
  .action-result a:hover { text-decoration: underline; }
</style>
</head>
<body>
<div class="header">
  <div class="logo">llm<span>trace</span></div>
  {{if .Watcher.Active}}<span class="badge autonomous">autonomous</span>{{end}}
  <span class="ts">{{.Now}}</span>
</div>
<div class="container">

  {{if .Watcher.Active}}
  <div class="watcher-bar">
    <div class="pulse"></div>
    <div>
      <div class="label">Autonomous Agent — Active</div>
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
    <div class="sub">baseline run-rate ${{printf "%.0f" .Projection.MonthlyBase}}/mo — this regression adds
      <strong style="color:#f87171">+${{printf "%.0f" .Projection.Overspend}}/mo</strong> if left unfixed</div>
  </div>
  {{end}}

  <div class="section-title">Investigate Any Dashboard <span class="vision-tag">Gemini Vision</span></div>
  <div class="vision-panel">
    <div class="vision-drop" id="visionDrop" onclick="document.getElementById('visionFile').click()">
      <div class="hint" id="visionHint">Drop in a screenshot of <strong>any</strong> LLM billing dashboard — Anthropic, OpenAI, a Grafana panel.
        Gemini reads the spend off the image, then the agent finds which deploy caused the spike.</div>
    </div>
    <input type="file" id="visionFile" accept="image/*" style="display:none" onchange="visionPicked()">
    <button class="btn" style="margin-top:12px" onclick="runVision()">Read &amp; Investigate →</button>
    <div class="chat-output" id="visionOutput"></div>
  </div>

  <div class="section-title">Daily Spend — Last 30 Days</div>
  <div class="chart-wrap">
    <canvas id="costChart"></canvas>
  </div>

  <div class="section-title">Ask the Agent</div>
  <div class="chat-box">
    <div class="chat-input-row">
      <input id="chatInput" type="text" autocomplete="off"
        placeholder="Ask about your LLM spend — e.g. why did prod-frontend spike?"
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
          ticks: { color: '#475569', maxTicksLimit: 10, font: { size: 11 } },
          grid: { color: '#1e2d3d' }
        },
        y: {
          ticks: { color: '#475569', font: { size: 11 }, callback: v => '$' + v.toFixed(2) },
          grid: { color: '#1e2d3d' }
        }
      },
      plugins: {
        legend: { labels: { color: '#94a3b8', font: { size: 12 }, boxWidth: 12 } },
        tooltip: {
          backgroundColor: '#151b27',
          borderColor: '#1e2d3d',
          borderWidth: 1,
          titleColor: '#e2e8f0',
          bodyColor: '#94a3b8',
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
    '<span class="fname">' + f.name + '</span> — ready to investigate';
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
