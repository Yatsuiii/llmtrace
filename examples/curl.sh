#!/usr/bin/env bash
# Test that llmtrace is proxying correctly with a raw curl call.
# Run llmtrace first: docker compose up -d

curl http://localhost:8080/v1/messages \
  -H "Content-Type: application/json" \
  -H "X-Llmtrace-Key: prod-frontend" \
  -d '{
    "model": "claude-haiku-4-5-20251001",
    "max_tokens": 64,
    "messages": [{"role": "user", "content": "ping"}]
  }'
