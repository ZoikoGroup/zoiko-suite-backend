package model

import (
	"errors"
	"sync"
	"time"
)

// ErrCircuitOpen is returned without calling the provider while the circuit
// is open (Doc 04 A29: "circuit opens, safe guidance, alert emitted").
var ErrCircuitOpen = errors.New("model circuit open")

// Breaker opens after Threshold consecutive failures and stays open for
// Cooldown, then lets one trial call through. OnOpen is the alert hook.
type Breaker struct {
	Threshold int
	Cooldown  time.Duration
	OnOpen    func()

	mu        sync.Mutex
	failures  int
	openUntil time.Time
	now       func() time.Time
}

func (b *Breaker) clock() time.Time {
	if b.now != nil {
		return b.now()
	}
	return time.Now()
}

func (b *Breaker) allow() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return !b.clock().Before(b.openUntil)
}

func (b *Breaker) success() {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.failures = 0
}

func (b *Breaker) failure() {
	b.mu.Lock()
	b.failures++
	opened := b.failures >= b.Threshold && b.Threshold > 0
	if opened {
		b.openUntil = b.clock().Add(b.Cooldown)
		b.failures = 0
	}
	hook := b.OnOpen
	b.mu.Unlock()
	if opened && hook != nil {
		hook()
	}
}

// Open reports whether the circuit is currently open.
func (b *Breaker) Open() bool { return !b.allow() }
