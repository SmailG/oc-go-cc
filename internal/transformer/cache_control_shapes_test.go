package transformer

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/routatic/proxy/internal/config"
	"github.com/routatic/proxy/pkg/types"
)

func userTurn(content string) *types.MessageRequest {
	return &types.MessageRequest{
		Model:     "claude-sonnet-5",
		MaxTokens: 10,
		Messages:  []types.Message{{Role: "user", Content: json.RawMessage(content)}},
	}
}

// wireMessageMarkers returns the number of cache_control fields in the
// serialised messages array.
func wireMessageMarkers(t *testing.T, decoded map[string]any) int {
	t.Helper()
	raw, err := json.Marshal(decoded["messages"])
	if err != nil {
		t.Fatalf("marshal messages: %v", err)
	}
	return strings.Count(string(raw), `"cache_control"`)
}

// partMarked reports whether a message's content array has a marker at index i.
func partMarked(t *testing.T, msg map[string]any, i int) bool {
	t.Helper()
	parts := contentParts(t, msg)
	return i < len(parts) && hasMarker(parts[i])
}

// On Model Studio the wire carries exactly the markers the client placed, each
// on the part or message its block became. A tool_result or image marker is
// not also lifted onto a text part the client never marked.
func TestModelStudioOneClientMarkerIsOneWireMarker(t *testing.T) {
	msModel := config.ModelConfig{ModelID: "qwen3.6-plus", Provider: config.ProviderModelStudio, Vision: true}
	const image = `{"type":"image","source":{"type":"base64","media_type":"image/png","data":"abc"},"cache_control":` + ephemeralJSON + `}`
	const toolResult = `{"type":"tool_result","tool_use_id":"toolu_1","content":"file body","cache_control":` + ephemeralJSON + `}`

	t.Run("tool_result marker then unmarked text", func(t *testing.T) {
		_, decoded := wireJSON(t, userTurn(`[`+toolResult+`,{"type":"text","text":"reminder"}]`), msModel)
		if n := wireMessageMarkers(t, decoded); n != 1 {
			t.Fatalf("markers on the wire = %d, want 1: %v", n, decoded["messages"])
		}
		msgs := wireMessages(t, decoded)
		if msgs[0]["role"] != "tool" || !partMarked(t, msgs[0], 0) {
			t.Errorf("tool message = %v, want the marker on its content part", msgs[0])
		}
		if s, ok := msgs[1]["content"].(string); !ok || s != "reminder" {
			t.Errorf("user message = %v, want the unmarked plain string", msgs[1])
		}
	})

	t.Run("unmarked text then tool_result marker", func(t *testing.T) {
		_, decoded := wireJSON(t, userTurn(`[{"type":"text","text":"hi"},`+toolResult+`]`), msModel)
		if n := wireMessageMarkers(t, decoded); n != 1 {
			t.Fatalf("markers on the wire = %d, want 1: %v", n, decoded["messages"])
		}
		msgs := wireMessages(t, decoded)
		if msgs[0]["role"] != "tool" || !partMarked(t, msgs[0], 0) {
			t.Errorf("tool message = %v, want the marker on its content part", msgs[0])
		}
		if s, ok := msgs[1]["content"].(string); !ok || s != "hi" {
			t.Errorf("user message = %v, want the unmarked plain string", msgs[1])
		}
	})

	t.Run("unmarked text then image marker", func(t *testing.T) {
		_, decoded := wireJSON(t, userTurn(`[{"type":"text","text":"look"},`+image+`]`), msModel)
		if n := wireMessageMarkers(t, decoded); n != 1 {
			t.Fatalf("markers on the wire = %d, want 1: %v", n, decoded["messages"])
		}
		msg := wireMessages(t, decoded)[0]
		if hasMarker(msg) || partMarked(t, msg, 0) || !partMarked(t, msg, 1) {
			t.Errorf("message = %v, want the marker on the image_url part only", msg)
		}
	})

	t.Run("a text marker and a tool_result marker stay two markers", func(t *testing.T) {
		_, decoded := wireJSON(t, userTurn(`[`+toolResult+`,{"type":"text","text":"note","cache_control":`+ephemeralJSON+`}]`), msModel)
		if n := wireMessageMarkers(t, decoded); n != 2 {
			t.Fatalf("markers on the wire = %d, want 2: %v", n, decoded["messages"])
		}
		msgs := wireMessages(t, decoded)
		if !partMarked(t, msgs[0], 0) || !partMarked(t, msgs[1], 0) {
			t.Errorf("messages = %v, want a marker on the tool part and on the text part", msgs)
		}
	})
}

// DeepSeek keeps lifting the first block's marker onto the user message,
// whichever block type carried it (also when Model Studio serves DeepSeek).
func TestDeepSeekLiftsToolResultMarkerOntoUserMessage(t *testing.T) {
	req := userTurn(`[
		{"type":"tool_result","tool_use_id":"toolu_1","content":"file body","cache_control":` + ephemeralJSON + `},
		{"type":"text","text":"reminder"}
	]`)
	for _, provider := range []string{config.ProviderOpenCodeGo, config.ProviderModelStudio} {
		t.Run("provider="+provider, func(t *testing.T) {
			raw, decoded := wireJSON(t, req, config.ModelConfig{ModelID: "deepseek-v4-pro", Provider: provider})
			msgs := wireMessages(t, decoded)
			if len(msgs) != 2 || !hasMarker(msgs[0]) || !hasMarker(msgs[1]) {
				t.Errorf("messages = %v, want a message-level marker on both the tool and the user message", msgs)
			}
			if n := strings.Count(raw, `"cache_control"`); n != 2 {
				t.Errorf("markers = %d, want 2 (byte-for-byte what DeepSeek always got): %s", n, raw)
			}
		})
	}
}

// System blocks collapse into one system message that carries the first marked
// block's marker, once. Documented in docs/adr/0002-client-owned-cache-directives.md.
func TestModelStudioSystemBlocksCollapseToOneMarker(t *testing.T) {
	req := &types.MessageRequest{
		Model:     "claude-sonnet-5",
		MaxTokens: 10,
		System: json.RawMessage(`[
			{"type":"text","text":"A","cache_control":` + ephemeralJSON + `},
			{"type":"text","text":"B"},
			{"type":"text","text":"C","cache_control":` + ephemeralJSON + `}
		]`),
		Messages: []types.Message{{Role: "user", Content: json.RawMessage(`"hi"`)}},
	}
	_, decoded := wireJSON(t, req, config.ModelConfig{ModelID: "qwen3.6-flash", Provider: config.ProviderModelStudio})
	msgs := wireMessages(t, decoded)
	if len(msgs) != 2 {
		t.Fatalf("messages = %d, want one system and one user: %v", len(msgs), msgs)
	}
	sys := contentParts(t, msgs[0])
	if len(sys) != 1 || sys[0]["text"] != "ABC" || !hasMarker(sys[0]) {
		t.Errorf("system content = %v, want one part with text ABC and the marker", sys)
	}
	if n := wireMessageMarkers(t, decoded); n != 1 {
		t.Errorf("markers on the wire = %d, want 1", n)
	}
}

// A message-level marker is not placed again when a part already carries one,
// wherever in the message that part sits.
func TestMoveCacheControlIntoContentSeesPartMarkerPastTheFirstPart(t *testing.T) {
	marker := &types.CacheControl{Type: "ephemeral"}
	content, _ := json.Marshal([]types.ChatContentPart{
		{Type: "text", Text: "a"},
		{Type: "image_url", ImageURL: &types.ImageURL{URL: "u"}, CacheControl: marker},
		{Type: "text", Text: "c"},
	})
	msgs := []types.ChatMessage{{Role: "user", Content: content, CacheControl: marker}}

	moveCacheControlIntoContent(msgs)

	parts := decodeParts(t, msgs[0])
	if parts[0].CacheControl != nil || parts[1].CacheControl == nil || parts[2].CacheControl != nil {
		t.Errorf("markers = %v / %v / %v, want only the image part marked", parts[0].CacheControl, parts[1].CacheControl, parts[2].CacheControl)
	}
}
