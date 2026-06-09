/**
 * Route Anthropic calls through llmtrace. One line change: baseURL.
 *
 * Run llmtrace first:
 *   docker compose up -d
 *
 * Then run this script:
 *   npm install @anthropic-ai/sdk
 *   node examples/node_anthropic.js
 */

import Anthropic from "@anthropic-ai/sdk";

const client = new Anthropic({
  baseURL: "http://localhost:8080", // point at llmtrace
  apiKey: "your-anthropic-key",
  defaultHeaders: {
    "X-Llmtrace-Key": "prod-frontend",
    "X-Llmtrace-Session": "demo-session",
  },
});

const message = await client.messages.create({
  model: "claude-haiku-4-5-20251001",
  max_tokens: 256,
  messages: [{ role: "user", content: "What is 2 + 2?" }],
});

console.log(message.content[0].text);
console.log("\nCall recorded in llmtrace — open http://localhost:8080 to see it.");
