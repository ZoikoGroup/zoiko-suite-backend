// Package circuitbreaker provides a simple circuit breaker pattern for
// external service calls. Per 03-microservices.md §17.7: "All inter-service
// calls must use circuit breakers, timeouts, retries, and dead-letter routing
// where applicable. A Tier 2 failure must not collapse a Tier 0 process."
package circuitbreaker

import (
	"context"
	"errors"
	"sync"
	"time"
)

// State represents the circuit breaker state.
type State int

const (
	StateClosed State = iota // Normal operation, requests go through
	StateOpen                // Circuit open, requests fail fast
	StateHalfOpen            // Testing if service recovered
)

var (
	ErrCircuitOpen = errors.New("circuit breaker is open")
)

// Config holds circuit breaker configuration.
type Config struct {
	// FailureThreshold is the number of consecutive failures before opening the circuit.
	FailureThreshold int
	// SuccessThreshold is the number of consecutive successes in half-open state before closing.
	SuccessThreshold int
	// Timeout is the duration the circuit stays open before transitioning to half-open.
	Timeout time.Duration
}

// DefaultConfig returns a sensible default configuration.
func DefaultConfig() Config {
	return Config{
		FailureThreshold: 5,
		SuccessThreshold: 2,
		Timeout:          30 * time.Second,
	}
}

// CircuitBreaker wraps a function with circuit breaker logic.
type CircuitBreaker struct {
	mu               sync.Mutex
	state            State
	failures         int
	successes        int
	lastFailureTime  time.Time
	config           Config
	name             string
}

// NewCircuitBreaker creates a new circuit breaker with the given name and config.
func NewCircuitBreaker(name string, config Config) *CircuitBreaker {
	return &CircuitBreaker{
		name:   name,
		state:  StateClosed,
		config: config,
	}
}

// Execute runs the given function if the circuit allows it.
func (cb *CircuitBreaker) Execute(ctx context.Context, fn func() error) error {
	cb.mu.Lock()
	state := cb.state
	cb.mu.Unlock()

	switch state {
	case StateClosed:
		return cb.executeClosed(ctx, fn)
	case StateOpen:
		return cb.executeOpen(ctx, fn)
	case StateHalfOpen:
		return cb.executeHalfOpen(ctx, fn)
	}
	return fn()
}

func (cb *CircuitBreaker) executeClosed(ctx context.Context, fn func() error) error {
	err := fn()
	cb.mu.Lock()
	defer cb.mu.Unlock()

	if err != nil {
		cb.failures++
		cb.lastFailureTime = time.Now()
		if cb.failures >= cb.config.FailureThreshold {
			cb.state = StateOpen
		}
	} else {
		cb.failures = 0
	}
	return err
}

func (cb *CircuitBreaker) executeOpen(ctx context.Context, fn func() error) error {
	cb.mu.Lock()
	timeSinceFailure := time.Since(cb.lastFailureTime)
	if timeSinceFailure >= cb.config.Timeout {
		cb.state = StateHalfOpen
		cb.successes = 0
		cb.mu.Unlock()
		return cb.executeHalfOpen(ctx, fn)
	}
	cb.mu.Unlock()
	return ErrCircuitOpen
}

func (cb *CircuitBreaker) executeHalfOpen(ctx context.Context, fn func() error) error {
	err := fn()
	cb.mu.Lock()
	defer cb.mu.Unlock()

	if err != nil {
		cb.state = StateOpen
		cb.failures = 1
		cb.lastFailureTime = time.Now()
		return err
	}

	cb.successes++
	if cb.successes >= cb.config.SuccessThreshold {
		cb.state = StateClosed
		cb.failures = 0
		cb.successes = 0
	}
	return nil
}

// GetState returns the current circuit breaker state.
func (cb *CircuitBreaker) GetState() State {
	cb.mu.Lock()
	defer cb.mu.Unlock()
	return cb.state
}

// String returns the state as a string.
func (s State) String() string {
	switch s {
	case StateClosed:
		return "CLOSED"
	case StateOpen:
		return "OPEN"
	case StateHalfOpen:
		return "HALF_OPEN"
	default:
		return "UNKNOWN"
	}
}