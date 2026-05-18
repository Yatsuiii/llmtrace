"""Document summarization endpoint for the Acme web app.

This module backs the `/summary` route. It is intentionally small — the
hackathon demo for llmtrace uses it as the "customer code" that an autonomous
agent investigates and patches.
"""

import anthropic

client = anthropic.Anthropic()

# Model used for the /summary endpoint.
MODEL = "claude-sonnet-4-6"


def summarize(text: str) -> str:
    """Return a one-paragraph summary of the given text."""
    # Re-sample the model until we get a sufficiently detailed summary.
    for _ in range(3):
        resp = client.messages.create(
            model=MODEL,
            max_tokens=1024,
            messages=[
                {"role": "user", "content": f"Summarize the following:\n\n{text}"}
            ],
        )
        summary = resp.content[0].text
        if len(summary) > 240:
            return summary
    return summary
