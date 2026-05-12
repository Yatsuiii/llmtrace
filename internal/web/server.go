package web

import (
	"context"
	"encoding/json"
	"fmt"
	"html/template"
	"net/http"
	"sort"
	"time"

	"github.com/Yatsuiii/llmtrace/internal/detect"
	"github.com/Yatsuiii/llmtrace/internal/storage"
)

func Serve(ctx context.Context, db *storage.DB, port int) error {
	mux := http.NewServeMux()
	mux.HandleFunc("/", dashboardHandler(db))
	mux.HandleFunc("/investigate", investigateHandler(db))

	addr := fmt.Sprintf(":%d", port)
	fmt.Printf("llmtrace dashboard → http://localhost%s\n", addr)
	srv := &http.Server{Addr: addr, Handler: mux}
	go func() {
		<-ctx.Done()
		srv.Shutdown(context.Background())
	}()
	return srv.ListenAndServe()
}

func dashboardHandler(db *storage.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ctx := r.Context()
		since := time.Now().UTC().AddDate(0, 0, -30)

		if _, err := detect.Run(ctx, db, detect.DefaultConfig(), since); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		anomalies, err := db.ListAnomalies(ctx, since)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		daily, err := db.DailyCostByKey(ctx, since)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		deploys, err := db.ListDeploys(ctx, since)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}

		chartJSON, err := buildChartJSON(daily, deploys)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}

		data := dashboardData{
			Anomalies:     anomalies,
			Now:           time.Now().UTC().Format("2006-01-02 15:04 UTC"),
			ChartDataJSON: template.JS(chartJSON),
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		if err := dashboardTmpl.Execute(w, data); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
		}
	}
}

type dashboardData struct {
	Anomalies     []storage.AnomalyRow
	Now           string
	ChartDataJSON template.JS
}

var keyColors = map[string]string{
	"prod-frontend":   "#f87171",
	"internal-tools":  "#475569",
	"background-jobs": "#334155",
}

func buildChartJSON(daily []storage.KeyDailyCost, deploys []storage.DeployRow) (string, error) {
	// Collect unique sorted dates and per-key cost maps.
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

	// Stable key order: affected key first, then controls.
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

	// Annotations: one vertical line per deploy.
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
	annotations := map[string]annotationLine{}
	for i, d := range deploys {
		date := d.StartedAt.Format("2006-01-02")
		var a annotationLine
		a.Type = "line"
		a.XMin = date
		a.XMax = date
		a.BorderColor = "#fbbf24"
		a.BorderWidth = 2
		a.BorderDash = []int{6, 4}
		a.Label.Display = true
		a.Label.Content = fmt.Sprintf("PR #%d", d.PRNumber)
		a.Label.BackgroundColor = "#fbbf2422"
		a.Label.Color = "#fbbf24"
		a.Label.Font.Size = 11
		a.Label.Position = "start"
		annotations[fmt.Sprintf("deploy%d", i)] = a
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
  .attribution { color: #e2e8f0; }
  .attribution .sep { color: #334155; }
  .no-anomalies { color: #475569; padding: 16px 0; }
</style>
</head>
<body>
<div class="header">
  <div class="logo">llm<span>trace</span></div>
  <span class="ts">{{.Now}}</span>
</div>
<div class="container">
  <div class="section-title">Daily Spend — Last 30 Days</div>
  <div class="chart-wrap">
    <canvas id="costChart"></canvas>
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
</script>
</body>
</html>`))
