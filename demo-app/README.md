# llmtrace-demo-app

A minimal stand-in for a real customer application — it exposes a `/summary`
endpoint backed by an LLM call in `summarizer.py`.

This repo exists so the [llmtrace](https://github.com/Yatsuiii/llmtrace)
autonomous agent has real code to investigate: when a spend anomaly is
detected, the agent reads the pull request that caused it, pinpoints the
regression, and opens a remediation PR here.
