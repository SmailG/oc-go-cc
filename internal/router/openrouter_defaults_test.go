package router

import (
	"testing"

	"oc-go-cc/internal/config"
)

func TestOpenRouterProfile_HasAllScenarios(t *testing.T) {
	models, fallbacks := OpenRouterProfile()

	wantModels := []Scenario{
		ScenarioBackground,
		ScenarioDefault,
		ScenarioThink,
		ScenarioComplex,
		ScenarioLongContext,
		ScenarioFast,
	}
	for _, s := range wantModels {
		mc, ok := models[string(s)]
		if !ok {
			t.Errorf("OpenRouter profile missing model for scenario %q", s)
			continue
		}
		if mc.Provider != config.ProviderOpenRouter {
			t.Errorf("scenario %q provider = %q, want %q", s, mc.Provider, config.ProviderOpenRouter)
		}
		if mc.ModelID == "" {
			t.Errorf("scenario %q has empty model_id", s)
		}
	}

	for _, s := range wantModels {
		if _, ok := fallbacks[string(s)]; !ok {
			t.Errorf("OpenRouter profile missing fallback list for scenario %q", s)
		}
	}
}

func TestOpenRouterProfile_LongContextThresholdMatchesLegacy(t *testing.T) {
	models, _ := OpenRouterProfile()
	mc, ok := models[string(ScenarioLongContext)]
	if !ok {
		t.Fatal("OpenRouter profile missing long_context entry")
	}
	if mc.ContextThreshold != 80000 {
		t.Errorf("long_context threshold = %d, want 80000", mc.ContextThreshold)
	}
}

func TestRoute_OpenRouterProfile_OverridesUserModels(t *testing.T) {
	// User configures their own models block, but routing_profile=openrouter
	// should completely replace those with the OpenRouter built-in set.
	cfg := &config.Config{
		RoutingProfile: config.RoutingProfileOpenRouter,
		APIKey:         "test",
		Models: map[string]config.ModelConfig{
			"default": {
				Provider: "opencode-go",
				ModelID:  "kimi-k2.6", // should be ignored
			},
		},
	}

	router := NewModelRouter(newTestAtomicConfig(cfg))

	messages := []MessageContent{
		{Role: "user", Content: "Write a hello world"},
	}

	result, err := router.Route(messages, 100, 0, "")
	if err != nil {
		t.Fatalf("Route() error = %v", err)
	}

	if result.Primary.Provider != config.ProviderOpenRouter {
		t.Errorf("primary provider = %q, want %q", result.Primary.Provider, config.ProviderOpenRouter)
	}
	if result.Primary.ModelID == "kimi-k2.6" {
		t.Error("route returned user's opencode-go model; profile override did not apply")
	}
	if len(result.Fallbacks) == 0 {
		t.Error("expected at least one fallback from OpenRouter profile")
	}
	for _, fb := range result.Fallbacks {
		if fb.Provider != config.ProviderOpenRouter {
			t.Errorf("fallback provider = %q, want %q", fb.Provider, config.ProviderOpenRouter)
		}
	}
}

func TestRoute_OpenRouterProfile_LongContextRouting(t *testing.T) {
	cfg := &config.Config{
		RoutingProfile: config.RoutingProfileOpenRouter,
		APIKey:         "test",
	}

	router := NewModelRouter(newTestAtomicConfig(cfg))

	messages := []MessageContent{{Role: "user", Content: "x"}}
	// Token count above 80K threshold should route to long_context scenario.
	result, err := router.Route(messages, 100_000, 0, "")
	if err != nil {
		t.Fatalf("Route() error = %v", err)
	}

	if result.Scenario != ScenarioLongContext {
		t.Errorf("scenario = %q, want %q", result.Scenario, ScenarioLongContext)
	}
	if result.Primary.ModelID != "minimax/minimax-m3" {
		t.Errorf("long_context primary = %q, want minimax/minimax-m3", result.Primary.ModelID)
	}
}

func TestRoute_NoProfile_UsesConfiguredModels(t *testing.T) {
	cfg := &config.Config{
		RoutingProfile: "", // legacy behavior
		APIKey:         "test",
		Models: map[string]config.ModelConfig{
			"default": {
				Provider: "opencode-go",
				ModelID:  "kimi-k2.6",
			},
		},
		Fallbacks: map[string][]config.ModelConfig{
			"default": {{Provider: "opencode-go", ModelID: "qwen3.5-plus"}},
		},
	}

	router := NewModelRouter(newTestAtomicConfig(cfg))

	result, err := router.Route([]MessageContent{{Role: "user", Content: "hi"}}, 100, 0, "")
	if err != nil {
		t.Fatalf("Route() error = %v", err)
	}

	if result.Primary.ModelID != "kimi-k2.6" {
		t.Errorf("primary = %q, want kimi-k2.6 (user config)", result.Primary.ModelID)
	}
	if result.Primary.Provider != "opencode-go" {
		t.Errorf("primary provider = %q, want opencode-go", result.Primary.Provider)
	}
}