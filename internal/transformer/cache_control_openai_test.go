package transformer

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/routatic/proxy/internal/config"
	"github.com/routatic/proxy/internal/core"
	"github.com/routatic/proxy/pkg/types"
)

const ephemeralJSON = `{"type":"ephemeral"}`

// cacheRequest is a Claude Code style request with a marker on the system
// block, on a user text block, on an assistant text block, on a tool_result
// and on a tool definition.
func cacheRequest() *types.MessageRequest {
	return &types.MessageRequest{
		Model:     "claude-sonnet-5",
		MaxTokens: 100,
		System:    json.RawMessage(`[{"type":"text","text":"You are helpful.","cache_control":` + ephemeralJSON + `}]`),
		Messages: []types.Message{
			{Role: "user", Content: json.RawMessage(`[{"type":"text","text":"first question","cache_control":` + ephemeralJSON + `}]`)},
			{Role: "assistant", Content: json.RawMessage(`[
				{"type":"text","text":"calling a tool","cache_control":` + ephemeralJSON + `},
				{"type":"tool_use","id":"toolu_1","name":"read","input":{"path":"a"}}
			]`)},
			{Role: "user", Content: json.RawMessage(`[
				{"type":"tool_result","tool_use_id":"toolu_1","content":"file body","cache_control":` + ephemeralJSON + `}
			]`)},
			{Role: "assistant", Content: json.RawMessage(`[{"type":"tool_use","id":"toolu_2","name":"read","input":{}, "cache_control":` + ephemeralJSON + `}]`)},
			{Role: "user", Content: json.RawMessage(`[
				{"type":"text","text":"last part one "},
				{"type":"text","text":"last part two","cache_control":` + ephemeralJSON + `}
			]`)},
		},
		Tools: []types.Tool{{
			Name:         "read",
			InputSchema:  json.RawMessage(`{"type":"object","properties":{}}`),
			CacheControl: &types.CacheControl{Type: "ephemeral"},
		}},
	}
}

// wireJSON marshals the request exactly as it goes upstream and decodes it
// generically, so assertions are on the serialised shape.
func wireJSON(t *testing.T, req *types.MessageRequest, model config.ModelConfig) (string, map[string]any) {
	t.Helper()
	out, err := NewRequestTransformer().TransformRequest(req, model)
	if err != nil {
		t.Fatalf("TransformRequest: %v", err)
	}
	raw, err := json.Marshal(out)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var decoded map[string]any
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	return string(raw), decoded
}

func wireMessages(t *testing.T, decoded map[string]any) []map[string]any {
	t.Helper()
	list, _ := decoded["messages"].([]any)
	msgs := make([]map[string]any, len(list))
	for i, m := range list {
		msgs[i], _ = m.(map[string]any)
	}
	return msgs
}

// contentParts returns the content array of a message, failing if content is a string.
func contentParts(t *testing.T, msg map[string]any) []map[string]any {
	t.Helper()
	list, ok := msg["content"].([]any)
	if !ok {
		t.Fatalf("message content is %T (%v), want a content-block array", msg["content"], msg["content"])
	}
	parts := make([]map[string]any, len(list))
	for i, p := range list {
		parts[i], _ = p.(map[string]any)
	}
	return parts
}

func hasMarker(m map[string]any) bool {
	cc, ok := m["cache_control"].(map[string]any)
	return ok && cc["type"] == "ephemeral"
}

func TestModelStudioKeepsCacheControlInsideContentBlocks(t *testing.T) {
	// "model_studio" (underscore) is a valid spelling of the provider.
	for _, provider := range []string{config.ProviderModelStudio, "model_studio"} {
		t.Run(provider, func(t *testing.T) {
			_, decoded := wireJSON(t, cacheRequest(), config.ModelConfig{ModelID: "qwen3.6-flash", Provider: provider})
			msgs := wireMessages(t, decoded)
			if len(msgs) != 6 {
				t.Fatalf("got %d messages, want 6: %v", len(msgs), msgs)
			}

			// No message carries a top-level cache_control; Alibaba reads it from content blocks.
			for i, m := range msgs {
				if _, present := m["cache_control"]; present {
					t.Errorf("message %d has a top-level cache_control: %v", i, m)
				}
			}

			// system
			sys := contentParts(t, msgs[0])
			if len(sys) != 1 || sys[0]["type"] != "text" || sys[0]["text"] != "You are helpful." || !hasMarker(sys[0]) {
				t.Errorf("system content = %v, want one text block with the marker", sys)
			}
			// user, first turn
			first := contentParts(t, msgs[1])
			if len(first) != 1 || first[0]["text"] != "first question" || !hasMarker(first[0]) {
				t.Errorf("first user content = %v, want text block with the marker", first)
			}
			// assistant with text and a tool call: marker on the text part
			asst := contentParts(t, msgs[2])
			if len(asst) != 1 || asst[0]["text"] != "calling a tool" || !hasMarker(asst[0]) {
				t.Errorf("assistant content = %v, want text block with the marker", asst)
			}
			if _, ok := msgs[2]["tool_calls"]; !ok {
				t.Errorf("assistant lost its tool_calls: %v", msgs[2])
			}
			// tool result: role "tool" carries the marker in a content part (the moving breakpoint of an agent loop)
			if msgs[3]["role"] != "tool" {
				t.Fatalf("message 3 role = %v, want tool", msgs[3]["role"])
			}
			toolParts := contentParts(t, msgs[3])
			if len(toolParts) != 1 || toolParts[0]["text"] != "file body" || !hasMarker(toolParts[0]) {
				t.Errorf("tool message content = %v, want text block with the marker", toolParts)
			}
			// structural drop: an assistant turn that is only tool_calls has no content part to carry the marker, and none is invented
			if s, ok := msgs[4]["content"].(string); !ok || s != "" {
				t.Errorf("tool-only assistant content = %v, want the empty string", msgs[4]["content"])
			}
			// last user message: the marker lands on the LAST text part only
			last := contentParts(t, msgs[5])
			if len(last) != 1 || last[0]["text"] != "last part one last part two" || !hasMarker(last[0]) {
				t.Errorf("last user content = %v, want one joined text block with the marker", last)
			}

			// tool definitions keep the marker, serialised as for DeepSeek
			tools, _ := decoded["tools"].([]any)
			if len(tools) != 1 {
				t.Fatalf("got %d tools, want 1", len(tools))
			}
			if tool, _ := tools[0].(map[string]any); !hasMarker(tool) {
				t.Errorf("tool definition lost its cache_control: %v", tool)
			}
		})
	}
}

// Part-level markers are kept where the client put them, on any part type.
func TestModelStudioKeepsMarkersOnNonTextParts(t *testing.T) {
	req := &types.MessageRequest{
		Model:     "claude-sonnet-5",
		MaxTokens: 10,
		Messages: []types.Message{{Role: "user", Content: json.RawMessage(`[
			{"type":"text","text":"look"},
			{"type":"image","source":{"type":"base64","media_type":"image/png","data":"abc"},"cache_control":` + ephemeralJSON + `}
		]`)}},
	}
	raw, decoded := wireJSON(t, req, config.ModelConfig{ModelID: "qwen3.6-plus", Provider: config.ProviderModelStudio, Vision: true})
	msg := wireMessages(t, decoded)[0]
	if _, present := msg["cache_control"]; present {
		t.Errorf("top-level cache_control on the message: %s", raw)
	}
	parts := contentParts(t, msg)
	if len(parts) != 2 || parts[0]["type"] != "text" || parts[1]["type"] != "image_url" {
		t.Fatalf("parts = %v, want text then image_url", parts)
	}
	if !hasMarker(parts[1]) {
		t.Errorf("image_url part lost the client's marker: %s", raw)
	}
}

func TestOtherProvidersStillStripEveryCacheControl(t *testing.T) {
	for _, provider := range []string{"", config.ProviderOpenCodeGo, config.ProviderOpenCodeZen, config.ProviderAWSBedrock, config.ProviderOpenRouter} {
		t.Run("provider="+provider, func(t *testing.T) {
			raw, decoded := wireJSON(t, cacheRequest(), config.ModelConfig{ModelID: "glm-5", Provider: provider})
			if strings.Contains(raw, "cache_control") {
				t.Fatalf("cache_control leaked to a non-DeepSeek, non-Model-Studio provider: %s", raw)
			}
			// Content stays in its pre-existing shape: plain strings.
			if s, ok := wireMessages(t, decoded)[0]["content"].(string); !ok || s != "You are helpful." {
				t.Errorf("system content = %v, want the plain string", wireMessages(t, decoded)[0]["content"])
			}
		})
	}
}

func TestDeepSeekCacheControlUnchanged(t *testing.T) {
	// Both on its own provider and when served by Model Studio: DeepSeek model
	// IDs keep the message-level form (and tool markers) they always had.
	for _, provider := range []string{config.ProviderOpenCodeGo, config.ProviderModelStudio} {
		t.Run("provider="+provider, func(t *testing.T) {
			_, decoded := wireJSON(t, cacheRequest(), config.ModelConfig{ModelID: "deepseek-v4-pro", Provider: provider})
			msgs := wireMessages(t, decoded)
			if !hasMarker(msgs[0]) {
				t.Errorf("system message lost its message-level marker: %v", msgs[0])
			}
			if s, ok := msgs[0]["content"].(string); !ok || s != "You are helpful." {
				t.Errorf("system content = %v, want the plain string", msgs[0]["content"])
			}
			if !hasMarker(msgs[1]) {
				t.Errorf("user message lost its message-level marker: %v", msgs[1])
			}
			tools, _ := decoded["tools"].([]any)
			if tool, _ := tools[0].(map[string]any); !hasMarker(tool) {
				t.Errorf("DeepSeek tool definition lost its marker: %v", tool)
			}
		})
	}
}

// The providers reach TransformRequest through the normalized form; the marker
// must survive that round trip.
func TestModelStudioMarkerSurvivesNormalizedPath(t *testing.T) {
	nreq := core.NormalizeRequest(cacheRequest())
	out := TransformRequestFromNormalized(nreq, config.ModelConfig{ModelID: "qwen3.6-flash", Provider: config.ProviderModelStudio})
	raw, err := json.Marshal(out)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	want := `"content":[{"type":"text","text":"You are helpful.","cache_control":` + ephemeralJSON + `}]`
	if !strings.Contains(string(raw), want) {
		t.Fatalf("system block marker missing from normalized-path body:\n%s", raw)
	}
}

func TestModelStudioLeavesUnmarkedMessagesAlone(t *testing.T) {
	req := &types.MessageRequest{
		Model:     "claude-sonnet-5",
		MaxTokens: 10,
		System:    json.RawMessage(`"plain system"`),
		Messages: []types.Message{
			{Role: "user", Content: json.RawMessage(`"plain user"`)},
			{Role: "assistant", Content: json.RawMessage(`[{"type":"text","text":"plain assistant"}]`)},
		},
	}
	raw, decoded := wireJSON(t, req, config.ModelConfig{ModelID: "qwen3.6-flash", Provider: config.ProviderModelStudio})
	for i, m := range wireMessages(t, decoded) {
		if _, ok := m["content"].(string); !ok {
			t.Errorf("message %d content = %v, want the unchanged plain string", i, m["content"])
		}
	}
	if strings.Contains(raw, "cache_control") {
		t.Errorf("a cache_control appeared from nowhere: %s", raw)
	}
}

func decodeParts(t *testing.T, msg types.ChatMessage) []types.ChatContentPart {
	t.Helper()
	var parts []types.ChatContentPart
	if err := json.Unmarshal(msg.Content, &parts); err != nil {
		t.Fatalf("content is not an array: %v", err)
	}
	return parts
}

// A message-level marker lands on the LAST part of any type; the request
// transformer produces such a message only with string content, so the array
// case is pinned directly.
func TestMoveCacheControlIntoContentPlacesMessageMarkerOnLastPart(t *testing.T) {
	marker := &types.CacheControl{Type: "ephemeral"}
	content, _ := json.Marshal([]types.ChatContentPart{
		{Type: "text", Text: "first"},
		{Type: "text", Text: "second"},
		{Type: "image_url", ImageURL: &types.ImageURL{URL: "data:image/png;base64,abc"}},
	})
	msgs := []types.ChatMessage{{Role: "user", Content: content, CacheControl: marker}}

	moveCacheControlIntoContent(msgs)

	if msgs[0].CacheControl != nil {
		t.Errorf("message-level marker was left on the message")
	}
	for i, p := range decodeParts(t, msgs[0]) {
		if wantMarker := i == 2; (p.CacheControl != nil) != wantMarker {
			t.Errorf("part %d (%s) marker = %v, want marker=%v", i, p.Type, p.CacheControl, wantMarker)
		}
	}
}

// When the client already put a marker on a part, that part keeps it and the
// copied message-level marker is not placed a second time.
func TestMoveCacheControlIntoContentKeepsPartMarkersWhereTheyAre(t *testing.T) {
	marker := &types.CacheControl{Type: "ephemeral"}
	content, _ := json.Marshal([]types.ChatContentPart{
		{Type: "text", Text: "first", CacheControl: marker},
		{Type: "image_url", ImageURL: &types.ImageURL{URL: "data:image/png;base64,abc"}},
	})
	msgs := []types.ChatMessage{{Role: "user", Content: content, CacheControl: marker}}

	moveCacheControlIntoContent(msgs)

	parts := decodeParts(t, msgs[0])
	if parts[0].CacheControl == nil || parts[1].CacheControl != nil {
		t.Errorf("markers = %v / %v, want text part marked and image part not", parts[0].CacheControl, parts[1].CacheControl)
	}
}

// Without a message-level marker, part markers pass through untouched.
func TestMoveCacheControlIntoContentLeavesPartOnlyMarkersAlone(t *testing.T) {
	marker := &types.CacheControl{Type: "ephemeral"}
	content, _ := json.Marshal([]types.ChatContentPart{
		{Type: "text", Text: "a"},
		{Type: "image_url", ImageURL: &types.ImageURL{URL: "u"}, CacheControl: marker},
	})
	msgs := []types.ChatMessage{{Role: "user", Content: content}}

	moveCacheControlIntoContent(msgs)

	if string(msgs[0].Content) != string(content) {
		t.Errorf("content changed:\n got %s\nwant %s", msgs[0].Content, content)
	}
}

// Multimodal messages are the only ones whose markers live on content parts.
func TestOtherProvidersStripMarkersFromMultimodalParts(t *testing.T) {
	req := &types.MessageRequest{
		Model:     "claude-sonnet-5",
		MaxTokens: 10,
		Messages: []types.Message{{Role: "user", Content: json.RawMessage(`[
			{"type":"text","text":"look","cache_control":` + ephemeralJSON + `},
			{"type":"image","source":{"type":"base64","media_type":"image/png","data":"abc"},"cache_control":` + ephemeralJSON + `}
		]`)}},
	}
	raw, decoded := wireJSON(t, req, config.ModelConfig{ModelID: "glm-5", Provider: config.ProviderOpenCodeGo, Vision: true})
	if parts := contentParts(t, wireMessages(t, decoded)[0]); len(parts) != 2 {
		t.Fatalf("parts = %v, want text and image_url", parts)
	}
	if strings.Contains(raw, "cache_control") {
		t.Fatalf("cache_control leaked on a multimodal part: %s", raw)
	}
}

// Structural drops: a marker with no content part to carry it is removed
// without inventing a part (and without indexing an empty array).
func TestMoveCacheControlIntoContentDropsMarkerWithNoPartToCarryIt(t *testing.T) {
	marker := &types.CacheControl{Type: "ephemeral"}
	for name, content := range map[string]string{
		"empty string": `""`,
		"empty array":  `[]`,
		"null":         `null`,
		"absent":       ``,
	} {
		t.Run(name, func(t *testing.T) {
			msgs := []types.ChatMessage{{Role: "assistant", Content: json.RawMessage(content), CacheControl: marker}}
			moveCacheControlIntoContent(msgs)
			if msgs[0].CacheControl != nil {
				t.Errorf("message-level marker survived")
			}
			if string(msgs[0].Content) != content {
				t.Errorf("content = %s, want it untouched (%s)", msgs[0].Content, content)
			}
		})
	}
}
