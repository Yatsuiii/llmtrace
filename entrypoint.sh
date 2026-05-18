#!/bin/sh
set -e

# Cloud Run injects $PORT; default to 8080 elsewhere.
PORT="${PORT:-8080}"

# Persistent volume if mounted, otherwise the ephemeral container fs.
DB="${LLMTRACE_DB:-/data/llmtrace.db}"
mkdir -p "$(dirname "$DB")"

if [ ! -f "$DB" ]; then
  echo "first run — seeding demo data into $DB"
  ./llmtrace seed --db "$DB"
fi

# Autonomous watcher is opt-in via AUTONOMOUS=1 so a deploy with a Gemini
# key set doesn't unconditionally spend quota on startup.
if [ "$AUTONOMOUS" = "1" ] && [ -n "$GEMINI_API_KEY" ]; then
  echo "starting on :$PORT in autonomous mode"
  exec ./llmtrace serve --port "$PORT" --db "$DB" --autonomous
fi

echo "starting on :$PORT (dashboard + on-demand agent)"
exec ./llmtrace serve --port "$PORT" --db "$DB"
