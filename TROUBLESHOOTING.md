# Troubleshooting

## Windows Scoop Background Mode

On Windows, `oc-go-cc serve -b` uses the native Windows process APIs and keeps
the Scoop shim path intact. This means background mode does not require `nohup`
or a Unix-like shell, and Scoop-provided environment variables continue to work.

## "invalid request body" Error

This means the proxy couldn't parse the request from Claude Code. Enable debug logging to see the raw request:

```json
{ "logging": { "level": "debug" } }
```

Or set the environment variable:

```bash
export OC_GO_CC_LOG_LEVEL=debug
```

## "all models failed" Error

All models in the fallback chain returned errors. Check:

1. Your API key is valid: `oc-go-cc validate`
2. You haven't exceeded your [usage limits](https://opencode.ai/auth)
3. The OpenCode Go service is reachable: `curl -H "Authorization: Bearer $OC_GO_CC_API_KEY" https://opencode.ai/zen/go/v1/models`

## Connection Refused

Make sure the proxy is running:

```bash
oc-go-cc status
```

And Claude Code is pointing to the right address:

```bash
echo $ANTHROPIC_BASE_URL  # Should be http://127.0.0.1:3456
```

## Streaming Not Working

The proxy transforms OpenAI SSE to Anthropic SSE in real-time. If streaming appears broken:

1. Set log level to `debug` to see the raw SSE chunks
2. Check that no proxy or firewall is buffering the connection
3. Try a non-streaming request first to verify the model works

## Claude Code exits mid-stream (`client disconnected during stream`)

This log means **Claude closed the HTTP connection** while the proxy was still streaming — usually a symptom, not the root cause.

Common cause: a large request (long context, many tools) where the upstream model takes several seconds before the first token. Claude Code times out if it does not receive Anthropic-format SSE events (`message_start`, etc.) within ~6 seconds. Comment-only keepalives are not enough.

Fix: rebuild and restart the proxy so it sends `message_start` immediately after opening the stream, before waiting on upstream:

```bash
make build
./bin/oc-go-cc stop
./bin/oc-go-cc serve
```

If it still happens, enable `debug` logging and check for upstream errors or model fallbacks on the same `request_id`.

## Debug Mode

For maximum logging, run with debug level:

```bash
OC_GO_CC_LOG_LEVEL=debug oc-go-cc serve
```

This logs:

- Raw Anthropic request body from Claude Code
- Transformed OpenAI request sent to OpenCode Go
- Raw OpenAI response received
- SSE stream events during streaming
