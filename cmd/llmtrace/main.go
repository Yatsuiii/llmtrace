package main

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"
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
	)
	if err := root.Execute(); err != nil {
		os.Exit(1)
	}
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
	cmd := &cobra.Command{
		Use:   "serve",
		Short: "Run the proxy server",
		RunE: func(cmd *cobra.Command, args []string) error {
			return notImplemented("serve")
		},
	}
	cmd.Flags().IntVar(&port, "port", 8080, "listen port")
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
	var format string
	cmd := &cobra.Command{
		Use:   "anomalies",
		Short: "List spend/latency anomalies",
		RunE: func(cmd *cobra.Command, args []string) error {
			return notImplemented("anomalies")
		},
	}
	cmd.Flags().IntVar(&days, "days", 30, "window to scan for anomalies")
	cmd.Flags().StringVar(&format, "format", "text", "output format: text | markdown")
	return cmd
}

func cmdAnalyze() *cobra.Command {
	var days int
	cmd := &cobra.Command{
		Use:   "analyze",
		Short: "Anomalies + deploy correlations",
		RunE: func(cmd *cobra.Command, args []string) error {
			return notImplemented("analyze")
		},
	}
	cmd.Flags().IntVar(&days, "days", 30, "window to analyze")
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
