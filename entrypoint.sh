#!/bin/sh
set -e
DB=/data/llmtrace.db
if [ ! -f "$DB" ]; then
  echo "first run — seeding demo data..."
  ./llmtrace seed --db "$DB"
fi
exec ./llmtrace serve --port 8080 --db "$DB"
