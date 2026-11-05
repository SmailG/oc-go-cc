# Model Studio Provider (Bailian / DashScope / Token Plan)

Alibaba Cloud Model Studio — also known as Bailian, and exposed through the DashScope API — serves
Qwen, GLM, DeepSeek, Kimi and other models. The **Token Plan** subscription uses the same account
and key on a dedicated gateway host.

`model-studio` is a first-class provider, registered in the provider registry
(`internal/server/server.go`), so requests take the provider path and never fall through to the
legacy OpenCode client.

## Two wire formats

| Format | Config field | Endpoint | Notes |
|--------|--------------|----------|-------|
| **Anthropic Messages** (preferred) | `anthropic_base_url` | `/v1/messages` | Response forwarded byte-for-byte; no format conversion |
| OpenAI Chat Completions | `base_url` | `/chat/completions` | Transformed Anthropic → OpenAI → Anthropic |

Anthropic is used whenever `anthropic_base_url` is set, because this proxy *receives* Anthropic
requests: passing them straight through means tool use, `thinking` blocks and `cache_control`
survive intact instead of making a lossy round trip through the OpenAI format.

The passthrough is asymmetric, and worth being precise about:

- The **response** is forwarded byte-for-byte.
- The **request** is rebuilt from the proxy's internal normalized form, which has no `metadata`
  field — so a client's `metadata` is **dropped** on this path (`top_p` is carried through). The
  model config's `temperature` and `max_tokens` are re-applied by `applyModelStudioOverrides`, so
  those behave the same as on the Chat Completions path — except when the client enabled extended
  thinking, where the client's values stand because Anthropic validates them against the thinking
  budget.

### Prompt caching on the Chat Completions path

On the Anthropic path (`anthropic_base_url`) the request goes through with its `cache_control`
markers intact. On the Chat Completions path the proxy forwards every marker the client sent, in
the form Alibaba's explicit cache reads. The rule is
`docs/adr/0002-client-owned-cache-directives.md`: a directive is dropped only when the wire format
cannot carry it or a failure has been observed, not on doubt. If Alibaba rejects a marker location,
that will be observed and handled then. The escape hatch is the Anthropic wire format (configure
`anthropic_base_url`; `wire_format: "anthropic"` pins it per model), which sends the markers as they
arrived. There is no separate switch.

- A marker on a system, user, assistant or tool-result (`role: "tool"`) message is written inside
  a content block (`"content":[{"type":"text","text":"...","cache_control":{"type":"ephemeral"}}]`),
  not as a field on the message. `qwen3.6-flash` and `qwen3.6-plus` cannot cache without it.
- A marker the client put on a content part (an image, for example) stays on that part. A
  message-level marker goes on the **last content part of any type**: in Anthropic semantics a
  marker caches the prefix through its block, and the converted message is one unit.
- Tool definitions keep their `cache_control` field, as the Anthropic path already sends it.
- The proxy never adds or infers a marker.
- Dropped because the wire format cannot carry it: a marker on a message with no content part
  (an assistant turn that is only `tool_calls`, or empty content); no empty part is invented.
- Not Model Studio's choice: `kimi-*` models skip the system-block marker, a rule from PR #56
  ("Kimi models reject cache_control"), and a `deepseek-*` model ID served by Model Studio keeps
  the DeepSeek form (message-level marker).

Cache use is read from the reply's `usage`: `prompt_tokens_details.cached_tokens` (cache reads) and
`prompt_tokens_details.cache_creation_input_tokens` (explicit-cache writes), both counted inside
`prompt_tokens`. They are reported to the client as `cache_read_input_tokens` and
`cache_creation_input_tokens`, and `input_tokens` is what remains
(`prompt_tokens` − reads − writes, never below 0). Streaming and non-streaming replies report the
same numbers.

> Not verified against the live API (token-plan quota exhausted): covered by unit tests on fixture
> bodies only.

## Quick start

```bash
# 1. Generate a config for this provider
routatic-proxy init --provider model-studio

# 2. Supply the credentials — BOTH are required, see the note below
export ROUTATIC_PROXY_API_KEY=sk-your-key
export ROUTATIC_PROXY_MODEL_STUDIO_API_KEY=sk-your-key

# 3. Run
routatic-proxy start
```

> **Both variables are required.** Config validation rejects a config with no
> global key at all (`api_key or api_keys is required`), and every init preset
> writes `"api_key": "${ROUTATIC_PROXY_API_KEY}"` at the top level. An
> unresolved `${...}` placeholder is a validation error, not a silent empty
> string — so exporting only `ROUTATIC_PROXY_MODEL_STUDIO_API_KEY` makes the
> proxy refuse to start. Set both to the same Model Studio key unless you also
> use another provider.

Token Plan subscribers: use `configs/config.token-plan.json`, which is wired for
the native Anthropic endpoint. It reads the key from `${BAILIAN_TOKEN_PLAN_API_KEY}`
in both the global and `model_studio` positions, so export that one variable —
or edit the file to use `ROUTATIC_PROXY_MODEL_STUDIO_API_KEY`.

## Endpoints

Both fields accept the bare base URL quoted in Alibaba's docs or the full path — the provider
appends the suffix when it is missing.

### Token Plan

```json
"model_studio": {
  "base_url": "https://token-plan.ap-southeast-1.maas.aliyuncs.com/compatible-mode/v1",
  "anthropic_base_url": "https://token-plan.ap-southeast-1.maas.aliyuncs.com/apps/anthropic"
}
```

Token Plan's Anthropic endpoint serves **only** `/v1/messages`.

### Standard Model Studio

```json
"model_studio": {
  "base_url": "https://dashscope-intl.aliyuncs.com/compatible-mode/v1"
}
```

| Gateway | `base_url` | `anthropic_base_url` |
|---------|-----------|---------------------|
| Model Studio international (default) | `https://dashscope-intl.aliyuncs.com/compatible-mode/v1` | *(not documented by Alibaba; leave unset)* |
| Model Studio China | `https://dashscope.aliyuncs.com/compatible-mode/v1` | *(not documented by Alibaba; leave unset)* |
| Token Plan (Singapore) | `https://token-plan.ap-southeast-1.maas.aliyuncs.com/compatible-mode/v1` | `https://token-plan.ap-southeast-1.maas.aliyuncs.com/apps/anthropic` |

Only the Token Plan Anthropic URL above has been verified against the live API. The standard
Model Studio gateways are not known to expose `/apps/anthropic`, so leave `anthropic_base_url`
unset for them and traffic will use Chat Completions.

Override either URL without editing the config:

```bash
export ROUTATIC_PROXY_MODEL_STUDIO_URL=https://token-plan.ap-southeast-1.maas.aliyuncs.com/compatible-mode/v1
export ROUTATIC_PROXY_MODEL_STUDIO_ANTHROPIC_URL=https://token-plan.ap-southeast-1.maas.aliyuncs.com/apps/anthropic
```

> Set **both** when switching gateways. `WireFormat` prefers the Anthropic endpoint whenever
> `anthropic_base_url` is non-empty, so overriding only `base_url` moves nothing — traffic keeps
> going to the Anthropic host already in the config.

## Configuration

```json
{
  "models": {
    "default": {
      "provider": "model-studio",
      "model_id": "qwen3.8-max",
      "temperature": 0.7,
      "max_tokens": 8192
    }
  },

  "model_studio": {
    "base_url": "https://dashscope-intl.aliyuncs.com/compatible-mode/v1",
    "anthropic_base_url": "",
    "api_key": "${ROUTATIC_PROXY_MODEL_STUDIO_API_KEY}",
    "api_keys": [],
    "timeout_ms": 300000,
    "stream_timeout_ms": 60000,
    "streaming_timeout_ms": 600000
  }
}
```

| Field | Type | Default | Description |
|-------|------|---------|-------------|
| `base_url` | string | `https://dashscope-intl.aliyuncs.com/compatible-mode/v1/chat/completions` | Chat Completions URL. `/chat/completions` is appended if absent. |
| `anthropic_base_url` | string | *(empty)* | Anthropic Messages URL. `/v1/messages` is appended if absent. When set, this becomes the default wire format. |
| `api_key` | string | — | Single key. Used only when `api_keys` is empty. |
| `api_keys` | string[] | — | Key pool, rotated round-robin. Takes precedence over `api_key`. |
| `timeout_ms` | int | `300000` | Whole-request timeout for non-streaming calls. |
| `stream_timeout_ms` | int | `timeout_ms` | Max gap between bytes on an active stream. Exceeding it fails over to the next model. |
| `streaming_timeout_ms` | int | — | **Deprecated** alias; read into `stream_timeout_ms` when that is unset. |

Keys fall back to the global `api_key` / `api_keys` when neither provider field is set.

### Environment variables

| Variable | Overrides |
|----------|-----------|
| `ROUTATIC_PROXY_MODEL_STUDIO_API_KEY` | `model_studio.api_key` |
| `ROUTATIC_PROXY_MODEL_STUDIO_API_KEYS` | `model_studio.api_keys` (comma-separated) |
| `ROUTATIC_PROXY_MODEL_STUDIO_URL` | `model_studio.base_url` |
| `ROUTATIC_PROXY_MODEL_STUDIO_ANTHROPIC_URL` | `model_studio.anthropic_base_url` |

A global key is still mandatory: validation fails without one, so also set
`ROUTATIC_PROXY_API_KEY` (or `api_key` in the file).

Precedence: `*_API_KEYS` → `*_API_KEY` → global `API_KEYS` → global `API_KEY`.

### Authentication

Verified against Token Plan: both `Authorization: Bearer <key>` and `x-api-key: <key>` authenticate
successfully, and `anthropic-version` is **not** required. A request with no credential returns
`401 InvalidApiKey`. The Chat Completions path sends `Authorization` only; the Anthropic path sends
both credential headers plus `anthropic-version`, matching the OpenCode Go provider.

## Choosing the wire format per model

`WireFormat` resolves in this order:

1. `wire_format: "openai"` or `"anthropic"` on the model — explicit, wins. (`"responses"` and
   `"gemini"` parse but are ignored, since Model Studio serves neither.)
2. `anthropic_tools_disabled: true` on the model — forces the Chat Completions transform path.
3. `anthropic_base_url` configured — Anthropic.
4. Otherwise — OpenAI Chat Completions.

```json
{
  "provider": "model-studio",
  "model_id": "qwen3.8-max",
  "wire_format": "openai"
}
```

## `extra_body`: vendor-specific request parameters

On the **Chat Completions path only**, a model's `extra_body` object is merged into the top level of
the outgoing request:

```json
{
  "provider": "model-studio",
  "model_id": "qwen3.8-flash",
  "wire_format": "openai",
  "extra_body": { "enable_thinking": false }
}
```

produces:

```json
{ "model": "qwen3.8-flash", "messages": [ ... ], "enable_thinking": false }
```

`extra_body` is not a nested key — it disappears into the request. On key collision `extra_body`
wins. It is opt-in per model, so models that omit it are unaffected, and the mechanism is
provider-agnostic.

The Anthropic passthrough has no request transform to merge into, so `extra_body` does not apply
there. It does not need to: thinking arrives as native `thinking` content blocks.

### `enable_thinking`

Probed against Token Plan compatible-mode with `qwen3.8-flash`:

| Request | Result |
|---------|--------|
| `enable_thinking: true`, non-streaming | HTTP 200, `reasoning_content` populated, `content` holds the answer |
| `enable_thinking: false`, non-streaming | HTTP 200, `reasoning_content` null, reasoning folded into `content` |
| parameter omitted, non-streaming | HTTP 200 |

So the parameter is honored and controls whether reasoning is returned separately — and it is
**not** rejected on non-streaming calls. Set it to `false` when you want reasoning suppressed or
folded in, not to avoid an error.

Note that `enable_thinking` is separate from the `thinking` model field: `thinking` is only
forwarded for DeepSeek models or when explicitly set on the model config
(`resolveThinkingAndEffort` in `internal/transformer/request.go`), whereas `extra_body` always
passes through verbatim.

## Model capabilities and the registry

Model Studio hosts families that are not in the built-in registry (`modelMetadata` in
`internal/config/model_registry.go`). This is handled deliberately:

- `ModelCapabilities` accepts **every** model ID, so an unfamiliar ID is never rejected outright.
- An unregistered model has `ContextWindow == 0`, and the capacity filter
  (`internal/router/capacity.go`) **skips** its context check rather than failing it.
- `SupportsTools` defaults to `true` for unregistered models.
- `Vision` defaults to `false` — so vision requests will skip the model.

Set these explicitly when they matter:

```json
{
  "provider": "model-studio",
  "model_id": "qwen3.8-max",
  "context_window": 262144,
  "max_output_tokens": 32768,
  "vision": true
}
```

Without `context_window`, no context-based routing or clamping applies to that model.

## Routing notes

`respect_requested_model` defaults to **true** in the loader. When it is on, a client that sends a
model string the proxy does not recognise (Claude Code sends `claude-sonnet-4-20250514`) bypasses
scenario routing entirely and lands on the default provider. Every shipped init preset sets it to
`false`; do the same, or map the incoming names with `model_family_overrides`:

```json
{
  "respect_requested_model": false,
  "model_family_overrides": {
    "sonnet": { "provider": "model-studio", "model_id": "qwen3.8-max" },
    "haiku":  { "provider": "model-studio", "model_id": "qwen3.8-flash" }
  }
}
```

Precedence: exact `model_overrides` → `model_family_overrides` (longest key first) →
`respect_requested_model` → scenario routing.

## Token Plan model availability

Which model IDs your key can call depends on your Token Plan tier; the API returns
`403 Access to model denied` for IDs outside it. The only reliable check is to ask the API —
make a one-token call and read the status:

```bash
curl -sS -o /dev/null -w '%{http_code}\n' \
  -X POST "$BASE/compatible-mode/v1/chat/completions" \
  -H "authorization: Bearer $KEY" -H 'content-type: application/json' \
  -d '{"model":"qwen3.8-max","max_tokens":1,"messages":[{"role":"user","content":"hi"}]}'
```

`200` means the model is in your tier; `403` means it is not.

> `bl usage free --model <id>` does **not** answer this question. It reports the separate
> free-inference token bucket under console credentials, not Token Plan tier entitlement — a model
> can show a full free bucket and still 403 on the Token Plan gateway.

A 403 whose body says `Access to model denied` is treated as a **per-model** failure, not a
credential failure: the fallback chain continues to the next model instead of blocking the whole
provider (`IsModelEntitlementError` in `internal/router/fallback.go`). Any other 403 (expired key,
account in arrears) still blocks the provider. If every model in a chain is denied, the 403 is
returned to the caller; a chain that mixes 403s with other failures is reported as a generic failure.
That is why mixing in-tier and out-of-tier IDs in one chain is safe.

Token Plan's own role mapping, for reference:

| Claude Code variable | Token Plan value |
|----------------------|------------------|
| `ANTHROPIC_MODEL` | `auto` |
| `ANTHROPIC_DEFAULT_HAIKU_MODEL` | `qwen3.6-flash` |
| `ANTHROPIC_DEFAULT_SONNET_MODEL` | `qwen3.8-flash` |
| `ANTHROPIC_DEFAULT_OPUS_MODEL` | `qwen3.8-max` |

A working reference config is at `configs/config.token-plan.json`.

## Limitations

- **No Responses or Gemini endpoint.** `wire_format` values for those are ignored and fall through
  to the default.
- **`extra_body` is not validated.** A malformed value is forwarded as-is and surfaces as an
  upstream 400.
- **`extra_body` does not apply on the Anthropic path.**
