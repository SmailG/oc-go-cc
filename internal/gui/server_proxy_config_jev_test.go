package gui

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/routatic/proxy/internal/config"
)

// The loader requires a key before it will accept any config.
func jevTestServer(path string, cfg *config.Config) *Server {
	return &Server{atomicCfg: config.NewAtomicConfig(cfg, path)}
}

func postProxyConfig(s *Server, patch map[string]any) *httptest.ResponseRecorder {
	body, err := json.Marshal(patch)
	if err != nil {
		panic(err)
	}
	rec := httptest.NewRecorder()
	s.handleProxyConfig(rec, httptest.NewRequest(http.MethodPost, "/api/proxy/config", bytes.NewReader(body)))
	return rec
}

// A disabled jev_routing block is never validated, so the block the toggle just
// enabled can be one the loader rejects. The patch endpoint must refuse it before
// replacing a working config on disk.
func TestHandleProxyConfig_BadJevBlockDoesNotOverwriteTheFile(t *testing.T) {
	t.Setenv("ROUTATIC_PROXY_API_KEY", "test-key")
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	original := []byte(`{"host":"127.0.0.1","port":3456,"jev_routing":{"enabled":false,"api_key_env":"sk-pasted-key-not-a-var-name"}}`)
	if err := os.WriteFile(path, original, 0o600); err != nil {
		t.Fatal(err)
	}
	s := jevTestServer(path, &config.Config{Host: "127.0.0.1", Port: 3456})

	rec := postProxyConfig(s, map[string]any{"jev_routing": map[string]any{"enabled": true}})

	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400 for a jev_routing block the loader would reject", rec.Code)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, original) {
		t.Errorf("the config file was replaced before validation: %s", got)
	}
	if s.atomicCfg.Get().JevRouting.Enabled {
		t.Error("a rejected patch must not have enabled Jev in the live config")
	}
}

// The toggle must reach the running proxy without a restart, and the resulting
// block must be the one the loader would produce.
func TestHandleProxyConfig_EnableJevHotReloadsTheLiveConfig(t *testing.T) {
	t.Setenv("ROUTATIC_PROXY_API_KEY", "test-key")
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	if err := os.WriteFile(path, []byte(`{"host":"127.0.0.1","port":3456}`), 0o600); err != nil {
		t.Fatal(err)
	}
	s := jevTestServer(path, &config.Config{Host: "127.0.0.1", Port: 3456})

	rec := postProxyConfig(s, map[string]any{"jev_routing": map[string]any{"enabled": true}})

	if rec.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204: %s", rec.Code, rec.Body.String())
	}

	live := s.atomicCfg.Get().JevRouting
	if !live.Enabled {
		t.Error("the running proxy did not pick up the toggle without a restart")
	}
	if live.Mode != config.JevModeActive {
		t.Errorf("mode = %q, want %q from applyJevDefaults", live.Mode, config.JevModeActive)
	}
	if live.APIKeyEnv != "TYPESAFE_AI_API_KEY" {
		t.Errorf("api_key_env = %q, want the default", live.APIKeyEnv)
	}

	saved, err := config.LoadFromPath(path)
	if err != nil {
		t.Fatalf("the written config must load cleanly: %v", err)
	}
	if !saved.JevRouting.Enabled {
		t.Error("jev_routing.enabled was not persisted")
	}
}

// Disabling again must also apply live, and must not be blocked by whatever the
// enabled block contained.
func TestHandleProxyConfig_DisableJevClearsTheLiveConfig(t *testing.T) {
	t.Setenv("ROUTATIC_PROXY_API_KEY", "test-key")
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	enabled := []byte(`{"host":"127.0.0.1","port":3456,"jev_routing":{"enabled":true,"mode":"active","model":"jev-1.14.0","timeout_ms":800,"api_key_env":"TYPESAFE_AI_API_KEY","base_url":"https://api.typesafe.ai"}}`)
	if err := os.WriteFile(path, enabled, 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.LoadFromPath(path)
	if err != nil {
		t.Fatal(err)
	}
	s := jevTestServer(path, cfg)
	if !s.atomicCfg.Get().JevRouting.Enabled {
		t.Fatal("test setup: Jev should start enabled")
	}

	rec := postProxyConfig(s, map[string]any{"jev_routing": map[string]any{"enabled": false}})

	if rec.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204: %s", rec.Code, rec.Body.String())
	}
	if s.atomicCfg.Get().JevRouting.Enabled {
		t.Error("the running proxy did not pick up the disable without a restart")
	}
}

// The jev_routing check is not the only loader rule that matters: any field the
// form exposes can be invalid. Validation must run on the merged document before
// it replaces config.json, or a failed reload leaves an unbootable file behind.
func TestHandleProxyConfig_InvalidFieldDoesNotOverwriteTheFile(t *testing.T) {
	t.Setenv("ROUTATIC_PROXY_API_KEY", "test-key")
	t.Setenv("MISSING_MODEL_STUDIO_KEY", "")
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	original := []byte(`{"host":"127.0.0.1","port":3456,"model_studio":{"api_key":"literal-key"}}`)
	if err := os.WriteFile(path, original, 0o600); err != nil {
		t.Fatal(err)
	}
	s := jevTestServer(path, &config.Config{Host: "127.0.0.1", Port: 3456})

	// An unresolved reference is rejected by validateSingleAPIKey after the
	// loader interpolates, exactly as it would be on the next start.
	rec := postProxyConfig(s, map[string]any{
		"model_studio": map[string]any{"api_key": "${MISSING_MODEL_STUDIO_KEY}"},
	})

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400: %s", rec.Code, rec.Body.String())
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, original) {
		t.Errorf("the config file was replaced by a document the loader rejects: %s", got)
	}
}
