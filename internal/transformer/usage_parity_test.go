package transformer

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/routatic/proxy/internal/core"
	"github.com/routatic/proxy/pkg/types"
)

// loadUsageFixture reads a recorded-shape Chat Completions response body.
func loadUsageFixture(t *testing.T, name string) (*types.ChatCompletionResponse, json.RawMessage) {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("testdata", "usage", name))
	if err != nil {
		t.Fatalf("read fixture %s: %v", name, err)
	}
	var resp types.ChatCompletionResponse
	if err := json.Unmarshal(raw, &resp); err != nil {
		t.Fatalf("unmarshal fixture %s: %v", name, err)
	}
	var generic struct {
		Usage json.RawMessage `json:"usage"`
	}
	if err := json.Unmarshal(raw, &generic); err != nil {
		t.Fatalf("unmarshal usage of %s: %v", name, err)
	}
	return &resp, generic.Usage
}

// usageViaTransformResponse runs the legacy non-streaming transformer.
func usageViaTransformResponse(t *testing.T, resp *types.ChatCompletionResponse) types.Usage {
	t.Helper()
	out, err := NewResponseTransformer().TransformResponse(resp, "m")
	if err != nil {
		t.Fatalf("TransformResponse: %v", err)
	}
	return out.Usage
}

// usageViaProviderBridge runs the non-streaming path every provider uses:
// OpenAIResponseToNormalized then DenormalizeResponse.
func usageViaProviderBridge(resp *types.ChatCompletionResponse) types.Usage {
	return core.DenormalizeResponse(OpenAIResponseToNormalized(resp, "m")).Usage
}

// usageViaStream runs the SSE path with the usage object as the final chunk.
func usageViaStream(t *testing.T, usage json.RawMessage) types.Usage {
	t.Helper()
	body := sseLines(
		`{"choices":[{"delta":{"content":"ok"}}]}`,
		`{"choices":[{"delta":{},"finish_reason":"stop"}]}`,
		`{"choices":[],"usage":`+string(usage)+`}`,
	)
	w := newMockResponseWriter()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := NewStreamHandler().ProxyStream(w, body, "m", ctx, 0, cancel); err != nil {
		t.Fatalf("ProxyStream: %v", err)
	}
	var got *types.Usage
	for _, ev := range parseSSEEvents(t, w.buf.String()) {
		if ev.Usage != nil {
			got = ev.Usage
		}
	}
	if got == nil {
		t.Fatalf("no usage event in stream:\n%s", w.buf.String())
		return types.Usage{}
	}
	return *got
}

// TestUsageParity_StreamingAndNonStreamingAgree is the Gap 3 / Gap 2 contract:
// for every provider usage shape, the streaming path and both non-streaming
// paths report the same Anthropic usage; input + cache_read + cache_creation
// equals prompt_tokens (Anthropic counts them as disjoint); nothing is negative.
func TestUsageParity_StreamingAndNonStreamingAgree(t *testing.T) {
	cases := []struct {
		fixture                         string
		wantInput, wantRead, wantCreate int
		wantSumIsPrompt                 bool
	}{
		// DeepSeek: miss is reported as cache creation (keeps the total).
		{"deepseek.json", 0, 800, 200, true},
		// Alibaba/OpenAI implicit cache: cached_tokens is included in prompt_tokens.
		{"alibaba_implicit.json", 400, 600, 0, true},
		// Alibaba explicit cache: hit + creation both inside prompt_tokens.
		{"alibaba_explicit.json", 200, 300, 500, true},
		// No cache fields at all: unchanged, everything is plain input.
		{"no_cache.json", 1000, 0, 0, true},
		// A details object carrying zeros is the same as no cache fields.
		{"details_without_cache.json", 1000, 0, 0, true},
		// DeepSeek fields win when non-zero, details are ignored.
		{"deepseek_wins.json", 0, 800, 200, true},
		// A DeepSeek hit with a zero miss still counts as DeepSeek usage.
		{"deepseek_hit_only_wins.json", 0, 1000, 0, true},
		// read + creation exceed prompt_tokens: input floors at 0, never negative.
		{"overcount.json", 0, 80, 50, false},
	}
	for _, tc := range cases {
		t.Run(tc.fixture, func(t *testing.T) {
			resp, rawUsage := loadUsageFixture(t, tc.fixture)

			legacy := usageViaTransformResponse(t, resp)
			bridge := usageViaProviderBridge(resp)
			stream := usageViaStream(t, rawUsage)

			if legacy != stream {
				t.Errorf("TransformResponse %+v != stream %+v", legacy, stream)
			}
			if bridge != stream {
				t.Errorf("provider bridge %+v != stream %+v", bridge, stream)
			}
			if stream.InputTokens != tc.wantInput ||
				stream.CacheReadInputTokens != tc.wantRead ||
				stream.CacheCreationInputTokens != tc.wantCreate {
				t.Errorf("stream usage = in %d read %d create %d, want in %d read %d create %d",
					stream.InputTokens, stream.CacheReadInputTokens, stream.CacheCreationInputTokens,
					tc.wantInput, tc.wantRead, tc.wantCreate)
			}
			if stream.InputTokens < 0 || stream.CacheReadInputTokens < 0 || stream.CacheCreationInputTokens < 0 {
				t.Errorf("negative usage: %+v", stream)
			}
			sum := stream.InputTokens + stream.CacheReadInputTokens + stream.CacheCreationInputTokens
			if tc.wantSumIsPrompt && sum != resp.Usage.PromptTokens {
				t.Errorf("input+read+creation = %d, want prompt_tokens %d", sum, resp.Usage.PromptTokens)
			}
			if bridge.OutputTokens != 20 && !strings.Contains(tc.fixture, "overcount") {
				t.Errorf("output tokens = %d, want 20", bridge.OutputTokens)
			}
		})
	}
}
