"""Document summarization endpoint for the Acme web app.

This module backs the `/summary` route. It is intentionally small — the
hackathon demo for llmtrace uses it as the "customer code" that an autonomous
agent investigates and patches.
"""

import anthropic

client = anthropic.Anthropic()

# Model used for the /summary endpoint.
MODEL = "claude-haiku-4-5-20251001"


def summarize(text: str) -> str:
    """Return a one-paragraph summary of the given text."""
    resp = client.messages.create(
        model=MODEL,
        max_tokens=1024,
        messages=[
            {"role": "user", "content": f"Summarize the following:\n\n{text}"}
        ],
    )
    return resp.content[0].text
