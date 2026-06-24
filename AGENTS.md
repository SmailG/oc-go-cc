## Project: oc-go-cc

`oc-go-cc` is a proxy server that sits between **Claude Code** and **OpenCode Go**. It intercepts Anthropic API requests, transforms them to **OpenAI Chat Completions** format, forwards them to OpenCode Go, and transforms responses back to **Anthropic SSE**.

### Commands (preferred)

Use the Makefile targets whenever possible:

```bash
make build   # Build binary to bin/oc-go-cc
make run     # Run without building (prints CLI help if no subcommand)
make test    # Run tests with race detector
make lint    # go vet + test + golangci-lint (if installed)
make clean   # Remove build artifacts
make install # Build and install to $GOPATH/bin
make dist    # Cross-compile for all platforms
```

Run a single package’s tests:

```bash
go test ./internal/router/ -v
```

Start the proxy server:

```bash
make build
./bin/oc-go-cc serve
```

### Architecture & key constraints

- **Model routing is config-driven, not code-driven**
  - Models are defined in `~/.config/oc-go-cc/config.json`.
  - Adding a new model **should not require code changes**, except when it requires the Anthropic endpoint (see `IsAnthropicModel()`).
- **Three upstream providers** (selected per-model via `provider` field in config)
  - `opencode-go` (default): OpenAI-compatible `/v1/chat/completions` + Anthropic-format `/v1/messages` for MiniMax/Qwen. Subscription billing.
  - `opencode-zen`: same surface plus `/v1/responses` (GPT) and `/v1/models/{id}` (Gemini). Pay-as-you-go.
  - `openrouter`: Anthropic-format `/v1/messages` ("Anthropic Skin") + OpenAI-compatible `/v1/chat/completions`. Pay-as-you-go. Bearer auth via `OC_GO_CC_OPENROUTER_API_KEY`. Provider constants in `internal/config/config.go`.
  - `internal/client/opencode.go` routes by provider + `IsAnthropicModel()` for endpoint format.
- **Routing profiles** (`routing_profile` config field, enum string)
  - `""` (default): uses user-configured `models`/`fallbacks` blocks as-is.
  - `"openrouter"`: replaces `models`/`fallbacks` with built-in OpenRouter-only model set in `internal/router/openrouter_defaults.go`. Scenario detection still runs (uses the profile's `long_context` threshold of 80K). `model_overrides` continue to work for explicit user-requested models. Env override: `OC_GO_CC_ROUTING_PROFILE`. Reference config: `configs/config.openrouter.json`.

### Scenario selection priority

Scenario detection is implemented in `internal/router/scenarios.go` and should be treated as ordered rules:

1. **Long Context** (>60K tokens) → MiniMax (1M context)
2. **Complex** (architectural patterns, tool operations, or **≥64 tools**) → GLM-5.1
3. **Think** (reasoning keywords in system prompt) → GLM-5
4. **Background** (simple read-only ops, no tools) → Qwen3.5 Plus
5. **Default** → Kimi K2.6

**Routing profile** (config): `prefer_capability` + `enable_streaming_scenario_routing` in `configs/config.example.json` use capable models and full scenario routing for streaming. Economy preset: `configs/config.economy.json` (qwen fast streaming). Tool-heavy: ≥64 tools → complex; capability mode also routes ≥8 tools → complex.

For streaming without capability mode, the router downgrades to fast models (Qwen3.6 Plus) for better TTFT.

### Data / types notes

- **Polymorphic field handling**
  - Anthropic `system` and `content` fields can be strings or arrays.
  - `pkg/types/` uses `json.RawMessage` with accessor methods like `SystemText()` / `ContentBlocks()` to support both shapes.

### Key files

- `cmd/oc-go-cc/main.go`: CLI entry point (cobra). Generates default config template.
- `internal/config/`: config types + JSON loader with `${VAR}` env interpolation.
- `internal/transformer/`: request/response format conversion (Anthropic ↔ OpenAI).
- `internal/router/fallback.go`: per-model circuit breaker (3 failures ⇒ 30s skip).
- `configs/config.example.json`: reference config (all options documented).

### Verification checklist (before declaring done)

Run at least:

```bash
make lint
make build
```

If you changed routing, transforms, or streaming behavior, also run:

```bash
make test
```

## Learned User Preferences

- After routing, transform, or streaming changes, run `make test` and `make build` (or `make lint`) before declaring work done.
- Persist durable proxy/Claude Code integration facts in `AGENTS.md` (learned sections) via continual-learning, not only in `LEARNINGS.md`.

## Learned Workspace Facts

- Local proxy for Claude Code: `http://127.0.0.1:3456` with `ANTHROPIC_BASE_URL` and `ANTHROPIC_AUTH_TOKEN=unused`.
- Heavy Claude Code streams often cancel after ~9–11s with `client_canceled=true` when upstream TTFT is slow and the client sees no real `content_block_*` activity.
- Tool-heavy threshold is **64 tools** (`internal/router/scenarios.go`); at or above that, routing uses **complex**, not streaming-fast Qwen, even when `stream=true`. Economy preset maps **complex** to **kimi-k2.6** (GLM fallbacks); capability config uses GLM-5.1.
- Streaming to Claude Code: send `message_start` with the **requested** client model before upstream connects; use placeholder **text** `content_block_start` plus periodic `text_delta` keepalives until real **text** or **tool_use** output arrives. **StreamActivity** stops keepalives on the first **client-visible** Anthropic event, not the first upstream byte. If upstream sends `reasoning_content` first (e.g. GLM), buffer it and keep the preamble text block open — closing early stops keepalives. On **tool-only** turns, skip thinking SSE flush and go straight to `tool_use`. `thinking_delta` and `ping` do not reset Claude's ~6s inactivity timer; gap keepalives must use `text_delta`. SSE comment `:keepalive` alone is insufficient.
- Upstream stream reads use `ctxReadCloser(clientCtx, …)` so Claude Code disconnect aborts blocked upstream `Read()` (handler timeout uses `clientCtx`, not `context.Background()`).
- Streaming model-chain fallback does not retry on `client disconnected` / `ErrClientDisconnected` — the HTTP connection is already closed; Claude Code may open a separate non-streaming request on the same model.
- All streaming SSE writes share one mutex on the handler `responseWriter` (heartbeat goroutine and `ProxyStream` must not write concurrently).
- Streaming logs include `upstream_connect` latency and `request_id`; correlate with `~/.claude/debug/latest` on disconnect investigations.
- `client disconnected during stream` means the HTTP client closed or canceled (`r.Context()`), not necessarily a proxy transform/upstream failure.
- Active runtime config uses the economy preset (`prefer_capability=false`, `enable_streaming_scenario_routing=false`; see `configs/config.economy.json`) unless the user copies `config.example.json` for capability-first routing.
- Start proxy with `./bin/oc-go-cc serve --background` (not foreground `serve`); bare `serve` dies when the terminal closes. Avoid `stop && serve` when `stop` may fail — run separately.

### Learnings / memory

Durable findings and “things we learned” live in `LEARNINGS.md`.
