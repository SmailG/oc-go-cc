package router

import (
	"context"
	"errors"
	"log/slog"
	"testing"
	"time"

	"oc-go-cc/internal/config"
)

func TestExecuteWithFallbackStopsOnCanceledContext(t *testing.T) {
	h := NewFallbackHandler(slog.Default(), 3, 30*time.Second)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	models := []config.ModelConfig{
		{ModelID: "m1"},
		{ModelID: "m2"},
	}

	calls := 0
	_, _, err := h.ExecuteWithFallback(ctx, models, func(ctx context.Context, m config.ModelConfig) ([]byte, error) {
		calls++
		return nil, errors.New("should not be called")
	})

	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	if calls != 0 {
		t.Fatalf("executor calls = %d, want 0", calls)
	}
}

func TestExecuteWithFallbackForceOpensCircuitOnInsufficientQuota(t *testing.T) {
	h := NewFallbackHandler(slog.Default(), 3, 30*time.Second)

	models := []config.ModelConfig{
		{ModelID: "qwen3.5-plus"},
		{ModelID: "qwen3.6-plus"},
		{ModelID: "glm-5.1"},
	}

	execCalls := make(map[string]int)
	executor := func(ctx context.Context, m config.ModelConfig) ([]byte, error) {
		execCalls[m.ModelID]++
		switch m.ModelID {
		case "qwen3.5-plus":
			return nil, errors.New(`API error 429: {"error":{"type":"insufficient_quota","message":"You exceeded your current quota"}}`)
		case "glm-5.1":
			return []byte(`{"ok":true}`), nil
		default:
			return nil, errors.New("should not be called")
		}
	}

	res, body, err := h.ExecuteWithFallback(context.Background(), models, executor)
	if err != nil {
		t.Fatalf("ExecuteWithFallback() err = %v", err)
	}
	if res.ModelID != "glm-5.1" {
		t.Fatalf("ModelID = %q, want glm-5.1", res.ModelID)
	}
	if string(body) != `{"ok":true}` {
		t.Fatalf("body = %s, want ok", string(body))
	}
	if execCalls["qwen3.6-plus"] != 0 {
		t.Fatalf("executor called qwen3.6-plus %d times, want 0 (skipped due to quota)", execCalls["qwen3.6-plus"])
	}

	// Subsequent calls should skip qwen due to open circuit.
	execCalls = make(map[string]int)
	_, _, _ = h.ExecuteWithFallback(context.Background(), models, executor)
	if execCalls["qwen3.5-plus"] != 0 {
		t.Fatalf("qwen3.5-plus calls = %d, want 0 (circuit open)", execCalls["qwen3.5-plus"])
	}
}
