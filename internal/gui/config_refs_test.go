package gui

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/routatic/proxy/internal/config"
)

func TestRestoreEnvRefs(t *testing.T) {
	raw := map[string]any{
		"api_key": "${GLOBAL_KEY}",
		"model_studio": map[string]any{
			"api_key":  "${TOKEN_PLAN_KEY}",
			"api_keys": []any{"${KEY_ONE}", "${KEY_TWO}"},
			"base_url": "https://example.test/v1",
		},
	}
	merged := map[string]any{
		"api_key": "resolved-global",
		"model_studio": map[string]any{
			"api_key":  "resolved-token-plan",
			"api_keys": []any{"resolved-one", "resolved-two"},
			"base_url": "https://example.test/v1",
		},
	}

	// The client changed only the base URL.
	restoreEnvRefs(raw, merged, map[string]any{"model_studio": map[string]any{"base_url": "https://new.test/v1"}})

	if merged["api_key"] != "${GLOBAL_KEY}" {
		t.Errorf("global key = %v, want the reference back", merged["api_key"])
	}
	ms := merged["model_studio"].(map[string]any)
	if ms["api_key"] != "${TOKEN_PLAN_KEY}" {
		t.Errorf("model_studio.api_key = %v, want the reference back", ms["api_key"])
	}
	if keys := ms["api_keys"].([]any); keys[0] != "${KEY_ONE}" || keys[1] != "${KEY_TWO}" {
		t.Errorf("api_keys = %v, want the original references", keys)
	}

	// A field the client did send must keep the client's value.
	merged["model_studio"].(map[string]any)["api_key"] = "typed-by-user"
	restoreEnvRefs(raw, merged, map[string]any{"model_studio": map[string]any{"api_key": "typed-by-user"}})
	if got := merged["model_studio"].(map[string]any)["api_key"]; got != "typed-by-user" {
		t.Errorf("model_studio.api_key = %v, a patched field must not be restored", got)
	}
}

// The form is filled from the live (interpolated) config, so before this guard a
// save of any unrelated field wrote every resolved key into config.json.
func TestHandleProxyConfig_PreservesEnvReferences(t *testing.T) {
	t.Setenv("ROUTATIC_PROXY_API_KEY", "resolved-global")
	t.Setenv("BAILIAN_TOKEN_PLAN_API_KEY", "resolved-token-plan")

	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	original := `{
  "host": "127.0.0.1",
  "port": 3456,
  "api_key": "${ROUTATIC_PROXY_API_KEY}",
  "hot_reload": false,
  "model_studio": {
    "base_url": "https://token-plan.example/v1",
    "api_key": "${BAILIAN_TOKEN_PLAN_API_KEY}"
  }
}`
	if err := os.WriteFile(path, []byte(original), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.LoadFromPath(path)
	if err != nil {
		t.Fatal(err)
	}
	s := jevTestServer(path, cfg)

	rec := postProxyConfig(s, map[string]any{"hot_reload": true})
	if rec.Code != http.StatusNoContent {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
	}

	written, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(written), "resolved-global") || strings.Contains(string(written), "resolved-token-plan") {
		t.Fatalf("a resolved key reached the config file:\n%s", written)
	}
	if !strings.Contains(string(written), "${ROUTATIC_PROXY_API_KEY}") {
		t.Errorf("global key reference was lost:\n%s", written)
	}
	if !strings.Contains(string(written), "${BAILIAN_TOKEN_PLAN_API_KEY}") {
		t.Errorf("model_studio key reference was lost:\n%s", written)
	}
	var reread map[string]any
	if err := json.Unmarshal(written, &reread); err != nil {
		t.Fatal(err)
	}
	if reread["hot_reload"] != true {
		t.Errorf("hot_reload = %v, want the patched value", reread["hot_reload"])
	}
}

func TestHandleConfigEnv(t *testing.T) {
	t.Setenv("ROUTATIC_PROXY_API_KEY", "global-key")
	t.Setenv("BAILIAN_TOKEN_PLAN_API_KEY", "secret-value")
	t.Setenv("TYPESAFE_AI_API_KEY", "")
	t.Setenv("UNUSED_KEY", "x")

	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	body := `{
  "host": "127.0.0.1",
  "port": 3456,
  "model_studio": {
    "api_key": "${BAILIAN_TOKEN_PLAN_API_KEY}",
    "base_url": "https://token-plan.example/v1"
  },
  "jev_routing": {"enabled": false, "api_key_env": "TYPESAFE_AI_API_KEY"}
}`
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.LoadFromPath(path)
	if err != nil {
		t.Fatal(err)
	}
	s := jevTestServer(path, cfg)

	rec := httptest.NewRecorder()
	s.handleConfigEnv(rec, httptest.NewRequest(http.MethodGet, "/api/config/env", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), "secret-value") {
		t.Fatalf("the response leaked a key value: %s", rec.Body.String())
	}

	var got struct {
		Refs map[string][]string `json:"refs"`
		Env  map[string]bool     `json:"env"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if names := got.Refs["model_studio.api_key"]; len(names) != 1 || names[0] != "BAILIAN_TOKEN_PLAN_API_KEY" {
		t.Errorf("model_studio.api_key refs = %v", names)
	}
	if names := got.Refs["jev_routing.api_key_env"]; len(names) != 1 || names[0] != "TYPESAFE_AI_API_KEY" {
		t.Errorf("jev refs = %v", names)
	}
	if !got.Env["BAILIAN_TOKEN_PLAN_API_KEY"] {
		t.Error("a set variable must report set")
	}
	if got.Env["TYPESAFE_AI_API_KEY"] {
		t.Error("an empty variable must report unset")
	}
	if _, ok := got.Env["UNUSED_KEY"]; ok {
		t.Error("unreferenced variables must not be reported")
	}
}

func TestHandleConfigEnv_RejectsNonGet(t *testing.T) {
	s := &Server{}
	rec := httptest.NewRecorder()
	s.handleConfigEnv(rec, httptest.NewRequest(http.MethodPost, "/api/config/env", nil))
	if rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("status = %d, want 405", rec.Code)
	}
}
