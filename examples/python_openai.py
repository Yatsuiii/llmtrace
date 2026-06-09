"""
Route OpenAI calls through llmtrace. One line change: base_url.

Run llmtrace first:
    docker compose up -d
    # or: OPENAI_API_KEY=sk-... go run ./cmd/llmtrace serve

Then run this script:
    pip install openai
    python examples/python_openai.py
"""

from openai import OpenAI

client = OpenAI(
    base_url="http://localhost:8080/v1",  # point at llmtrace
    api_key="your-openai-key",
    default_headers={
        "X-Llmtrace-Key": "prod-frontend",
    },
)

response = client.chat.completions.create(
    model="gpt-4o-mini",
    messages=[{"role": "user", "content": "What is 2 + 2?"}],
)

print(response.choices[0].message.content)
print("\nCall recorded in llmtrace — open http://localhost:8080 to see it.")
