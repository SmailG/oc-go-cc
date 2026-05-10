// Package router defines HTTP route registration and middleware chaining.
package router

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"oc-go-cc/internal/config"
)

// CircuitState represents the state of a circuit breaker.
type CircuitState int

const (
	CircuitClosed   CircuitState = iota // Normal operation
	CircuitHalfOpen                     // Testing if service recovered
	CircuitOpen                         // Failing fast, not attempting calls
)

// CircuitBreaker tracks failure rates and prevents calls to failing models.
type CircuitBreaker struct {
	mu               sync.Mutex
	state            CircuitState
	failureCount     int
	successCount     int
	lastFailureTime  time.Time
	threshold        int           // failures before opening circuit
	recoveryTimeout  time.Duration // how long to wait before half-open
	halfOpenMaxCalls int           // max test calls in half-open state
	halfOpenCalls    int
}

// ForceOpen immediately opens the circuit breaker.
// Use this for non-retryable failures (e.g. quota exceeded) to avoid wasting attempts.
func (cb *CircuitBreaker) ForceOpen() {
	cb.mu.Lock()
	defer cb.mu.Unlock()

	cb.lastFailureTime = time.Now()
	cb.state = CircuitOpen
}

// NewCircuitBreaker creates a circuit breaker with default thresholds.
func NewCircuitBreaker(threshold int, recoveryTimeout time.Duration) *CircuitBreaker {
	return &CircuitBreaker{
		state:            CircuitClosed,
		threshold:        threshold,
		recoveryTimeout:  recoveryTimeout,
		halfOpenMaxCalls: 3,
	}
}

// AllowRequest returns true if the circuit allows a request.
func (cb *CircuitBreaker) AllowRequest() bool {
	cb.mu.Lock()
	defer cb.mu.Unlock()

	switch cb.state {
	case CircuitClosed:
		return true
	case CircuitOpen:
		// Check if recovery timeout has elapsed
		if time.Since(cb.lastFailureTime) > cb.recoveryTimeout {
			cb.state = CircuitHalfOpen
			cb.halfOpenCalls = 0
			return true
		}
		return false
	case CircuitHalfOpen:
		if cb.halfOpenCalls < cb.halfOpenMaxCalls {
			cb.halfOpenCalls++
			return true
		}
		return false
	}
	return false
}

// RecordSuccess records a successful call.
func (cb *CircuitBreaker) RecordSuccess() {
	cb.mu.Lock()
	defer cb.mu.Unlock()

	switch cb.state {
	case CircuitHalfOpen:
		cb.successCount++
		if cb.successCount >= cb.halfOpenMaxCalls {
			cb.state = CircuitClosed
			cb.failureCount = 0
			cb.successCount = 0
		}
	case CircuitClosed:
		cb.failureCount = 0
	}
}

// RecordFailure records a failed call.
func (cb *CircuitBreaker) RecordFailure() {
	cb.mu.Lock()
	defer cb.mu.Unlock()

	cb.lastFailureTime = time.Now()
	cb.failureCount++

	switch cb.state {
	case CircuitHalfOpen:
		cb.state = CircuitOpen
		cb.successCount = 0
	case CircuitClosed:
		if cb.failureCount >= cb.threshold {
			cb.state = CircuitOpen
		}
	}
}

// State returns the current circuit state.
func (cb *CircuitBreaker) State() CircuitState {
	cb.mu.Lock()
	defer cb.mu.Unlock()
	return cb.state
}

// FallbackResult contains the result of a fallback attempt.
type FallbackResult struct {
	ModelID     string
	Success     bool
	Error       error
	Attempted   int
	TotalModels int
}

// FallbackHandler manages model fallback with circuit breaker protection.
type FallbackHandler struct {
	logger          *slog.Logger
	circuitBreakers map[string]*CircuitBreaker
	cbThreshold     int
	cbTimeout       time.Duration
	mu              sync.Mutex
}

// NewFallbackHandler creates a new fallback handler with circuit breakers.
func NewFallbackHandler(logger *slog.Logger, cbThreshold int, cbTimeout time.Duration) *FallbackHandler {
	if logger == nil {
		logger = slog.Default()
	}
	if cbThreshold <= 0 {
		cbThreshold = 3
	}
	if cbTimeout <= 0 {
		cbTimeout = 30 * time.Second
	}

	return &FallbackHandler{
		logger:          logger,
		circuitBreakers: make(map[string]*CircuitBreaker),
		cbThreshold:     cbThreshold,
		cbTimeout:       cbTimeout,
	}
}

// getCircuitBreaker returns or creates a circuit breaker for a model.
func (h *FallbackHandler) getCircuitBreaker(modelID string) *CircuitBreaker {
	h.mu.Lock()
	defer h.mu.Unlock()

	cb, exists := h.circuitBreakers[modelID]
	if !exists {
		cb = NewCircuitBreaker(h.cbThreshold, h.cbTimeout)
		h.circuitBreakers[modelID] = cb
	}
	return cb
}

// ExecuteWithFallback tries models in sequence until one succeeds.
// Respects circuit breaker state to skip models that are failing repeatedly.
func (h *FallbackHandler) ExecuteWithFallback(
	ctx context.Context,
	models []config.ModelConfig,
	executor func(context.Context, config.ModelConfig) ([]byte, error),
) (*FallbackResult, []byte, error) {
	totalModels := len(models)

	for i := 0; i < len(models); i++ {
		model := models[i]
		// If the request was canceled (client disconnected / caller aborted),
		// do not attempt fallbacks and do not penalize circuits.
		if err := ctx.Err(); err != nil {
			return &FallbackResult{
				ModelID:     model.ModelID,
				Success:     false,
				Attempted:   i,
				TotalModels: totalModels,
			}, nil, fmt.Errorf("request canceled: %w", err)
		}

		cb := h.getCircuitBreaker(model.ModelID)

		// Skip models with open circuit breakers
		if !cb.AllowRequest() {
			h.logger.Info("circuit breaker open, skipping model",
				"model", model.ModelID,
				"attempt", i+1,
				"total", totalModels,
			)
			continue
		}

		h.logger.Info("attempting model",
			"model", model.ModelID,
			"attempt", i+1,
			"total", totalModels,
		)

		body, err := executor(ctx, model)
		if err == nil {
			cb.RecordSuccess()
			h.logger.Info("model succeeded",
				"model", model.ModelID,
				"attempt", i+1,
			)
			return &FallbackResult{
				ModelID:     model.ModelID,
				Success:     true,
				Attempted:   i + 1,
				TotalModels: totalModels,
			}, body, nil
		}

		// If the provider reports an insufficient quota, open the circuit
		// immediately and skip other models from the same family (e.g. qwen*).
		if isInsufficientQuotaError(err) {
			cb.ForceOpen()
			h.logger.Warn("model quota exceeded, opening circuit",
				"model", model.ModelID,
				"error", err,
			)

			// Skip subsequent Qwen fallbacks in this chain; they are likely to
			// fail for the same account/quota and just waste attempts.
			if isQwenModel(model.ModelID) {
				for i+1 < len(models) && isQwenModel(models[i+1].ModelID) {
					next := models[i+1].ModelID
					h.getCircuitBreaker(next).ForceOpen()
					h.logger.Info("skipping qwen fallback due to quota",
						"model", next,
					)
					i++
				}
			}
			continue
		}

		// Don't treat request cancellation as a model failure; callers can
		// decide how to surface a canceled request.
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return &FallbackResult{
				ModelID:     model.ModelID,
				Success:     false,
				Attempted:   i + 1,
				TotalModels: totalModels,
			}, nil, err
		}

		cb.RecordFailure()
		h.logger.Warn("model failed, trying fallback",
			"model", model.ModelID,
			"error", err,
			"remaining", totalModels-i-1,
			"circuit_state", cb.State(),
		)
	}

	return &FallbackResult{
		ModelID:     models[0].ModelID,
		Success:     false,
		Attempted:   totalModels,
		TotalModels: totalModels,
	}, nil, fmt.Errorf("all models failed (%d attempts)", totalModels)
}

func isInsufficientQuotaError(err error) bool {
	if err == nil {
		return false
	}
	s := strings.ToLower(err.Error())
	// Examples seen in provider errors:
	// - `"type":"insufficient_quota"`
	// - `"code":"insufficient_quota"`
	// - "exceeded your current quota"
	return strings.Contains(s, "insufficient_quota") || strings.Contains(s, "exceeded your current quota")
}

func isQwenModel(modelID string) bool {
	return strings.HasPrefix(strings.ToLower(modelID), "qwen")
}

// GetFallbackChain returns the fallback chain for a given primary model.
func GetFallbackChain(primary config.ModelConfig, fallbacks map[string][]config.ModelConfig) []config.ModelConfig {
	chain := []config.ModelConfig{primary}

	if fb, exists := fallbacks[primary.ModelID]; exists {
		chain = append(chain, fb...)
	}

	return chain
}

// IsRetryableError determines if an error is worth retrying with a fallback.
func IsRetryableError(err error) bool {
	if err == nil {
		return false
	}

	// Quota exhaustion is not retryable; it requires operational intervention.
	if isInsufficientQuotaError(err) {
		return false
	}

	errStr := err.Error()
	// Retry on network errors, timeouts, rate limits, server errors
	retryable := []string{
		"timeout",
		"connection refused",
		"connection reset",
		"rate limit",
		"429",
		"503",
		"502",
		"500",
	}

	for _, sub := range retryable {
		if strings.Contains(errStr, sub) {
			return true
		}
	}
	return false
}

// GetCircuitStates returns the state of all circuit breakers.
func (h *FallbackHandler) GetCircuitStates() map[string]string {
	h.mu.Lock()
	defer h.mu.Unlock()

	states := make(map[string]string)
	for modelID, cb := range h.circuitBreakers {
		state := cb.State()
		switch state {
		case CircuitClosed:
			states[modelID] = "closed"
		case CircuitHalfOpen:
			states[modelID] = "half_open"
		case CircuitOpen:
			states[modelID] = "open"
		default:
			states[modelID] = "unknown"
		}
	}
	return states
}
