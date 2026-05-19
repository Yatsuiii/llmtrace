# llmtrace

**Autonomous AI agent that tells you which deploy caused your LLM bill to spike.**

Every team shipping AI features eventually asks: *"Why did our LLM spend double last week?"*
Helicone, Langfuse, and LiteLLM show you *that* it spiked. llmtrace tells you *why* — by joining spend anomalies to the deploy that caused them, then reasoning over the evidence with an AI agent.

Built for the [AI Agent Olympics Hackathon](https://lablab.ai/event/ai-agents-hackathon) · Milan AI Week 2026

**🟢 Live demo:** https://llmtrace-681081536857.asia-south1.run.app — deployed on Google Cloud Run, autonomous agent running on Gemini.

---

## How it works

```
Your LLM calls → llmtrace proxy → provider (Anthropic / OpenAI)
                      │
                      ▼
              SQLite ledger (calls, costs, latency, prompt fingerprints)
                      │
                      ├── Anomaly detector (rolling baseline + sigma threshold)
                      │
                      └── Gemini agent ──► query_model_distribution
                                      ──► get_deploys_in_window
                                      ──► diff_prompt_model_mix
                                      ──► Attribution + confidence score
```

1. **Proxy** — point your code at `llmtrace serve` instead of `api.anthropic.com`. It forwards requests and records token usage, cost, latency, model, and prompt fingerprint per call.
2. **Detect** — flags per-key spend anomalies using a 7-day rolling baseline + sigma threshold.
3. **Investigate** — a Gemini-powered agent autonomously queries the ledger, finds nearby deploys, diffs the model/prompt mix before and after, and produces a causal attribution with a confidence score.

## Demo

The demo scenario: a team's summary endpoint was silently switched from `claude-haiku` to `claude-sonnet` in PR #129. The new prompt also added a retry loop, bumping call volume 60%. Daily spend on `prod-frontend` jumped from **$4.56 → $19.20** overnight — a 4.2× spike, 28σ above baseline.

The agent finds it in three tool calls:

```
anomaly: key=prod-frontend date=2026-05-03 actual=$12.92 baseline=$4.68 delta=+$8.24 sigma=28.0σ

[tool] query_model_distribution key=prod-frontend 2026-05-01 → 2026-05-05
       → 2 rows, 3554 total calls

[tool] get_deploys_in_window 2026-05-03T08:00:00Z → 2026-05-03T16:00:00Z
       → 1 deploys found

[tool] diff_prompt_model_mix prompt=19e978e38915 pivot=2026-05-03T14:05:00Z
       → before: 91% haiku / after: 89% sonnet (+58% volume)

── Attribution ──────────────────────────────────────────────────────────
The spend anomaly on prod-frontend on 2026-05-03 was caused by deploy
gha-129-summary-sonnet (PR #129) — "switch summary endpoint to
claude-sonnet" — which completed at 2026-05-03T14:05:00Z.

This deploy shifted prompt hash 19e978e38915 from predominantly
claude-haiku to predominantly claude-sonnet, a more expensive model.

Confidence: 0.95

Recommendation: Evaluate if the quality improvement from claude-sonnet
justifies the cost increase. Consider A/B testing or gradual rollout
for future model changes.
```

## Quickstart — Docker (Cloud Run / any host)

```bash
git clone https://github.com/Yatsuiii/llmtrace.git
cd llmtrace
cp .env.example .env          # add your GEMINI_API_KEY
docker compose up -d
```

Open `http://localhost:8080` — dashboard loads with demo data auto-seeded on first run.

Deployed on **Google Cloud Run** via `gcloud run deploy --source .` — the live demo above runs exactly this image.

**Requirements:** Docker, a `GEMINI_API_KEY` (free tier at Google AI Studio).

### Manual / local

```bash
go run ./cmd/llmtrace seed          # seed 30 days of demo data
GEMINI_API_KEY=xxx go run ./cmd/llmtrace serve
# open http://localhost:8080
```

## CLI

```
llmtrace seed                        seed demo scenario into ledger
llmtrace serve [--port 8080]         run dashboard + agent server
llmtrace anomalies [--days 30]       detect and list spend anomalies
llmtrace analyze [--days 30]         detect anomalies + AI investigation
```

`analyze` without a browser — streams the full agent investigation to stdout:

```bash
GEMINI_API_KEY=xxx llmtrace analyze --days 30

detected 2 anomaly(ies)

── Anomaly 1/2: prod-frontend on 2026-05-03 ─────────────────────────
[tool] query_model_distribution ...
[tool] get_deploys_in_window ...
[tool] diff_prompt_model_mix ...

── Attribution ──────────────────────────────
...Confidence: 0.95
```

## Architecture

| Layer | What it does |
|---|---|
| `internal/proxy` | HTTP reverse proxy — forwards to Anthropic/OpenAI, records call telemetry |
| `internal/storage` | SQLite ledger — calls, API keys, anomalies, deploys, correlations |
| `internal/detect` | Rolling 7-day baseline + sigma threshold anomaly detection |
| `internal/agent` | Gemini multi-turn tool-calling agent — autonomous causal investigation |
| `internal/web` | Dashboard (Chart.js cost trend + deploy markers) + SSE investigation stream |
| `internal/seed` | Deterministic demo scenario seeder (reproducible with fixed RNG) |

## Why not Helicone / Langfuse / LiteLLM?

| Tool | What it does | What it misses |
|---|---|---|
| Helicone | Hosted observability + caching | Hosted-only, no deploy correlation, no causal agent |
| Portkey | AI gateway with routing | Feature-stew, no anomaly attribution |
| LiteLLM | Open-source proxy | No anomaly detection, no deploy join |
| Langfuse | LLM observability | Trace-focused, not cost-focused |
| **llmtrace** | **Self-hosted gateway + autonomous causal agent** | MVP: Anthropic focus, single tenant |

The key difference: llmtrace is not a dashboard — it's an agent that *investigates*. It uses the same architectural pattern as [costtrace](https://github.com/Yatsuiii/costtrace) does for AWS FinOps, applied to LLM spend.

## Stack

- **Go** — proxy, ledger, anomaly detection, web server (`net/http` + `html/template`)
- **SQLite** — `modernc.org/sqlite` (pure Go, no CGo, single file)
- **Gemini** — `google.golang.org/genai` SDK, tool-calling agent loop
- **Chart.js** — cost trend chart with deploy annotation (CDN, no build pipeline)
- **Google Cloud Run** — containerized production deploy (`gcloud run deploy --source .`)

## License

MIT
