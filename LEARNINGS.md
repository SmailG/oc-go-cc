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
- OpenRouter's Anthropic Skin (`/v1/messages`) accepts raw Anthropic-format requests for any model in its catalog. Stream cancellation is NOT supported for MiniMax on OpenRouter (per docs); billing continues if Claude Code disconnects mid-stream. The Anthropic Skin returns `redacted_thinking` blocks alongside `thinking` blocks — Claude Code may not handle `redacted_thinking` (worth a real-session smoke test, currently unverified).
- There is **no `routing_profile` config field** — it was a fork-local mechanism, removed in the 2026-09-15 upstream sync. An OpenRouter-only setup is a plain config file: `configs/config.openrouter.json` spells out `models`/`fallbacks` directly, which needs no Go code and cannot drift from upstream. (The `long_context` threshold works the same way, read from that model entry's `context_threshold`.)

### Operational gotchas

- `make run` does not start the server by itself; start the proxy with:

```bash
make build
./bin/oc-go-cc serve
```

- Streaming request logs can interleave across concurrent requests; request correlation IDs are important for interpreting logs like “client disconnected during stream”.
- Claude Code disconnects after ~6s without **Anthropic SSE events** (`message_start`, `ping`, deltas). SSE comment keepalives (`:keepalive`) do **not** reset that timer, so upstream's comment-frame heartbeat keeps the socket warm while the client gives up anyway. Current behaviour: `handleStreaming` writes `message_start` before attempting any model, and `responseWriter.WriteKeepalive` then emits `ping` **events** rather than comment frames. Two constraints that are easy to miss:
  - The preamble goes through `WritePreamble`, which does **not** set `ssePayloadWritten`. That flag means "committed to a model" and permanently disables fallback — writing the preamble normally would kill fallback on every stream.
  - It is **skipped when any model in the chain is raw Anthropic passthrough** (`chainUsesRawAnthropic`), because that path forwards the upstream's own `message_start` verbatim and the client would see two. A nil provider registry counts as raw.
- Stream `message_start` / non-streaming responses must echo **`anthropicReq.Model`** (e.g. `claude-opus-4-7`), not the upstream routed model ID (`qwen3.6-plus`). A model mismatch can abort the session mid-stream even after `message_start`.
- The streaming heartbeat goroutine and `ProxyStream` both write to the same `http.ResponseWriter`; without a mutex, interleaved `ping` and `content_block_delta` frames corrupt SSE and show up as `client disconnected during stream` after ~10s on large tool-heavy requests.
- Claude Code can show **"Cooked/Brewed for Ns"** then stop with no answer when upstream TTFT exceeds ~9–11s. For **200+ tools** the cause is the size of the OpenAI POST body, not the model, so `RouteForStreaming` forcing `qwen3.6-plus` made it worse. Current behaviour: `DetectScenarioWithTools` routes a request with **≥64 tools** (`toolHeavyThreshold`) to **complex** before any text heuristic; long-context still wins over it. `enable_streaming_scenario_routing: true` separately stops the fast downgrade and needs no code.
  - `DetectScenario`/`Route` are kept as zero-tool wrappers around `DetectScenarioWithTools`/`RouteWithTools` **on purpose**: changing their signatures would touch ~42 upstream test call sites and re-conflict on every sync.
  - `thinking_delta` alone does not reset Claude Code's inactivity timer — that is why the heartbeat sends `ping`, not a thinking delta.
