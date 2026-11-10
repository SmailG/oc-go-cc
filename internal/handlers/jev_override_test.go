package handlers

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/routatic/proxy/internal/config"
	"github.com/routatic/proxy/internal/router"
	"github.com/routatic/proxy/pkg/types"
)

const jevOverrideTestKeyEnv = "JEV_OVERRIDE_TEST_KEY"

// jevOverrideStub answers every request with a valid Jev body after delay.
func jevOverrideStub(t *testing.T, delay time.Duration) (*httptest.Server, *atomic.Int32) {
	t.Helper()
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		time.Sleep(delay)
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprint(w, `{"model":"jev-1.14.0","answers":{
			"action":{"type":"score","score":2,"confidence":0.9,"probabilities":{"0":0.1,"1":0.2,"2":0.7}},
			"architecture":{"type":"noul","noul":0.9}},"usage":{"input_tokens":10,"output_tokens":4}}`)
	}))
	t.Cleanup(srv.Close)
	return srv, &hits
}

func jevOverrideHandler(t *testing.T, baseURL, mode string) *MessagesHandler {
	t.Helper()
	t.Setenv(jevOverrideTestKeyEnv, "test-key-not-real")
	respect := false // default is true, which would bypass scenario routing for every request
	return newTestMessagesHandler(t, &config.Config{
		RespectRequestedModel: &respect,
		Models: map[string]config.ModelConfig{
			"default": {Provider: "opencode-go", ModelID: "kimi-k2.6"},
			"fast":    {Provider: "opencode-go", ModelID: "fast-m"},
			"complex": {Provider: "opencode-go", ModelID: "complex-m"},
		},
		Fallbacks: map[string][]config.ModelConfig{
			"default": {{Provider: "opencode-go", ModelID: "mimo-v2.5-pro"}},
			"fast":    {{Provider: "opencode-go", ModelID: "mimo-v2.5-pro"}},
		},
		ModelOverrides:       map[string]config.ModelConfig{"exact-model": {Provider: "opencode-go", ModelID: "ov-exact"}},
		ModelFamilyOverrides: map[string]config.ModelConfig{"opus": {Provider: "opencode-go", ModelID: "ov-family"}},
		JevRouting: config.JevRoutingConfig{
			Enabled: true, Mode: mode, Model: "jev-1.14.0", TimeoutMs: 2000,
			APIKeyEnv: jevOverrideTestKeyEnv, BaseURL: baseURL,
		},
	})
}

func jevOverrideMessages(text string) []router.MessageContent {
	return []router.MessageContent{{Role: "user", Content: text, PromptText: text}}
}

// Review finding 1: a request matched by a model override must never reach Jev:
// the override already chose the primary, so no text may leave the process and an
// active-mode request must not wait on Jev for a chain that only orders fallbacks.
func TestBuildModelChain_OverrideNeverReachesJev(t *testing.T) {
	cases := []struct{ name, model string }{
		{"exact override", "exact-model"},
		{"family override", "claude-opus-4-20250514"},
	}
	for _, mode := range []string{config.JevModeShadow, config.JevModeActive} {
		for _, streaming := range []bool{false, true} {
			for _, tc := range cases {
				t.Run(fmt.Sprintf("%s/%s/streaming=%v", mode, tc.name, streaming), func(t *testing.T) {
					srv, hits := jevOverrideStub(t, 250*time.Millisecond)
					h := jevOverrideHandler(t, srv.URL, mode)
					start := time.Now()
					chain, result, err := h.buildModelChain(tc.model, jevOverrideMessages("Hello there"), 100, streaming, 4096, false, 0)
					elapsed := time.Since(start)
					if err != nil {
						t.Fatalf("buildModelChain: %v", err)
					}
					if result.Scenario != router.ScenarioOverride || len(chain) == 0 {
						t.Fatalf("expected an override route, got scenario %s chain %v", result.Scenario, chainIDs(chain))
					}
					if elapsed > 150*time.Millisecond {
						t.Errorf("building the override chain took %v: it waited on Jev", elapsed)
					}
					time.Sleep(400 * time.Millisecond) // let a (wrongly) started shadow goroutine reach the stub
					if n := hits.Load(); n != 0 {
						t.Errorf("an override-routed request reached Jev %d times", n)
					}
				})
			}
		}
	}
}

// Control for the test above: with no override the same handler does reach Jev, so
// the zeroes above cannot come from a stub that is never called.
func TestBuildModelChain_NoOverrideStillReachesJev(t *testing.T) {
	for _, streaming := range []bool{false, true} {
		t.Run(fmt.Sprintf("streaming=%v", streaming), func(t *testing.T) {
			srv, hits := jevOverrideStub(t, 0)
			h := jevOverrideHandler(t, srv.URL, config.JevModeShadow)
			if _, _, err := h.buildModelChain("some-other-model", jevOverrideMessages("Hello there"), 100, streaming, 4096, false, 0); err != nil {
				t.Fatalf("buildModelChain: %v", err)
			}
			deadline := time.Now().Add(3 * time.Second)
			for hits.Load() != 1 && time.Now().Before(deadline) {
				time.Sleep(10 * time.Millisecond)
			}
			if n := hits.Load(); n != 1 {
				t.Errorf("Jev hits = %d, want exactly 1", n)
			}
		})
	}
}

// Review survivor X2: only blocks of type "text" feed the classifier, whatever
// other block types carry in a "text" field.
func TestExtractPromptText_IgnoresNonTextBlocksWithATextField(t *testing.T) {
	raw := `[
		{"type":"document","text":"LEAK-DOCUMENT"},
		{"type":"tool_result","tool_use_id":"t1","text":"LEAK-TOOL-RESULT","content":"LEAK-CONTENT"},
		{"type":"thinking","thinking":"x","text":"LEAK-THINKING"},
		{"type":"text","text":"real prompt"}
	]`
	var blocks []types.ContentBlock
	if err := json.Unmarshal([]byte(raw), &blocks); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got := extractPromptText(blocks); got != "real prompt" {
		t.Errorf("extractPromptText = %q, want only the text block", got)
	}
}

// Review finding 3: the harness's own record of a tool call is not authored text.
func TestIsHarnessToolEcho(t *testing.T) {
	cases := []struct {
		text string
		want bool
	}{
		{"Called the Read tool with the following input: {\"file_path\":\"/Users/x/secret.go\"}", true},
		{"  \nCalled the Bash tool with the following input: {}", true},
		{"Result of calling the Read tool:\n1\tpackage main", true},
		{"Called the office and left a message", false},
		{"Please explain the Result of calling the function", false},
		{"fix the parser", false},
		{"", false},
	}
	for _, tc := range cases {
		if got := isHarnessToolEcho(tc.text); got != tc.want {
			t.Errorf("isHarnessToolEcho(%q) = %v, want %v", tc.text, got, tc.want)
		}
	}
	blocks := []types.ContentBlock{
		{Type: "text", Text: "Called the Read tool with the following input: {\"file_path\":\"/p\"}"},
		{Type: "text", Text: "Result of calling the Read tool: FILE BODY"},
		{Type: "text", Text: "fix the parser"},
	}
	if got := extractPromptText(blocks); got != "fix the parser" {
		t.Errorf("extractPromptText = %q, want only the authored block", got)
	}
}
