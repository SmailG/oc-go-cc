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

// Review finding 9: api_key_env is the NAME of an environment variable, not a secret.
func TestShouldAnonymize_KeepsApiKeyEnvRedactsSecrets(t *testing.T) {
	if shouldAnonymize("api_key_env") {
		t.Error("api_key_env must survive the anonymized export")
	}
	for _, k := range []string{"api_key", "api_keys", "token", "client_secret", "password", "credential"} {
		if !shouldAnonymize(k) {
			t.Errorf("%q must still be redacted", k)
		}
	}
}

// Review finding 9: an import must be validated before the working config on disk is replaced.
func TestHandleConfigImport_BadJevBlockDoesNotOverwriteTheFile(t *testing.T) {
	// The loader requires a key; this test targets jev_routing specifically.
	t.Setenv("ROUTATIC_PROXY_API_KEY", "test-key")
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	original := []byte(`{"host":"127.0.0.1","port":3456}`)
	if err := os.WriteFile(path, original, 0o600); err != nil {
		t.Fatal(err)
	}
	s := &Server{atomicCfg: config.NewAtomicConfig(&config.Config{Host: "127.0.0.1", Port: 3456}, path)}

	bad := map[string]any{"config": map[string]any{
		"host": "127.0.0.1", "port": 3456,
		"jev_routing": map[string]any{"enabled": true, "api_key_env": "***REDACTED***"},
	}, "apply": true}
	body, _ := json.Marshal(bad)
	rec := httptest.NewRecorder()
	s.handleConfigImport(rec, httptest.NewRequest(http.MethodPost, "/api/config/import", bytes.NewReader(body)))

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
}

// An import must run the full loader over the exact document it is about to
// persist, not just the host/port and jev checks: an unresolved key from an
// export→import on a machine missing that variable used to be written first and
// fail on Reload, leaving config.json unbootable.
func TestHandleConfigImport_InvalidFieldDoesNotOverwriteTheFile(t *testing.T) {
	t.Setenv("ROUTATIC_PROXY_API_KEY", "test-key")
	t.Setenv("MISSING_IMPORT_KEY", "")
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	original := []byte(`{"host":"127.0.0.1","port":3456,"model_studio":{"api_key":"literal-key"}}`)
	if err := os.WriteFile(path, original, 0o600); err != nil {
		t.Fatal(err)
	}
	s := &Server{atomicCfg: config.NewAtomicConfig(&config.Config{Host: "127.0.0.1", Port: 3456}, path)}

	body, err := json.Marshal(map[string]any{
		"apply": true,
		"config": map[string]any{
			"host": "127.0.0.1", "port": 3456,
			"model_studio": map[string]any{"api_key": "${MISSING_IMPORT_KEY}"},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	rec := httptest.NewRecorder()
	s.handleConfigImport(rec, httptest.NewRequest(http.MethodPost, "/api/config/import", bytes.NewReader(body)))

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 for a document the loader rejects: %s", rec.Code, rec.Body.String())
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, original) {
		t.Errorf("the config file was replaced by a document the loader rejects: %s", got)
	}
}
