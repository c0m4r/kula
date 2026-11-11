# AI Assistant

Kula ships with an optional AI assistant powered by a **local** [Ollama](https://github.com/ollama/ollama)
model. All inference runs on your own machine through the local Ollama (or OpenAI-compatible)
API — nothing is sent to any external service.

## Requirements

- Ollama running locally (default `http://localhost:11434`).
- A model pulled into Ollama (e.g. `ollama pull gemma3:4b`).

Any other OpenAI-compatible server on the same host works too: the proxy probes Ollama's
`/api/tags` first and falls back to `/v1/models` and `/v1/chat/completions`.

## Enable it

```yaml
ollama:
  enabled: true
  url: "http://localhost:11434"   # must be a loopback address
  model: "gemma4:e4b"
  timeout: "300s"                 # max time to wait for a streaming response
```

> **Security:** the `url` is validated at config load time to allow only loopback addresses
> (`localhost`, `127.0.0.1`, `::1`). This prevents the assistant from being abused to make
> Kula issue requests to arbitrary internal hosts (SSRF).

When enabled, a 🤖 button appears in the dashboard header.

## Features

- **Multi-session conversations** — open independent chat threads and switch between them.
- **Per-chart analysis** — click the 🤖 icon on any chart card to open a session pre-loaded
  with that chart's recent data as CSV, so you can ask "what's causing this spike?"
- **Agentic tool calling** — the model can call a `get_metrics` tool to pull historical time
  series for `cpu`, `memory`, `swap`, `load`, `network`, `disk_io`, `disk_space` or `gpu` on
  demand (downsampled to about 100 points, default range the last hour, at most 31 days, as
  in the history API; up to 5 tool-call rounds of 8 calls per turn).
- **Model selector** — switch between any locally-available Ollama model mid-session.
- **Draggable & resizable panel** — drag by the header, resize from the bottom-right grip.
- **Streaming responses** with markdown rendering. The panel shows up to 32,768 characters of
  an answer; a longer one is cut there with a notice, and the rest is not fetched.

## How requests are protected

Kula proxies to Ollama through a hardened endpoint (`/api/ollama/*`). The proxy:

- **Bounds what reaches the model** — only your questions and the model's earlier answers are
  forwarded from the browser (no system or tool messages), the newest 20 of them; the prompt
  and each question are clamped to 2000 characters, each earlier answer to 8000, and the
  session's metrics snapshot or chart data to 16,000. Null bytes are stripped.
- **Runs only offered models** — a chat uses the configured `model` or one the backend lists
  in the model selector; any other name falls back to the configured model.
- **Rate-limits** — 10 chat requests per IP per minute; 60 metadata (model list/context)
  requests per IP per minute; at most 4 chats answered at once, from all clients together.
- **Caps bodies & responses** — chat request body max 32 KB (the panel leaves out the oldest
  turns of a long conversation to stay under it); Ollama response stream max 10 MB.
- **Limits the tool loop** — at most 5 rounds of up to 8 `get_metrics` calls per chat turn.
- **Stays on the configured address** — `ollama.url` must be loopback, and a redirect from it
  is not followed: the request fails, and the Kula log names the redirect target, so you can
  set `ollama.url` to that address instead.
- **Keeps backend errors in the log** — when the backend is down or answers with an error, the
  browser is told only that the request failed; the Kula log has the details.

See the developer [Web Server & API](../dev/07-web-server-api.md) and
[Security Model](../dev/08-security.md) for the full picture.

## Picking a model

Smaller instruction-tuned models (3–8B) respond quickly and are usually sufficient for
interpreting metrics. Larger models give better analysis at the cost of latency — raise
`timeout` if a large model streams slowly.

Next: [Prometheus Exporter](11-prometheus.md).
