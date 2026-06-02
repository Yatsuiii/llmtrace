package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"log"
	"os"
	"strconv"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"github.com/Yatsuiii/llmtrace/internal/actions"
	"github.com/Yatsuiii/llmtrace/internal/agent"
	"github.com/Yatsuiii/llmtrace/internal/correlate"
	"github.com/Yatsuiii/llmtrace/internal/deploys"
	"github.com/Yatsuiii/llmtrace/internal/detect"
	"github.com/Yatsuiii/llmtrace/internal/seed"
	"github.com/Yatsuiii/llmtrace/internal/storage"
	"github.com/Yatsuiii/llmtrace/internal/watcher"
	"github.com/Yatsuiii/llmtrace/internal/web"
)

func closeDB(db *storage.DB) {
	if err := db.Close(); err != nil {
		log.Printf("db close: %v", err)
	}
}

func main() {
	root := &cobra.Command{
		Use:   "llmtrace",
		Short: "LLM call tracing with cost & latency anomaly detection",
	}
	root.AddCommand(
		cmdInit(),
		cmdServe(),
		cmdWatch(),
		cmdKeys(),
		cmdStats(),
		cmdTail(),
		cmdAnomalies(),
		cmdAnalyze(),
		cmdExplain(),
		cmdReport(),
		cmdSeed(),
		cmdSyncDeploys(),
		cmdCorrelate(),
	)
	if err := root.Execute(); err != nil {
		os.Exit(1)
	}
}

func cmdSeed() *cobra.Command {
	var path string
	cmd := &cobra.Command{
		Use:   "seed",
		Short: "Plant the demo scenario into the ledger (wipes existing data)",
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := context.Background()
			db, err := storage.Open(path)
			if err != nil {
				return err
			}
			defer closeDB(db)
			n, err := seed.Run(ctx, db)
			if err != nil {
				return err
			}
			fmt.Printf("seeded %d calls into %s\n", n, path)
			since := seed.Start()
			daily, err := db.DailyCostByKey(ctx, since)
			if err != nil {
				return err
			}
			summary := map[string]struct{ pre, post float64 }{}
			deployDay := since.AddDate(0, 0, 22).Format("2006-01-02")
			for _, d := range daily {
				s := summary[d.APIKeyID]
				if d.Date < deployDay {
					s.pre += d.CostUSD
				} else {
					s.post += d.CostUSD
				}
				summary[d.APIKeyID] = s
			}
			preDays := 22.0
			postDays := 8.0
			fmt.Println()
			fmt.Println("per-key cost (avg $/day):")
			fmt.Printf("  %-18s  %-10s  %-10s  %-10s\n", "key", "pre", "post", "ratio")
			for k, s := range summary {
				pre := s.pre / preDays
				post := s.post / postDays
				ratio := 0.0
				if pre > 0 {
					ratio = post / pre
				}
				fmt.Printf("  %-18s  $%-9.2f  $%-9.2f  %.2fx\n", k, pre, post, ratio)
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&path, "db", "llmtrace.db", "path to SQLite ledger")
	return cmd
}

func cmdSyncDeploys() *cobra.Command {
	var dbPath string
	var days int
	cmd := &cobra.Command{
		Use:   "sync-deploys",
		Short: "Ingest deploy events from GitHub Actions into the ledger",
		Long: "Pulls successful workflow runs matching DEPLOY_WORKFLOW_PATTERN (default \"deploy.*\") " +
			"from the repo in GITHUB_REPO and records them as deploy events. Requires GITHUB_TOKEN.",
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := context.Background()
			db, err := storage.Open(dbPath)
			if err != nil {
				return err
			}
			defer closeDB(db)

			gh := actions.GitHubFromEnv()
			if !gh.Enabled() {
				return fmt.Errorf("set GITHUB_TOKEN and GITHUB_REPO to ingest deploys")
			}
			n, err := deploys.Sync(ctx, db, gh, os.Getenv("DEPLOY_WORKFLOW_PATTERN"),
				time.Duration(days)*24*time.Hour)
			if err != nil {
				return err
			}
			fmt.Printf("ingested %d deploy(s) from %s into %s\n", n, gh.Repo, dbPath)
			return nil
		},
	}
	cmd.Flags().StringVar(&dbPath, "db", "llmtrace.db", "path to SQLite ledger")
	cmd.Flags().IntVar(&days, "days", 30, "lookback window in days")
	return cmd
}

func cmdCorrelate() *cobra.Command {
	var dbPath string
	var days int
	cmd := &cobra.Command{
		Use:   "correlate",
		Short: "Match spend anomalies to the deploys that likely caused them",
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := context.Background()
			db, err := storage.Open(dbPath)
			if err != nil {
				return err
			}
			defer closeDB(db)

			since := time.Now().UTC().AddDate(0, 0, -days)
			if _, err := detect.Run(ctx, db, detect.DefaultConfig(), since); err != nil {
				return err
			}
			results, err := correlate.Run(ctx, db, correlate.DefaultConfig(), since)
			if err != nil {
				return err
			}
			if len(results) == 0 {
				fmt.Println("no anomaly/deploy correlations found")
				return nil
			}
			for _, r := range results {
				fmt.Printf("\nanomaly #%d  →  deploy %s (PR #%d) %q\n", r.AnomalyID, r.Deploy.ID, r.Deploy.PRNumber, r.Deploy.Title)
				fmt.Printf("  confidence %.2f\n", r.Confidence)
				for _, e := range r.Evidence {
					fmt.Printf("    [%s] %s\n", e.Kind, e.Description)
				}
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&dbPath, "db", "llmtrace.db", "path to SQLite ledger")
	cmd.Flags().IntVar(&days, "days", 30, "lookback window in days")
	return cmd
}

func cmdInit() *cobra.Command {
	return &cobra.Command{
		Use:   "init",
		Short: "Write a starter config.toml",
		RunE: func(cmd *cobra.Command, args []string) error {
			const cfg = `[proxy]
listen = "0.0.0.0:8080"

[providers.anthropic]
upstream_key_env = "ANTHROPIC_API_KEY"

[github]
repo = ""
token_env = "GITHUB_TOKEN"
deploy_workflow_pattern = "deploy.*"

[detection]
baseline_days = 7
sigma_threshold = 2.5
min_delta_usd = 5

[output]
format = "text"
`
			const path = "config.toml"
			if _, err := os.Stat(path); err == nil {
				return fmt.Errorf("%s already exists", path)
			}
			if err := os.WriteFile(path, []byte(cfg), 0644); err != nil {
				return err
			}
			fmt.Printf("wrote %s — set ANTHROPIC_API_KEY and run: llmtrace serve\n", path)
			return nil
		},
	}
}

func cmdServe() *cobra.Command {
	var port int
	var dbPath string
	var autonomous bool
	cmd := &cobra.Command{
		Use:   "serve",
		Short: "Run the dashboard server",
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := context.Background()
			db, err := storage.Open(dbPath)
			if err != nil {
				return err
			}
			defer closeDB(db)

			var w *watcher.Watcher
			if autonomous {
				w, err = buildWatcher(ctx, db)
				if err != nil {
					return err
				}
				go w.Run(ctx)
			}
			return web.Serve(ctx, db, port, w)
		},
	}
	cmd.Flags().IntVar(&port, "port", 8080, "listen port")
	cmd.Flags().StringVar(&dbPath, "db", "llmtrace.db", "path to SQLite ledger")
	cmd.Flags().BoolVar(&autonomous, "autonomous", false, "enable autonomous anomaly watcher")
	return cmd
}

func cmdWatch() *cobra.Command {
	var dbPath string
	var interval int
	cmd := &cobra.Command{
		Use:   "watch",
		Short: "Run the autonomous watcher loop (no web server)",
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := context.Background()
			db, err := storage.Open(dbPath)
			if err != nil {
				return err
			}
			defer closeDB(db)

			w, err := buildWatcher(ctx, db)
			if err != nil {
				return err
			}
			if interval > 0 {
				w = watcher.New(db, mustInvestigator(db), actions.GitHubFromEnv(),
					watcher.Config{Interval: time.Duration(interval) * time.Minute, LookbackDays: 30})
			}
			emit := func(msg string) { fmt.Println(msg) }
			ch := w.Subscribe()
			go func() {
				for msg := range ch {
					emit(msg)
				}
			}()
			return w.Run(ctx)
		},
	}
	cmd.Flags().StringVar(&dbPath, "db", "llmtrace.db", "path to SQLite ledger")
	cmd.Flags().IntVar(&interval, "interval", 15, "scan interval in minutes")
	return cmd
}

func buildWatcher(ctx context.Context, db *storage.DB) (*watcher.Watcher, error) {
	apiKey := os.Getenv("GEMINI_API_KEY")
	if apiKey == "" {
		return nil, fmt.Errorf("GEMINI_API_KEY required for autonomous mode")
	}
	inv, err := agent.New(db, apiKey, os.Getenv("GEMINI_MODEL"))
	if err != nil {
		return nil, fmt.Errorf("init agent: %w", err)
	}
	gh := actions.GitHubFromEnv()
	cfg := watcher.DefaultConfig()
	return watcher.New(db, inv, gh, cfg), nil
}

func mustInvestigator(db *storage.DB) *agent.Investigator {
	inv, err := agent.New(db, os.Getenv("GEMINI_API_KEY"), os.Getenv("GEMINI_MODEL"))
	if err != nil {
		panic(err)
	}
	return inv
}

func cmdKeys() *cobra.Command {
	var dbPath string
	cmd := &cobra.Command{Use: "keys", Short: "Manage inbound API keys"}
	cmd.PersistentFlags().StringVar(&dbPath, "db", "llmtrace.db", "path to SQLite ledger")

	addCmd := &cobra.Command{
		Use:   "add",
		Short: "Mint a new API key",
		RunE: func(cmd *cobra.Command, args []string) error {
			label, _ := cmd.Flags().GetString("label")
			ctx := context.Background()
			db, err := storage.Open(dbPath)
			if err != nil {
				return err
			}
			defer closeDB(db)
			raw := make([]byte, 24)
			if _, err := rand.Read(raw); err != nil {
				return err
			}
			key := "lt-" + hex.EncodeToString(raw)
			id := key[:10]
			if err := db.InsertAPIKey(ctx, storage.APIKeyRow{
				ID:        id,
				HashedKey: key,
				Label:     label,
				Active:    true,
				CreatedAt: time.Now().UTC(),
			}); err != nil {
				return err
			}
			fmt.Printf("key id:  %s\nkey:     %s\nlabel:   %s\n\nSet X-Llmtrace-Key: %s on your requests.\n", id, key, label, id)
			return nil
		},
	}
	addCmd.Flags().String("label", "", "human-readable label for this key")

	listCmd := &cobra.Command{
		Use:   "list",
		Short: "List API keys",
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := context.Background()
			db, err := storage.Open(dbPath)
			if err != nil {
				return err
			}
			defer closeDB(db)
			keys, err := db.ListAPIKeys(ctx)
			if err != nil {
				return err
			}
			if len(keys) == 0 {
				fmt.Println("no keys — run: llmtrace keys add --label dev")
				return nil
			}
			tw := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
			fmt.Fprintln(tw, "ID\tLABEL\tACTIVE\tRPM\tBUDGET\tCREATED")
			for _, k := range keys {
				budget := "unlimited"
				if k.BudgetUSD > 0 {
					budget = fmt.Sprintf("$%.2f", k.BudgetUSD)
				}
				rpm := "unlimited"
				if k.RateLimitRPM > 0 {
					rpm = strconv.Itoa(k.RateLimitRPM)
				}
				active := "yes"
				if !k.Active {
					active = "no"
				}
				fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\n",
					k.ID, k.Label, active, rpm, budget, k.CreatedAt.Format("2006-01-02"))
			}
			return tw.Flush()
		},
	}

	revokeCmd := &cobra.Command{
		Use:   "revoke <key-id>",
		Short: "Revoke an API key",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := context.Background()
			db, err := storage.Open(dbPath)
			if err != nil {
				return err
			}
			defer closeDB(db)
			if err := db.SetAPIKeyActive(ctx, args[0], false); err != nil {
				return err
			}
			fmt.Printf("revoked %s\n", args[0])
			return nil
		},
	}

	cmd.AddCommand(addCmd, listCmd, revokeCmd)
	return cmd
}

func cmdStats() *cobra.Command {
	var days int
	var dbPath string
	cmd := &cobra.Command{
		Use:   "stats",
		Short: "Ledger summary by key/model",
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := context.Background()
			db, err := storage.Open(dbPath)
			if err != nil {
				return err
			}
			defer closeDB(db)
			since := time.Now().UTC().AddDate(0, 0, -days)
			rows, err := db.CallSummary(ctx, since)
			if err != nil {
				return err
			}
			if len(rows) == 0 {
				fmt.Printf("no calls in the last %d days\n", days)
				return nil
			}
			fmt.Printf("call summary — last %d days\n\n", days)
			tw := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
			fmt.Fprintln(tw, "KEY\tMODEL\tCALLS\tINPUT TOKENS\tOUTPUT TOKENS\tCOST USD\tAVG LATENCY")
			var totalCost float64
			for _, r := range rows {
				totalCost += r.CostUSD
				fmt.Fprintf(tw, "%s\t%s\t%d\t%d\t%d\t$%.4f\t%dms\n",
					r.APIKeyID, r.Model, r.Calls, r.InputTokens, r.OutputTokens, r.CostUSD, int(r.AvgLatencyMs))
			}
			tw.Flush()
			fmt.Printf("\ntotal cost: $%.4f\n", totalCost)
			return nil
		},
	}
	cmd.Flags().IntVar(&days, "days", 7, "window in days")
	cmd.Flags().StringVar(&dbPath, "db", "llmtrace.db", "path to SQLite ledger")
	return cmd
}

func cmdTail() *cobra.Command {
	var dbPath string
	var n int
	cmd := &cobra.Command{
		Use:   "tail",
		Short: "Print the most recent LLM calls from the ledger",
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := context.Background()
			db, err := storage.Open(dbPath)
			if err != nil {
				return err
			}
			defer closeDB(db)
			calls, err := db.RecentCalls(ctx, n)
			if err != nil {
				return err
			}
			if len(calls) == 0 {
				fmt.Println("no calls in ledger yet")
				return nil
			}
			for _, c := range calls {
				status := ""
				if c.ErrorClass != "" {
					status = " [" + c.ErrorClass + "]"
				}
				fmt.Printf("%s  %-18s  %-30s  in:%-6d  out:%-6d  $%.4f  %dms%s\n",
					c.Timestamp.Format("15:04:05"), c.APIKeyID, c.Model,
					c.InputTokens, c.OutputTokens, c.CostUSD, c.LatencyMs, status)
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&dbPath, "db", "llmtrace.db", "path to SQLite ledger")
	cmd.Flags().IntVar(&n, "n", 20, "number of recent calls to show")
	return cmd
}

func cmdAnomalies() *cobra.Command {
	var days int
	var dbPath string
	cmd := &cobra.Command{
		Use:   "anomalies",
		Short: "Detect and list spend anomalies",
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := context.Background()
			db, err := storage.Open(dbPath)
			if err != nil {
				return err
			}
			defer closeDB(db)
			since := time.Now().UTC().AddDate(0, 0, -days)
			cfg := detect.DefaultConfig()
			anomalies, err := detect.Run(ctx, db, cfg, since)
			if err != nil {
				return err
			}
			if len(anomalies) == 0 {
				fmt.Println("no anomalies detected")
				return nil
			}
			for _, a := range anomalies {
				fmt.Printf("ANOMALY  key:%-18s  date:%s  actual:$%.2f  baseline:$%.2f  delta:+$%.2f  %.1fσ\n",
					a.APIKeyID, a.Date, a.ActualValue, a.BaselineValue, a.Delta, a.Sigma)
			}
			return nil
		},
	}
	cmd.Flags().IntVar(&days, "days", 30, "window to scan")
	cmd.Flags().StringVar(&dbPath, "db", "llmtrace.db", "path to SQLite ledger")
	return cmd
}

func cmdAnalyze() *cobra.Command {
	var days int
	var dbPath string
	cmd := &cobra.Command{
		Use:   "analyze",
		Short: "Detect anomalies and investigate each with AI agent",
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := context.Background()
			db, err := storage.Open(dbPath)
			if err != nil {
				return err
			}
			defer closeDB(db)

			since := time.Now().UTC().AddDate(0, 0, -days)
			anomalies, err := detect.Run(ctx, db, detect.DefaultConfig(), since)
			if err != nil {
				return err
			}
			if len(anomalies) == 0 {
				fmt.Println("no anomalies detected")
				return nil
			}
			fmt.Printf("detected %d anomaly(ies)\n\n", len(anomalies))

			apiKey := os.Getenv("GEMINI_API_KEY")
			if apiKey == "" {
				fmt.Println("set GEMINI_API_KEY to enable agent investigation")
				for _, a := range anomalies {
					fmt.Printf("  ANOMALY  key:%-18s  date:%s  delta:+$%.2f  %.1fσ\n",
						a.APIKeyID, a.Date, a.Delta, a.Sigma)
				}
				return nil
			}

			inv, err := agent.New(db, apiKey, os.Getenv("GEMINI_MODEL"))
			if err != nil {
				return fmt.Errorf("init agent: %w", err)
			}
			emit := func(msg string) { fmt.Println(msg) }
			for i, a := range anomalies {
				if i > 0 {
					fmt.Println()
				}
				fmt.Printf("── Anomaly %d/%d: %s on %s ─────────────────────────\n",
					i+1, len(anomalies), a.APIKeyID, a.Date)
				if _, err := inv.Investigate(ctx, a, emit); err != nil {
					fmt.Printf("[error] %v\n", err)
				}
			}
			return nil
		},
	}
	cmd.Flags().IntVar(&days, "days", 30, "window to analyze")
	cmd.Flags().StringVar(&dbPath, "db", "llmtrace.db", "path to SQLite ledger")
	return cmd
}

func cmdExplain() *cobra.Command {
	var dbPath string
	cmd := &cobra.Command{
		Use:   "explain <anomaly-id>",
		Short: "Deep-dive a single anomaly with the AI agent",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			id, err := strconv.ParseInt(args[0], 10, 64)
			if err != nil {
				return fmt.Errorf("anomaly-id must be an integer")
			}
			ctx := context.Background()
			db, err := storage.Open(dbPath)
			if err != nil {
				return err
			}
			defer closeDB(db)
			a, err := db.GetAnomaly(ctx, id)
			if err != nil {
				return err
			}
			fmt.Printf("anomaly #%d  key:%s  date:%s  actual:$%.2f  baseline:$%.2f  delta:+$%.2f  %.1fσ\n\n",
				a.ID, a.APIKeyID, a.Date, a.ActualValue, a.BaselineValue, a.Delta, a.Sigma)

			apiKey := os.Getenv("GEMINI_API_KEY")
			if apiKey == "" {
				return fmt.Errorf("set GEMINI_API_KEY to enable agent investigation")
			}
			inv, err := agent.New(db, apiKey, os.Getenv("GEMINI_MODEL"))
			if err != nil {
				return fmt.Errorf("init agent: %w", err)
			}
			emit := func(msg string) { fmt.Println(msg) }
			if _, err := inv.Investigate(ctx, a, emit); err != nil {
				return err
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&dbPath, "db", "llmtrace.db", "path to SQLite ledger")
	return cmd
}

func cmdReport() *cobra.Command {
	var days int
	var format string
	var dbPath string
	cmd := &cobra.Command{
		Use:   "report",
		Short: "Spend and anomaly report",
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := context.Background()
			db, err := storage.Open(dbPath)
			if err != nil {
				return err
			}
			defer closeDB(db)
			since := time.Now().UTC().AddDate(0, 0, -days)
			summary, err := db.CallSummary(ctx, since)
			if err != nil {
				return err
			}
			anomalies, _ := db.ListAnomalies(ctx, since)

			var totalCalls int64
			var totalCost float64
			for _, r := range summary {
				totalCalls += r.Calls
				totalCost += r.CostUSD
			}

			md := format == "markdown"
			if md {
				fmt.Printf("## LLMTrace Report — last %d days\n\n", days)
				fmt.Printf("**Period:** %s to %s\n\n", since.Format("2006-01-02"), time.Now().UTC().Format("2006-01-02"))
				fmt.Printf("**Total calls:** %d  |  **Total cost:** $%.4f\n\n", totalCalls, totalCost)
				if len(summary) > 0 {
					fmt.Print("### Cost by key/model\n\n")
					fmt.Println("| Key | Model | Calls | Cost USD |")
					fmt.Println("|---|---|---|---|")
					for _, r := range summary {
						fmt.Printf("| %s | %s | %d | $%.4f |\n", r.APIKeyID, r.Model, r.Calls, r.CostUSD)
					}
					fmt.Println()
				}
				if len(anomalies) > 0 {
					fmt.Print("### Anomalies\n\n")
					for _, a := range anomalies {
						fmt.Printf("- **%s** on %s: $%.2f actual vs $%.2f baseline (+$%.2f, %.1fσ)\n",
							a.APIKeyID, a.Date, a.ActualValue, a.BaselineValue, a.Delta, a.Sigma)
					}
					fmt.Println()
				} else {
					fmt.Print("### Anomalies\n\nNo anomalies detected.\n\n")
				}
				fmt.Printf("*Generated by [llmtrace](https://github.com/Yatsuiii/llmtrace)*\n")
			} else {
				fmt.Printf("llmtrace report — last %d days\n", days)
				fmt.Printf("period:      %s to %s\n", since.Format("2006-01-02"), time.Now().UTC().Format("2006-01-02"))
				fmt.Printf("total calls: %d\n", totalCalls)
				fmt.Printf("total cost:  $%.4f\n\n", totalCost)
				if len(summary) > 0 {
					fmt.Println("cost by key/model:")
					tw := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
					fmt.Fprintln(tw, "  KEY\tMODEL\tCALLS\tCOST")
					for _, r := range summary {
						fmt.Fprintf(tw, "  %s\t%s\t%d\t$%.4f\n", r.APIKeyID, r.Model, r.Calls, r.CostUSD)
					}
					tw.Flush()
					fmt.Println()
				}
				if len(anomalies) > 0 {
					fmt.Printf("anomalies (%d):\n", len(anomalies))
					for _, a := range anomalies {
						fmt.Printf("  %s on %s: +$%.2f (%.1fσ)\n", a.APIKeyID, a.Date, a.Delta, a.Sigma)
					}
				} else {
					fmt.Println("anomalies: none")
				}
			}
			return nil
		},
	}
	cmd.Flags().IntVar(&days, "days", 30, "window to report on")
	cmd.Flags().StringVar(&format, "format", "text", "output format: text | markdown")
	cmd.Flags().StringVar(&dbPath, "db", "llmtrace.db", "path to SQLite ledger")
	return cmd
}

