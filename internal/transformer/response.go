package transformer

import (
	"encoding/json"
	"fmt"
	"time"

	"github.com/routatic/proxy/pkg/types"
)

// ResponseTransformer converts OpenAI responses to Anthropic format.
type ResponseTransformer struct{}

// nonNegative clamps an integer to zero. Used when subtracting cache-token
// counts from prompt totals: defends against upstream payloads where the
// reported parts don't consistently sum to the whole.
func nonNegative(n int) int {
	if n < 0 {
		return 0
	}
	return n
}

// openAIUsageToAnthropic maps Chat Completions usage to Anthropic's disjoint
// token counts: input_tokens excludes cache reads and cache writes, so
// input + cache_read + cache_creation equals prompt_tokens. Every OpenAI-format
// path (non-streaming transformer, provider bridge, SSE stream) goes through
// here so they cannot disagree.
//
// DeepSeek reports prompt_cache_hit_tokens / prompt_cache_miss_tokens; the miss
// count is reported as cache creation, which keeps the total intact. Other
// OpenAI-compatible providers (Alibaba Model Studio) report
// prompt_tokens_details.cached_tokens and, for explicit cache,
// cache_creation_input_tokens. The DeepSeek fields win when either is non-zero.
func openAIUsageToAnthropic(usage types.UsageInfo) types.Usage {
	read := usage.PromptCacheHitTokens
	creation := usage.PromptCacheMissTokens
	if read == 0 && creation == 0 && usage.PromptTokensDetails != nil {
		read = usage.PromptTokensDetails.CachedTokens
		creation = usage.PromptTokensDetails.CacheCreationInputTokens
	}
	return types.Usage{
		InputTokens:              nonNegative(usage.PromptTokens - read - creation),
		OutputTokens:             usage.CompletionTokens,
		CacheCreationInputTokens: creation,
		CacheReadInputTokens:     read,
	}
}

// NewResponseTransformer creates a new response transformer.
func NewResponseTransformer() *ResponseTransformer {
	return &ResponseTransformer{}
}

// TransformResponse converts an OpenAI ChatCompletionResponse to Anthropic MessageResponse.
func (t *ResponseTransformer) TransformResponse(
	openaiResp *types.ChatCompletionResponse,
	originalModel string,
) (*types.MessageResponse, error) {
	if len(openaiResp.Choices) == 0 {
		return nil, fmt.Errorf("no choices in response")
	}

	choice := openaiResp.Choices[0]

	// Transform content blocks from the OpenAI message.
	contentBlocks, err := t.transformContent(choice.Message)
	if err != nil {
		return nil, fmt.Errorf("failed to transform content: %w", err)
	}

	// Map OpenAI finish reason to Anthropic stop reason.
	stopReason := t.mapFinishReason(choice.FinishReason)

	// Build Anthropic response.
	anthropicResp := &types.MessageResponse{
		ID:           openaiResp.ID,
		Type:         "message",
		Role:         "assistant",
		Content:      contentBlocks,
		Model:        originalModel,
		StopReason:   stopReason,
		StopSequence: "",
		// Per Anthropic Messages API spec, `input_tokens` excludes cache reads
		// and writes; see openAIUsageToAnthropic.
		Usage: openAIUsageToAnthropic(openaiResp.Usage),
	}

	return anthropicResp, nil
}

// transformContent converts an OpenAI message to Anthropic content blocks.
func (t *ResponseTransformer) transformContent(msg types.ChatMessage) ([]types.ContentBlock, error) {
	var blocks []types.ContentBlock

	// Preserve reasoning content as a thinking block so it round-trips correctly
	// on multi-turn tool-calling conversations.
	// Clients running the extended-thinking beta discard thinking blocks that
	// carry no signature, which empties the whole response. Upstream
	// OpenAI-compatible providers never send one, so stand in a placeholder.
	if msg.ReasoningContent != nil && *msg.ReasoningContent != "" {
		blocks = append(blocks, types.ContentBlock{
			Type:      "thinking",
			Thinking:  *msg.ReasoningContent,
			Signature: thinkingSignaturePlaceholder,
		})
	}

	// Handle tool calls — each becomes a tool_use content block.
	for _, tc := range msg.ToolCalls {
		// Arguments come as a JSON string from OpenAI, pass as raw JSON
		inputJSON := json.RawMessage(`{}`)
		if tc.Function.Arguments != "" {
			inputJSON = json.RawMessage(tc.Function.Arguments)
		}

		blocks = append(blocks, types.ContentBlock{
			Type:  "tool_use",
			ID:    sanitizeToolID(tc.ID),
			Name:  tc.Function.Name,
			Input: inputJSON,
		})
	}

	// Handle text content.
	if text := msg.ContentText(); text != "" {
		blocks = append(blocks, types.ContentBlock{
			Type: "text",
			Text: text,
		})
	}

	// Ensure at least one content block exists
	if len(blocks) == 0 {
		blocks = append(blocks, types.ContentBlock{
			Type: "text",
			Text: "",
		})
	}

	return blocks, nil
}

// mapFinishReason maps OpenAI finish reasons to Anthropic stop reasons.
func (t *ResponseTransformer) mapFinishReason(reason string) string {
	switch reason {
	case "stop":
		return "end_turn"
	case "length":
		return "max_tokens"
	case "tool_calls", "tool_use":
		return "tool_use"
	case "content_filter":
		return "end_turn"
	default:
		return "end_turn"
	}
}

// TransformErrorResponse converts an HTTP error into an Anthropic-style error map.
func TransformErrorResponse(statusCode int, message string) map[string]interface{} {
	return map[string]interface{}{
		"type": "error",
		"error": map[string]interface{}{
			"type":    mapHTTPStatusToErrorType(statusCode),
			"message": message,
		},
	}
}

// mapHTTPStatusToErrorType maps HTTP status codes to Anthropic error type strings.
func mapHTTPStatusToErrorType(statusCode int) string {
	switch {
	case statusCode == 400:
		return "invalid_request_error"
	case statusCode == 401:
		return "authentication_error"
	case statusCode == 403:
		return "permission_error"
	case statusCode == 404:
		return "not_found_error"
	case statusCode == 429:
		return "rate_limit_error"
	case statusCode >= 500:
		return "api_error"
	default:
		return "api_error"
	}
}

// TransformResponsesResponse converts an OpenAI ResponsesResponse to Anthropic MessageResponse.
func (t *ResponseTransformer) TransformResponsesResponse(
	responsesResp *types.ResponsesResponse,
	originalModel string,
) (*types.MessageResponse, error) {
	if len(responsesResp.Output) == 0 {
		return nil, fmt.Errorf("no output in response")
	}

	var contentBlocks []types.ContentBlock
	hasToolCall := false

	for _, output := range responsesResp.Output {
		switch output.Type {
		case "message":
			for _, c := range output.Content {
				if c.Type == "output_text" {
					contentBlocks = append(contentBlocks, types.ContentBlock{
						Type: "text",
						Text: c.Text,
					})
				}
			}
		case "function_call":
			hasToolCall = true
			inputJSON := json.RawMessage(`{}`)
			if output.Arguments != "" {
				inputJSON = json.RawMessage(normalizeToolArguments(output.Arguments))
			}
			contentBlocks = append(contentBlocks, types.ContentBlock{
				Type:  "tool_use",
				ID:    sanitizeToolID(output.CallID),
				Name:  output.Name,
				Input: inputJSON,
			})
		}
	}

	if len(contentBlocks) == 0 {
		contentBlocks = append(contentBlocks, types.ContentBlock{
			Type: "text",
			Text: "",
		})
	}

	// A function_call output becomes a tool_use block, so the turn must report
	// tool_use. Reporting end_turn here makes the client treat the turn as
	// finished and skip running the tool.
	stopReason := "end_turn"
	if hasToolCall {
		stopReason = "tool_use"
	}

	anthropicResp := &types.MessageResponse{
		ID:         responsesResp.ID,
		Type:       "message",
		Role:       "assistant",
		Content:    contentBlocks,
		Model:      originalModel,
		StopReason: stopReason,
		Usage: types.Usage{
			InputTokens:  responsesResp.Usage.InputTokens,
			OutputTokens: responsesResp.Usage.OutputTokens,
		},
	}

	return anthropicResp, nil
}

// TransformGeminiResponse converts a GeminiResponse to Anthropic MessageResponse.
func (t *ResponseTransformer) TransformGeminiResponse(
	geminiResp *types.GeminiResponse,
	originalModel string,
) (*types.MessageResponse, error) {
	if len(geminiResp.Candidates) == 0 {
		return nil, fmt.Errorf("no candidates in response")
	}

	candidate := geminiResp.Candidates[0]
	var contentBlocks []types.ContentBlock

	for _, part := range candidate.Content.Parts {
		if part.Text != "" {
			contentBlocks = append(contentBlocks, types.ContentBlock{
				Type: "text",
				Text: part.Text,
			})
		}
	}

	if len(contentBlocks) == 0 {
		contentBlocks = append(contentBlocks, types.ContentBlock{
			Type: "text",
			Text: "",
		})
	}

	stopReason := "end_turn"
	switch candidate.FinishReason {
	case "MAX_TOKENS":
		stopReason = "max_tokens"
	}

	anthropicResp := &types.MessageResponse{
		ID:         fmt.Sprintf("gemini_%d", time.Now().UnixNano()),
		Type:       "message",
		Role:       "assistant",
		Content:    contentBlocks,
		Model:      originalModel,
		StopReason: stopReason,
	}

	if geminiResp.UsageMetadata != nil {
		anthropicResp.Usage = types.Usage{
			InputTokens:  geminiResp.UsageMetadata.PromptTokenCount,
			OutputTokens: geminiResp.UsageMetadata.CandidatesTokenCount,
		}
	}

	return anthropicResp, nil
}
