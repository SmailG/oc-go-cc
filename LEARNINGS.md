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
