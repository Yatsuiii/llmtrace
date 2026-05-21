# llmtrace

**Autonomous FinOps co-pilot for LLM-powered apps.** Finds the deploy that spiked your LLM bill, and explains why with evidence.

[![live demo](https://img.shields.io/badge/live%20demo-online-brightgreen)](https://llmtrace-681081536857.asia-south1.run.app) ![go](https://img.shields.io/badge/go-1.22-00ADD8?logo=go) ![license](https://img.shields.io/badge/license-MIT-blue) ![status](https://img.shields.io/badge/status-active%20development-orange)

```
$ llmtrace analyze --days 30

anomaly  key=prod-frontend  2026-05-03  $12.92 vs $4.68 baseline  (+$8.24, 28σ)

  → caused by deploy gha-129-summary-sonnet (PR #129)
    "switch summary endpoint to claude-sonnet", merged 14:05 UTC
    shifted prompt 19e978e3 from 91% haiku to 89% sonnet (+58% volume)
    confidence 0.95
```

---

## The problem

Every team shipping AI features eventually asks the same question: *"Why did our LLM spend double last week?"*

Existing tools (Helicone, Portkey, Langfuse, LiteLLM) will show you *that* spend went up. None of them tell you *what shipped* that made it go up. The deploy-to-spend join is the same gap [costtrace](https://github.com/Yatsuiii/costtrace) closes for AWS cost. Nobody had closed it for LLM cost.

`llmtrace` does the join. It records every LLM call through a self-hosted proxy, detects per-key spend anomalies on a rolling baseline, correlates them to GitHub Actions deploy events in a time window, then dispatches an autonomous Gemini agent to reason over the evidence and name the responsible pull request with a confidence score.

> Live demo: **https://llmtrace-681081536857.asia-south1.run.app**
> Deployed on Google Cloud Run. Autonomous agent runs on Gemini 2.0 Flash.

---

## How it works

```
Your LLM calls  ─►  llmtrace proxy  ─►  Anthropic / OpenAI
                         │
                         ▼
                 SQLite ledger (calls · cost · latency · prompt fingerprint)
                         │
                         ├──  Anomaly detector  (7d rolling baseline + σ threshold)
                         │
                         └──  Gemini agent  ─►  query_model_distribution
                                           ─►  get_deploys_in_window
                                           ─►  diff_prompt_model_mix
                                           ─►  Attribution + confidence score + PR link
```

1. **Proxy.** Point your app at `llmtrace serve` instead of `api.anthropic.com`. It forwards every request and records token usage, cost, latency, model, and a prompt fingerprint per call.
2. **Detect.** A rolling 7-day baseline flags per-key spend anomalies above a configurable σ threshold.
3. **Investigate.** A Gemini agent autonomously queries the ledger, finds nearby deploys, diffs the model+prompt mix before and after each deploy, and produces a causal attribution with evidence.

<p align="center">
  <img src="demo/dash-shot.png" alt="llmtrace dashboard" width="900">
</p>

---

## Demo scenario

A team's summary endpoint was silently switched from `claude-haiku` to `claude-sonnet` in PR #129. The new prompt added a retry loop on top, pushing call volume up 60%. Daily spend on the `prod-frontend` key jumped from **$4.56 to $19.20** overnight. That's a 4.2× spike, 28σ above baseline.

The agent finds it in three tool calls:

```
anomaly: key=prod-frontend date=2026-05-03 actual=$12.92 baseline=$4.68 delta=+$8.24 sigma=28.0σ

[tool] query_model_distribution key=prod-frontend 2026-05-01 → 2026-05-05
       → 2 rows, 3554 total calls

[tool] get_deploys_in_window 2026-05-03T08:00:00Z → 2026-05-03T16:00:00Z
       → 1 deploys found

[tool] diff_prompt_model_mix prompt=19e978e38915 pivot=2026-05-03T14:05:00Z
       → before: 91% haiku · after: 89% sonnet (+58% volume)

── Attribution ──────────────────────────────────────────────────────────
The spend anomaly on prod-frontend on 2026-05-03 was caused by deploy
gha-129-summary-sonnet (PR #129) "switch summary endpoint to claude-sonnet",
which completed at 2026-05-03T14:05:00Z.

This deploy shifted prompt hash 19e978e38915 from predominantly claude-haiku
to predominantly claude-sonnet, a more expensive model.

Confidence: 0.95

Recommendation: Evaluate if the quality improvement from claude-sonnet
justifies the cost increase. Consider A/B testing or gradual rollout
for future model changes.
```

---

## Why not Helicone / Portkey / Langfuse / LiteLLM?

| Tool | Shape | Does deploy-to-spend join? |
|---|---|:-:|
| Helicone | Hosted observability + caching | no |
| Portkey | AI gateway with routing | no |
| LiteLLM | Open-source proxy | no |
| Langfuse | LLM observability platform | no |
| **llmtrace** | **Self-hosted gateway with an autonomous causal agent** | **yes** |

The wedge in one sentence: *deploy-to-LLM-spend causality with zero hosted-SaaS dependency.* The architectural pattern is borrowed from [costtrace](https://github.com/Yatsuiii/costtrace), where it does the same job for AWS cost, and applied to a different domain.

---

## Quickstart

### Docker (Cloud Run or any host)

```bash
git clone https://github.com/Yatsuiii/llmtrace.git
cd llmtrace
cp .env.example .env          # add your GEMINI_API_KEY
docker compose up -d
```

Open `http://localhost:8080`. The dashboard loads with a demo scenario auto-seeded on first run.

Deployed on Google Cloud Run via `gcloud run deploy --source .`. The live demo above runs exactly this image.

Requirements: Docker and a `GEMINI_API_KEY` (free tier available at Google AI Studio).

### Local Go build

```bash
go run ./cmd/llmtrace seed                # seed 30 days of demo data
GEMINI_API_KEY=xxx go run ./cmd/llmtrace serve
# open http://localhost:8080
```

---

## CLI

```
llmtrace seed                        seed demo scenario into ledger
llmtrace serve [--port 8080]         run dashboard + agent server
llmtrace anomalies [--days 30]       detect and list spend anomalies
llmtrace analyze [--days 30]         detect anomalies + AI investigation
```

`analyze` streams the full agent investigation to stdout, no browser needed:

```bash
GEMINI_API_KEY=xxx llmtrace analyze --days 30

detected 2 anomaly(ies)

── Anomaly 1/2: prod-frontend on 2026-05-03 ─────────────────────────
[tool] query_model_distribution ...
[tool] get_deploys_in_window ...
[tool] diff_prompt_model_mix ...

── Attribution ──────────────────────────────
... Confidence: 0.95
```

---

## Architecture

| Layer | What it does |
|---|---|
| `internal/proxy` | HTTP reverse proxy. Forwards to Anthropic and OpenAI, records call telemetry. |
| `internal/storage` | SQLite ledger via `modernc.org/sqlite`. Tables for calls, API keys, anomalies, deploys, correlations. |
| `internal/detect` | Rolling 7-day baseline plus σ-threshold anomaly detection. |
| `internal/agent` | Gemini multi-turn tool-calling agent. Autonomous causal investigation. |
| `internal/web` | Dashboard (Chart.js cost trend with deploy markers) and SSE investigation stream. |
| `internal/seed` | Deterministic demo scenario seeder, reproducible with fixed RNG. |

---

## Roadmap

`llmtrace` is in active development. The single-tenant MVP is shipped and live. Next milestones:

- **v0.2 · Real deploy correlation.** Replace the seeded deploy table with live GitHub Actions ingestion. Time-window matcher plus lineage scoring (model_change, prompt_change, error_spike) with additive-capped confidence.
- **v0.3 · Multi-provider depth.** First-class OpenAI parser (already stubbed), then Bedrock InvokeModel.
- **v0.4 · Anomaly memory.** Embed every resolved anomaly and its root-cause PR. Similarity search over past incidents so the agent can say *"this looks like the spike from April 18, same author, same prompt, fixed by reverting deploy X."*
- **v0.5 · Forecast mode.** Convert rolling baselines into per-key spend forecasts. Predict which keys will blow budget within the hour, not just flag after the fact.

Out of scope (explicit): multi-tenant SaaS, web UI dashboard polish beyond the current minimum, caching layer (that's Helicone/Portkey territory), prompt routing.

---

## Honest limitations

This is an MVP. Things to know before relying on it:

- **Single-tenant.** No per-tenant isolation. Run one instance per team.
- **Anthropic-first parsing.** OpenAI is supported but with less testing on streaming edge cases. Bedrock is not yet implemented.
- **Deploy correlation in MVP is seeded, not live.** The live GitHub Actions ingestion is v0.2 work (see roadmap). Today the agent reasons against a pre-populated deploy table.
- **Pricing is hardcoded** in `internal/pricing/rates.go` and needs manual updates when providers change rates.
- **SSE streaming is single-process.** No horizontal-scaling story for the dashboard yet.
- **Production security needs work.** TLS-required mode exists but inbound API key rotation is manual. Don't run on a public IP without a reverse-proxy in front.

These are MVP scope decisions, not unknowns. The trajectory is in the roadmap.

---

## Stack

- **Go** for the proxy, ledger, anomaly detection, web server. `net/http` and `html/template`, no framework.
- **SQLite** via `modernc.org/sqlite` (pure Go, no CGo, single file on disk).
- **Gemini** via `google.golang.org/genai` SDK for the tool-calling agent loop.
- **Chart.js** for the dashboard cost trend and deploy annotations (CDN, no build pipeline).
- **Google Cloud Run** for the production deploy (`gcloud run deploy --source .`).

---

## License

MIT.

---

<sub>Part of the [trace family](https://github.com/Yatsuiii) of operational tooling. See also [costtrace](https://github.com/Yatsuiii/costtrace) for the same pattern applied to AWS cost.</sub>

<sub>An earlier version of llmtrace was originally built during the AI Agent Olympics Hackathon at Milan AI Week 2026. It is now under continued active development as part of the trace family.</sub>
