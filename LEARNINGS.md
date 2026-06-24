## Learnings (durable notes)

This file captures durable, high-signal learnings discovered while operating or modifying `oc-go-cc`.
Keep `AGENTS.md` focused on the project runbook; put “things we learned” here.

### Workflow preferences

- Prefer proactive, autonomous execution with minimal back-and-forth.
- Prefer Makefile targets (`make build`, `make test`, `make lint`) as the canonical verification path.
- Investigations should be evidence-based (trace logs to concrete code paths) and include practical fixes.

### Known provider quirks

- Some OpenAI-compatible providers reject unknown fields inside OpenAI `messages` (e.g. `messages[0].cache_control`).
  - For OpenAI Chat Completions requests, do **not** forward Anthropic `cache_control` into `messages`.

### Operational gotchas

- `make run` does not start the server by itself; start the proxy with:

```bash
make build
./bin/oc-go-cc serve
```

- Streaming request logs can interleave across concurrent requests; request correlation IDs are important for interpreting logs like “client disconnected during stream”.
- Claude Code disconnects after ~6s without **Anthropic SSE events** (`message_start`, `ping`, deltas, etc.). SSE comment keepalives (`:keepalive`) do not reset that timer. On large requests (long context, many tools), upstream TTFT can exceed 6s while `GetStreamingBody` blocks — send `message_start` immediately after flushing stream headers, before waiting on upstream, and emit Anthropic `ping` events every ~3s until content arrives.
- Stream `message_start` / non-streaming responses must echo **`anthropicReq.Model`** (e.g. `claude-opus-4-7`), not the upstream routed model ID (`qwen3.6-plus`). A model mismatch can abort the session mid-stream even after `message_start`.
- The streaming heartbeat goroutine and `ProxyStream` both write to the same `http.ResponseWriter`; without a mutex, interleaved `ping` and `content_block_delta` frames corrupt SSE and show up as `client disconnected during stream` after ~10s on large tool-heavy requests.
- Claude Code can show **"Cooked/Brewed for Ns"** then stop with no answer when upstream TTFT exceeds ~9–11s. SSE `text_delta` keepalives help only if the client counts them; the dominant issue for **200+ tools** is a huge OpenAI POST body while `RouteForStreaming` forced `qwen3.6-plus`. Fix: treat **≥64 tools** as **complex** scenario (not streaming-fast), log `upstream_connect` latency, keep `text_delta` keepalives on the preamble text block until real text/tool output. **Do not** close that block when upstream sends `reasoning_content` first — buffer reasoning while keepalives continue; `thinking_delta` alone does not reset Claude Code's inactivity timer.
