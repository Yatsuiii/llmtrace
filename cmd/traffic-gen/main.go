// traffic-gen simulates a production app making LLM calls through llmtrace.
// It runs in two phases separated by a "deploy" event:
//
//	Phase 1 (normal): low volume, cheap model (claude-haiku)
//	Phase 2 (spike):  high volume, expensive model (claude-sonnet), triggered by --spike
//
// Run on Instance 2, pointed at Instance 1:
//
//	traffic-gen --target http://INSTANCE1_IP --key prod-frontend
//	# in another terminal, trigger the spike:
//	traffic-gen --target http://INSTANCE1_IP --spike
package main

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"math/rand"
	"net/http"
	"os"
	"time"
)

const (
	promptHash   = "19e978e38915"
	keyHaiku     = "claude-haiku-3"
	keySonnet    = "claude-sonnet-4-6"
)

func main() {
	target := flag.String("target", "http://localhost:8080", "llmtrace instance URL")
	apiKey := flag.String("key", "prod-frontend", "API key ID to use")
	spike := flag.Bool("spike", false, "send a deploy event + switch to spike mode immediately")
	normalRPM := flag.Int("normal-rpm", 6, "calls per minute in normal mode")
	spikeRPM := flag.Int("spike-rpm", 14, "calls per minute in spike mode")
	flag.Parse()

	if *spike {
		triggerSpike(*target, *apiKey)
		return
	}

	fmt.Printf("traffic-gen → %s  key=%s\n", *target, *apiKey)
	fmt.Println("Phase 1: normal traffic (haiku, low volume)")
	fmt.Println("Run with --spike in another terminal to trigger the anomaly")
	fmt.Println()

	rng := rand.New(rand.NewSource(time.Now().UnixNano()))
	ticker := time.NewTicker(time.Minute / time.Duration(*normalRPM))
	spikeTicker := time.NewTicker(time.Minute / time.Duration(*spikeRPM))
	spikeMode := false

	for {
		var t *time.Ticker
		if spikeMode {
			t = spikeTicker
		} else {
			t = ticker
		}
		<-t.C

		var model string
		var inputTokens, outputTokens int
		var costUSD float64

		if spikeMode {
			// Post-deploy: mostly sonnet, 10% haiku
			if rng.Float64() < 0.90 {
				model = keySonnet
				inputTokens = 4800 + rng.Intn(400)
				outputTokens = 750 + rng.Intn(150)
				costUSD = float64(inputTokens)*0.000003 + float64(outputTokens)*0.000015
			} else {
				model = keyHaiku
				inputTokens = 4800 + rng.Intn(400)
				outputTokens = 750 + rng.Intn(150)
				costUSD = float64(inputTokens)*0.00000025 + float64(outputTokens)*0.00000125
			}
		} else {
			// Pre-deploy: mostly haiku, 10% sonnet
			if rng.Float64() < 0.90 {
				model = keyHaiku
				inputTokens = 4800 + rng.Intn(400)
				outputTokens = 750 + rng.Intn(150)
				costUSD = float64(inputTokens)*0.00000025 + float64(outputTokens)*0.00000125
			} else {
				model = keySonnet
				inputTokens = 4800 + rng.Intn(400)
				outputTokens = 750 + rng.Intn(150)
				costUSD = float64(inputTokens)*0.000003 + float64(outputTokens)*0.000015
			}
		}

		latency := int64(200 + rng.Intn(300))
		postCall(*target, *apiKey, model, inputTokens, outputTokens, costUSD, latency)

		if spikeMode {
			fmt.Printf("  [spike] %s  in=%d out=%d  $%.5f\n", model, inputTokens, outputTokens, costUSD)
		} else {
			fmt.Printf("  [normal] %s  in=%d out=%d  $%.5f\n", model, inputTokens, outputTokens, costUSD)
		}

		// Check if spike was triggered externally (via --spike flag in another process).
		// We detect this by polling a local state file.
		if !spikeMode {
			if _, err := os.Stat("/tmp/llmtrace-spike-trigger"); err == nil {
				fmt.Println()
				fmt.Println(">>> Spike trigger detected — switching to post-deploy mode")
				spikeMode = true
				os.Remove("/tmp/llmtrace-spike-trigger")
			}
		}
	}
}

func triggerSpike(target, apiKey string) {
	fmt.Println("Triggering deploy event + spike mode...")

	// Post the deploy event to Instance 1.
	deploy := map[string]any{
		"id":        fmt.Sprintf("gha-%d-summary-sonnet", time.Now().Unix()),
		"pr_number": 142,
		"title":     "switch summary endpoint to claude-sonnet",
		"repo":      "org/app",
		"branch":    "main",
	}
	postJSON(target+"/ingest/deploy", deploy)
	fmt.Println("Deploy event posted to llmtrace")

	// Signal the running traffic-gen to switch modes.
	os.WriteFile("/tmp/llmtrace-spike-trigger", []byte("1"), 0644)
	fmt.Println("Spike trigger written — traffic-gen will switch to spike mode on next tick")
}

func postCall(target, apiKey, model string, inputTokens, outputTokens int, costUSD float64, latencyMs int64) {
	payload := map[string]any{
		"api_key_id":    apiKey,
		"provider":      "anthropic",
		"model":         model,
		"endpoint":      "/v1/messages",
		"input_tokens":  inputTokens,
		"output_tokens": outputTokens,
		"cost_usd":      costUSD,
		"latency_ms":    latencyMs,
		"status":        200,
		"prompt_hash":   promptHash,
	}
	if err := postJSON(target+"/ingest/call", payload); err != nil {
		fmt.Printf("  [error] %v\n", err)
	}
}

func postJSON(url string, payload any) error {
	b, _ := json.Marshal(payload)
	resp, err := http.Post(url, "application/json", bytes.NewReader(b))
	if err != nil {
		return fmt.Errorf("post %s: %w", url, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		return fmt.Errorf("post %s: status %d", url, resp.StatusCode)
	}
	return nil
}
