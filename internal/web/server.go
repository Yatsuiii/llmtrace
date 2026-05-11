package web

import (
	"context"
	"fmt"
	"html/template"
	"net/http"
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

		cfg := detect.DefaultConfig()
		if _, err := detect.Run(ctx, db, cfg, since); err != nil {
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

		data := dashboardData{
			Anomalies: anomalies,
			Daily:     daily,
			Now:       time.Now().UTC().Format("2006-01-02 15:04 UTC"),
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		if err := dashboardTmpl.Execute(w, data); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
		}
	}
}

type dashboardData struct {
	Anomalies []storage.AnomalyRow
	Daily     []storage.KeyDailyCost
	Now       string
}

var dashboardTmpl = template.Must(template.New("dashboard").Funcs(template.FuncMap{
	"add": func(a, b float64) float64 { return a + b },
}).Parse(`<!DOCTYPE html>
<html lang="en">
<head>
<meta charset="UTF-8">
<meta name="viewport" content="width=device-width, initial-scale=1.0">
<title>llmtrace</title>
<style>
  * { box-sizing: border-box; margin: 0; padding: 0; }
  body { font-family: 'Segoe UI', system-ui, sans-serif; background: #0f1117; color: #e2e8f0; min-height: 100vh; }
  .header { padding: 24px 32px; border-bottom: 1px solid #1e2d3d; display: flex; align-items: center; gap: 12px; }
  .header h1 { font-size: 1.4rem; font-weight: 600; color: #fff; }
  .header .sub { font-size: 0.85rem; color: #64748b; margin-left: auto; }
  .container { max-width: 1100px; margin: 0 auto; padding: 32px; }
  .section-title { font-size: 0.75rem; font-weight: 600; text-transform: uppercase; letter-spacing: 0.08em; color: #64748b; margin-bottom: 16px; }
  .anomaly-card { background: #151b27; border: 1px solid #1e3a5f; border-radius: 8px; padding: 20px 24px; margin-bottom: 12px; }
  .anomaly-card .top { display: flex; align-items: baseline; gap: 12px; margin-bottom: 8px; }
  .badge { font-size: 0.7rem; font-weight: 700; padding: 2px 8px; border-radius: 4px; text-transform: uppercase; letter-spacing: 0.04em; }
  .badge-red { background: #7f1d1d; color: #fca5a5; }
  .key-label { font-size: 1rem; font-weight: 600; color: #e2e8f0; }
  .date-label { font-size: 0.85rem; color: #64748b; }
  .anomaly-card .metrics { display: flex; gap: 32px; font-size: 0.85rem; }
  .metric { display: flex; flex-direction: column; gap: 2px; }
  .metric .label { color: #64748b; font-size: 0.75rem; }
  .metric .value { color: #f1f5f9; font-weight: 500; }
  .metric .value.hot { color: #f87171; }
  .investigate-btn { margin-top: 14px; padding: 7px 16px; font-size: 0.85rem; font-weight: 500;
    background: #1d4ed8; color: #fff; border: none; border-radius: 6px; cursor: pointer; transition: background 0.15s; }
  .investigate-btn:hover { background: #2563eb; }
  .stream-box { margin-top: 14px; background: #0a0e1a; border: 1px solid #1e2d3d; border-radius: 6px;
    padding: 14px 16px; font-family: 'Cascadia Code', 'Fira Code', monospace; font-size: 0.78rem;
    color: #94a3b8; line-height: 1.6; max-height: 320px; overflow-y: auto; display: none; }
  .stream-box.visible { display: block; }
  .no-anomalies { color: #64748b; font-size: 0.9rem; padding: 24px 0; }
</style>
</head>
<body>
<div class="header">
  <h1>llmtrace</h1>
  <span class="sub">{{.Now}}</span>
</div>
<div class="container">
  <div class="section-title">Spend Anomalies — Last 30 Days</div>
  {{if .Anomalies}}
  {{range .Anomalies}}
  <div class="anomaly-card" id="card-{{.ID}}">
    <div class="top">
      <span class="badge badge-red">anomaly</span>
      <span class="key-label">{{.APIKeyID}}</span>
      <span class="date-label">{{.Date}}</span>
    </div>
    <div class="metrics">
      <div class="metric"><span class="label">actual</span><span class="value hot">${{printf "%.2f" .ActualValue}}</span></div>
      <div class="metric"><span class="label">baseline (7d avg)</span><span class="value">${{printf "%.2f" .BaselineValue}}</span></div>
      <div class="metric"><span class="label">delta</span><span class="value hot">+${{printf "%.2f" .Delta}}</span></div>
      <div class="metric"><span class="label">sigma</span><span class="value hot">{{printf "%.1f" .Sigma}}σ</span></div>
      <div class="metric"><span class="label">calls</span><span class="value">{{.SampleSize}}</span></div>
    </div>
    <button class="investigate-btn" onclick="investigate({{.ID}}, this)">Investigate →</button>
    <div class="stream-box" id="stream-{{.ID}}"></div>
  </div>
  {{end}}
  {{else}}
  <div class="no-anomalies">No anomalies detected in the last 30 days.</div>
  {{end}}
</div>
<script>
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
    const chunk = dec.decode(value);
    // SSE: lines look like "data: ...\n\n"
    chunk.split('\n').forEach(line => {
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
