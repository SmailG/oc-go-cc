// Package config handles application configuration loading and validation.
package config

import "encoding/json"

// RoutingProfile selects a built-in model set. Empty string uses the user-
// configured `models`/`fallbacks` blocks unchanged. Recognized profiles:
//   - "openrouter": replaces `models` and `fallbacks` with a fixed OpenRouter-
//     only model set (see internal/router/openrouter_defaults.go). All entries
//     route through the OpenRouter provider regardless of the original
//     opencode-go/opencode-zen settings.
type RoutingProfile string

const (
	RoutingProfileOpenRouter RoutingProfile = "openrouter"

	// Provider identifier strings used in ModelConfig.Provider. Exposed here so
	// that router-side code can build model configs without importing the
	// client package (which would create an import cycle).
	ProviderOpenCodeGo  = "opencode-go"
	ProviderOpenCodeZen = "opencode-zen"
	ProviderOpenRouter  = "openrouter"
)

// Config holds the complete application configuration.
type Config struct {
	APIKey                         string   `json:"api_key"`
	APIKeys                        []string `json:"api_keys"`
	Host                           string   `json:"host"`
	Port                           int      `json:"port"`
	HotReload                      bool     `json:"hot_reload"`
	EnableStreamingScenarioRouting bool     `json:"enable_streaming_scenario_routing"`
	// PreferCapability selects capable models and full scenario routing for streaming
	// (no downgrade to qwen "fast"). Use when quality should be closer to native Claude Code.
	PreferCapability      bool                     `json:"prefer_capability"`
	RespectRequestedModel bool                     `json:"respect_requested_model"`
	// RoutingProfile, when non-empty, replaces the configured Models and
	// Fallbacks maps with a built-in profile. Currently only "openrouter"
	// is recognized.
	RoutingProfile      RoutingProfile           `json:"routing_profile"`
	Models              map[string]ModelConfig   `json:"models"`
	Fallbacks           map[string][]ModelConfig `json:"fallbacks"`
	ModelOverrides      map[string]ModelConfig   `json:"model_overrides"`
	OpenCodeGo          OpenCodeGoConfig         `json:"opencode_go"`
	OpenCodeZen         OpenCodeZenConfig        `json:"opencode_zen"`
	OpenRouter          OpenRouterConfig         `json:"openrouter"`
	Logging             LoggingConfig            `json:"logging"`
}

// ModelConfig defines routing rules for a specific model.
type ModelConfig struct {
	Provider         string          `json:"provider"`
	ModelID          string          `json:"model_id"`
	Temperature      float64         `json:"temperature"`
	MaxTokens        int             `json:"max_tokens"`
	ContextThreshold int             `json:"context_threshold"`
	ReasoningEffort  string          `json:"reasoning_effort"`
	Thinking         json.RawMessage `json:"thinking,omitempty"`
	Vision           bool            `json:"vision"`
}

// OpenCodeGoConfig holds the upstream OpenCode Go API settings.
type OpenCodeGoConfig struct {
	BaseURL          string `json:"base_url"`
	AnthropicBaseURL string `json:"anthropic_base_url"`
	TimeoutMs        int    `json:"timeout_ms"`
}

// OpenCodeZenConfig holds the upstream OpenCode Zen API settings.
type OpenCodeZenConfig struct {
	BaseURL          string `json:"base_url"`
	AnthropicBaseURL string `json:"anthropic_base_url"`
	ResponsesBaseURL string `json:"responses_base_url"`
	GeminiBaseURL    string `json:"gemini_base_url"`
	TimeoutMs        int    `json:"timeout_ms"`
}

// OpenRouterConfig holds the upstream OpenRouter API settings.
// OpenRouter exposes two endpoint formats we route to:
//   - /v1/messages ("Anthropic Skin") — raw Anthropic-format passthrough for
//     any model. Used when the upstream provider is OpenRouter and the model
//     is in IsAnthropicModel().
//   - /v1/chat/completions (OpenAI-compatible) — for OpenAI-format models on
//     OpenRouter (GLM/Kimi/Qwen/DeepSeek/etc.). Used when the request flows
//     through the standard OpenAI transformation pipeline.
type OpenRouterConfig struct {
	APIKey           string   `json:"api_key"`
	APIKeys          []string `json:"api_keys"`
	BaseURL          string   `json:"base_url"`
	AnthropicBaseURL string   `json:"anthropic_base_url"`
	TimeoutMs        int      `json:"timeout_ms"`
}

// EffectiveAPIKeys returns the pool of API keys for rotation.
// APIKeys takes precedence; falls back to the single APIKey field.
func (c *OpenRouterConfig) EffectiveAPIKeys() []string {
	if len(c.APIKeys) > 0 {
		return c.APIKeys
	}
	if c.APIKey != "" {
		return []string{c.APIKey}
	}
	return nil
}

// LoggingConfig controls application logging behavior.
type LoggingConfig struct {
	Level    string `json:"level"`
	Requests bool   `json:"requests"`
}

// EffectiveAPIKeys returns the pool of API keys for rotation.
// APIKeys takes precedence; falls back to the single APIKey field.
func (c *Config) EffectiveAPIKeys() []string {
	if len(c.APIKeys) > 0 {
		return c.APIKeys
	}
	if c.APIKey != "" {
		return []string{c.APIKey}
	}
	return nil
}
