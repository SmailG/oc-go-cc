// Package handlers contains HTTP request handlers for API endpoints.
package handlers

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"oc-go-cc/internal/client"
	"oc-go-cc/internal/config"
	"oc-go-cc/internal/metrics"
	"oc-go-cc/internal/middleware"
	"oc-go-cc/internal/router"
	"oc-go-cc/internal/token"
	"oc-go-cc/internal/transformer"
	"oc-go-cc/pkg/types"
)

// MessagesHandler handles /v1/messages requests.
type MessagesHandler struct {
	client              *client.OpenCodeClient
	modelRouter         *router.ModelRouter
	fallbackHandler     *router.FallbackHandler
	requestTransformer  *transformer.RequestTransformer
	responseTransformer *transformer.ResponseTransformer
	streamHandler       *transformer.StreamHandler
	tokenCounter        *token.Counter
	logger              *slog.Logger
	rateLimiter         *middleware.RateLimiter
	requestDedup        *middleware.RequestDeduplicator
	requestIDGen        *middleware.RequestIDGenerator
	metrics             *metrics.Metrics
}

// responseWriter wraps http.ResponseWriter to track if headers were written.
// A mutex serializes writes: the heartbeat goroutine and ProxyStream must not
// interleave SSE frames on the same connection (that corrupts the stream and
// causes Claude Code to disconnect).
type responseWriter struct {
	http.ResponseWriter
	wroteHeader            bool
	mu                     sync.Mutex
	keepaliveIndex         int // active block index for gap keepalives; -1 = ping only
	keepaliveMode          transformer.KeepaliveMode
	preambleKeepaliveIndex int // open placeholder text block for TTFT / fallback; -1 if closed
	lastContentWrite       time.Time
	inKeepalive            bool
}

const streamKeepaliveGap = 300 * time.Millisecond

func (w *responseWriter) WriteHeader(code int) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if !w.wroteHeader {
		w.wroteHeader = true
		w.ResponseWriter.WriteHeader(code)
	}
}

func (w *responseWriter) Write(b []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.writeUnlocked(b)
}

func (w *responseWriter) writeUnlocked(b []byte) (int, error) {
	if !w.wroteHeader {
		w.wroteHeader = true
		w.ResponseWriter.WriteHeader(http.StatusOK)
	}
	n, err := w.ResponseWriter.Write(b)
	if err == nil && !w.inKeepalive {
		w.lastContentWrite = time.Now()
	}
	return n, err
}

func (w *responseWriter) keepaliveState() (idx int, mode transformer.KeepaliveMode, preambleIdx int, idle time.Duration) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.keepaliveIndex, w.keepaliveMode, w.preambleKeepaliveIndex, time.Since(w.lastContentWrite)
}

func (w *responseWriter) SetStreamKeepalive(index int, mode transformer.KeepaliveMode) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.keepaliveIndex = index
	w.keepaliveMode = mode
}

func (w *responseWriter) SetPreambleKeepaliveIndex(index int) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.preambleKeepaliveIndex = index
}

func (w *responseWriter) NoteStreamContentWrite() {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.lastContentWrite = time.Now()
}

func (w *responseWriter) emitKeepaliveAt(idx int, mode transformer.KeepaliveMode) error {
	idxCopy := idx
	var event types.MessageEvent
	switch mode {
	case transformer.KeepaliveText:
		event = types.MessageEvent{
			Type:  "content_block_delta",
			Index: &idxCopy,
			Delta: &types.Delta{Type: "text_delta", Text: "\u200b"},
		}
	case transformer.KeepaliveThinking:
		// thinking_delta keepalives do not reset Claude Code's ~6s inactivity timer.
		event = types.MessageEvent{
			Type:  "content_block_delta",
			Index: &idxCopy,
			Delta: &types.Delta{Type: "text_delta", Text: "\u200b"},
		}
	case transformer.KeepaliveToolInput:
		event = types.MessageEvent{
			Type:  "content_block_delta",
			Index: &idxCopy,
			Delta: &types.Delta{Type: "input_json_delta", PartialJSON: " "},
		}
	default:
		return nil
	}
	if err := transformer.WriteSSEEventUnlocked(w.ResponseWriter, event); err != nil {
		return err
	}
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
	return nil
}

func (w *responseWriter) emitKeepalive() error {
	w.mu.Lock()
	defer w.mu.Unlock()

	idx := w.keepaliveIndex
	mode := w.keepaliveMode
	if idx < 0 {
		idx = w.preambleKeepaliveIndex
		mode = transformer.KeepaliveText
	}
	if idx < 0 || time.Since(w.lastContentWrite) < streamKeepaliveGap {
		return nil
	}

	w.inKeepalive = true
	defer func() { w.inKeepalive = false }()
	return w.emitKeepaliveAt(idx, mode)
}

// lockedSSEWriter serializes ProxyStream SSE writes with the heartbeat goroutine.
type lockedSSEWriter struct {
	parent *responseWriter
}

func (l *lockedSSEWriter) Header() http.Header { return l.parent.ResponseWriter.Header() }

func (l *lockedSSEWriter) WriteHeader(code int) { l.parent.WriteHeader(code) }

func (l *lockedSSEWriter) Write(b []byte) (int, error) {
	l.parent.mu.Lock()
	defer l.parent.mu.Unlock()
	return l.parent.writeUnlocked(b)
}

func (l *lockedSSEWriter) Flush() {
	l.parent.mu.Lock()
	defer l.parent.mu.Unlock()
	if f, ok := l.parent.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

func (l *lockedSSEWriter) SetStreamKeepalive(index int, mode transformer.KeepaliveMode) {
	l.parent.SetStreamKeepalive(index, mode)
}

func (l *lockedSSEWriter) SetPreambleKeepaliveIndex(index int) {
	l.parent.SetPreambleKeepaliveIndex(index)
}

func (l *lockedSSEWriter) NoteStreamContentWrite() {
	l.parent.NoteStreamContentWrite()
}

// keepaliveActivityFor is retained for StreamActivity wiring; gap keepalives no longer stop at first content.
func keepaliveActivityFor(_ http.ResponseWriter) *transformer.StreamActivity {
	return nil
}

// Flush implements http.Flusher for SSE streaming support.
func (w *responseWriter) Flush() {
	w.mu.Lock()
	defer w.mu.Unlock()
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// NewMessagesHandler creates a new messages handler.
func NewMessagesHandler(
	openCodeClient *client.OpenCodeClient,
	modelRouter *router.ModelRouter,
	fallbackHandler *router.FallbackHandler,
	tokenCounter *token.Counter,
	metrics *metrics.Metrics,
) *MessagesHandler {
	return &MessagesHandler{
		client:              openCodeClient,
		modelRouter:         modelRouter,
		fallbackHandler:     fallbackHandler,
		requestTransformer:  transformer.NewRequestTransformer(),
		responseTransformer: transformer.NewResponseTransformer(),
		streamHandler:       transformer.NewStreamHandler(),
		tokenCounter:        tokenCounter,
		logger:              slog.Default(),
		rateLimiter:         middleware.NewRateLimiter(100, time.Minute),
		requestDedup:        middleware.NewRequestDeduplicator(500 * time.Millisecond),
		requestIDGen:        middleware.NewRequestIDGenerator(),
		metrics:             metrics,
	}
}

// HandleMessages handles POST /v1/messages.
func (h *MessagesHandler) HandleMessages(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	// Generate or get request ID for correlation
	requestID := r.Header.Get("X-Request-ID")
	if requestID == "" {
		requestID = h.requestIDGen.Generate()
	}
	w.Header().Set("X-Request-ID", requestID)
	logger := h.logger.With("request_id", requestID)

	// Rate limiting
	clientIP := middleware.GetClientIP(r)
	if !h.rateLimiter.Allow(clientIP) {
		h.metrics.RecordRateLimited()
		logger.Warn("rate limited", "client", clientIP)
		http.Error(w, "rate limited", http.StatusTooManyRequests)
		return
	}

	// Read the raw request body for debug logging
	var rawBody json.RawMessage
	if err := json.NewDecoder(r.Body).Decode(&rawBody); err != nil {
		h.sendError(w, http.StatusBadRequest, "invalid request body", err)
		return
	}

	// Deduplicate - skip duplicate requests
	if _, ok := h.requestDedup.TryAcquire(rawBody); !ok {
		h.metrics.RecordDeduplicated()
		logger.Info("duplicate request skipped")
		return
	}

	// Parse into Anthropic request
	var anthropicReq types.MessageRequest
	if err := json.Unmarshal(rawBody, &anthropicReq); err != nil {
		h.sendError(w, http.StatusBadRequest, "invalid request body", err)
		return
	}

	// Validate request
	if err := anthropicReq.Validate(); err != nil {
		h.sendError(w, http.StatusBadRequest, err.Error(), nil)
		return
	}

	// Record metrics
	isStreaming := anthropicReq.Stream != nil && *anthropicReq.Stream
	h.metrics.RecordRequest(isStreaming)

	logger.Info("received request",
		"model", anthropicReq.Model,
		"streaming", isStreaming,
		"messages", len(anthropicReq.Messages),
		"tools", len(anthropicReq.Tools),
		"max_tokens", anthropicReq.MaxTokens,
	)

	// Build message content for routing and token counting.
	var routerMessages []router.MessageContent
	var tokenMessages []token.MessageContent
	systemText := anthropicReq.SystemText()

	for _, msg := range anthropicReq.Messages {
		blocks := msg.ContentBlocks()
		content := extractTextFromBlocks(blocks)
		mc := router.MessageContent{
			Role:    msg.Role,
			Content: content,
		}
		routerMessages = append(routerMessages, mc)
		tokenMessages = append(tokenMessages, token.MessageContent{
			Role:    msg.Role,
			Content: content,
		})
	}

	// Count tokens.
	tokenCount, err := h.tokenCounter.CountMessages(systemText, tokenMessages)
	if err != nil {
		logger.Warn("failed to count tokens", "error", err)
		tokenCount = 0
	}

	// Route to appropriate model and build fallback chain.
	modelChain, routeResult, err := h.buildModelChain(anthropicReq.Model, routerMessages, tokenCount, len(anthropicReq.Tools), isStreaming)
	if err != nil {
		h.sendError(w, http.StatusInternalServerError, "routing failed", err)
		return
	}

	logger.Info("routing request",
		"scenario", routeResult.Scenario,
		"model", routeResult.Primary.ModelID,
		"provider", routeResult.Primary.Provider,
		"tokens", tokenCount,
		"tools", len(anthropicReq.Tools),
		"prefer_capability", h.modelRouter.IsPreferCapabilityEnabled(),
		"streaming_scenario_routing", h.modelRouter.IsStreamingScenarioRoutingEnabled(),
	)

	if isStreaming {
		// Streaming: use ProxyStream for real-time SSE transformation
		h.handleStreaming(logger, w, r, &anthropicReq, modelChain, rawBody)
	} else {
		// Non-streaming: execute with fallback and return full response
		h.handleNonStreaming(logger, w, r, &anthropicReq, modelChain, rawBody)
	}
}

// buildModelChain resolves the request to a model chain (primary + fallbacks),
// honoring model_overrides (with a deduplicated scenario safety-net) and
// respecting the streaming-scenario-routing toggle.
//
// Precedence:
//  1. If requestedModel matches an entry in model_overrides, use that as the
//     primary and append the scenario chain as a deduplicated safety net.
//  2. Otherwise, fall through to scenario-based routing via routeOnce.
func (h *MessagesHandler) buildModelChain(
	requestedModel string,
	routerMessages []router.MessageContent,
	tokenCount int,
	toolCount int,
	isStreaming bool,
) ([]config.ModelConfig, router.RouteResult, error) {
	if requestedModel != "" {
		if overrideResult, ok := h.modelRouter.RouteWithOverride(requestedModel); ok {
			scenarioResult, err := h.routeOnce(routerMessages, tokenCount, toolCount, "", isStreaming)
			if err != nil {
				// Override is valid; surface the scenario routing error rather
				// than silently dropping the safety net.
				return overrideResult.GetModelChain(), overrideResult, err
			}
			chain := appendUniqueModels(overrideResult.GetModelChain(), scenarioResult.GetModelChain())
			return chain, overrideResult, nil
		}
	}

	result, err := h.routeOnce(routerMessages, tokenCount, toolCount, requestedModel, isStreaming)
	if err != nil {
		return nil, result, err
	}
	return result.GetModelChain(), result, nil
}

// routeOnce performs scenario-based routing, honoring the streaming-scenario-routing
// toggle. Pass requestedModel="" to force scenario routing (used for the override
// safety-net chain), or a non-empty value to let resolveRequestedModel kick in
// (only when respect_requested_model is enabled and no override matched).
func (h *MessagesHandler) routeOnce(
	routerMessages []router.MessageContent,
	tokenCount int,
	toolCount int,
	requestedModel string,
	isStreaming bool,
) (router.RouteResult, error) {
	if isStreaming && !h.modelRouter.IsStreamingScenarioRoutingEnabled() {
		// When false, RouteForStreaming prefers fast models (economy TTFT).
		// When true, Route() applies full scenario rules to streaming as well.
		return h.modelRouter.RouteForStreaming(routerMessages, tokenCount, toolCount, requestedModel), nil
	}
	return h.modelRouter.Route(routerMessages, tokenCount, toolCount, requestedModel)
}

// appendUniqueModels appends models from extra to base, skipping any model_id
// already present in base. The first occurrence of a ModelID is kept; later
// duplicates are dropped. Order of the base chain is preserved.
func appendUniqueModels(base, extra []config.ModelConfig) []config.ModelConfig {
	if len(extra) == 0 {
		return base
	}
	seen := make(map[string]struct{}, len(base))
	for _, m := range base {
		seen[m.ModelID] = struct{}{}
	}
	for _, m := range extra {
		if _, ok := seen[m.ModelID]; ok {
			continue
		}
		base = append(base, m)
		seen[m.ModelID] = struct{}{}
	}
	return base
}

// ensureStreamTextPlaceholder opens the placeholder text block once per stream (for TTFT keepalives).
func ensureStreamTextPlaceholder(rw *responseWriter, preamble *transformer.StreamPreamble) error {
	if preamble.TextPlaceholderIndex >= 0 {
		return nil
	}
	idx, err := transformer.WriteTextPlaceholderBlock(rw)
	if err != nil {
		return err
	}
	preamble.TextPlaceholderIndex = idx
	rw.SetStreamKeepalive(idx, transformer.KeepaliveText)
	rw.SetPreambleKeepaliveIndex(idx)
	rw.NoteStreamContentWrite()
	return nil
}

// handleStreaming handles a streaming request with real-time SSE proxying.
func (h *MessagesHandler) handleStreaming(
	logger *slog.Logger,
	w http.ResponseWriter,
	r *http.Request,
	anthropicReq *types.MessageRequest,
	modelChain []config.ModelConfig,
	rawBody json.RawMessage,
) {
	clientCtx := r.Context()
	clientModel := anthropicReq.Model

	rw := &responseWriter{
		ResponseWriter:         w,
		keepaliveIndex:         -1,
		preambleKeepaliveIndex: -1,
		lastContentWrite:       time.Now(),
	}

	// Set SSE headers immediately so Claude Code knows the stream is alive.
	// Use rw (not the bare ResponseWriter) so headers and flush stay consistent
	// if middleware wraps the writer.
	rw.Header().Set("Content-Type", "text/event-stream")
	rw.Header().Set("Cache-Control", "no-cache")
	rw.Header().Set("Connection", "keep-alive")
	rw.Header().Set("X-Accel-Buffering", "no")
	rw.WriteHeader(http.StatusOK)
	rw.Flush()

	// Anthropic ping events while waiting on upstream / between chunks.
	// SSE comment keepalives do not reset Claude Code's ~6s inactivity timer.
	var finished int32
	heartbeatDone := make(chan struct{})
	go func() {
		ticker := time.NewTicker(500 * time.Millisecond)
		defer ticker.Stop()

		for {
			select {
			case <-ticker.C:
				if atomic.LoadInt32(&finished) == 1 {
					return
				}
				var err error
				idx, _, preambleIdx, idle := rw.keepaliveState()
				if idle >= streamKeepaliveGap && (idx >= 0 || preambleIdx >= 0) {
					err = rw.emitKeepalive()
				}
				if err != nil {
					return
				}
			case <-heartbeatDone:
				return
			case <-clientCtx.Done():
				return
			}
		}
	}()
	defer func() {
		atomic.StoreInt32(&finished, 1)
		close(heartbeatDone)
	}()

	streamStart := time.Now()

	// One message_start per stream; model fallbacks must not emit another.
	var streamPreamble transformer.StreamPreamble
	if err := transformer.WriteMessageStart(rw, clientModel); err != nil {
		if clientCtx.Err() == context.Canceled || err == transformer.ErrClientDisconnected {
			logger.Info("client disconnected before stream preamble")
			return
		}
		logger.Warn("failed to write message_start", "error", err)
		h.sendStreamError(rw, "failed to start stream")
		return
	}
	streamPreamble.MessageStartSent = true

	for _, model := range modelChain {
		select {
		case <-clientCtx.Done():
			logger.Info("client disconnected, stopping streaming fallbacks")
			return
		default:
		}

		h.logger.Info("attempting streaming model", "model", model.ModelID, "provider", model.Provider)

		ctx, cancel := context.WithTimeout(clientCtx, 2*time.Minute)

		// Zen models use their own endpoint classification
		if client.IsZen(model) {
			endpointType := client.ClassifyEndpoint(model.ModelID)
			switch endpointType {
			case client.EndpointAnthropic:
				modelBody := replaceModelInRawBody(rawBody, model.ModelID)
				if err := ensureStreamTextPlaceholder(rw, &streamPreamble); err != nil {
					cancel()
					if clientCtx.Err() == context.Canceled || err == transformer.ErrClientDisconnected {
						logger.Info("client disconnected before anthropic stream")
						return
					}
					logger.Warn("failed to write stream placeholder", "model", model.ModelID, "error", err)
					continue
				}
				if err := h.handleAnthropicStreaming(ctx, rw, modelBody, model, streamPreamble); err != nil {
					cancel()
					if clientCtx.Err() == context.Canceled {
						h.logger.Info("client disconnected during anthropic stream")
						return
					}
					h.logger.Warn("anthropic streaming failed", "model", model.ModelID, "error", err)
					continue
				}
				cancel()
				latency := time.Since(streamStart)
				h.metrics.RecordSuccess(model.ModelID, latency)
				h.logger.Info("streaming completed", "model", model.ModelID, "latency", latency)
				return

			case client.EndpointResponses:
				if err := ensureStreamTextPlaceholder(rw, &streamPreamble); err != nil {
					cancel()
					if clientCtx.Err() == context.Canceled || err == transformer.ErrClientDisconnected {
						logger.Info("client disconnected before responses stream")
						return
					}
					logger.Warn("failed to write stream placeholder", "model", model.ModelID, "error", err)
					continue
				}
				if err := h.handleResponsesStreaming(ctx, rw, anthropicReq, model, clientCtx, clientModel, streamPreamble); err != nil {
					cancel()
					if clientCtx.Err() == context.Canceled {
						h.logger.Info("client disconnected during responses stream")
						return
					}
					h.logger.Warn("responses streaming failed", "model", model.ModelID, "error", err)
					continue
				}
				cancel()
				latency := time.Since(streamStart)
				h.metrics.RecordSuccess(model.ModelID, latency)
				h.logger.Info("streaming completed", "model", model.ModelID, "latency", latency)
				return

			case client.EndpointGemini:
				if err := ensureStreamTextPlaceholder(rw, &streamPreamble); err != nil {
					cancel()
					if clientCtx.Err() == context.Canceled || err == transformer.ErrClientDisconnected {
						logger.Info("client disconnected before gemini stream")
						return
					}
					logger.Warn("failed to write stream placeholder", "model", model.ModelID, "error", err)
					continue
				}
				if err := h.handleGeminiStreaming(ctx, rw, anthropicReq, model, clientCtx, clientModel, streamPreamble); err != nil {
					cancel()
					if clientCtx.Err() == context.Canceled {
						h.logger.Info("client disconnected during gemini stream")
						return
					}
					h.logger.Warn("gemini streaming failed", "model", model.ModelID, "error", err)
					continue
				}
				cancel()
				latency := time.Since(streamStart)
				h.metrics.RecordSuccess(model.ModelID, latency)
				h.logger.Info("streaming completed", "model", model.ModelID, "latency", latency)
				return

			default:
				// Fall through to OpenAI-compatible handling
			}
		} else if client.IsAnthropicModel(model.ModelID) {
			// Go provider Anthropic-native models (MiniMax, Qwen)
			modelBody := replaceModelInRawBody(rawBody, model.ModelID)
			if err := ensureStreamTextPlaceholder(rw, &streamPreamble); err != nil {
				cancel()
				if clientCtx.Err() == context.Canceled || err == transformer.ErrClientDisconnected {
					logger.Info("client disconnected before anthropic stream")
					return
				}
				logger.Warn("failed to write stream placeholder", "model", model.ModelID, "error", err)
				continue
			}
			if err := h.handleAnthropicStreaming(ctx, rw, modelBody, model, streamPreamble); err != nil {
				cancel()
				if clientCtx.Err() == context.Canceled {
					h.logger.Info("client disconnected during anthropic stream")
					return
				}
				h.logger.Warn("anthropic streaming failed", "model", model.ModelID, "error", err)
				continue
			}
			cancel()
			latency := time.Since(streamStart)
			h.metrics.RecordSuccess(model.ModelID, latency)
			h.logger.Info("streaming completed", "model", model.ModelID, "latency", latency)
			return
		}

		// OpenAI-compatible models (both Go and Zen)
		openaiReq, err := h.requestTransformer.TransformRequest(anthropicReq, model)
		if err != nil {
			cancel()
			logger.Warn("request transform failed", "model", model.ModelID, "error", err)
			continue
		}

		if err := ensureStreamTextPlaceholder(rw, &streamPreamble); err != nil {
			cancel()
			if clientCtx.Err() == context.Canceled || err == transformer.ErrClientDisconnected {
				logger.Info("client disconnected before upstream stream")
				return
			}
			logger.Warn("failed to write stream placeholder", "model", model.ModelID, "error", err)
			continue
		}

		upstreamStart := time.Now()
		streamBody, err := h.client.GetStreamingBody(ctx, model.ModelID, openaiReq, model)
		upstreamConnect := time.Since(upstreamStart)
		if err != nil {
			cancel()
			if clientCtx.Err() == context.Canceled {
				logger.Info("client disconnected during upstream request",
					"upstream_connect", upstreamConnect,
				)
				return
			}
			logger.Warn("streaming request failed", "model", model.ModelID, "error", err, "upstream_connect", upstreamConnect)
			continue
		}
		logger.Info("upstream stream connected", "model", model.ModelID, "upstream_connect", upstreamConnect)

		activity := keepaliveActivityFor(rw)
		streamW := &lockedSSEWriter{parent: rw}
		if err := h.streamHandler.ProxyStream(streamW, streamBody, clientModel, clientCtx, streamPreamble, activity); err != nil {
			_ = streamBody.Close()
			cancel()
			if err == transformer.ErrClientDisconnected {
				logger.Info("client disconnected during stream",
					"latency", time.Since(streamStart),
					"client_canceled", clientCtx.Err() != nil,
				)
				return
			}
			if clientCtx.Err() == context.Canceled {
				logger.Info("client disconnected during stream (context canceled)")
				return
			}
			logger.Warn("stream proxy failed", "model", model.ModelID, "error", err)
			continue
		}

		_ = streamBody.Close()
		cancel()
		rw.SetStreamKeepalive(-1, transformer.KeepaliveNone)
		latency := time.Since(streamStart)
		h.metrics.RecordSuccess(model.ModelID, latency)
		logger.Info("streaming completed", "model", model.ModelID, "latency", latency)
		return
	}

	h.metrics.RecordFailure()
	if !rw.wroteHeader {
		h.sendError(w, http.StatusBadGateway, "all streaming models failed", nil)
	} else {
		h.sendStreamError(rw, "all upstream models failed")
	}
}

// handleResponsesStreaming handles streaming for OpenAI Responses endpoint.
func (h *MessagesHandler) handleResponsesStreaming(
	ctx context.Context,
	w http.ResponseWriter,
	anthropicReq *types.MessageRequest,
	model config.ModelConfig,
	clientCtx context.Context,
	clientModel string,
	preamble transformer.StreamPreamble,
) error {
	req, err := h.requestTransformer.TransformToResponses(anthropicReq, model)
	if err != nil {
		return fmt.Errorf("responses transform failed: %w", err)
	}

	streamBody, err := h.client.GetResponsesStreamingBody(ctx, model.ModelID, req, model)
	if err != nil {
		return err
	}

	activity := keepaliveActivityFor(w)
	if err := h.streamHandler.ProxyResponsesStream(w, streamBody, clientModel, clientCtx, preamble, activity); err != nil {
		_ = streamBody.Close()
		return err
	}

	_ = streamBody.Close()
	return nil
}

// handleGeminiStreaming handles streaming for Gemini endpoint.
func (h *MessagesHandler) handleGeminiStreaming(
	ctx context.Context,
	w http.ResponseWriter,
	anthropicReq *types.MessageRequest,
	model config.ModelConfig,
	clientCtx context.Context,
	clientModel string,
	preamble transformer.StreamPreamble,
) error {
	req, err := h.requestTransformer.TransformToGemini(anthropicReq, model)
	if err != nil {
		return fmt.Errorf("gemini transform failed: %w", err)
	}

	streamBody, err := h.client.GetGeminiStreamingBody(ctx, model.ModelID, req, model)
	if err != nil {
		return err
	}

	activity := keepaliveActivityFor(w)
	if err := h.streamHandler.ProxyGeminiStream(w, streamBody, clientModel, clientCtx, preamble, activity); err != nil {
		_ = streamBody.Close()
		return err
	}

	_ = streamBody.Close()
	return nil
}

// replaceModelInRawBody replaces the model field in raw JSON body with the actual model ID.
func replaceModelInRawBody(rawBody json.RawMessage, modelID string) json.RawMessage {
	bodyStr := string(rawBody)

	if idx := strings.Index(bodyStr, `"model":"`); idx != -1 {
		start := idx + len(`"model":"`)
		if end := strings.Index(bodyStr[start:], `"`); end != -1 {
			oldModel := bodyStr[start : start+end]
			newBody := bodyStr[:start] + modelID + bodyStr[start+end:]
			slog.Debug("replaced model in request body",
				"old_model", oldModel,
				"new_model", modelID,
				"success", true)
			return json.RawMessage(newBody)
		}
	}

	slog.Warn("could not find model field in request body, using original",
		"body_preview", bodyStr[:min(len(bodyStr), 200)])
	return rawBody
}

// handleAnthropicStreaming sends a raw Anthropic request to the Anthropic endpoint.
func (h *MessagesHandler) handleAnthropicStreaming(
	ctx context.Context,
	w http.ResponseWriter,
	rawBody json.RawMessage,
	model config.ModelConfig,
	preamble transformer.StreamPreamble,
) error {
	h.logger.Debug("sending anthropic streaming request",
		"model_id", model.ModelID,
		"body_preview", string(rawBody)[:min(len(rawBody), 200)])

	resp, err := h.client.SendAnthropicRequest(ctx, rawBody, true, model)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()

	activity := keepaliveActivityFor(w)
	if err := transformer.CopyAnthropicSSE(w, resp.Body, preamble.MessageStartSent, activity); err != nil {
		if ctx.Err() == context.Canceled {
			return transformer.ErrClientDisconnected
		}
		return fmt.Errorf("failed to copy response: %w", err)
	}

	return nil
}

// sendStreamError sends an error event in the SSE stream.
func (h *MessagesHandler) sendStreamError(w http.ResponseWriter, message string) {
	h.logger.Error("sending stream error", "message", message)

	errorEvent := map[string]interface{}{
		"type": "error",
		"error": map[string]interface{}{
			"type":    "api_error",
			"message": message,
		},
	}

	data, _ := json.Marshal(errorEvent)
	_, _ = fmt.Fprintf(w, "event: error\ndata: %s\n\n", string(data))

	if f, ok := w.(http.Flusher); ok {
		f.Flush()
	}
}

// handleNonStreaming handles a non-streaming request with fallback.
func (h *MessagesHandler) handleNonStreaming(
	logger *slog.Logger,
	w http.ResponseWriter,
	r *http.Request,
	anthropicReq *types.MessageRequest,
	modelChain []config.ModelConfig,
	rawBody json.RawMessage,
) {
	ctx := r.Context()
	startTime := time.Now()

	result, responseBody, err := h.fallbackHandler.ExecuteWithFallback(
		ctx,
		modelChain,
		func(ctx context.Context, model config.ModelConfig) ([]byte, error) {
			// Zen models use their own endpoint classification
			if client.IsZen(model) {
				endpointType := client.ClassifyEndpoint(model.ModelID)
				switch endpointType {
				case client.EndpointAnthropic:
					return h.executeAnthropicRequest(ctx, rawBody, model)
				case client.EndpointResponses:
					return h.executeResponsesRequest(ctx, anthropicReq, model)
				case client.EndpointGemini:
					return h.executeGeminiRequest(ctx, anthropicReq, model)
				default:
					// Fall through to OpenAI-compatible handling
				}
			} else if client.IsAnthropicModel(model.ModelID) {
				// Go provider Anthropic-native models (MiniMax, Qwen)
				return h.executeAnthropicRequest(ctx, rawBody, model)
			}

			// OpenAI-compatible models (both Go and Zen)
			return h.executeOpenAIRequest(ctx, anthropicReq, model)
		},
	)

	if err != nil {
		// If the client canceled the request, don't report this as a model failure.
		// This often happens when Claude Code retries/supersedes a request.
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			logger.Info("request canceled", "error", err)
			return
		}
		h.metrics.RecordFailure()
		h.sendError(w, http.StatusBadGateway, "all models failed", err)
		return
	}

	latency := time.Since(startTime)
	h.metrics.RecordSuccess(result.ModelID, latency)

	logger.Info("request completed",
		"model", result.ModelID,
		"attempts", result.Attempted,
		"latency", latency,
	)

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(responseBody)
}

// executeAnthropicRequest executes a request to the Anthropic endpoint (for MiniMax models).
func (h *MessagesHandler) executeAnthropicRequest(
	ctx context.Context,
	rawBody json.RawMessage,
	model config.ModelConfig,
) ([]byte, error) {
	resp, err := h.client.SendAnthropicRequest(ctx, rawBody, false, model)
	if err != nil {
		return nil, fmt.Errorf("anthropic request failed: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("failed to read response: %w", err)
	}

	h.logger.Debug("anthropic response", "body", string(body))

	return body, nil
}

// executeOpenAIRequest executes a request to the OpenAI endpoint with transformation.
func (h *MessagesHandler) executeOpenAIRequest(
	ctx context.Context,
	anthropicReq *types.MessageRequest,
	model config.ModelConfig,
) ([]byte, error) {
	openaiReq, err := h.requestTransformer.TransformRequest(anthropicReq, model)
	if err != nil {
		return nil, fmt.Errorf("request transform failed: %w", err)
	}

	resp, err := h.client.ChatCompletionNonStreaming(ctx, model.ModelID, openaiReq, model)
	if err != nil {
		return nil, fmt.Errorf("chat completion failed: %w", err)
	}

	anthropicResp, err := h.responseTransformer.TransformResponse(resp, model.ModelID)
	if err != nil {
		return nil, fmt.Errorf("response transform failed: %w", err)
	}

	return json.Marshal(anthropicResp)
}

// executeResponsesRequest executes a request to the OpenAI Responses endpoint.
func (h *MessagesHandler) executeResponsesRequest(
	ctx context.Context,
	anthropicReq *types.MessageRequest,
	model config.ModelConfig,
) ([]byte, error) {
	req, err := h.requestTransformer.TransformToResponses(anthropicReq, model)
	if err != nil {
		return nil, fmt.Errorf("responses transform failed: %w", err)
	}

	resp, err := h.client.ResponsesCompletionNonStreaming(ctx, model.ModelID, req, model)
	if err != nil {
		return nil, fmt.Errorf("responses completion failed: %w", err)
	}

	anthropicResp, err := h.responseTransformer.TransformResponsesResponse(resp, model.ModelID)
	if err != nil {
		return nil, fmt.Errorf("response transform failed: %w", err)
	}

	return json.Marshal(anthropicResp)
}

// executeGeminiRequest executes a request to the Gemini endpoint.
func (h *MessagesHandler) executeGeminiRequest(
	ctx context.Context,
	anthropicReq *types.MessageRequest,
	model config.ModelConfig,
) ([]byte, error) {
	req, err := h.requestTransformer.TransformToGemini(anthropicReq, model)
	if err != nil {
		return nil, fmt.Errorf("gemini transform failed: %w", err)
	}

	resp, err := h.client.GeminiCompletionNonStreaming(ctx, model.ModelID, req, model)
	if err != nil {
		return nil, fmt.Errorf("gemini completion failed: %w", err)
	}

	anthropicResp, err := h.responseTransformer.TransformGeminiResponse(resp, model.ModelID)
	if err != nil {
		return nil, fmt.Errorf("response transform failed: %w", err)
	}

	return json.Marshal(anthropicResp)
}

// extractTextFromBlocks extracts plain text from Anthropic content blocks.
func extractTextFromBlocks(blocks []types.ContentBlock) string {
	var content string
	for _, block := range blocks {
		switch block.Type {
		case "text":
			content += block.Text
		case "tool_use":
			content += fmt.Sprintf("[Tool Use: %s]", block.Name)
		case "tool_result":
			content += block.TextContent()
		case "thinking":
			// Skip thinking blocks for text extraction
		case "image":
			content += "[Image]"
		}
	}
	return content
}

// sendError sends an error response in Anthropic format.
func (h *MessagesHandler) sendError(w http.ResponseWriter, statusCode int, message string, err error) {
	h.logger.Error("request error",
		"status", statusCode,
		"message", message,
		"error", err,
	)

	if rw, ok := w.(*responseWriter); ok && rw.wroteHeader {
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(statusCode)

	errorResp := transformer.TransformErrorResponse(statusCode, message)
	_ = json.NewEncoder(w).Encode(errorResp)
}
