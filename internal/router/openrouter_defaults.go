package router

import (
	"oc-go-cc/internal/config"
)

// OpenRouterProfileName is the routing_profile value that activates the
// OpenRouter-only built-in model set.
const OpenRouterProfileName = "openrouter"

// OpenRouterProfile returns the OpenRouter-only built-in model set. All entries
// use the OpenRouter provider with model IDs from OpenRouter's catalog
// (https://openrouter.ai/models).
//
// The set mirrors the structure of configs/config.example.json so that:
//   - the same scenario names apply (background/default/think/complex/long_context/fast)
//   - the long_context threshold matches the example config (80K tokens)
//   - fallbacks provide graceful degradation within OpenRouter when a primary
//     model returns an error or is rate-limited
//
// When this profile is active, the user's `models`/`fallbacks` blocks are
// ignored — the entire route decision is governed by this profile.
func OpenRouterProfile() (map[string]config.ModelConfig, map[string][]config.ModelConfig) {
	models := map[string]config.ModelConfig{
		string(ScenarioBackground): {
			Provider:    config.ProviderOpenRouter,
			ModelID:     "qwen/qwen3-coder",
			Temperature: 0.5,
			MaxTokens:   2048,
		},
		string(ScenarioDefault): {
			Provider:    config.ProviderOpenRouter,
			ModelID:     "minimax/minimax-m3",
			Temperature: 0.7,
			MaxTokens:   4096,
		},
		string(ScenarioThink): {
			Provider:    config.ProviderOpenRouter,
			ModelID:     "z-ai/glm-5.2",
			Temperature: 0.7,
			MaxTokens:   8192,
		},
		string(ScenarioComplex): {
			Provider:    config.ProviderOpenRouter,
			ModelID:     "z-ai/glm-5.2",
			Temperature: 0.7,
			MaxTokens:   4096,
		},
		string(ScenarioLongContext): {
			Provider:         config.ProviderOpenRouter,
			ModelID:          "minimax/minimax-m3",
			Temperature:      0.7,
			MaxTokens:        16384,
			ContextThreshold: 80000,
		},
		string(ScenarioFast): {
			Provider:    config.ProviderOpenRouter,
			ModelID:     "qwen/qwen3-coder",
			Temperature: 0.7,
			MaxTokens:   4096,
		},
	}

	fallbacks := map[string][]config.ModelConfig{
		string(ScenarioDefault): {
			{Provider: config.ProviderOpenRouter, ModelID: "minimax/minimax-m3"},
			{Provider: config.ProviderOpenRouter, ModelID: "z-ai/glm-5.1"},
		},
		string(ScenarioBackground): {
			{Provider: config.ProviderOpenRouter, ModelID: "minimax/minimax-m3"},
			{Provider: config.ProviderOpenRouter, ModelID: "moonshotai/kimi-k2.6"},
		},
		string(ScenarioThink): {
			{Provider: config.ProviderOpenRouter, ModelID: "minimax/minimax-m3"},
			{Provider: config.ProviderOpenRouter, ModelID: "moonshotai/kimi-k2.6"},
		},
		string(ScenarioComplex): {
			{Provider: config.ProviderOpenRouter, ModelID: "moonshotai/kimi-k2.6"},
			{Provider: config.ProviderOpenRouter, ModelID: "minimax/minimax-m3"},
			{Provider: config.ProviderOpenRouter, ModelID: "z-ai/glm-5.2"},
		},
		string(ScenarioLongContext): {
			{Provider: config.ProviderOpenRouter, ModelID: "moonshotai/kimi-k2.6"},
			{Provider: config.ProviderOpenRouter, ModelID: "z-ai/glm-5.2"},
		},
		string(ScenarioFast): {
			{Provider: config.ProviderOpenRouter, ModelID: "minimax/minimax-m3"},
			{Provider: config.ProviderOpenRouter, ModelID: "moonshotai/kimi-k2.6"},
		},
	}

	return models, fallbacks
}
