"""
Route Anthropic calls through llmtrace. One line change: base_url.

Run llmtrace first:
    docker compose up -d
    # or: go run ./cmd/llmtrace serve

Then run this script:
    pip install anthropic
    python examples/python_anthropic.py
"""

import anthropic

client = anthropic.Anthropic(
    base_url="http://localhost:8080",  # point at llmtrace instead of api.anthropic.com
    api_key="your-anthropic-key",
    default_headers={
        "X-Llmtrace-Key": "prod-frontend",   # tag calls by team/service
        "X-Llmtrace-Session": "demo-session", # optional: group calls into a session
    },
)

message = client.messages.create(
    model="claude-haiku-4-5-20251001",
    max_tokens=256,
    messages=[{"role": "user", "content": "What is 2 + 2?"}],
)

print(message.content[0].text)
print("\nCall recorded in llmtrace — open http://localhost:8080 to see it.")
