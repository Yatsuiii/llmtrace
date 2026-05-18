#!/usr/bin/env bash
# Creates the synthetic "customer app" repo that the llmtrace agent investigates.
#
# It commits a clean summarizer endpoint, then opens and merges THREE PRs that
# all land on the anomaly day:
#   PR #1  bump anthropic SDK   — innocent (touches requirements.txt)
#   PR #2  switch to sonnet     — the regression (touches summarizer.py)
#   PR #3  tidy README          — innocent (touches README.md)
# The agent must read all three diffs and isolate PR #2.
#
# Requires: gh CLI authenticated (run `gh auth status` to check).
#
# Usage:  ./scripts/setup-demo-repo.sh [repo-name]
set -euo pipefail

REPO="${1:-llmtrace-demo-app}"
HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
OWNER="$(gh api user --jq .login)"
WORK="$(mktemp -d)"

echo "==> Creating demo repo ${OWNER}/${REPO}"
cp -r "${HERE}/../demo-app/." "${WORK}/"
cd "${WORK}"
git init -q
git branch -M main
git add .
git commit -q -m "initial summarizer endpoint"
gh repo create "${REPO}" --public --source=. --remote=origin --push

# open_pr <branch> <title> <body> — branches off current main, commits staged
# changes, pushes, opens the PR, and merges it.
open_pr() {
  git push -q -u origin "$1"
  gh pr create --title "$2" --body "$3" --base main --head "$1"
  sleep 2
  gh pr merge "$1" --merge --delete-branch
  git checkout -q main
  git pull -q origin main
}

echo "==> PR #1 — dependency bump (innocent)"
git checkout -q -b deps-bump
echo "anthropic>=0.45.0" > requirements.txt
git add -A && git commit -q -m "bump anthropic SDK to 0.45"
open_pr deps-bump "bump anthropic SDK to 0.45" "Routine dependency update — no behaviour change."

echo "==> PR #2 — the regression (guilty)"
git checkout -q -b switch-summary-to-sonnet
cp "${HERE}/bad-summarizer.py" summarizer.py
git add -A && git commit -q -m "switch summary endpoint to claude-sonnet"
open_pr switch-summary-to-sonnet "switch summary endpoint to claude-sonnet" \
  "Upgrades the /summary endpoint to claude-sonnet for higher-quality summaries. Adds a re-sampling loop so short summaries are retried until detailed enough."

echo "==> PR #3 — README tidy (innocent)"
git checkout -q -b readme-tidy
printf '\n## Status\n\nUsed as the demo subject for llmtrace.\n' >> README.md
git add -A && git commit -q -m "tidy README wording"
open_pr readme-tidy "tidy README wording" "Minor documentation cleanup."

echo ""
echo "==> Done. Demo repo ready: https://github.com/${OWNER}/${REPO}"
echo "    Add this to your llmtrace .env:"
echo "      GITHUB_REPO=${OWNER}/${REPO}"
