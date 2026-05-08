# llmtrace

Deploy-to-LLM-spend causal attribution.

> Which prompt change caused this $4k/week spike?

`llmtrace` is a self-hosted reverse proxy for LLM provider APIs (Anthropic, OpenAI, Bedrock) with a built-in cost ledger, latency tracking, and anomaly detection. It joins LLM spend anomalies to the deploy events that caused them — using the same architectural pattern as [costtrace](https://github.com/Yatsuiii/costtrace) does for AWS spend.

**Status: scaffolding.** Tracking issues against the 3-week MVP scope.

## How it works (planned)

1. **Proxy** — your code points at `llmtrace serve` instead of `api.anthropic.com`. It forwards requests upstream and records token usage, cost, latency, model, and prompt fingerprint per call.
2. **Detect** — flags per-key spend/latency anomalies using a rolling baseline + sigma threshold.
3. **Correlate** — matches anomalies to GitHub Actions deploys, scores confidence based on model-change / prompt-change evidence.

## How it differs from other tools

| Tool | What it does | What it misses |
|---|---|---|
| Helicone | Hosted observability + caching | Hosted-only, no deploy correlation |
| Portkey | AI gateway with routing/caching | Feature-stew, no anomaly attribution |
| LiteLLM | Open-source proxy | No anomaly detection, no deploy join |
| Langfuse | LLM observability platform | Trace-focused, not cost-focused |
| **llmtrace** | **Deploy → LLM-spend causal chain** | MVP: Anthropic only, single tenant |

## License

MIT
