# Demo Script — llmtrace (3–4 min)

Recording checklist before you start:
- [ ] `llmtrace seed` has been run (fresh DB)
- [ ] `GEMINI_API_KEY` set, server running on port 8080
- [ ] Browser open at `http://localhost:8080` (or live Vultr URL)
- [ ] Terminal visible alongside browser (split screen)
- [ ] Font size bumped (browser 125%, terminal 16pt)

---

## Section 1 — The Problem (30 sec, talking head or voiceover)

> "Every team building with LLMs eventually gets surprised by a bill.
> Helicone and Langfuse tell you *that* spend went up.
> But nobody tells you *which deploy caused it*.
> That's what llmtrace does."

---

## Section 2 — The Dashboard (45 sec)

**On screen:** browser at the dashboard URL.

> "This is the llmtrace dashboard. Thirty days of LLM call data
> across three API keys. prod-frontend in red, two control keys in grey."

Point to the chart.

> "You can see everything was flat — about four dollars a day — until
> May third. Then prod-frontend spikes to twenty dollars overnight
> and stays there. The amber line is a deploy marker.
> PR 129 landed at 14:05 UTC. The spend broke loose eleven minutes later."

Scroll down to the anomaly cards.

> "The detector flagged it at 28 sigma above the seven-day baseline.
> Eight dollars and twenty-four cents above expected on the first day alone."

---

## Section 3 — The Agent (90 sec, the hero moment)

**On screen:** anomaly card for 2026-05-03.

Click **"Investigate →"**.

> "I'm clicking Investigate. This kicks off an autonomous agent —
> powered by Gemini — that queries the call ledger with three tools."

Wait for tool lines to stream in. Narrate as they appear:

> "First tool: model distribution around the anomaly date.
> It sees two models in use on prod-frontend —
> haiku and sonnet."

> "Second tool: deploy lookup in a four-hour window around May 3rd.
> One deploy found: PR 129, 'switch summary endpoint to claude-sonnet'."

> "Third tool: prompt model diff. Same prompt fingerprint —
> same code path — but before the deploy it was ninety-one percent haiku.
> After: eighty-nine percent sonnet. And call volume jumped
> fifty-eight percent — the new prompt has a retry loop."

Wait for the attribution to stream in.

> "There's the attribution. Zero-point-nine-five confidence.
> PR 129 caused it. The model swap plus the retry loop
> multiplied cost by four-point-two times."

---

## Section 4 — CLI (30 sec)

**Switch to terminal.**

```bash
GEMINI_API_KEY=xxx llmtrace analyze --days 30
```

> "Same investigation from the terminal — no browser needed.
> Pipes into CI, scripts, Slack bots. Wherever you run post-deploy checks."

Let it stream the first few lines, then cut.

---

## Section 5 — Close (20 sec)

> "llmtrace is self-hosted, open-source, and deploys in two minutes
> on a six-dollar Vultr VM. No SaaS dependency, no data leaves your infra.
>
> Every team at this hackathon is burning tokens right now.
> When your bill surprises you next week —
> llmtrace tells you which deploy to blame."

**Show:** GitHub repo URL on screen.

---

## Tips

- Keep Section 3 as the longest — the live streaming is the most visual.
- If the agent hits a rate limit and shows the retry message, that's fine — it demonstrates real behaviour, don't cut it.
- Record at 1080p, export at 1080p30. lablab.ai accepts YouTube/Loom links.
- One take is fine; the streaming output is different each run (model output varies).
