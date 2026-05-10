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
