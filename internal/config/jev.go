package config

import (
	"fmt"
	"net"
	"net/url"
	"regexp"
	"strings"
)

// Jev routing modes. Shadow computes the Jev classification and logs it next to
// the keyword scenario without changing the route; active lets Jev decide.
const (
	JevModeShadow = "shadow"
	JevModeActive = "active"
)

const (
	defaultJevModel     = "jev-1.14.0"
	defaultJevTimeoutMs = 800
	// maxJevTimeoutMs caps timeout_ms: in active mode the request waits for Jev
	// up to this long, so a typo like 30000 must not stall every request.
	maxJevTimeoutMs     = 10000
	defaultJevAPIKeyEnv = "TYPESAFE_AI_API_KEY"
	defaultJevBaseURL   = "https://api.typesafe.ai"
)

// jevAPIKeyEnvPattern accepts only an environment variable NAME. A real key
// pasted into api_key_env (or a ${VAR} that interpolation expanded into one)
// contains characters outside this set, so the load fails instead of the key
// being treated as a name.
var jevAPIKeyEnvPattern = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// JevRoutingConfig controls the opt-in TypeSafe System One (Jev) request
// classifier. When Enabled, the first 600 characters of the latest user message
// are sent to the TypeSafe API on every request that reaches keyword routing.
// The API key is read from the environment variable named by APIKeyEnv at call
// time; it is never stored in the config file.
type JevRoutingConfig struct {
	Enabled   bool   `json:"enabled"`
	Mode      string `json:"mode"`
	Model     string `json:"model"`
	TimeoutMs int    `json:"timeout_ms"`
	APIKeyEnv string `json:"api_key_env"`
	BaseURL   string `json:"base_url"`
}

func applyJevDefaults(j *JevRoutingConfig) {
	if j.Mode == "" {
		j.Mode = JevModeActive
	}
	if j.Model == "" {
		j.Model = defaultJevModel
	}
	if j.TimeoutMs == 0 {
		j.TimeoutMs = defaultJevTimeoutMs
	}
	if j.APIKeyEnv == "" {
		j.APIKeyEnv = defaultJevAPIKeyEnv
	}
	if j.BaseURL == "" {
		j.BaseURL = defaultJevBaseURL
	}
}

// validateJevRouting checks an enabled block. A disabled block is inert, like
// anthropic_first. Error text never includes the api_key_env value, which may be
// a pasted secret.
func validateJevRouting(j JevRoutingConfig) error {
	if !j.Enabled {
		return nil
	}
	if j.Mode != JevModeShadow && j.Mode != JevModeActive {
		return fmt.Errorf("jev_routing.mode must be %q or %q", JevModeShadow, JevModeActive)
	}
	if j.TimeoutMs < 1 || j.TimeoutMs > maxJevTimeoutMs {
		return fmt.Errorf("jev_routing.timeout_ms must be between 1 and %d", maxJevTimeoutMs)
	}
	u, err := url.Parse(j.BaseURL)
	if err != nil || u.Hostname() == "" || (u.Scheme != "http" && u.Scheme != "https") {
		return fmt.Errorf("jev_routing.base_url must be an absolute http or https URL")
	}
	// The key and the prompt text travel to this URL: no cleartext off the machine.
	if u.Scheme == "http" && !isLoopbackHost(u.Hostname()) {
		return fmt.Errorf("jev_routing.base_url must use https unless the host is loopback")
	}
	if !jevAPIKeyEnvPattern.MatchString(j.APIKeyEnv) {
		return fmt.Errorf("jev_routing.api_key_env must be the NAME of an environment variable (letters, digits, underscore), not a key")
	}
	return nil
}

// isLoopbackHost reports whether host is localhost or a loopback IP literal, the
// only places plain http is acceptable (local stubs and tunnels).
func isLoopbackHost(host string) bool {
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// CheckJevRouting applies the block's defaults to a copy and validates it, so a
// caller that writes a config to disk (the dashboard import) can refuse a bad
// jev_routing block before the file is replaced, with the same rules the loader
// enforces at start.
func CheckJevRouting(j JevRoutingConfig) error {
	applyJevDefaults(&j)
	return validateJevRouting(j)
}
