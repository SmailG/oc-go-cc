# Configuration

## Config File

Location: `~/.config/routatic-proxy/config.json`

Override with `ROUTATIC_PROXY_CONFIG` environment variable.

For migration, `~/.config/oc-go-cc/config.json` is loaded when the new config file does not exist, and every `OC_GO_CC_*` environment variable is still accepted as a fallback for its `ROUTATIC_PROXY_*` replacement.

## Full Config Reference

```json
{
  "api_key": "${ROUTATIC_PROXY_API_KEY}",
  "host": "127.0.0.1",
  "port": 3456,
  "hot_reload": false,
  "anthropic_first": {
    "enabled": false,
    "base_url": "https://api.anthropic.com"
  },

  "jev_routing": {
    "enabled": false,
    "mode": "active",
    "model": "jev-1.14.0",
    "timeout_ms": 800,
    "api_key_env": "TYPESAFE_AI_API_KEY",
    "base_url": "https://api.typesafe.ai"
  },

  "enable_cost_based_routing": false,
  "cost_routing": {
    "enabled": true,
    "prefer_providers": ["opencode-go", "aws-bedrock"],
    "max_context_window": 1000000,
    "penalty_per_provider": {
      "openrouter": 0.05
    }
  },

  "models": {
    "default": {
      "provider": "opencode-go",
      "model_id": "deepseek-v4-pro",
      "temperature": 0.7,
      "max_tokens": 8192,
      "reasoning_effort": "max",
      "thinking": { "type": "enabled" }
    },
    "background": {
      "provider": "opencode-go",
      "model_id": "deepseek-v4-flash",
      "temperature": 0.5,
      "max_tokens": 2048
    },
    "think": {
      "provider": "opencode-go",
      "model_id": "glm-5.2",
      "temperature": 0.7,
      "max_tokens": 8192
    },
    "complex": {
      "provider": "opencode-go",
      "model_id": "deepseek-v4-pro",
      "temperature": 0.7,
      "max_tokens": 8192,
      "reasoning_effort": "max",
      "thinking": { "type": "enabled" }
    },
    "long_context": {
      "provider": "opencode-go",
      "model_id": "minimax-m3",
      "temperature": 0.7,
      "max_tokens": 16384,
      "context_threshold": 80000
    },
    "fast": {
      "provider": "opencode-go",
      "model_id": "deepseek-v4-flash",
      "temperature": 0.7,
      "max_tokens": 4096
    }
  },

  "fallbacks": {
    "default": [
      { "provider": "opencode-go", "model_id": "qwen3.7-plus" },
      { "provider": "opencode-go", "model_id": "qwen3.7-max" },
      { "provider": "opencode-zen", "model_id": "nemotron-3-ultra-free" },
      { "provider": "opencode-zen", "model_id": "mimo-v2.5-free" },
      { "provider": "opencode-zen", "model_id": "deepseek-v4-flash-free" }
    ],
    "think": [{ "provider": "opencode-go", "model_id": "qwen3.7-plus" }],
    "complex": [{ "provider": "opencode-go", "model_id": "qwen3.7-plus" }],
    "long_context": [{ "provider": "opencode-go", "model_id": "qwen3.7-plus" }],
    "fast": [{ "provider": "opencode-go", "model_id": "qwen3.7-plus" }]
  },

  "model_overrides": {
    "claude-sonnet-4.5": {
      "provider": "opencode-zen",
      "model_id": "claude-sonnet-4.5",
      "temperature": 0.7,
      "max_tokens": 8192,
      "vision": true
    },
    "deepseek-v4-pro": {
      "provider": "opencode-zen",
      "model_id": "deepseek-v4-pro",
      "temperature": 0.7,
      "max_tokens": 8192,
      "reasoning_effort": "max",
      "thinking": {
        "type": "enabled"
      }
    },
    "deepseek-v4-flash-free": {
      "provider": "opencode-zen",
      "model_id": "deepseek-v4-flash-free",
      "temperature": 0.7,
      "max_tokens": 4096
    }
  },

  "opencode_go": {
    "base_url": "https://opencode.ai/zen/go/v1/chat/completions",
    "anthropic_base_url": "https://opencode.ai/zen/go/v1/messages",
    "timeout_ms": 300000
  },

  "opencode_zen": {
    "base_url": "https://opencode.ai/zen/v1/chat/completions",
    "anthropic_base_url": "https://opencode.ai/zen/v1/messages",
    "responses_base_url": "https://opencode.ai/zen/v1/responses",
    "gemini_base_url": "https://opencode.ai/zen/v1/models",
    "timeout_ms": 300000
  },

  "logging": {
    "level": "info",
    "requests": true
  }
}
```

## Anthropic-First Failover

Enable this mode to keep Anthropic as Claude Code's primary API and use the configured OpenCode model chain only while Anthropic is unavailable:

```json
{
  "anthropic_first": {
    "enabled": true,
    "base_url": "https://api.anthropic.com"
  }
}
```

Configure Claude Code with only the proxy address:

```bash
export ANTHROPIC_BASE_URL=http://127.0.0.1:3456
unset ANTHROPIC_AUTH_TOKEN ANTHROPIC_API_KEY
```

Leaving the credential variables unset preserves the saved Claude Pro, Max, Team, or Enterprise login. The proxy forwards the raw request, OAuth credential, `anthropic-version`, and complete `anthropic-beta` capability header to Anthropic.

Fallback occurs for HTTP 408, 429, 5xx, and transport failures before a response starts. HTTP 400, 401, 403, 404, and other request errors are returned unchanged. After a failure, the proxy honors `Retry-After`; otherwise it uses exponential backoff from 30 seconds to 15 minutes. One real user request probes recovery while concurrent requests continue through OpenCode. No synthetic health requests are sent.

Once response bytes have started, a failed stream cannot be restarted on another model without duplicating content. `/v1/messages/count_tokens` remains local and does not affect availability state.

When OpenCode Go returns `GoUsageLimitError`, remaining Go models are skipped for that request and the chain advances to Zen. The default chain uses Qwen3.7 Plus, Qwen3.7 Max, then the currently working Zen-free Nemotron 3 Ultra, MiMo V2.5, and DeepSeek V4 Flash models. Free Zen endpoints are time-limited and may retain data under [OpenCode's documented privacy terms](https://opencode.ai/docs/zen/#privacy).

## Jev Request Classifier (`jev_routing`)

Scenario routing decides `complex`, `think` and `background` with substring keywords on the latest user message, so it misfires on negations and prose ("keep it simple, don't over-optimize" routes to `complex`). `jev_routing` is an opt-in, fail-open classifier that asks the TypeSafe System One model (Jev) what kind of work the request is. It is **off by default**.

> **Privacy: when enabled, the first 600 characters of the latest user message are sent to TypeSafe (`api.typesafe.ai`) on every request that reaches keyword routing.** A request matched by `model_overrides` or `model_family_overrides` is never sent: the override already chose the model, and its fallback chain is built without Jev. Only the user's text is sent: no system prompt, earlier turns, tool results or file contents. A turn that carries only tool output (no text the user typed) is not sent at all. Claude Code's own tagged blocks (`<system-reminder>`, `<command-name>`, `<command-message>`, `<command-args>`, `<local-command-stdout>`, `<local-command-stderr>`, `<local-command-caveat>`) and its tool-call echoes (text blocks starting `Called the <X> tool with the following input` or `Result of calling the`) are removed before the 600-character cut, so they use none of the budget and are never sent; a turn that holds only those is not sent either. An opening tag with no closing tag drops that tag and everything after it (fail closed); a stray closing tag is left in place. **Whatever the user typed or pasted is sent as written**, including a file path or a pasted snippet inside the first 600 characters. Text over 64 KB is not classified at all. Do not enable it for sessions whose prompts must not leave your machine. When it is enabled the proxy prints this notice at startup.

| Field | Default | Meaning |
| --- | --- | --- |
| `enabled` | `false` | Turn the classifier on. A disabled block changes nothing. It only runs for requests that reach scenario routing: a request with a non-empty `model` is resolved by `respect_requested_model` first (see **What runs first**). |
| `mode` | `"active"` | `"active"` lets Jev decide (waits at most `timeout_ms`, falls back to the keyword result on any failure); `"shadow"` only logs what Jev would choose and routes as before. `enabled` stays `false` by default, so nothing is sent until you turn it on. |
| `model` | `"jev-1.14.0"` | TypeSafe model id sent in the request. |
| `timeout_ms` | `800` | Deadline for the call, 1 to 10000. |
| `api_key_env` | `"TYPESAFE_AI_API_KEY"` | **Name** of the environment variable that holds the API key. The key is read from the environment on each call; it is never stored in the config file or logged. A value that is not a plain variable name fails the load when `enabled` is `true` (a disabled block is not validated). |
| `base_url` | `"https://api.typesafe.ai"` | TypeSafe API base URL. Must be `https`, except `http` is accepted for a loopback host (`localhost`, `127.0.0.0/8`, `::1`) so a local stub or tunnel works: the key and the prompt text travel here, so no cleartext off the machine. Redirects are never followed; a 3xx is a failed call and the keyword result stands. |

```bash
export TYPESAFE_AI_API_KEY=...   # set in the environment the proxy starts from
```

**What runs first.** The exact checks stay ahead of Jev and are unchanged: token count above the long-context threshold, a new image, and a tool count at or above the tool-heavy threshold. Jev only sees requests those leave undecided.

**Requests that name a model.** Before scenario routing, a request with a non-empty `model` is resolved by `respect_requested_model`. When that option is unset or `true` (the default), the requested model is used and scenario routing is skipped, so Jev is never called for it; Jev runs for such requests only when `respect_requested_model` is `false`. A request with an empty `model` goes straight to scenario routing. (A `model_overrides` / `model_family_overrides` match also skips Jev, see **Overrides**.)

**Shadow mode.** The route is still the keyword scenario. Jev runs in a background goroutine, so the request is never delayed: `timeout_ms` bounds the background call, not the request. At most 16 classifications are in flight; a request over that bound is simply not sampled (logged as a warning, throttled like the other warnings). Each completed call logs one line:

```
level=INFO msg="jev routing" mode=shadow keyword_scenario=complex jev_level=1 jev_scenario=default architecture_prob=0.12 agree=false latency_ms=212
```

`architecture_prob` is a second question ("does the request ask for an architecture-level or multi-component change?") and is for the log only; it does not affect routing. Set `mode` to `shadow` to run it for a while and compare `agree=false` lines before switching to `active`.

**Active mode.** Jev replaces the keyword decision: level 2 (needs design or deep reasoning) is `complex`, level 1 (ordinary coding work) is `default`, level 0 (trivial or read-only) is `background` only if the keyword matcher's no-tool precondition holds (none of its tool or file-changing words such as `edit`, `write` or `tool` appear in any message of the conversation, tool output included, so in a long tool-using session level 0 mostly lands on `default`), otherwise `default`. The request waits for Jev, at most `timeout_ms`.

**Limits of active mode.** (1) A turn that only carries tool output has no authored text, so nothing is sent for it. Active mode reuses Jev's decision for the latest authored user message (kept in memory for 30 minutes, at most 256 entries, keyed by a hash of the text), so a task keeps its model through a tool loop; if no decision is cached (expired, or the text was never classified) the keyword result stands. (1a) Claude Code streams, so active mode needs `enable_streaming_scenario_routing: true`; with the default `false` a stream routes to `fast` without a Jev call, and the startup notice warns about it. The default `timeout_ms` is 800; in active mode the request waits up to that long when Jev is slow or failing. (2) `think` is not reachable in active mode on the non-streaming path (Jev's three levels map to `background`, `default` and `complex`), and `agree` is always `false` in a log line whose keyword scenario is `think`. (3) Measured classifier latency was 158 to 455 ms, so a 300 ms deadline is tight for a cold connection.

**Fail-open.** A missing key, timeout, network error, non-200 response (including 401, 429, 5xx), malformed body or out-of-range value falls back to the keyword result and logs a warning with the reason (never the key or the response body). Warnings are throttled per distinct reason: the first occurrence logs immediately, then at most one line per minute for that reason, carrying `suppressed=N` for the occurrences it skipped. A missing key or a Jev outage is therefore one line a minute, not one per request.

**Overrides.** When `model_overrides` or `model_family_overrides` matches, the override is the primary and the scenario chain behind it is built by `RouteWithToolsNoJev` / `RouteForStreamingNoJev`, which never call Jev, so the fallback order is the same as without `jev_routing` and no text is sent.

**Streaming.** A streaming request goes through `RouteWithTools` (and so through Jev, in either mode) only when `enable_streaming_scenario_routing` is `true`. With the default `false` it takes the streaming path, which routes to `fast`. In **shadow** mode that path now also makes the same background Jev call and logs the same line (agree, levels, latency), so Claude Code streams are sampled; `keyword_scenario` in the line is what the keyword matcher decides for the text, not the `fast` scenario the stream is sent to. It shares the 16-slot bound and `timeout_ms`, skips what the non-streaming path skips (long-context or image requests, turns with no authored text), and never delays the request. The tool-heavy check is not applied there because the streaming path does not know the tool count. The route is unchanged, including the complex/think downgrade to `fast`. In **active** mode the streaming path still makes no Jev call and routes to `fast` as before.

## Providers

routatic-proxy supports four providers for upstream API calls:

### OpenCode Go (`opencode-go`)

- Default provider for most models
- Uses OpenAI Chat Completions and Anthropic Messages endpoints
- Pricing: $5/month subscription + usage-based

### OpenCode Zen (`opencode-zen`)

- Curated, tested models with pay-as-you-go pricing
- Supports additional endpoint formats:
  - **Chat Completions** (`/v1/chat/completions`) — OpenAI-compatible models
  - **Anthropic Messages** (`/v1/messages`) — Claude, Qwen models
  - **OpenAI Responses** (`/v1/responses`) — GPT models
  - **Google Gemini** (`/v1/models/{id}`) — Gemini models
- Set `"provider": "opencode-zen"` in your model config to use Zen

### AWS Bedrock (`aws-bedrock`)

- Models hosted on AWS Bedrock Mantle
- Supports two wire formats:
  - **OpenAI Chat Completions** (`/v1/chat/completions`) — default, works with most models
  - **Anthropic Messages** (`/v1/messages`) — for Claude and other Anthropic-native models
- Supports the `OpenAI-Project` header for project-based routing
- Bedrock-specific API key falls back to the global key pool if unset
- Set `"provider": "aws-bedrock"` in your model config to use Bedrock

```json
{
  "aws_bedrock": {
    "base_url": "https://bedrock-mantle.us-east-1.api.aws/v1/chat/completions",
    "anthropic_base_url": "https://bedrock-mantle.us-east-1.api.aws/v1/messages",
    "api_key": "${BEDROCK_API_KEY}",
    "project_id": "proj_xxx",
    "wire_format": "openai",
    "timeout_ms": 300000,
    "stream_timeout_ms": 60000,
    "streaming_timeout_ms": 600000
  }
}
```

Set `wire_format: "anthropic"` for models that need raw Anthropic Messages format (e.g., Claude on Bedrock). Requires `anthropic_base_url` to be configured.

### OpenRouter (`openrouter`)

- Unified API for accessing 200+ models from multiple providers (OpenAI, Anthropic, Google, Meta, Mistral, and more)
- Uses OpenAI Chat Completions API format
- Pay-as-you-go pricing with competitive rates
- Set `"provider": "openrouter"` in your model config to use OpenRouter

#### Configuration Schema

```json
{
  "openrouter": {
    "name": "openrouter",
    "base_url": "https://openrouter.ai/api/v1",
    "api_key": "${OPENROUTER_API_KEY}",
    "api_keys": ["${OPENROUTER_KEY_1}", "${OPENROUTER_KEY_2}"],
    "enabled": true,
    "timeout_ms": 300000,
    "stream_timeout_ms": 60000
  }
}
```

| Field | Type | Required | Description |
|-------|------|----------|-------------|
| `name` | `string` | No | Provider display name (defaults to "openrouter") |
| `base_url` | `string` | No | API endpoint base URL. Default: `https://openrouter.ai/api/v1` |
| `api_key` | `string` | Yes* | Single API key for authentication. Required if `api_keys` not set |
| `api_keys` | `string[]` | Yes* | Multiple API keys for round-robin rotation. Required if `api_key` not set |
| `enabled` | `bool` | No | Whether this provider is active. Default: `true` |
| `timeout_ms` | `int` | No | Request timeout in milliseconds. Default: `300000` (5 minutes) |
| `stream_timeout_ms` | `int` | No | Per-chunk timeout during streaming. Default: `60000` (1 minute) |

*At least one of `api_key` or `api_keys` must be configured.

#### Environment Variable Overrides

| Variable | Description | Precedence |
|----------|-------------|------------|
| `ROUTATIC_PROXY_OPENROUTER_API_KEY` | Single API key override | Highest |
| `ROUTATIC_PROXY_OPENROUTER_API_KEYS` | Comma-separated keys for round-robin | Highest |
| `ROUTATIC_PROXY_OPENROUTER_BASE_URL` | Custom base URL override | Highest |

Environment variables take precedence over config file values. Config values support `${VAR}` interpolation.

Precedence order: `*_API_KEYS` → `*_API_KEY` → config file `api_keys` → config file `api_key`

#### Example Configurations

**Single-key setup:**

```json
{
  "openrouter": {
    "api_key": "sk-or-v1-xxxxxxxxxxxxxxxxxxxxxxxx"
  }
}
```

**Multi-key round-robin for load balancing:**

```json
{
  "openrouter": {
    "api_keys": [
      "sk-or-v1-key-1",
      "sk-or-v1-key-2",
      "sk-or-v1-key-3"
    ]
  }
}
```

**Custom base URL (for enterprise/self-hosted):**

```json
{
  "openrouter": {
    "base_url": "https://openrouter.mycompany.com/api/v1",
    "api_key": "${OPENROUTER_API_KEY}",
    "enabled": true
  }
}
```

#### Integration with Cost-Based Routing

OpenRouter works seamlessly with `cost_routing`. Use `penalty_per_provider` to adjust effective costs:

```json
{
  "cost_routing": {
    "enabled": true,
    "prefer_providers": ["openrouter", "opencode-go"],
    "max_context_window": 1000000,
    "penalty_per_provider": {
      "openrouter": 0.02,
      "opencode-go": 0.0,
      "aws-bedrock": 0.05
    }
  }
}
```

Penalties are additive to the raw model cost. Example: a model costing $0.10/1M tokens on OpenRouter with a 0.02 penalty has effective cost $0.12/1M tokens. Use this to bias routing preferences without excluding providers entirely.

#### Model Resolution via Catalog

Models are referenced using the `provider/model-name` pattern. OpenRouter models use the `openrouter/` prefix:

```json
{
  "model_overrides": {
    "claude-opus-4": {
      "provider": "openrouter",
      "model_id": "anthropic/claude-opus-4",
      "temperature": 0.7,
      "max_tokens": 8192,
      "vision": true
    },
    "gpt-4o": {
      "provider": "openrouter",
      "model_id": "openai/gpt-4o",
      "temperature": 0.7,
      "max_tokens": 4096
    },
    "gemini-2.5-pro": {
      "provider": "openrouter",
      "model_id": "google/gemini-2.5-pro-preview-07-11",
      "temperature": 0.7,
      "max_tokens": 8192
    }
  }
}
```

**Discovering models:**

1. Visit [openrouter.ai/models](https://openrouter.ai/models) for the complete model list
2. Use the `routatic-proxy models` command to see cached catalog entries
3. Check the [OpenRouter API docs](https://openrouter.ai/docs) for pricing and context limits

The `model_id` in your config must match OpenRouter's model identifier exactly (e.g., `anthropic/claude-opus-4`, `openai/gpt-4o`, `google/gemini-2.5-pro-preview-07-11`).

#### Use Cases

**Accessing specific models:** Use OpenRouter when you need models not available on other providers:

```json
{
  "models": {
    "complex": {
      "provider": "openrouter",
      "model_id": "anthropic/claude-opus-4",
      "temperature": 0.7,
      "max_tokens": 8192,
      "reasoning_effort": "max"
    }
  }
}
```

**Fallback chains:** Include OpenRouter as a fallback when primary providers fail:

```json
{
  "fallbacks": {
    "default": [
      { "provider": "opencode-go", "model_id": "deepseek-v4-pro" },
      { "provider": "openrouter", "model_id": "anthropic/claude-sonnet-4.8" },
      { "provider": "openrouter", "model_id": "openai/gpt-4.1" }
    ]
  }
}
```

**Cost optimization:** Use `cost_routing` with provider penalties to automatically select the cheapest available model:

```json
{
  "cost_routing": {
    "enabled": true,
    "prefer_providers": ["openrouter"],
    "penalty_per_provider": {
      "openrouter": -0.01
    }
  }
}
```

**Specialized models:** Access niche models for specific tasks:

```json
{
  "models": {
    "think": {
      "provider": "openrouter",
      "model_id": "deepseek/deepseek-r1-free",
      "temperature": 0.6,
      "max_tokens": 8192
    },
    "long_context": {
      "provider": "openrouter",
      "model_id": "google/gemini-1.5-pro",
      "temperature": 0.7,
      "max_tokens": 16384,
      "context_threshold": 80000
    }
  }
}
```

## Environment Variables

Environment variables override config file values. Config values also support `${VAR}` interpolation.

| Variable                | Description                                 | Default                                          |
| ----------------------- | ------------------------------------------- | ------------------------------------------------ |
| `ROUTATIC_PROXY_API_KEY`      | OpenCode Go API key (**required**)          | —                                                |
| `ROUTATIC_PROXY_CONFIG`       | Custom config file path                     | `~/.config/routatic-proxy/config.json`                 |
| `ROUTATIC_PROXY_HOST`         | Proxy listen host                           | `127.0.0.1`                                      |
| `ROUTATIC_PROXY_PORT`         | Proxy listen port                           | `3456`                                           |
| `ROUTATIC_PROXY_OPENCODE_URL` | OpenCode Go API endpoint                    | `https://opencode.ai/zen/go/v1/chat/completions` |
| `ROUTATIC_PROXY_OPENCODE_ZEN_URL` | OpenCode Zen API endpoint              | `https://opencode.ai/zen/v1/chat/completions`    |
| `ROUTATIC_PROXY_OPENROUTER_API_KEY` | OpenRouter single API key           | —                                                |
| `ROUTATIC_PROXY_OPENROUTER_API_KEYS` | OpenRouter key pool (comma-separated) | —                                             |
| `ROUTATIC_PROXY_MODEL_STUDIO_API_KEY` | Model Studio single API key         | —                                                |
| `ROUTATIC_PROXY_MODEL_STUDIO_API_KEYS` | Model Studio key pool (comma-separated) | —                                            |
| `ROUTATIC_PROXY_MODEL_STUDIO_URL` | Model Studio Chat Completions endpoint | `https://dashscope-intl.aliyuncs.com/compatible-mode/v1/chat/completions` |
| `ROUTATIC_PROXY_MODEL_STUDIO_ANTHROPIC_URL` | Model Studio native Anthropic endpoint (empty by default; when set it becomes the preferred wire format) | — |
| `ROUTATIC_PROXY_LOG_LEVEL`    | Log level: `debug`, `info`, `warn`, `error` | `info`                                           |
| `ROUTATIC_PROXY_GUI_PORT`     | Dashboard port                              | `3445`                                           |
| `ROUTATIC_PROXY_LOG_FILE`     | Log file the dashboard's Proxy Logs panel follows (read-only; the panel never writes) | `~/.config/routatic-proxy/routatic-proxy.log` |

Legacy equivalents such as `OC_GO_CC_API_KEY`, `OC_GO_CC_CONFIG`, and `OC_GO_CC_PORT` continue to work. When both names are set, the `ROUTATIC_PROXY_*` value wins.

## Hot Reload

By default, config changes require a server restart. Enable hot reload to watch for config file changes and apply them automatically:

```json
{
  "hot_reload": true
}
```

When enabled, the proxy watches the config directory for changes (handling editors that save via rename/create) and reloads the config automatically. You can also trigger a manual reload by sending `SIGHUP` to the process:

```bash
kill -HUP <PID>
```

## Model Routing

The proxy automatically detects the type of request and routes to the appropriate model based on context size and content analysis:

| Scenario         | Trigger                                             | Default Model       | Why                                          |
| ---------------- | --------------------------------------------------- | ------------------- | -------------------------------------------- |
| **Long Context** | >100K tokens (configurable)                         | `minimax-m3`        | 1M context window                            |
| **Vision**       | Latest user message contains an image               | (not preconfigured) | Splits into `vision` / `vision_complex`      |
| **Complex**      | "architect", "refactor", "complex" in system prompt | `deepseek-v4-pro`   | Best reasoning & architectural understanding |
| **Think**        | "think", "plan", "reason" in system prompt          | `glm-5.2`           | Strong reasoning at lower cost               |
| **Background**   | "read file", "grep", "list directory"               | `deepseek-v4-flash` | Cheap, perfect for simple ops                |
| **Default**      | Everything else                                     | `deepseek-v4-pro`   | Best balance of quality & cost               |

**See [MODELS.md](MODELS.md) for detailed model capabilities, costs, and routing recommendations.**

DeepSeek V4 users can set any scenario model to `deepseek-v4-pro` or `deepseek-v4-flash`. For deterministic max thinking, add `reasoning_effort: "max"` and `thinking: {"type":"enabled"}` to that scenario's model config and fallback entries.

### Routing in Detail

| Scenario         | Trigger                                                                      | Config Key            | Default Model  |
| ---------------- | ---------------------------------------------------------------------------- | --------------------- | -------------- |
| **Default**      | Standard chat                                                                | `models.default`      | `deepseek-v4-pro`   |
| **Complex**      | Architectural keywords or tool-heavy operations                              | `models.complex`      | `deepseek-v4-pro`   |
| **Think**        | System prompt contains "think", "plan", "reason"; or thinking content blocks | `models.think`        | `glm-5.2`           |
| **Long Context** | Token count exceeds `context_threshold` (default 100K)                       | `models.long_context` | `minimax-m3`        |
| **Vision**       | Latest user message contains an image                                        | `models.vision`       | (not preconfigured) |
| **Background**   | File read, directory list, grep patterns                                     | `models.background`   | `deepseek-v4-flash` |

Routing priority: **Long Context** > **Vision** > **Complex** > **Think** > **Background** > **Default**

## Cost-Based Routing

When enabled, the proxy uses a catalog of model pricing data to automatically select the cheapest eligible model for each scenario, rather than always using the statically configured primary model.

```json
{
  "cost_routing": {
    "enabled": true,
    "prefer_providers": ["opencode-go", "aws-bedrock"],
    "max_context_window": 1000000,
    "penalty_per_provider": {
      "openrouter": 0.05
    }
  }
}
```

| Field | Type | Description |
|-------|------|-------------|
| `enabled` | `bool` | Activates cost-aware model selection. Can also be set via the legacy `enable_cost_based_routing` top-level flag. |
| `prefer_providers` | `string[]` | Restricts candidate providers globally. When set, only models on these providers are considered. Intersected with per-scenario `preferred_providers` when both are set. |
| `max_context_window` | `int64` | Hard cap on candidate model context window. Models exceeding this size are excluded. `0` (default) means no cap. |
| `penalty_per_provider` | `map[string]float64` | Per-provider cost penalty added to the effective cost during selection. Use this to bias away from providers without removing them entirely. |

When enabled, `SelectCheapest` resolves all eligible provider/model pairs for the matched scenario, applies the max context window cap, filters by the preferred providers set, and sorts by effective cost (raw cost + penalty). The cheapest candidate wins. This replaces the static `models.<scenario>` primary model.

```json
{
  "cost_routing": {
    "penalty_per_provider": {
      "opencode-go": 0.1,
      "openrouter": 0.05
    }
  }
}
```

Penalties are additive to the raw cost. A model on `opencode-go` with base cost 2.0 and a penalty of 0.1 has effective cost 2.1.

## Fallback Chains

When a model request fails (network error, rate limit, server error), the proxy tries the next model in the fallback chain:

```
Primary model -> Fallback 1 -> Fallback 2 -> ... -> Error (all failed)
```

Each model also has a **circuit breaker** that tracks consecutive failures. After 3 failures, the circuit opens and that model is skipped for 30 seconds, then tested again (half-open state).

## Model Overrides (`model_overrides`)

`model_overrides` lets you map a specific client-requested model name (the value of the `model` field in `/v1/messages`) to a fixed `ModelConfig`. This is useful when you want clients to be able to request a particular model (e.g. `claude-sonnet-4.5`) without that model going through the scenario router.

When a request arrives, the proxy checks `model_overrides[<model>]` **first**. If the requested model has an entry, the override is used as the primary. The fallback chain is `fallbacks[<model>]`, falling back to `fallbacks["default"]` if no override-specific entry exists. The scenario-routed chain is then appended as a **safety-net fallback** (deduplicated by `model_id`).

```json
{
  "model_overrides": {
    "claude-sonnet-4.5": {
      "provider": "opencode-zen",
      "model_id": "claude-sonnet-4.5",
      "temperature": 0.7,
      "max_tokens": 8192,
      "vision": true
    },
    "deepseek-v4-pro": {
      "provider": "opencode-zen",
      "model_id": "deepseek-v4-pro",
      "temperature": 0.7,
      "max_tokens": 8192,
      "reasoning_effort": "max",
      "thinking": {
        "type": "enabled"
      }
    }
  }
}
```

Each entry accepts the same fields as a `ModelConfig` (`provider`, `model_id`, `temperature`, `max_tokens`, `reasoning_effort`, `thinking`, etc.). `model_id` is required; `provider` must be `"opencode-go"`, `"opencode-zen"`, or `"aws-bedrock"` (or omitted to inherit the default).

See `routatic-proxy models` for the complete list of available Zen models across all endpoint types (Claude, GPT, Gemini, and free-tier).

### Routing precedence

When a request arrives, the proxy selects a model chain using the following order:

1. **`model_overrides[<model>]`** — if the request's `model` field has an entry, use it as the primary and append the scenario chain as a safety net.
2. **`respect_requested_model`** — if enabled and `models[<model>]` is configured, use the requested model with default fallbacks.
3. **Scenario routing** — fall back to the scenario chain (`default`, `background`, `think`, `complex`, `long_context`, `fast`).

> **Trust model:** any client whose requests flow through the proxy can select from the configured `model_overrides` set without additional authentication. If you run the proxy as a shared service, treat `model_overrides` as a privileged allowlist.

### Streaming Scenario Routing

`enable_streaming_scenario_routing` controls whether streaming requests are evaluated by the full scenario router or routed directly to the `fast` scenario.

> **Note for Claude Code `/review-code`, `/ultracode`, and multi-agent workflows**
>
> If you use Claude Code workflows that dispatch many subagents or produce many parallel tool calls, enable streaming scenario routing:
>
> ```json
> {
>   "enable_streaming_scenario_routing": true
> }
> ```
>
> Without this option, streaming requests are routed through the `fast` scenario even when the request is actually tool-heavy. This can route complex Claude Code workloads, such as `/review-code` with many `Agent` tool calls, to a fast model that may not handle parallel tool-call orchestration reliably.
>
> When enabled, streaming requests are evaluated by the same scenario router as non-streaming requests, allowing large or tool-heavy workloads to use `complex` or `long_context` models instead of always using the `fast` model.

Recommended setup for Claude Code review workflows:

```json
{
  "enable_streaming_scenario_routing": true,
  "models": {
    "fast": {
      "provider": "opencode-go",
      "model_id": "deepseek-v4-flash",
      "max_tokens": 4096
    },
    "complex": {
      "provider": "opencode-go",
      "model_id": "minimax-m3",
      "max_tokens": 8192
    },
    "long_context": {
      "provider": "opencode-go",
      "model_id": "minimax-m3",
      "max_tokens": 16384,
      "context_threshold": 80000
    }
  }
}
```

Use the `fast` scenario for short/simple requests. Use `complex` or `long_context` for code review, multi-agent dispatch, large diffs, many tools, or long-context Claude Code sessions.

## Claude Code Model Picker

You can select proxy models from Claude Code's `/model` picker in two ways.

### Type any model name (always works)

Claude Code's `/model` picker also accepts a free-form model name. Type any value the proxy understands — a scenario alias (`default`, `fast`, `complex`, …), a `model_overrides` key, or a catalog canonical name like `opencode-go/kimi-k2.6` — and the proxy routes it. No extra configuration is needed; this works regardless of Claude Code version.

### Gateway model discovery (opt-in, adds entries to the picker)

Recent Claude Code versions can auto-populate the picker by querying the proxy's [`GET /v1/models`](docs/reference-api.md#get-v1models) endpoint. When enabled, discovered models appear in `/model` labeled **"From gateway"** alongside the built-in entries (Sonnet, Opus, …).

Enable it by setting, alongside `ANTHROPIC_BASE_URL`:

```bash
export ANTHROPIC_BASE_URL=http://127.0.0.1:3456
export ANTHROPIC_AUTH_TOKEN=unused
export CLAUDE_CODE_ENABLE_GATEWAY_MODEL_DISCOVERY=1
```

Discovery only runs when all of these hold: `ANTHROPIC_BASE_URL` is set, `CLAUDE_CODE_ENABLE_GATEWAY_MODEL_DISCOVERY=1`, no `CLAUDE_CODE_USE_*` provider variable is set, the base URL is not `api.anthropic.com`, and the Claude Code version supports it (≥ 2.1.129). Results are cached to `~/.claude/cache/gateway-models.json`.

> **Important — Claude Code filters discovered model IDs.** Claude Code only shows discovered models whose `id` begins with **`claude`** or **`anthropic`**. The proxy's scenario aliases (`default`, `fast`, …) and catalog names (`opencode-go/kimi-k2.6`) are therefore **filtered out of the picker**. To make a proxy model appear via discovery, give it a `claude-*` name — the natural fit is a [`model_overrides`](#model-overrides-model_overrides) key:
>
> ```json
> {
>   "model_overrides": {
>     "claude-glm-5.2": { "provider": "opencode-go", "model_id": "glm-5.2" }
>   }
> }
> ```
>
> `claude-glm-5.2` then appears in the picker (labeled "From gateway"), and selecting it routes to GLM-5.2. Models with non-`claude`/`anthropic` IDs remain fully usable — just type them into `/model` directly.

## Using with CC-Switch

[CC-Switch](https://github.com/farion1231/cc-switch) is a desktop app for managing and hot-switching Claude Code providers. routatic-proxy works with it out of the box — the proxy speaks the Anthropic API that Claude Code (and therefore CC-Switch) already expects, so you add it like any other custom provider.

### Add routatic-proxy as a custom provider

1. Start the proxy: `routatic-proxy serve` (default listen address `http://127.0.0.1:3456`).
2. In CC-Switch, click **Add Provider → Custom** and fill in:

   | CC-Switch field | Value |
   |-----------------|-------|
   | **Name** | `routatic-proxy` (any label) |
   | **Endpoint URL** | `http://127.0.0.1:3456` |
   | **API Key** | any non-empty value (e.g. `unused`) — see note below |

   CC-Switch writes these into Claude Code's config as:

   ```json
   {
     "env": {
       "ANTHROPIC_BASE_URL": "http://127.0.0.1:3456",
       "ANTHROPIC_AUTH_TOKEN": "unused"
     }
   }
   ```

   These are the exact two environment variables the proxy relies on — the same ones from the manual quickstart in the [README](README.md).
3. **Enable** the provider. Claude Code hot-reloads it, so no restart is needed.

> **About the API Key field:** the token in `ANTHROPIC_AUTH_TOKEN` is what Claude Code sends to the *proxy*, not what the proxy sends upstream. Your real upstream keys live in the proxy's own config (`opencode_go.api_key`, `openrouter.api_key`, etc.) or environment (`ROUTATIC_PROXY_*`). If you set `api_key` / `api_keys` in the proxy config, that value must match what CC-Switch sends; if you leave proxy auth unset, any non-empty token works.

### Configure specific models

You have two ways to control which model a CC-Switch-selected request runs on:

- **Let Claude Code pick, and honor it** — with `respect_requested_model: true` (the default), the proxy uses whatever model string Claude Code sends, resolving it against your `models` config and the catalog. Set it to `false` to force scenario-based routing regardless of the requested model.
- **Pin a model alias** — use [`model_overrides`](#model-overrides-model_overrides) to map a client-visible model name to a fixed upstream model. For example, requesting `claude-sonnet-4.5` can be routed to any provider/model you choose.

### CC-Switch "Fetch Models" button

CC-Switch's custom-provider form has a **Fetch Models** button that calls the OpenAI-style `GET /v1/models` endpoint to populate a model dropdown. The proxy implements this endpoint: it returns every model identifier you can request — config `models` aliases, `model_overrides` keys, and catalog canonical names (`provider/model`). See [docs/reference-api.md](docs/reference-api.md#get-v1models).

If the dropdown looks short, it usually means the model catalog has not synced into local storage yet; the scenario aliases (`default`, `fast`, `complex`, …) and any `model_overrides` keys always appear.

### Troubleshooting

- **CC-Switch reports the provider is unreachable** — confirm the proxy is running (`routatic-proxy status`) and the endpoint URL/port match `host`/`port` in your proxy config.
- **401 / auth errors from the proxy** — the token CC-Switch sends must satisfy the proxy's `api_key` / `api_keys` (or those must be unset). This is proxy-side auth, unrelated to your upstream provider keys.
- **Wrong model runs** — check routing precedence: `model_overrides` wins, then `respect_requested_model`, then scenario routing. See [Routing precedence](#routing-precedence).
