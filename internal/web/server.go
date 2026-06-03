package web

import (
	"context"
	_ "embed"
	"encoding/json"
	"fmt"
	"html/template"
	"net/http"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/Yatsuiii/llmtrace/internal/agent"
	"github.com/Yatsuiii/llmtrace/internal/api"
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
	mux.HandleFunc("/api/cost", api.CostHandler(db))
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
		callSummary, err := db.CallSummary(ctx, since)
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
			Keys:          buildKeySummaries(daily, anomalies, callSummary),
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
	Keys          []keySummary
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
		return "·"
	}
	return t.Format("15:04:05 UTC")
}

// keySummary is one row of the dashboard watchlist: a per-key 30-day rollup.
type keySummary struct {
	Key        string
	Spend30d   float64
	PerDay     float64
	DeltaLabel string
	Sonnet     int
	Haiku      int
	MixLabel   string
	Sigma      float64
	Hot        bool
	Status     string
	Spark      template.HTML
}

// buildKeySummaries rolls the daily cost series, anomalies, and call mix into
// one watchlist row per key. Keys with an anomaly sort first, then by spend.
func buildKeySummaries(daily []storage.KeyDailyCost, anomalies []storage.AnomalyRow, calls []storage.CallSummaryRow) []keySummary {
	type series struct {
		total  float64
		points []float64
	}
	km := map[string]*series{}
	var order []string
	for _, d := range daily {
		s := km[d.APIKeyID]
		if s == nil {
			s = &series{}
			km[d.APIKeyID] = s
			order = append(order, d.APIKeyID)
		}
		s.total += d.CostUSD
		s.points = append(s.points, d.CostUSD)
	}

	anom := map[string]storage.AnomalyRow{}
	for _, a := range anomalies {
		if _, ok := anom[a.APIKeyID]; !ok {
			anom[a.APIKeyID] = a
		}
	}

	type mix struct{ sonnet, haiku, other int64 }
	mm := map[string]*mix{}
	for _, c := range calls {
		m := mm[c.APIKeyID]
		if m == nil {
			m = &mix{}
			mm[c.APIKeyID] = m
		}
		switch {
		case strings.Contains(c.Model, "sonnet"):
			m.sonnet += c.Calls
		case strings.Contains(c.Model, "haiku"):
			m.haiku += c.Calls
		default:
			m.other += c.Calls
		}
	}

	sort.SliceStable(order, func(i, j int) bool {
		_, hi := anom[order[i]]
		_, hj := anom[order[j]]
		if hi != hj {
			return hi
		}
		return km[order[i]].total > km[order[j]].total
	})

	var out []keySummary
	for _, k := range order {
		s := km[k]
		if len(s.points) == 0 {
			continue
		}
		a, hot := anom[k]
		ks := keySummary{
			Key:      k,
			Spend30d: s.total,
			PerDay:   s.total / 30.0,
			Spark:    sparkSVG(s.points, hot),
			Hot:      hot,
		}
		if hot {
			ratio := 0.0
			if a.BaselineValue > 0 {
				ratio = a.ActualValue / a.BaselineValue
			}
			ks.DeltaLabel = fmt.Sprintf("▲ %.1f×", ratio)
			ks.Sigma = a.Sigma
			switch {
			case a.Sigma >= 10:
				ks.Status = "CRITICAL"
			case a.Sigma >= 5:
				ks.Status = "HIGH"
			default:
				ks.Status = "FLAGGED"
			}
		} else {
			ks.DeltaLabel = "━ flat"
			ks.Status = "OK"
		}
		if m := mm[k]; m != nil {
			tot := m.sonnet + m.haiku + m.other
			if tot > 0 {
				ks.Sonnet = int(m.sonnet * 100 / tot)
				ks.Haiku = int(m.haiku * 100 / tot)
				switch {
				case ks.Sonnet >= 100:
					ks.MixLabel = "sonnet"
				case ks.Haiku >= 100:
					ks.MixLabel = "haiku"
				case ks.Sonnet >= ks.Haiku:
					ks.MixLabel = fmt.Sprintf("%d%% sonnet", ks.Sonnet)
				default:
					ks.MixLabel = fmt.Sprintf("%d%% haiku", ks.Haiku)
				}
			}
		}
		if ks.Sonnet == 0 && ks.Haiku == 0 {
			ks.Sonnet = 100
			ks.MixLabel = "·"
		}
		out = append(out, ks)
	}
	return out
}

// sparkSVG renders a per-key daily-cost sparkline as an inline SVG polyline.
func sparkSVG(points []float64, hot bool) template.HTML {
	n := len(points)
	if n < 2 {
		return template.HTML(`<svg class="spark" width="90" height="26"></svg>`)
	}
	var max float64
	for _, v := range points {
		if v > max {
			max = v
		}
	}
	if max <= 0 {
		max = 1
	}
	var b strings.Builder
	for i, v := range points {
		x := 2.0 + float64(i)*(86.0/float64(n-1))
		y := 24.0 - (v/max)*20.0
		if i > 0 {
			b.WriteByte(' ')
		}
		fmt.Fprintf(&b, "%.1f,%.1f", x, y)
	}
	color := "#1d9a4a"
	if hot {
		color = "#ff4d4d"
	}
	return template.HTML(fmt.Sprintf(
		`<svg class="spark" width="90" height="26" viewBox="0 0 90 26"><polyline fill="none" stroke="%s" stroke-width="1.5" points="%s"/></svg>`,
		color, b.String()))
}

var keyColors = map[string]string{
	"prod-frontend":   "#ffb000",
	"internal-tools":  "#2f7d4e",
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
			color = "#4a4f5a"
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
	// One marker per deploy day; multiple deploys on a day collapse to a count.
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
		a.BorderColor = "#ff4d4d"
		a.BorderWidth = 2
		a.BorderDash = []int{6, 4}
		a.Label.Display = true
		if n == 1 {
			a.Label.Content = "deploy"
		} else {
			a.Label.Content = fmt.Sprintf("%d deploys", n)
		}
		a.Label.BackgroundColor = "#ff4d4d22"
		a.Label.Color = "#ff9b9b"
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

//go:embed dashboard.html.tmpl
var dashboardHTML string

var dashboardTmpl = template.Must(template.New("dashboard").Parse(dashboardHTML))
