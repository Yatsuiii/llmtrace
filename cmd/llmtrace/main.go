package main

import (
	"context"
	"fmt"
	"os"
	"time"

	"github.com/spf13/cobra"

	"github.com/Yatsuiii/llmtrace/internal/agent"
	"github.com/Yatsuiii/llmtrace/internal/detect"
	"github.com/Yatsuiii/llmtrace/internal/seed"
	"github.com/Yatsuiii/llmtrace/internal/storage"
	"github.com/Yatsuiii/llmtrace/internal/web"
)

func main() {
	root := &cobra.Command{
		Use:   "llmtrace",
		Short: "LLM call tracing with cost & latency anomaly detection",
	}
	root.AddCommand(
		cmdInit(),
		cmdServe(),
		cmdKeys(),
		cmdStats(),
		cmdTail(),
		cmdAnomalies(),
		cmdAnalyze(),
		cmdExplain(),
		cmdReport(),
		cmdSeed(),
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
			defer db.Close()
			n, err := seed.Run(ctx, db)
			if err != nil {
				return err
			}
			fmt.Printf("seeded %d calls into %s\n", n, path)
			since := time.Date(2026, 4, 11, 0, 0, 0, 0, time.UTC)
			daily, err := db.DailyCostByKey(ctx, since)
			if err != nil {
				return err
			}
			summary := map[string]struct{ pre, post float64 }{}
			deployDay := "2026-05-03"
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

func cmdInit() *cobra.Command {
	return &cobra.Command{
		Use:   "init",
		Short: "Interactive setup → config.toml",
		RunE: func(cmd *cobra.Command, args []string) error {
			return notImplemented("init")
		},
	}
}

func cmdServe() *cobra.Command {
	var port int
	var dbPath string
	cmd := &cobra.Command{
		Use:   "serve",
		Short: "Run the dashboard server",
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := context.Background()
			db, err := storage.Open(dbPath)
			if err != nil {
				return err
			}
			defer db.Close()
			return web.Serve(ctx, db, port)
		},
	}
	cmd.Flags().IntVar(&port, "port", 8080, "listen port")
	cmd.Flags().StringVar(&dbPath, "db", "llmtrace.db", "path to SQLite ledger")
	return cmd
}

func cmdKeys() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "keys",
		Short: "Manage inbound API keys",
	}
	cmd.AddCommand(
		&cobra.Command{
			Use:   "add",
			Short: "Mint a new API key",
			RunE: func(cmd *cobra.Command, args []string) error {
				return notImplemented("keys add")
			},
		},
		&cobra.Command{
			Use:   "list",
			Short: "List API keys",
			RunE: func(cmd *cobra.Command, args []string) error {
				return notImplemented("keys list")
			},
		},
		&cobra.Command{
			Use:   "revoke <key-id>",
			Short: "Revoke an API key",
			Args:  cobra.ExactArgs(1),
			RunE: func(cmd *cobra.Command, args []string) error {
				return notImplemented("keys revoke")
			},
		},
	)
	return cmd
}

func cmdStats() *cobra.Command {
	var days int
	cmd := &cobra.Command{
		Use:   "stats",
		Short: "Ledger summary by key/model",
		RunE: func(cmd *cobra.Command, args []string) error {
			return notImplemented("stats")
		},
	}
	cmd.Flags().IntVar(&days, "days", 7, "window in days")
	return cmd
}

func cmdTail() *cobra.Command {
	return &cobra.Command{
		Use:   "tail",
		Short: "Live request stream",
		RunE: func(cmd *cobra.Command, args []string) error {
			return notImplemented("tail")
		},
	}
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
			defer db.Close()
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
			defer db.Close()

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
				if err := inv.Investigate(ctx, a, emit); err != nil {
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
	return &cobra.Command{
		Use:   "explain <anomaly-id>",
		Short: "Deep-dive a single anomaly",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return notImplemented("explain")
		},
	}
}

func cmdReport() *cobra.Command {
	var days int
	var format string
	cmd := &cobra.Command{
		Use:   "report",
		Short: "Polished report",
		RunE: func(cmd *cobra.Command, args []string) error {
			return notImplemented("report")
		},
	}
	cmd.Flags().IntVar(&days, "days", 30, "window to report on")
	cmd.Flags().StringVar(&format, "format", "text", "output format: text | markdown")
	return cmd
}

func notImplemented(name string) error {
	return fmt.Errorf("%s: not implemented yet", name)
}
