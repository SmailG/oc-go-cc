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
- **Two upstream API endpoints**
  - OpenAI endpoint (`/v1/chat/completions`): used by most models (GLM, Kimi, MiMo, Qwen).
  - Anthropic endpoint (`/v1/messages`): used only by MiniMax models.
  - `internal/client/opencode.go` routes by model ID via `IsAnthropicModel()`.

### Scenario selection priority

Scenario detection is implemented in `internal/router/scenarios.go` and should be treated as ordered rules:

1. **Long Context** (>60K tokens) → MiniMax (1M context)
2. **Complex** (architectural patterns, tool operations) → GLM-5.1
3. **Think** (reasoning keywords in system prompt) → GLM-5
4. **Background** (simple read-only ops, no tools) → Qwen3.5 Plus
5. **Default** → Kimi K2.6

For streaming, the router downgrades to fast models (Qwen3.6 Plus) for better TTFT.

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

### Learnings / memory

Durable findings and “things we learned” live in `LEARNINGS.md`.
