package gui

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"regexp"
	"strings"

	"github.com/routatic/proxy/internal/config"
)

var sensitiveFieldPatterns = []*regexp.Regexp{
	regexp.MustCompile(`(?i)api[_-]?key`),
	regexp.MustCompile(`(?i)token`),
	regexp.MustCompile(`(?i)secret`),
	regexp.MustCompile(`(?i)password`),
	regexp.MustCompile(`(?i)credential`),
}

// anonymizeConfig returns a copy of cfg with secret-looking fields redacted.
func anonymizeConfig(cfg *config.Config) *config.Config {
	data, err := json.Marshal(cfg)
	if err != nil {
		return cfg
	}

	var raw map[string]interface{}
	if err := json.Unmarshal(data, &raw); err != nil {
		return cfg
	}

	anonymizeMap(raw)

	result := &config.Config{}
	data, _ = json.Marshal(raw)
	_ = json.Unmarshal(data, result)
	return result
}

// anonymizeMap redacts sensitive keys in m, descending into nested objects.
func anonymizeMap(m map[string]interface{}) {
	for key, value := range m {
		if shouldAnonymize(key) {
			switch v := value.(type) {
			case string:
				if v != "" {
					m[key] = "***REDACTED***"
				}
			case []interface{}:
				m[key] = []string{"***REDACTED***"}
			}
		} else if nested, ok := value.(map[string]interface{}); ok {
			anonymizeMap(nested)
		}
	}
}

// shouldAnonymize reports whether a config key holds a secret value.
func shouldAnonymize(key string) bool {
	lowerKey := strings.ToLower(key)
	// api_key_env is the NAME of an environment variable, not a secret; redacting
	// it would make an exported config unloadable once imported.
	if lowerKey == "api_key_env" {
		return false
	}
	for _, pattern := range sensitiveFieldPatterns {
		if pattern.MatchString(lowerKey) {
			return true
		}
	}
	return false
}

// handleConfigExport streams the current config as a JSON download.
func (s *Server) handleConfigExport(w http.ResponseWriter, r *http.Request) {
	if s.atomicCfg == nil {
		http.Error(w, "config not available", http.StatusServiceUnavailable)
		return
	}

	cfg := s.atomicCfg.Get()

	anonymize := r.URL.Query().Get("anonymize") == "true"
	if anonymize {
		cfg = anonymizeConfig(cfg)
	}

	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Content-Disposition", "attachment; filename=routatic-proxy-config.json")

	encoder := json.NewEncoder(w)
	encoder.SetIndent("", "  ")
	_ = encoder.Encode(cfg)
}

// handleConfigImport validates an uploaded config and applies it when requested.
func (s *Server) handleConfigImport(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	if s.atomicCfg == nil {
		http.Error(w, "config not available", http.StatusServiceUnavailable)
		return
	}

	var req struct {
		Config json.RawMessage `json:"config"`
		Apply  bool            `json:"apply"`
	}

	// Bound the body like the patch and test-send endpoints do. A full config
	// gets a larger cap than a partial patch, but it is still bounded. Read it
	// first so the size error is distinguishable from malformed JSON.
	r.Body = http.MaxBytesReader(w, r.Body, maxConfigImportBytes)
	body, err := io.ReadAll(r.Body)
	if err != nil {
		var maxErr *http.MaxBytesError
		if errors.As(err, &maxErr) {
			http.Error(w, "config import too large", http.StatusRequestEntityTooLarge)
			return
		}
		http.Error(w, fmt.Sprintf("failed to read request: %v", err), http.StatusBadRequest)
		return
	}
	if err := json.Unmarshal(body, &req); err != nil {
		http.Error(w, fmt.Sprintf("invalid request: %v", err), http.StatusBadRequest)
		return
	}

	var cfg config.Config
	if err := json.Unmarshal(req.Config, &cfg); err != nil {
		http.Error(w, fmt.Sprintf("invalid config: %v", err), http.StatusBadRequest)
		return
	}

	if cfg.Host == "" {
		http.Error(w, "host is required", http.StatusBadRequest)
		return
	}
	if cfg.Port < 1 || cfg.Port > 65535 {
		http.Error(w, "port must be between 1 and 65535", http.StatusBadRequest)
		return
	}

	resp := map[string]interface{}{
		"valid":  true,
		"config": cfg,
	}

	if req.Apply {
		configPath := s.atomicCfg.Path()
		data, err := json.MarshalIndent(cfg, "", "  ")
		if err != nil {
			s.logInternal("marshalling imported config", err)
			http.Error(w, "could not encode the config", http.StatusInternalServerError)
			return
		}

		// Shared with the partial-update endpoint so an import cannot interleave
		// with a save's read-merge-write sequence.
		s.configMu.Lock()
		defer s.configMu.Unlock()

		// Same sequence as the patch endpoint: validate the exact document the
		// loader would read, then swap it in atomically. Reload alone validates
		// too late — config.json has already been replaced by then, so a rejected
		// key or URL would leave an unbootable file on disk.
		tmp, err := writeTempConfig(configPath, data)
		if err != nil {
			s.logInternal("staging imported config", err)
			http.Error(w, "could not stage the config", http.StatusInternalServerError)
			return
		}
		defer func() { _ = os.Remove(tmp) }()

		merged, err := config.LoadFromPath(tmp)
		if err != nil {
			http.Error(w, fmt.Sprintf("config rejected: %v", err), http.StatusBadRequest)
			return
		}

		// A disabled jev_routing block is inert and skipped by the loader, so the
		// block this import just enabled may be one the loader would reject.
		if err := config.CheckJevRouting(merged.JevRouting); err != nil {
			http.Error(w, fmt.Sprintf("invalid config: %v", err), http.StatusBadRequest)
			return
		}

		// Atomic replace: the validated document becomes config.json in one rename.
		if err := os.Rename(tmp, configPath); err != nil {
			s.logInternal("replacing config file", err)
			http.Error(w, "could not replace the config file", http.StatusInternalServerError)
			return
		}

		if err := s.atomicCfg.Reload(); err != nil {
			s.logInternal("reloading config", err)
			http.Error(w, "could not reload the config", http.StatusInternalServerError)
			return
		}

		resp["applied"] = true
	}

	writeJSON(w, resp)
}
