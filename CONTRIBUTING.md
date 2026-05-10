# Contributing

## Development

```bash
# Build (version auto-detected from git)
make build

# Run in development mode
make run

# Run tests with race detector
make test

# Run go vet
make vet

# Clean build artifacts
make clean

# Install to $GOPATH/bin
make install

# Build cross-platform release binaries
make dist
```

Run a single test: `go test ./internal/router/ -v`

## How It Works

```
┌─────────────┐     Anthropic API      ┌─────────────┐     OpenAI API       ┌─────────────┐
│  Claude Code ├──────────────────────►│  oc-go-cc    ├────────────────────►│  OpenCode Go │
│  (CLI)       │  POST /v1/messages   │  (Proxy)     │  /chat/completions  │  (Upstream)  │
│              │◄──────────────────────┤              │◄────────────────────┤              │
└─────────────┘   Anthropic SSE        └─────────────┘   OpenAI SSE          └─────────────┘
```

1. Claude Code sends a request in [Anthropic Messages API](https://docs.anthropic.com/en/api/messages) format
2. oc-go-cc parses the request, counts tokens, and selects a model via routing rules
3. The request is transformed to [OpenAI Chat Completions](https://platform.openai.com/docs/api-reference/chat) format
4. The transformed request is sent to OpenCode Go's endpoint
5. The response (streaming or non-streaming) is transformed back to Anthropic format
6. Claude Code receives the response as if it came from Anthropic directly

### What Gets Transformed

| Anthropic                                                    | OpenAI                                  |
| ------------------------------------------------------------ | --------------------------------------- |
| `system` (string or array)                                   | `messages[0]` with `role: "system"`     |
| `content: [{"type":"text","text":"..."}]`                    | `content: "..."`                        |
| `tool_use` content blocks                                    | `tool_calls` array                      |
| `tool_result` content blocks                                 | `role: "tool"` messages                 |
| `thinking` content blocks                                    | `reasoning_content`                     |
| `stop_reason: "end_turn"`                                    | `finish_reason: "stop"`                 |
| `stop_reason: "tool_use"`                                    | `finish_reason: "tool_calls"`           |
| SSE `message_start` / `content_block_delta` / `message_stop` | SSE `role` / `delta.content` / `[DONE]` |

### DeepSeek V4 Thinking Mode

DeepSeek V4 Pro and Flash use the OpenAI-compatible `/chat/completions` endpoint through OpenCode Go. They support thinking mode and configurable reasoning effort.

For Claude Code and other agentic coding workflows, configure DeepSeek V4 models with:

```json
{
  "provider": "opencode-go",
  "model_id": "deepseek-v4-pro",
  "max_tokens": 8192,
  "reasoning_effort": "max",
  "thinking": {
    "type": "enabled"
  }
}
```

`oc-go-cc` forwards these fields to OpenCode Go as OpenAI Chat Completions parameters:

- `reasoning_effort`: controls DeepSeek V4 thinking effort (`high` or `max`)
- `thinking`: enables or disables DeepSeek V4 thinking mode

DeepSeek V4 thinking responses are returned as OpenAI `reasoning_content` and transformed back into Anthropic `thinking` blocks for Claude Code.

## Architecture

```
cmd/oc-go-cc/main.go           CLI entry point (cobra commands)
internal/
├── config/
│   ├── config.go               Config types
│   ├── loader.go               JSON loading, env overrides, ${VAR} interpolation
│   ├── watcher.go              Hot reload file watcher (fsnotify)
│   └── atomic.go               Atomic config swap for concurrent access
├── router/
│   ├── model_router.go         Model selection based on scenario
│   ├── scenarios.go            Scenario detection (default/think/long_context/background)
│   └── fallback.go             Fallback handler with circuit breaker
├── server/
│   └── server.go               HTTP server setup, graceful shutdown, PID management
├── handlers/
│   ├── messages.go             POST /v1/messages handler (streaming + non-streaming)
│   └── health.go               Health check and token counting endpoints
├── transformer/
│   ├── request.go              Anthropic → OpenAI request transformation
│   ├── response.go             OpenAI → Anthropic response transformation
│   └── stream.go               Real-time SSE stream transformation
├── client/
│   └── opencode.go             OpenCode Go HTTP client
├── daemon/
│   ├── launchd.go              macOS launchd plist management
│   ├── background.go           Background daemon fork
│   └── process.go              PID file and process management
└── token/
    └── counter.go              Tiktoken token counter (cl100k_base)
pkg/types/
├── anthropic.go                Anthropic API types (polymorphic system/content fields)
└── openai.go                   OpenAI API types
configs/
└── config.example.json         Example configuration
```

### Key Design Decisions

- **Polymorphic field handling**: Anthropic's `system` and `content` fields accept both strings and arrays. We use `json.RawMessage` with accessor methods (`SystemText()`, `ContentBlocks()`) to handle both formats correctly.
- **Real-time stream proxying**: SSE events are transformed in-flight, not buffered. This means Claude Code sees responses as they arrive from OpenCode Go.
- **Circuit breaker per model**: Each model gets its own circuit breaker. After 3 consecutive failures, the model is skipped for 30 seconds, then tested again.
- **Environment variable interpolation**: Config values like `"${OC_GO_CC_API_KEY}"` are resolved at load time, so you never need to put secrets in the config file.

## API Endpoints

The proxy exposes these endpoints that Claude Code expects:

| Method | Path                        | Description                           |
| ------ | --------------------------- | ------------------------------------- |
| `POST` | `/v1/messages`              | Main chat endpoint (Anthropic format) |
| `POST` | `/v1/messages/count_tokens` | Token counting                        |
| `GET`  | `/health`                   | Health check                          |
