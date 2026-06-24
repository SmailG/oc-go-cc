// Package router defines HTTP route registration and middleware chaining,
// as well as model selection based on request scenarios.
package router

import (
	"fmt"

	"oc-go-cc/internal/config"
)

// ModelRouter handles model selection based on scenarios.
type ModelRouter struct {
	atomic *config.AtomicConfig
}

// NewModelRouter creates a new model router.
func NewModelRouter(atomic *config.AtomicConfig) *ModelRouter {
	return &ModelRouter{atomic: atomic}
}

// RouteResult contains the selected model and fallback chain.
type RouteResult struct {
	Primary   config.ModelConfig
	Fallbacks []config.ModelConfig
	Scenario  Scenario
}

// resolveRequestedModel checks if the user-specified model should override
// scenario-based routing. Returns the route result and true if it matched,
// or zero value and false if scenario routing should proceed normally.
func (r *ModelRouter) resolveRequestedModel(cfg *config.Config, requestedModel string) (RouteResult, bool) {
	if !cfg.RespectRequestedModel || requestedModel == "" {
		return RouteResult{}, false
	}

	// Look up the requested model in config to inherit its settings
	primary, ok := cfg.Models[requestedModel]
	if !ok {
		// Unknown model — create a bare config and inherit defaults
		primary = config.ModelConfig{
			Provider: "opencode-go",
			ModelID:  requestedModel,
		}
		if def, ok := cfg.Models["default"]; ok {
			primary.Temperature = def.Temperature
			primary.MaxTokens = def.MaxTokens
		}
	}

	fallbacks := cfg.Fallbacks["default"]

	return RouteResult{
		Primary:   primary,
		Fallbacks: fallbacks,
		Scenario:  ScenarioDefault,
	}, true
}

// Route determines which model to use for a request.
// If respect_requested_model is enabled and requestedModel is provided, it overrides scenario-based routing.
func (r *ModelRouter) Route(messages []MessageContent, tokenCount int, toolCount int, requestedModel string) (RouteResult, error) {
	cfg := r.atomic.Get()

	if result, ok := r.resolveRequestedModel(cfg, requestedModel); ok {
		return result, nil
	}

	// Apply built-in routing profile (e.g. "openrouter"). When a profile is
	// active, scenario detection (which reads the long_context threshold from
	// the user config) must see the profile's model set, so we synthesize a
	// cfg with the profile's models merged in for detection purposes only.
	detectCfg := cfg
	if cfg.RoutingProfile != "" {
		detectCfg = r.applyProfileToConfig(cfg)
	}

	// Use scenario-based routing
	result := DetectScenario(messages, tokenCount, toolCount, detectCfg)

	models, fallbacks := r.resolveProfile(cfg)

	// Get primary model for scenario
	primary, ok := models[string(result.Scenario)]
	if !ok {
		// Fall back to default if scenario model not configured
		primary, ok = models["default"]
		if !ok {
			return RouteResult{}, fmt.Errorf("no default model configured")
		}
	}

	// Get fallbacks for scenario
	scenarioFallbacks := fallbacks[string(result.Scenario)]
	if len(scenarioFallbacks) == 0 {
		// Fall back to default fallbacks
		scenarioFallbacks = fallbacks["default"]
	}

	return RouteResult{
		Primary:   primary,
		Fallbacks: scenarioFallbacks,
		Scenario:  result.Scenario,
	}, nil
}

// resolveProfile returns the (models, fallbacks) maps to use for the active
// routing profile. When a routing_profile is set, the built-in profile is
// used; otherwise the configured maps are returned unchanged.
func (r *ModelRouter) resolveProfile(cfg *config.Config) (map[string]config.ModelConfig, map[string][]config.ModelConfig) {
	switch cfg.RoutingProfile {
	case config.RoutingProfileOpenRouter:
		return OpenRouterProfile()
	}
	return cfg.Models, cfg.Fallbacks
}

// applyProfileToConfig returns a shallow copy of cfg with Models and Fallbacks
// replaced by the active profile's maps. Scenario detection reads cfg.Models
// (e.g. for the long_context threshold), so detection must see the profile's
// values; the original cfg is left untouched so user-configured values remain
// available for explicit lookups.
func (r *ModelRouter) applyProfileToConfig(cfg *config.Config) *config.Config {
	profileModels, profileFallbacks := r.resolveProfile(cfg)
	clone := *cfg
	clone.Models = profileModels
	clone.Fallbacks = profileFallbacks
	return &clone
}

// IsStreamingScenarioRoutingEnabled returns whether streaming requests should use
// scenario-based routing instead of always routing to the fast model.
func (r *ModelRouter) IsStreamingScenarioRoutingEnabled() bool {
	return r.atomic.Get().EnableStreamingScenarioRouting
}

// IsPreferCapabilityEnabled returns whether routing favors capable models over TTFT.
func (r *ModelRouter) IsPreferCapabilityEnabled() bool {
	return r.atomic.Get().PreferCapability
}

// RouteWithOverride checks if the requested model matches a model_overrides entry.
//
// When matched, the returned RouteResult uses the override ModelConfig as the
// primary. The fallback chain is fallbacks[<requestedModel>], falling back to
// fallbacks["default"] when the override key has no entry (matching the
// behavior of Route and RouteForStreaming). The caller (MessagesHandler) is
// expected to merge a scenario-derived safety-net chain on top.
//
// Returns the override RouteResult and true if matched, or a zero value and
// false if the requested model has no entry in model_overrides.
func (r *ModelRouter) RouteWithOverride(requestedModel string) (RouteResult, bool) {
	cfg := r.atomic.Get()
	if cfg.ModelOverrides == nil {
		return RouteResult{}, false
	}
	override, ok := cfg.ModelOverrides[requestedModel]
	if !ok {
		return RouteResult{}, false
	}
	fallbacks := cfg.Fallbacks[requestedModel]
	if len(fallbacks) == 0 {
		fallbacks = cfg.Fallbacks["default"]
	}
	return RouteResult{
		Primary:   override,
		Fallbacks: fallbacks,
		Scenario:  ScenarioOverride,
	}, true
}

// GetModelChain returns the full chain of models to try (primary + fallbacks).
func (rr *RouteResult) GetModelChain() []config.ModelConfig {
	chain := []config.ModelConfig{rr.Primary}
	chain = append(chain, rr.Fallbacks...)
	return chain
}

// RouteForStreaming determines which model to use for streaming requests.
// Prioritizes fast TTFT (time-to-first-token) over capability.
// If respect_requested_model is enabled and requestedModel is provided, it overrides scenario-based routing.
func (r *ModelRouter) RouteForStreaming(messages []MessageContent, tokenCount int, toolCount int, requestedModel string) RouteResult {
	cfg := r.atomic.Get()

	if result, ok := r.resolveRequestedModel(cfg, requestedModel); ok {
		return result
	}

	// Otherwise, use scenario-based routing for streaming
	result := RouteForStreaming(messages, tokenCount, toolCount, cfg)

	// Get primary model for scenario
	primary, ok := cfg.Models[string(result.Scenario)]
	if !ok {
		// Fall back to fast scenario if not configured
		primary, ok = cfg.Models["fast"]
		if !ok {
			// Fall back to default
			primary = cfg.Models["default"]
		}
	}

	// Get fallbacks for scenario
	fallbacks := cfg.Fallbacks[string(result.Scenario)]
	if len(fallbacks) == 0 {
		// Fall back to fast fallbacks
		fallbacks = cfg.Fallbacks["fast"]
	}

	return RouteResult{
		Primary:   primary,
		Fallbacks: fallbacks,
		Scenario:  result.Scenario,
	}
}
