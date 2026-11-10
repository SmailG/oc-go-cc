package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// loadJevConfig writes body as a config file (with a valid api_key) and loads it.
func loadJevConfig(t *testing.T, jevBlock string) (*Config, error) {
	t.Helper()
	t.Setenv("ROUTATIC_PROXY_API_KEY", "")
	t.Setenv("OC_GO_CC_API_KEY", "")
	body := `{"api_key": "test-key-123"`
	if jevBlock != "" {
		body += `, "jev_routing": ` + jevBlock
	}
	body += `}`
	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	return LoadFromPath(path)
}

func TestJevRouting_DefaultsWhenBlockAbsent(t *testing.T) {
	cfg, err := loadJevConfig(t, "")
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	j := cfg.JevRouting
	if j.Enabled {
		t.Error("jev_routing must be disabled by default")
	}
	if j.Mode != JevModeActive {
		t.Errorf("Mode = %q, want %q", j.Mode, JevModeActive)
	}
	if j.Model != "jev-1.14.0" {
		t.Errorf("Model = %q, want jev-1.14.0", j.Model)
	}
	if j.TimeoutMs != 800 {
		t.Errorf("TimeoutMs = %d, want 800", j.TimeoutMs)
	}
	if j.APIKeyEnv != "TYPESAFE_AI_API_KEY" {
		t.Errorf("APIKeyEnv = %q, want TYPESAFE_AI_API_KEY", j.APIKeyEnv)
	}
	if j.BaseURL != "https://api.typesafe.ai" {
		t.Errorf("BaseURL = %q, want https://api.typesafe.ai", j.BaseURL)
	}
}

func TestJevRouting_ExplicitValuesKept(t *testing.T) {
	cfg, err := loadJevConfig(t, `{"enabled": true, "mode": "active", "model": "jev-latest",
		"timeout_ms": 150, "api_key_env": "MY_JEV_KEY", "base_url": "http://127.0.0.1:9999"}`)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	j := cfg.JevRouting
	if !j.Enabled || j.Mode != JevModeActive || j.Model != "jev-latest" ||
		j.TimeoutMs != 150 || j.APIKeyEnv != "MY_JEV_KEY" || j.BaseURL != "http://127.0.0.1:9999" {
		t.Errorf("explicit values were not kept: %+v", j)
	}
}

func TestJevRouting_Validation(t *testing.T) {
	// A pasted secret must never be echoed back in the error text.
	const pastedSecret = "sk-canary-0123456789"
	cases := []struct {
		name    string
		block   string
		wantErr string
	}{
		{"bad mode", `{"enabled": true, "mode": "live"}`, "jev_routing.mode"},
		{"negative timeout", `{"enabled": true, "timeout_ms": -5}`, "jev_routing.timeout_ms"},
		{"timeout above cap", `{"enabled": true, "timeout_ms": 10001}`, "jev_routing.timeout_ms"},
		{"relative base url", `{"enabled": true, "base_url": "api.typesafe.ai"}`, "jev_routing.base_url"},
		{"ftp base url", `{"enabled": true, "base_url": "ftp://api.typesafe.ai"}`, "jev_routing.base_url"},
		{"key pasted as env name", `{"enabled": true, "api_key_env": "` + pastedSecret + `"}`, "jev_routing.api_key_env"},
		{"unresolved placeholder as env name", `{"enabled": true, "api_key_env": "${NOT_SET_ANYWHERE_X}"}`, "jev_routing.api_key_env"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := loadJevConfig(t, tc.block)
			if err == nil {
				t.Fatalf("expected error containing %q, got nil", tc.wantErr)
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("error %q does not name %q", err, tc.wantErr)
			}
			if strings.Contains(err.Error(), pastedSecret) {
				t.Errorf("error echoes the api_key_env value: %v", err)
			}
		})
	}
}

func TestJevRouting_DisabledBlockIsNotValidated(t *testing.T) {
	if _, err := loadJevConfig(t, `{"enabled": false, "mode": "live", "timeout_ms": -1}`); err != nil {
		t.Errorf("a disabled jev_routing block must not fail the load, got %v", err)
	}
}

// With the key variable named by api_key_env unset, the load still succeeds:
// the classifier is fail-open, so a missing key degrades routing, not startup.
func TestJevRouting_MissingKeyEnvDoesNotFailLoad(t *testing.T) {
	t.Setenv("TYPESAFE_AI_API_KEY", "")
	if _, err := loadJevConfig(t, `{"enabled": true}`); err != nil {
		t.Errorf("missing key env must not fail the load, got %v", err)
	}
}
