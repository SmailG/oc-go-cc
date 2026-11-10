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

// The patch must leave every reference the client did not touch exactly as it
// was stored — at any depth, including array elements that are objects, which is
// how fallbacks and extra_body are shaped.
func TestMergeConfigPatch_PreservesReferencesAnywhere(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	original := `{
  "host": "127.0.0.1",
  "port": 3456,
  "api_key": "${GLOBAL_KEY}",
  "model_studio": {"api_key": "${TOKEN_PLAN_KEY}"},
  "fallbacks": {
    "default": [
      {"provider": "model-studio", "model_id": "qwen3.8-max", "api_key": "${NESTED_KEY}"}
    ]
  },
  "unknown_future_field": {"keep": "${KEEP_KEY}"}
}`
	if err := os.WriteFile(path, []byte(original), 0o600); err != nil {
		t.Fatal(err)
	}

	out, err := mergeConfigPatch(path, map[string]any{"hot_reload": true})
	if err != nil {
		t.Fatal(err)
	}
	for _, ref := range []string{"${GLOBAL_KEY}", "${TOKEN_PLAN_KEY}", "${NESTED_KEY}", "${KEEP_KEY}"} {
		if !strings.Contains(string(out), ref) {
			t.Errorf("%s was not preserved:\n%s", ref, out)
		}
	}
	var got map[string]any
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatal(err)
	}
	if got["hot_reload"] != true {
		t.Errorf("hot_reload = %v, want the patched value", got["hot_reload"])
	}
	if _, ok := got["unknown_future_field"]; !ok {
		t.Error("a key this build does not model must survive a save")
	}
}

// A field the client did send replaces the stored value, including a reference.
func TestMergeConfigPatch_PatchedReferenceIsReplaced(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	if err := os.WriteFile(path, []byte(`{"api_key":"${GLOBAL_KEY}"}`), 0o600); err != nil {
		t.Fatal(err)
	}

	out, err := mergeConfigPatch(path, map[string]any{"api_key": "typed-by-user"})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(out), "${GLOBAL_KEY}") {
		t.Errorf("a patched field must not be restored:\n%s", out)
	}
	if !strings.Contains(string(out), "typed-by-user") {
		t.Errorf("the patch was not applied:\n%s", out)
	}
}

// An env override changes the running config but must never be written to disk
// when an unrelated field is saved.
func TestMergeConfigPatch_DoesNotPersistEnvOverrides(t *testing.T) {
	t.Setenv("ROUTATIC_PROXY_API_KEY", "env-value")
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	if err := os.WriteFile(path, []byte(`{"api_key":"file-value"}`), 0o600); err != nil {
		t.Fatal(err)
	}

	out, err := mergeConfigPatch(path, map[string]any{"hot_reload": true})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(out), "env-value") {
		t.Errorf("an env override reached the config file:\n%s", out)
	}
	if !strings.Contains(string(out), "file-value") {
		t.Errorf("the literal file value was replaced:\n%s", out)
	}
}

// The form is filled from the live (interpolated) config, so before this guard a
// save of any unrelated field wrote every resolved key into config.json.
func TestHandleProxyConfig_PreservesEnvReferences(t *testing.T) {
	t.Setenv("ROUTATIC_PROXY_API_KEY", "resolved-global")
	t.Setenv("BAILIAN_TOKEN_PLAN_API_KEY", "resolved-token-plan")
	t.Setenv("NESTED_VENDOR_KEY", "resolved-nested")

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
  },
  "fallbacks": {
    "default": [
      {"provider": "opencode-go", "model_id": "kimi-k2.6", "extra_body": {"vendor_key": "${NESTED_VENDOR_KEY}"}}
    ]
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
	if strings.Contains(string(written), "resolved-global") ||
		strings.Contains(string(written), "resolved-token-plan") ||
		strings.Contains(string(written), "resolved-nested") {
		t.Fatalf("a resolved key reached the config file:\n%s", written)
	}
	if !strings.Contains(string(written), "${ROUTATIC_PROXY_API_KEY}") {
		t.Errorf("global key reference was lost:\n%s", written)
	}
	if !strings.Contains(string(written), "${BAILIAN_TOKEN_PLAN_API_KEY}") {
		t.Errorf("model_studio key reference was lost:\n%s", written)
	}
	// The array-of-objects depth is the one the old restore step missed.
	if !strings.Contains(string(written), "${NESTED_VENDOR_KEY}") {
		t.Errorf("a reference nested in fallbacks was resolved into the file:\n%s", written)
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
