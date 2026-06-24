// Package transformer handles request/response transformation and token counting.
package transformer

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	"oc-go-cc/pkg/types"
)

// ErrClientDisconnected is returned when the client disconnects during streaming.
var ErrClientDisconnected = fmt.Errorf("client disconnected")

// StreamActivity fires a one-shot callback when the proxy emits the first real
// Anthropic content event to the client (not preamble/keepalive heartbeats).
type StreamActivity struct {
	once sync.Once
	fn   func()
}

// NewStreamActivity creates a callback hook for first downstream stream content.
func NewStreamActivity(fn func()) *StreamActivity {
	return &StreamActivity{fn: fn}
}

func (a *StreamActivity) fire() {
	if a == nil || a.fn == nil {
		return
	}
	a.once.Do(a.fn)
}

// placeholderKeepaliveState keeps the preamble text block open (and text_delta
// keepalives running) while upstream streams reasoning before any user-visible output.
type placeholderKeepaliveState struct {
	idx          int
	open         bool
	reasoningBuf strings.Builder
}

func newPlaceholderKeepaliveState(preamble StreamPreamble) placeholderKeepaliveState {
	if !preamble.MessageStartSent || preamble.TextPlaceholderIndex < 0 {
		return placeholderKeepaliveState{idx: -1}
	}
	return placeholderKeepaliveState{idx: preamble.TextPlaceholderIndex, open: true}
}

func (p *placeholderKeepaliveState) active() bool {
	return p.open && p.idx >= 0
}

// releaseForText ends TTFT keepalives but keeps the preamble text block open for reuse.
func (p *placeholderKeepaliveState) releaseForText(contentIndex *int, contentStarted *bool) bool {
	if !p.active() || p.reasoningBuf.Len() > 0 {
		return false
	}
	p.open = false
	*contentIndex = p.idx
	*contentStarted = true
	return true
}

func (h *StreamHandler) closePlaceholderAndFlushReasoning(
	w http.ResponseWriter,
	flusher http.Flusher,
	p *placeholderKeepaliveState,
	contentIndex *int,
	contentStarted *bool,
	reasoningStarted *bool,
) error {
	if !p.active() {
		return nil
	}

	idx := p.idx
	if err := writeContentBlockStop(w, idx); err != nil {
		return ErrClientDisconnected
	}
	p.open = false
	*contentStarted = false
	setPreambleKeepaliveIndex(w, -1)

	if p.reasoningBuf.Len() > 0 {
		*contentIndex = p.idx + 1
		*reasoningStarted = true
		startEvent := types.MessageEvent{
			Type:         "content_block_start",
			Index:        contentIndex,
			ContentBlock: &types.ContentBlock{Type: "thinking", Thinking: ""},
		}
		if err := writeSSEEvent(w, startEvent); err != nil {
			return ErrClientDisconnected
		}
		delta := types.Delta{
			Type:     "thinking_delta",
			Thinking: p.reasoningBuf.String(),
		}
		setStreamKeepalive(w, *contentIndex, KeepaliveThinking)
		event := types.MessageEvent{
			Type:  "content_block_delta",
			Index: contentIndex,
			Delta: &delta,
		}
		if err := writeSSEEvent(w, event); err != nil {
			return ErrClientDisconnected
		}
		p.reasoningBuf.Reset()
		if err := writeContentBlockStop(w, *contentIndex); err != nil {
			return ErrClientDisconnected
		}
		*reasoningStarted = false
		(*contentIndex)++
	} else {
		*contentIndex = p.idx + 1
	}
	flusher.Flush()
	return nil
}

// closePlaceholderForToolTransition ends the preamble text block without emitting
// thinking SSE. thinking_delta keepalives do not reset Claude Code's inactivity
// timer; on tool-only turns we go straight to tool_use after the placeholder.
func (h *StreamHandler) closePlaceholderForToolTransition(
	w http.ResponseWriter,
	flusher http.Flusher,
	p *placeholderKeepaliveState,
	contentIndex *int,
	contentStarted *bool,
) error {
	if !p.active() {
		return nil
	}
	idx := p.idx
	if err := writeContentBlockStop(w, idx); err != nil {
		return ErrClientDisconnected
	}
	p.open = false
	p.reasoningBuf.Reset()
	*contentStarted = false
	setPreambleKeepaliveIndex(w, -1)
	*contentIndex = p.idx + 1
	flusher.Flush()
	return nil
}

// StreamKeepalive is implemented by the streaming responseWriter so the
// transformer can target gap-based keepalives on the active content block.
type StreamKeepalive interface {
	SetStreamKeepalive(index int, mode KeepaliveMode)
	SetPreambleKeepaliveIndex(index int)
	NoteStreamContentWrite()
}

// KeepaliveMode selects which Anthropic delta type to emit during idle gaps.
type KeepaliveMode int

const (
	KeepaliveNone KeepaliveMode = iota
	KeepaliveText
	KeepaliveThinking
	KeepaliveToolInput
)

func setStreamKeepalive(w http.ResponseWriter, index int, mode KeepaliveMode) {
	if sk, ok := w.(StreamKeepalive); ok {
		sk.SetStreamKeepalive(index, mode)
	}
}

func setPreambleKeepaliveIndex(w http.ResponseWriter, index int) {
	if sk, ok := w.(StreamKeepalive); ok {
		sk.SetPreambleKeepaliveIndex(index)
	}
}

func writeContentBlockStop(w http.ResponseWriter, idx int) error {
	setStreamKeepalive(w, -1, KeepaliveNone)
	stopEvent := types.MessageEvent{Type: "content_block_stop", Index: &idx}
	return writeSSEEvent(w, stopEvent)
}

func noteStreamContentWrite(w http.ResponseWriter) {
	if sk, ok := w.(StreamKeepalive); ok {
		sk.NoteStreamContentWrite()
	}
}

// StreamHandler handles streaming SSE transformation from OpenAI to Anthropic format.
type StreamHandler struct {
	responseTransformer *ResponseTransformer
}

// NewStreamHandler creates a new stream handler.
func NewStreamHandler() *StreamHandler {
	return &StreamHandler{
		responseTransformer: NewResponseTransformer(),
	}
}

// WriteMessageStart sends the initial Anthropic message_start SSE event.
// clientModel must match the model Claude Code sent in the request (not the upstream ID).
func WriteMessageStart(w http.ResponseWriter, clientModel string) error {
	msgStart := types.MessageEvent{
		Type: "message_start",
		Message: &types.MessageResponse{
			ID:      "msg_" + generateID(),
			Type:    "message",
			Role:    "assistant",
			Content: []types.ContentBlock{},
			Model:   clientModel,
			Usage:   types.Usage{OutputTokens: 1},
		},
	}
	if err := writeSSEEvent(w, msgStart); err != nil {
		return ErrClientDisconnected
	}
	if flusher, ok := w.(http.Flusher); ok {
		flusher.Flush()
	}
	return nil
}

// StreamPreamble tracks SSE frames sent before the upstream body is connected.
type StreamPreamble struct {
	MessageStartSent     bool
	TextPlaceholderIndex int // index of open placeholder text block, or -1
}

// WriteTextPlaceholderBlock opens an empty text content block for TTFT text_delta keepalives.
func WriteTextPlaceholderBlock(w http.ResponseWriter) (int, error) {
	idx := 0
	startEvent := types.MessageEvent{
		Type:         "content_block_start",
		Index:        &idx,
		ContentBlock: &types.ContentBlock{Type: "text", Text: ""},
	}
	if err := writeSSEEvent(w, startEvent); err != nil {
		return -1, ErrClientDisconnected
	}
	if flusher, ok := w.(http.Flusher); ok {
		flusher.Flush()
	}
	return idx, nil
}

// WriteStreamPreamble sends message_start plus a placeholder text block so Claude Code
// sees content_block activity during slow upstream TTFT. Thinking keepalives do not
// reset the client "Cooked/Brewed" timer; text_delta does.
func WriteStreamPreamble(w http.ResponseWriter, clientModel string) (StreamPreamble, error) {
	if err := WriteMessageStart(w, clientModel); err != nil {
		return StreamPreamble{}, err
	}
	idx, err := WriteTextPlaceholderBlock(w)
	if err != nil {
		return StreamPreamble{}, err
	}
	return StreamPreamble{MessageStartSent: true, TextPlaceholderIndex: idx}, nil
}

// WritePing sends an Anthropic keep-alive ping event.
func WritePing(w http.ResponseWriter) error {
	if err := writeSSEEvent(w, types.MessageEvent{Type: "ping"}); err != nil {
		return ErrClientDisconnected
	}
	if flusher, ok := w.(http.Flusher); ok {
		flusher.Flush()
	}
	return nil
}

// WriteTextKeepalive emits a minimal text_delta on an open text block (TTFT keepalive).
// Uses zero-width space so gap keepalives do not append visible characters.
func WriteTextKeepalive(w http.ResponseWriter, index int) error {
	idx := index
	event := types.MessageEvent{
		Type:  "content_block_delta",
		Index: &idx,
		Delta: &types.Delta{Type: "text_delta", Text: "\u200b"},
	}
	if err := writeSSEEvent(w, event); err != nil {
		return ErrClientDisconnected
	}
	if flusher, ok := w.(http.Flusher); ok {
		flusher.Flush()
	}
	return nil
}

// WriteThinkingKeepalive emits a minimal thinking_delta during idle gaps on a thinking block.
func WriteThinkingKeepalive(w http.ResponseWriter, index int) error {
	idx := index
	event := types.MessageEvent{
		Type:  "content_block_delta",
		Index: &idx,
		Delta: &types.Delta{Type: "thinking_delta", Thinking: "\u200b"},
	}
	if err := writeSSEEvent(w, event); err != nil {
		return ErrClientDisconnected
	}
	if flusher, ok := w.(http.Flusher); ok {
		flusher.Flush()
	}
	return nil
}

// WriteToolInputKeepalive emits a minimal input_json_delta during idle gaps on a tool_use block.
func WriteToolInputKeepalive(w http.ResponseWriter, index int) error {
	idx := index
	event := types.MessageEvent{
		Type:  "content_block_delta",
		Index: &idx,
		Delta: &types.Delta{Type: "input_json_delta", PartialJSON: " "},
	}
	if err := writeSSEEvent(w, event); err != nil {
		return ErrClientDisconnected
	}
	if flusher, ok := w.(http.Flusher); ok {
		flusher.Flush()
	}
	return nil
}

// ctxReadCloser aborts blocked upstream reads when the client disconnects.
type ctxReadCloser struct {
	ctx context.Context
	rc  io.ReadCloser
}

func newCtxReadCloser(ctx context.Context, rc io.ReadCloser) *ctxReadCloser {
	return &ctxReadCloser{ctx: ctx, rc: rc}
}

func (c *ctxReadCloser) Read(p []byte) (int, error) {
	if err := c.ctx.Err(); err != nil {
		return 0, err
	}
	type readResult struct {
		n   int
		err error
	}
	ch := make(chan readResult, 1)
	go func() {
		n, err := c.rc.Read(p)
		ch <- readResult{n, err}
	}()
	select {
	case <-c.ctx.Done():
		return 0, c.ctx.Err()
	case res := <-ch:
		return res.n, res.err
	}
}

func (c *ctxReadCloser) Close() error {
	return c.rc.Close()
}

// ProxyStream takes an OpenAI streaming response and writes Anthropic-format SSE to the writer.
// It reads OpenAI ChatCompletionChunk SSE events and transforms them into Anthropic MessageEvent SSE events.
// The clientCtx is used to detect client disconnection and abort early.
// When preamble has MessageStartSent, message_start was already written before upstream connect.
//
// CRITICAL: This function reads directly from resp.Body without buffering to minimize latency.
// Per deep research: "Don't use bufio.Scanner or bufio.Reader on the response body - it adds buffering"
func (h *StreamHandler) ProxyStream(
	w http.ResponseWriter,
	openaiResp io.ReadCloser,
	clientModel string,
	clientCtx context.Context,
	preamble StreamPreamble,
	activity *StreamActivity,
) error {
	flusher, ok := w.(http.Flusher)
	if !ok {
		return fmt.Errorf("streaming not supported by response writer")
	}

	if !preamble.MessageStartSent {
		if err := WriteMessageStart(w, clientModel); err != nil {
			return err
		}
	}

	defer openaiResp.Close()
	body := newCtxReadCloser(clientCtx, openaiResp)

	// Read directly from response body without buffering.
	// Use a tight loop with a line buffer - no bufio.Reader.
	contentIndex := 0
	var lineBuf bytes.Buffer
	contentStarted := false
	if preamble.TextPlaceholderIndex >= 0 {
		contentIndex = preamble.TextPlaceholderIndex
	}
	reasoningStarted := false
	stopSent := false
	toolUseCount := 0
	startedToolCalls := make(map[int]int) // maps OpenAI tool call index → Anthropic content block index
	ph := newPlaceholderKeepaliveState(preamble)

	// Read in larger chunks for efficiency, then parse lines
	readBuf := make([]byte, 4096)

	for {
		// Check if client disconnected
		select {
		case <-clientCtx.Done():
			return ErrClientDisconnected
		default:
		}

		// Read chunk from upstream
		n, err := body.Read(readBuf)
		if n > 0 {
			// Process bytes immediately
			for i := 0; i < n; i++ {
				b := readBuf[i]
				if b == '\n' {
					line := lineBuf.String()
					lineBuf.Reset()

					// Process complete line
					if err := h.processSSELine(w, flusher, line, &contentIndex, &contentStarted, &reasoningStarted, &stopSent, &toolUseCount, startedToolCalls, clientModel, activity, &ph); err != nil {
						return err
					}
				} else {
					lineBuf.WriteByte(b)
				}
			}
		}

		if err == io.EOF {
			// Process any remaining data in buffer
			if lineBuf.Len() > 0 {
				line := lineBuf.String()
				if err := h.processSSELine(w, flusher, line, &contentIndex, &contentStarted, &reasoningStarted, &stopSent, &toolUseCount, startedToolCalls, clientModel, activity, &ph); err != nil {
					return err
				}
			}
			break
		}
		if err != nil {
			if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
				return ErrClientDisconnected
			}
			return fmt.Errorf("failed to read stream: %w", err)
		}
	}

	if ph.active() {
		if err := h.closePlaceholderAndFlushReasoning(w, flusher, &ph, &contentIndex, &contentStarted, &reasoningStarted); err != nil {
			return err
		}
	} else if ph.reasoningBuf.Len() > 0 {
		if err := h.closePlaceholderAndFlushReasoning(w, flusher, &ph, &contentIndex, &contentStarted, &reasoningStarted); err != nil {
			return err
		}
	}

	if err := closeOpenStreamBlocks(w, &contentIndex, &contentStarted, &reasoningStarted); err != nil {
		return err
	}

	// Send stop events for any tool blocks not yet closed (e.g. upstream
	// disconnected without sending a finish_reason chunk).
	if len(startedToolCalls) > 0 {
		type toolBlockEntry struct {
			oi       int
			blockIdx int
		}
		entries := make([]toolBlockEntry, 0, len(startedToolCalls))
		for oi, blockIdx := range startedToolCalls {
			entries = append(entries, toolBlockEntry{oi, blockIdx})
		}
		sort.Slice(entries, func(i, j int) bool {
			return entries[i].blockIdx < entries[j].blockIdx
		})
		for _, e := range entries {
			idx := e.blockIdx
			stopEvent := types.MessageEvent{
				Type:  "content_block_stop",
				Index: &idx,
			}
			if err := writeSSEEvent(w, stopEvent); err != nil {
				return ErrClientDisconnected
			}
		}
	}

	// Send message_delta if not already sent.
	// If tool calls were in progress when the stream ended,
	// the stop reason should be "tool_use" rather than "end_turn".
	if !stopSent {
		stopReason := "end_turn"
		if len(startedToolCalls) > 0 {
			stopReason = "tool_use"
		}
		msgDelta := types.MessageEvent{
			Type: "message_delta",
			Delta: &types.Delta{
				StopReason: stopReason,
			},
			Usage: usageInfoToAnthropic(nil),
		}
		if err := writeSSEEvent(w, msgDelta); err != nil {
			return ErrClientDisconnected
		}
		stopSent = true
	}

	// Send message_stop event to signal stream completion.
	stopEvent := types.MessageEvent{
		Type: "message_stop",
	}
	if err := writeSSEEvent(w, stopEvent); err != nil {
		return ErrClientDisconnected
	}
	flusher.Flush()

	return nil
}

func closeActiveContentBlock(
	w http.ResponseWriter,
	contentIndex *int,
	contentStarted *bool,
	reasoningStarted *bool,
) error {
	if !*contentStarted && !*reasoningStarted {
		return nil
	}
	idx := *contentIndex
	if err := writeContentBlockStop(w, idx); err != nil {
		return ErrClientDisconnected
	}
	*contentIndex++
	*reasoningStarted = false
	*contentStarted = false
	return nil
}

func closeOpenStreamBlocks(
	w http.ResponseWriter,
	contentIndex *int,
	contentStarted *bool,
	reasoningStarted *bool,
) error {
	if *reasoningStarted {
		idx := *contentIndex
		if err := writeContentBlockStop(w, idx); err != nil {
			return ErrClientDisconnected
		}
		*contentIndex++
		*reasoningStarted = false
	}
	if *contentStarted {
		idx := *contentIndex
		if err := writeContentBlockStop(w, idx); err != nil {
			return ErrClientDisconnected
		}
		*contentStarted = false
	}
	return nil
}

// processSSELine processes a single SSE line from upstream.
// Per deep research: "Treat SSE primarily as a text protocol" - minimize JSON parsing.
func (h *StreamHandler) processSSELine(
	w http.ResponseWriter,
	flusher http.Flusher,
	line string,
	contentIndex *int,
	contentStarted *bool,
	reasoningStarted *bool,
	stopSent *bool,
	toolUseCount *int,
	startedToolCalls map[int]int,
	clientModel string,
	activity *StreamActivity,
	ph *placeholderKeepaliveState,
) error {
	line = strings.TrimSpace(line)

	// Skip empty lines
	if line == "" {
		return nil
	}

	// Skip non-data lines (event: lines, id: lines, etc.)
	if !strings.HasPrefix(line, "data: ") {
		return nil
	}

	data := strings.TrimPrefix(line, "data: ")
	if data == "" {
		return nil
	}

	// Handle [DONE] marker
	if data == "[DONE]" {
		return nil
	}

	// Fast path: check if this is a content chunk without full JSON parsing.
	// Skip the fast path when reasoning_content is also present in the same
	// chunk — falling through to JSON parsing ensures both fields are handled
	// correctly. Otherwise reasoning_content gets silently dropped, and on the
	// next turn DeepSeek rejects the request with:
	//   "The reasoning_content in the thinking mode must be passed back to the API."
	if !strings.Contains(data, `"reasoning_content"`) &&
		!strings.Contains(data, `"finish_reason"`) &&
		!strings.Contains(data, `"tool_calls"`) &&
		!strings.Contains(data, `"usage"`) {
		if idx := strings.Index(data, `"delta":{"content":"`); idx != -1 {
			// Extract content directly
			start := idx + len(`"delta":{"content":"`)
			end := strings.Index(data[start:], `"`)
			if end != -1 {
				content := data[start : start+end]
				if content != "" {
					_ = ph.releaseForText(contentIndex, contentStarted)
					if ph.active() {
						if err := h.closePlaceholderAndFlushReasoning(w, flusher, ph, contentIndex, contentStarted, reasoningStarted); err != nil {
							return err
						}
					}
					if !*contentStarted {
						// If reasoning was already started, close it first
						if *reasoningStarted {
							idx := *contentIndex
							if err := writeContentBlockStop(w, idx); err != nil {
								return ErrClientDisconnected
							}
							*contentIndex++
							*reasoningStarted = false
						}
						*contentStarted = true
						// Send content_block_start
						startEvent := types.MessageEvent{
							Type:         "content_block_start",
							Index:        contentIndex,
							ContentBlock: &types.ContentBlock{Type: "text", Text: ""},
						}
						if err := writeSSEEvent(w, startEvent); err != nil {
							return ErrClientDisconnected
						}
						setStreamKeepalive(w, *contentIndex, KeepaliveText)
					}

					// Send content_block_delta
					delta := types.Delta{
						Type: "text_delta",
						Text: content,
					}
					event := types.MessageEvent{
						Type:  "content_block_delta",
						Index: contentIndex,
						Delta: &delta,
					}
					if err := writeSSEEvent(w, event); err != nil {
						return ErrClientDisconnected
					}
					flusher.Flush()
					activity.fire()
				}
				return nil
			}
		}
	}

	// For tool calls and other complex cases, fall back to full JSON parsing
	var chunk types.ChatCompletionChunk
	if err := json.Unmarshal([]byte(data), &chunk); err != nil {
		// Skip malformed chunks - don't fail the whole stream
		return nil
	}

	if len(chunk.Choices) == 0 {
		if chunk.Usage != nil {
			if *stopSent {
				// Stop reason already sent — emit usage-only message_delta (no duplicate stop_reason).
				event := types.MessageEvent{
					Type:  "message_delta",
					Delta: &types.Delta{},
					Usage: usageInfoToAnthropic(chunk.Usage),
				}
				if err := writeSSEEvent(w, event); err != nil {
					return ErrClientDisconnected
				}
				flusher.Flush()
				setStreamKeepalive(w, *contentIndex, KeepaliveThinking)
			} else {
				if err := h.sendUsageDelta(w, flusher, chunk.Usage); err != nil {
					return err
				}
				*stopSent = true
			}
		}
		return nil
	}

	choice := chunk.Choices[0]

	// Handle reasoning content deltas
	if choice.Delta.ReasoningContent != nil && *choice.Delta.ReasoningContent != "" {
		if ph.active() {
			ph.reasoningBuf.WriteString(*choice.Delta.ReasoningContent)
		} else {
			if !*reasoningStarted {
				// If text was already started, close it first
				if *contentStarted {
					idx := *contentIndex
					if err := writeContentBlockStop(w, idx); err != nil {
						return ErrClientDisconnected
					}
					*contentIndex++
					*contentStarted = false
				}
				*reasoningStarted = true
				startEvent := types.MessageEvent{
					Type:         "content_block_start",
					Index:        contentIndex,
					ContentBlock: &types.ContentBlock{Type: "thinking", Thinking: ""},
				}
				if err := writeSSEEvent(w, startEvent); err != nil {
					return ErrClientDisconnected
				}
				setStreamKeepalive(w, *contentIndex, KeepaliveThinking)
			}

			delta := types.Delta{
				Type:     "thinking_delta",
				Thinking: *choice.Delta.ReasoningContent,
			}
			event := types.MessageEvent{
				Type:  "content_block_delta",
				Index: contentIndex,
				Delta: &delta,
			}
			if err := writeSSEEvent(w, event); err != nil {
				return ErrClientDisconnected
			}
			flusher.Flush()
		}
	}

	// Handle text content deltas
	if textContent := choice.Delta.ContentText(); textContent != "" {
		_ = ph.releaseForText(contentIndex, contentStarted)
		if ph.active() {
			if err := h.closePlaceholderAndFlushReasoning(w, flusher, ph, contentIndex, contentStarted, reasoningStarted); err != nil {
				return err
			}
		}
		if !*contentStarted {
			// If reasoning was already started, close it first
			if *reasoningStarted {
				idx := *contentIndex
				if err := writeContentBlockStop(w, idx); err != nil {
					return ErrClientDisconnected
				}
				*contentIndex++
				*reasoningStarted = false
			}
			*contentStarted = true
			startEvent := types.MessageEvent{
				Type:         "content_block_start",
				Index:        contentIndex,
				ContentBlock: &types.ContentBlock{Type: "text", Text: ""},
			}
			if err := writeSSEEvent(w, startEvent); err != nil {
				return ErrClientDisconnected
			}
			setStreamKeepalive(w, *contentIndex, KeepaliveText)
		}

		delta := types.Delta{
			Type: "text_delta",
			Text: textContent,
		}
		event := types.MessageEvent{
			Type:  "content_block_delta",
			Index: contentIndex,
			Delta: &delta,
		}
		if err := writeSSEEvent(w, event); err != nil {
			return ErrClientDisconnected
		}
		flusher.Flush()
		activity.fire()
	}

	// Handle tool call deltas.
	// OpenAI streams tool calls incrementally: the first chunk for a given
	// tool call carries id + name (+ possibly empty arguments), subsequent
	// chunks carry only incremental arguments.  We must create exactly one
	// content_block_start per tool call, then stream deltas for it.
	if len(choice.Delta.ToolCalls) > 0 {
		if ph.active() {
			if err := h.closePlaceholderForToolTransition(w, flusher, ph, contentIndex, contentStarted); err != nil {
				return err
			}
		}
		for _, tc := range choice.Delta.ToolCalls {
			oi := tc.Index // OpenAI tool_calls array index

			blockIdx, exists := startedToolCalls[oi]
			if !exists {
				if tc.Function.Name == "" {
					// Ghost chunk: this index was closed and recycled, but
					// has no name/id. Ignore — the real tool call was
					// already fully processed.
					continue
				}
				if *contentStarted || *reasoningStarted {
					idx := *contentIndex
					if err := writeContentBlockStop(w, idx); err != nil {
						return ErrClientDisconnected
					}
					*contentStarted = false
					*reasoningStarted = false
				}
				// First time seeing this logical tool call — start a new block.
				*contentIndex++
				*toolUseCount++
				blockIdx = *contentIndex
				startedToolCalls[oi] = blockIdx
				setStreamKeepalive(w, blockIdx, KeepaliveToolInput)

				toolID := tc.ID
				if toolID == "" {
					toolID = fmt.Sprintf("toolu_%s", generateID())
				}
				startEvent := types.MessageEvent{
					Type:  "content_block_start",
					Index: &blockIdx,
					ContentBlock: &types.ContentBlock{
						Type:  "tool_use",
						ID:    toolID,
						Name:  tc.Function.Name,
						Input: json.RawMessage(`{}`),
					},
				}
				if err := writeSSEEvent(w, startEvent); err != nil {
					return ErrClientDisconnected
				}
				activity.fire()
			}

			// Send argument delta (if any) — whether new or continuation.
			if tc.Function.Arguments != "" {
				delta := types.Delta{
					Type:        "input_json_delta",
					PartialJSON: tc.Function.Arguments,
				}
				event := types.MessageEvent{
					Type:  "content_block_delta",
					Index: &blockIdx,
					Delta: &delta,
				}
				if err := writeSSEEvent(w, event); err != nil {
					return ErrClientDisconnected
				}
				activity.fire()
			}
			flusher.Flush()
		}
	}

	// Handle finish reason
	if choice.FinishReason != "" {
		if err := closeActiveContentBlock(w, contentIndex, contentStarted, reasoningStarted); err != nil {
			return err
		}

		// Close any open tool_use blocks in ascending index order.
		if len(startedToolCalls) > 0 {
			type toolBlockEntry struct {
				oi       int
				blockIdx int
			}
			entries := make([]toolBlockEntry, 0, len(startedToolCalls))
			for oi, blockIdx := range startedToolCalls {
				entries = append(entries, toolBlockEntry{oi, blockIdx})
			}
			sort.Slice(entries, func(i, j int) bool {
				return entries[i].blockIdx < entries[j].blockIdx
			})
			for _, e := range entries {
				if err := writeContentBlockStop(w, e.blockIdx); err != nil {
					return ErrClientDisconnected
				}
			}
			// Clear so EOF cleanup won't emit duplicate stops
			for oi := range startedToolCalls {
				delete(startedToolCalls, oi)
			}
		}
		*toolUseCount = 0

		msgDelta := types.MessageEvent{
			Type: "message_delta",
			Delta: &types.Delta{
				StopReason: h.responseTransformer.mapFinishReason(choice.FinishReason),
			},
			Usage: usageInfoToAnthropic(chunk.Usage),
		}
		if err := writeSSEEvent(w, msgDelta); err != nil {
			return ErrClientDisconnected
		}
		*stopSent = true
		flusher.Flush()
	}

	return nil
}

func (h *StreamHandler) sendUsageDelta(w http.ResponseWriter, flusher http.Flusher, usage *types.UsageInfo) error {
	event := types.MessageEvent{
		Type: "message_delta",
		Delta: &types.Delta{
			StopReason: "end_turn",
		},
		Usage: usageInfoToAnthropic(usage),
	}
	if err := writeSSEEvent(w, event); err != nil {
		return ErrClientDisconnected
	}
	flusher.Flush()
	return nil
}

func usageInfoToAnthropic(usage *types.UsageInfo) *types.Usage {
	if usage == nil {
		return &types.Usage{
			InputTokens:  0,
			OutputTokens: 0,
		}
	}
	return &types.Usage{
		// Per Anthropic Messages API spec, `input_tokens` is the count of
		// regular input tokens — i.e. tokens that were neither read from the
		// cache nor written to the cache this turn. OpenAI's `prompt_tokens`
		// is the *total* prompt size. We must subtract the cache parts here
		// for the same reason TransformResponse does — see the longer comment
		// in response.go.
		InputTokens:              nonNegative(usage.PromptTokens - usage.PromptCacheHitTokens - usage.PromptCacheMissTokens),
		OutputTokens:             usage.CompletionTokens,
		CacheCreationInputTokens: usage.PromptCacheMissTokens,
		CacheReadInputTokens:     usage.PromptCacheHitTokens,
	}
}

// WriteSSEEventUnlocked writes one SSE frame without acquiring stream locks (caller holds them).
func WriteSSEEventUnlocked(w http.ResponseWriter, event types.MessageEvent) error {
	return writeSSEEvent(w, event)
}

// writeSSEEvent writes a single SSE event to the HTTP response writer.
// Format: "event: <type>\ndata: <json>\n\n"
func writeSSEEvent(w http.ResponseWriter, event types.MessageEvent) error {
	data, err := json.Marshal(event)
	if err != nil {
		return fmt.Errorf("failed to marshal event: %w", err)
	}

	_, err = fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event.Type, string(data))
	return err
}

// generateID creates a unique identifier based on current time.
func generateID() string {
	return fmt.Sprintf("%d", time.Now().UnixNano())
}

// CopyAnthropicSSE copies Anthropic-format SSE from upstream to the client. When
// skipMessageStart is true, the first message_start event is dropped (already sent as preamble).
func CopyAnthropicSSE(w io.Writer, r io.Reader, skipMessageStart bool, activity *StreamActivity) error {
	if !skipMessageStart {
		_, err := io.Copy(w, r)
		return err
	}

	br := bufio.NewReader(r)
	for {
		event, err := readSSEEventBlock(br)
		if err != nil {
			if err == io.EOF {
				return nil
			}
			return err
		}
		if skipMessageStart && isMessageStartSSEBlock(event) {
			skipMessageStart = false
			continue
		}
		if countsAsClientStreamActivity(event) {
			activity.fire()
		}
		if _, err := w.Write(event); err != nil {
			return err
		}
	}
}

func readSSEEventBlock(br *bufio.Reader) ([]byte, error) {
	var block bytes.Buffer
	for {
		line, err := br.ReadString('\n')
		if len(line) > 0 {
			block.WriteString(line)
		}
		if err != nil {
			if err == io.EOF {
				if block.Len() == 0 {
					return nil, io.EOF
				}
				return block.Bytes(), nil
			}
			return nil, err
		}
		if line == "\n" && block.Len() > 0 {
			return block.Bytes(), nil
		}
	}
}

func countsAsClientStreamActivity(block []byte) bool {
	for _, line := range strings.Split(string(block), "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		data := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		var ev types.MessageEvent
		if json.Unmarshal([]byte(data), &ev) != nil {
			continue
		}
		switch ev.Type {
		case "content_block_start":
			return ev.ContentBlock != nil && ev.ContentBlock.Type == "tool_use"
		case "content_block_delta":
			if ev.Delta == nil {
				return false
			}
			switch ev.Delta.Type {
			case "text_delta", "input_json_delta":
				return true
			}
		}
	}
	return false
}

func isMessageStartSSEBlock(block []byte) bool {
	for _, line := range strings.Split(string(block), "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		data := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		var ev types.MessageEvent
		if json.Unmarshal([]byte(data), &ev) == nil && ev.Type == "message_start" {
			return true
		}
	}
	return false
}

// ProxyResponsesStream takes an OpenAI Responses streaming response and writes Anthropic-format SSE.
func (h *StreamHandler) ProxyResponsesStream(
	w http.ResponseWriter,
	responsesResp io.ReadCloser,
	clientModel string,
	clientCtx context.Context,
	preamble StreamPreamble,
	activity *StreamActivity,
) error {
	flusher, ok := w.(http.Flusher)
	if !ok {
		return fmt.Errorf("streaming not supported by response writer")
	}

	if !preamble.MessageStartSent {
		if err := WriteMessageStart(w, clientModel); err != nil {
			return err
		}
	}

	contentIndex := 0
	var lineBuf bytes.Buffer
	contentStarted := preamble.MessageStartSent && preamble.TextPlaceholderIndex >= 0
	if contentStarted {
		contentIndex = preamble.TextPlaceholderIndex
	}
	stopSent := false
	readBuf := make([]byte, 4096)

	for {
		select {
		case <-clientCtx.Done():
			return ErrClientDisconnected
		default:
		}

		n, err := responsesResp.Read(readBuf)
		if n > 0 {
			for i := 0; i < n; i++ {
				b := readBuf[i]
				if b == '\n' {
					line := lineBuf.String()
					lineBuf.Reset()
					if err := h.processResponsesSSELine(w, flusher, line, &contentIndex, &contentStarted, &stopSent, clientModel, activity); err != nil {
						return err
					}
				} else {
					lineBuf.WriteByte(b)
				}
			}
		}

		if err == io.EOF {
			if lineBuf.Len() > 0 {
				line := lineBuf.String()
				if err := h.processResponsesSSELine(w, flusher, line, &contentIndex, &contentStarted, &stopSent, clientModel, activity); err != nil {
					return err
				}
			}
			break
		}
		if err != nil {
			return fmt.Errorf("failed to read stream: %w", err)
		}
	}

	if contentStarted {
		stopEvent := types.MessageEvent{
			Type:  "content_block_stop",
			Index: &contentIndex,
		}
		if err := writeSSEEvent(w, stopEvent); err != nil {
			return ErrClientDisconnected
		}
	}

	if !stopSent {
		msgDelta := types.MessageEvent{
			Type: "message_delta",
			Delta: &types.Delta{
				StopReason: "end_turn",
			},
			Usage: &types.Usage{InputTokens: 0, OutputTokens: 0},
		}
		if err := writeSSEEvent(w, msgDelta); err != nil {
			return ErrClientDisconnected
		}
		stopSent = true
	}

	stopEvent := types.MessageEvent{
		Type: "message_stop",
	}
	if err := writeSSEEvent(w, stopEvent); err != nil {
		return ErrClientDisconnected
	}
	flusher.Flush()

	return nil
}

func (h *StreamHandler) processResponsesSSELine(
	w http.ResponseWriter,
	flusher http.Flusher,
	line string,
	contentIndex *int,
	contentStarted *bool,
	stopSent *bool,
	clientModel string,
	activity *StreamActivity,
) error {
	line = strings.TrimSpace(line)
	if line == "" || !strings.HasPrefix(line, "data: ") {
		return nil
	}

	data := strings.TrimPrefix(line, "data: ")
	if data == "" || data == "[DONE]" {
		return nil
	}

	var chunk types.ResponsesChunk
	if err := json.Unmarshal([]byte(data), &chunk); err != nil {
		return nil
	}

	if chunk.Type == "response.output_text.delta" && chunk.Delta != "" {
		if !*contentStarted {
			*contentStarted = true
			startEvent := types.MessageEvent{
				Type:         "content_block_start",
				Index:        contentIndex,
				ContentBlock: &types.ContentBlock{Type: "text", Text: ""},
			}
			if err := writeSSEEvent(w, startEvent); err != nil {
				return ErrClientDisconnected
			}
		}

		delta := types.Delta{
			Type: "text_delta",
			Text: chunk.Delta,
		}
		event := types.MessageEvent{
			Type:  "content_block_delta",
			Index: contentIndex,
			Delta: &delta,
		}
		if err := writeSSEEvent(w, event); err != nil {
			return ErrClientDisconnected
		}
		flusher.Flush()
		activity.fire()
	}

	if chunk.Type == "response.completed" || chunk.Type == "response.done" {
		if !*stopSent {
			msgDelta := types.MessageEvent{
				Type: "message_delta",
				Delta: &types.Delta{
					StopReason: "end_turn",
				},
				Usage: usageInfoToAnthropic(nil),
			}
			if err := writeSSEEvent(w, msgDelta); err != nil {
				return ErrClientDisconnected
			}
			*stopSent = true
			flusher.Flush()
		}
	}

	return nil
}

// ProxyGeminiStream takes a Gemini streaming response and writes Anthropic-format SSE.
func (h *StreamHandler) ProxyGeminiStream(
	w http.ResponseWriter,
	geminiResp io.ReadCloser,
	clientModel string,
	clientCtx context.Context,
	preamble StreamPreamble,
	activity *StreamActivity,
) error {
	flusher, ok := w.(http.Flusher)
	if !ok {
		return fmt.Errorf("streaming not supported by response writer")
	}

	if !preamble.MessageStartSent {
		if err := WriteMessageStart(w, clientModel); err != nil {
			return err
		}
	}

	contentIndex := 0
	var lineBuf bytes.Buffer
	contentStarted := preamble.MessageStartSent && preamble.TextPlaceholderIndex >= 0
	if contentStarted {
		contentIndex = preamble.TextPlaceholderIndex
	}
	stopSent := false
	readBuf := make([]byte, 4096)

	for {
		select {
		case <-clientCtx.Done():
			return ErrClientDisconnected
		default:
		}

		n, err := geminiResp.Read(readBuf)
		if n > 0 {
			for i := 0; i < n; i++ {
				b := readBuf[i]
				if b == '\n' {
					line := lineBuf.String()
					lineBuf.Reset()
					if err := h.processGeminiSSELine(w, flusher, line, &contentIndex, &contentStarted, &stopSent, clientModel, activity); err != nil {
						return err
					}
				} else {
					lineBuf.WriteByte(b)
				}
			}
		}

		if err == io.EOF {
			if lineBuf.Len() > 0 {
				line := lineBuf.String()
				if err := h.processGeminiSSELine(w, flusher, line, &contentIndex, &contentStarted, &stopSent, clientModel, activity); err != nil {
					return err
				}
			}
			break
		}
		if err != nil {
			return fmt.Errorf("failed to read stream: %w", err)
		}
	}

	if contentStarted {
		stopEvent := types.MessageEvent{
			Type:  "content_block_stop",
			Index: &contentIndex,
		}
		if err := writeSSEEvent(w, stopEvent); err != nil {
			return ErrClientDisconnected
		}
	}

	if !stopSent {
		msgDelta := types.MessageEvent{
			Type: "message_delta",
			Delta: &types.Delta{
				StopReason: "end_turn",
			},
			Usage: &types.Usage{InputTokens: 0, OutputTokens: 0},
		}
		if err := writeSSEEvent(w, msgDelta); err != nil {
			return ErrClientDisconnected
		}
		stopSent = true
	}

	stopEvent := types.MessageEvent{
		Type: "message_stop",
	}
	if err := writeSSEEvent(w, stopEvent); err != nil {
		return ErrClientDisconnected
	}
	flusher.Flush()

	return nil
}

func (h *StreamHandler) processGeminiSSELine(
	w http.ResponseWriter,
	flusher http.Flusher,
	line string,
	contentIndex *int,
	contentStarted *bool,
	stopSent *bool,
	clientModel string,
	activity *StreamActivity,
) error {
	line = strings.TrimSpace(line)
	if line == "" || !strings.HasPrefix(line, "data: ") {
		return nil
	}

	data := strings.TrimPrefix(line, "data: ")
	if data == "" {
		return nil
	}

	var chunk types.GeminiStreamChunk
	if err := json.Unmarshal([]byte(data), &chunk); err != nil {
		return nil
	}

	if len(chunk.Candidates) > 0 {
		candidate := chunk.Candidates[0]
		for _, part := range candidate.Content.Parts {
			if part.Text != "" {
				if !*contentStarted {
					*contentStarted = true
					startEvent := types.MessageEvent{
						Type:         "content_block_start",
						Index:        contentIndex,
						ContentBlock: &types.ContentBlock{Type: "text", Text: ""},
					}
					if err := writeSSEEvent(w, startEvent); err != nil {
						return ErrClientDisconnected
					}
				}

				delta := types.Delta{
					Type: "text_delta",
					Text: part.Text,
				}
				event := types.MessageEvent{
					Type:  "content_block_delta",
					Index: contentIndex,
					Delta: &delta,
				}
				if err := writeSSEEvent(w, event); err != nil {
					return ErrClientDisconnected
				}
				flusher.Flush()
				activity.fire()
			}
		}

		if candidate.FinishReason != "" && !*stopSent {
			if *contentStarted {
				stopEvent := types.MessageEvent{
					Type:  "content_block_stop",
					Index: contentIndex,
				}
				if err := writeSSEEvent(w, stopEvent); err != nil {
					return ErrClientDisconnected
				}
				*contentStarted = false
			}

			stopReason := "end_turn"
			if candidate.FinishReason == "MAX_TOKENS" {
				stopReason = "max_tokens"
			}

			msgDelta := types.MessageEvent{
				Type: "message_delta",
				Delta: &types.Delta{
					StopReason: stopReason,
				},
				Usage: usageInfoToAnthropic(nil),
			}
			if err := writeSSEEvent(w, msgDelta); err != nil {
				return ErrClientDisconnected
			}
			*stopSent = true
			flusher.Flush()
		}
	}

	return nil
}
