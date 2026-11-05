# Provider Prompt Cache directives are owned by the client

The proxy preserves cache directives supplied at the request, tool, system, and
message-block levels, but it does not add, move, or infer cache boundaries.

## When a directive may be dropped

A provider adapter drops a directive only when (a) the wire format cannot
carry it, or (b) a failure was **observed** with that provider. Never on
suspicion. The rule is provider-scoped: an observation about one provider does
not strip markers for another.

- Observed failure, kept as a strip: some OpenAI-compatible providers reject
  `cache_control` inside OpenAI `messages` (`LEARNINGS.md`, "Known provider
  quirks"), so those providers get it stripped. The one model-ID-based case is
  Kimi: `transformMessages` (`internal/transformer/request.go`) skips the
  system-block marker for `kimi-*` models (PR #56, commit 424daa1, "Kimi models
  reject cache_control"). It matches on model ID, not provider, and is not
  extended to other positions or providers.
- Wire format cannot carry it: a marker on a message with no content part to
  carry it (an assistant turn that is only `tool_calls`) is dropped; no empty
  part is invented.

This favors predictable request behavior over automatic cache tuning.

## Shaping on the OpenAI Chat Completions path

Where the provider keeps markers (Model Studio; DeepSeek keeps its
message-level form), they are forwarded as the client sent them, with the
conversion to OpenAI messages shaping them as follows:

- A message carries at most one **message-level** marker, and it is written on
  the message's last content part. A marker the client put on a content part
  (an image) stays on that part, and the message-level marker is not placed
  again when a part already carries one.
- Adjacent text blocks of a message are joined into one text part; the marker
  sits at the end of the joined part. On a user turn the first marked text block supplies it; on an assistant turn the first marked block of any type does (see the last rule).
- System blocks are joined into one system message that uses the marker of the
  first marked system block (the request-level `cache_control` is the fallback
  when no block is marked). `[A(cc), B, C(cc)]` becomes one `ABC` message with
  one marker.
- A `tool_result` marker belongs to its own `role: "tool"` message, an `image`
  marker to its image part; neither is also placed on the user text of the same
  turn (Model Studio).
- An assistant `tool_use` block has no carrier on the OpenAI wire (tool calls
  have no `cache_control` field). Its marker is dropped when the turn has no
  text content; when the turn has text, the marker is placed on that text part,
  as is a marker on an assistant `thinking` block.
