# AI Assistant

Kula ships with an optional AI assistant powered by a **local** [Ollama](https://github.com/ollama/ollama)
model. All inference runs on your own machine through the local Ollama (or OpenAI-compatible)
API — nothing is sent to any external service.

## Requirements

- Ollama running locally (default `http://localhost:11434`).
- A model pulled into Ollama, preferably one that supports tool calling (e.g.
  `ollama pull qwen3:4b` or `ollama pull llama3.2:3b`; see [Picking a model](#picking-a-model)).

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
> (`localhost`, `127.0.0.1`, `::1`), and every connection is checked to go to a loopback
> address, whatever `localhost` resolves to. This prevents the assistant from being abused to
> make Kula issue requests to arbitrary internal hosts (SSRF).

When enabled, a 🤖 button appears in the dashboard header.

## Features

- **Multi-session conversations** — open independent chat threads and switch between them,
  even while an answer is being written: it goes on in its own session and is there when you
  come back. A chat session is named after its first question. **−** deletes a session (it
  asks first when the session has messages); deleting the last one starts a fresh one.
- **Per-chart analysis** — click the ✨ icon on any chart card to open a session pre-loaded
  with the series you can see on that chart as CSV (a series hidden through the legend is left
  out), so you can ask "what's causing this spike?". Long ranges are cut into 50 rows, each
  with the mean and the maximum, so a short spike is kept. Clicking ✨ again returns to that
  chart's session; **Analyse again with fresh data** in the line under the session bar sends
  the chart as it is now.
- **Snapshot with a time** — a chat session pins a metrics snapshot, shown as "Snapshot
  22:14:03" under the session bar with a **Refresh** button. A question asked more than a
  minute after the snapshot takes a new one first. The snapshot carries the sample's time and
  zone, the host name and the CPU core count, and the model is told the server's current time.
- **Stop and Retry** — while an answer is written, **Send** becomes **Stop**, which ends it and
  keeps what arrived. The answer says what it is waiting for ("Waiting for *model*…",
  "Thinking…", "Fetching CPU Usage (-6h)…") and counts the seconds of a long wait. A failed
  answer says why (backend down, too many questions, signed out) and offers **Retry**; a failed
  question is not sent to the model again with the next one.
- **Data used** — under an answer, the history the model fetched for it.
- **Agentic tool calling** — the model can call a `get_metrics` tool to pull historical time
  series for `cpu`, `memory`, `swap`, `load`, `network`, `disk_io`, `disk_space` or `gpu` on
  demand (downsampled to about 100 points, default range the last hour, at most 31 days, as
  in the history API; up to 5 tool-call rounds of 8 calls per turn). The data comes with its
  range, zone and step, and with dates when the range spans more than a day. Application,
  container and sensor history is not available to the model. Models without tool support
  still work, without this tool (see [Picking a model](#picking-a-model)).
- **Thinking models** — the reasoning of a model that thinks before answering (e.g. `qwen3`)
  is shown apart from the answer, collapsed.
- **Model selector** — switch between any locally-available Ollama model mid-session; each
  answer is labelled with the model that wrote it, and the choice is remembered.
- **Draggable & resizable panel** — drag by the header, resize from the bottom-right grip; the
  position and size are remembered. On a phone the panel is a bottom sheet instead.
- **Streaming responses** with markdown rendering (code blocks are shown exactly as written,
  with a **Copy** button; links are shown as text, not made clickable); the model is told which
  markdown the panel renders. **Copy** under an answer copies its text. The panel shows up to 32,768 characters of an answer; a longer one is cut there with
  a notice, and its stream is stopped.
- **Keyboard** — Enter sends, Shift+Enter starts a new line, and an Enter that ends an input
  method's composition does not send. Escape closes the panel while focus is in it, and focus
  returns to the button that opened it. An empty input sends nothing.

When the backend is down the panel says so and Send is disabled; when Ollama runs but has no
model, the panel names the `ollama pull` command to run. Text typed meanwhile is never sent on
its own once the backend is back. Conversations live in the browser tab and are gone when it
closes.

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
- **Stays on the configured address** — `ollama.url` must be loopback, a connection to an
  address that is not loopback is refused even when the name resolves to one, and a redirect
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

Prefer a model that supports tool calling, such as `qwen3:4b` or `llama3.2:3b`: only such a
model can fetch history with `get_metrics`. (Ollama's model library marks them with a "tools"
tag.) A model without tool support, such as `gemma3`, still answers: the first time the backend
refuses the tool for it, Kula repeats the request without the tool and remembers the model,
so its later chats are sent without it from the start. Its answers rest only on the session's
metrics snapshot or the chart data, and the panel says that the model cannot read history.
The memory lasts until Kula restarts, or until the model no longer appears in the backend's
model list.

Next: [Prometheus Exporter](11-prometheus.md).
