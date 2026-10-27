// Package flags holds feature flags and the kill switch (Doc 04 §20,
// Doc 08 §26). Every change needs an actor and an approval reference and
// emits publicai.kill_switch.changed.v1. Reads fail closed.
package flags

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"time"

	"zoiko.io/public-ai-svc/internal/events"
	"zoiko.io/public-ai-svc/internal/store"
)

// Flag names.
const (
	PublicAIEnabled        = "public_ai_enabled"
	ModelGenerationEnabled = "model_generation_enabled"
	StreamEnabled          = "stream_enabled"
	PricingClaimsEnabled   = "pricing_claims_enabled"
	TrialCTAEnabled        = "trial_cta_enabled"
	PerIntentDisable       = "per_intent_disable"
	ApprovedFallbackMode   = "approved_fallback_mode"
	MaxConcurrentTurns     = "max_concurrent_turns"
)

// Values is the effective flag set.
type Values struct {
	PublicAIEnabled        bool
	ModelGenerationEnabled bool
	StreamEnabled          bool
	PricingClaimsEnabled   bool
	TrialCTAEnabled        bool
	DisabledIntents        map[string]bool
	FallbackMode           string // "" or "static"
	MaxConcurrentTurns     int
}

// Defaults apply when a flag was never set.
func Defaults() Values {
	return Values{PublicAIEnabled: true, ModelGenerationEnabled: true, PricingClaimsEnabled: true, TrialCTAEnabled: true,
		DisabledIntents: map[string]bool{}, MaxConcurrentTurns: 200}
}

// FailClosed is used when flags cannot be read: the assistant is off.
func FailClosed() Values {
	v := Defaults()
	v.PublicAIEnabled, v.ModelGenerationEnabled = false, false
	return v
}

func apply(v *Values, name string, raw []byte) error {
	switch name {
	case PublicAIEnabled:
		return json.Unmarshal(raw, &v.PublicAIEnabled)
	case ModelGenerationEnabled:
		return json.Unmarshal(raw, &v.ModelGenerationEnabled)
	case StreamEnabled:
		var b bool
		if err := json.Unmarshal(raw, &b); err != nil {
			return err
		}
		if b {
			return fmt.Errorf("stream_enabled cannot be turned on in R0")
		}
		return nil
	case PricingClaimsEnabled:
		return json.Unmarshal(raw, &v.PricingClaimsEnabled)
	case TrialCTAEnabled:
		return json.Unmarshal(raw, &v.TrialCTAEnabled)
	case PerIntentDisable:
		var ids []string
		if err := json.Unmarshal(raw, &ids); err != nil {
			return err
		}
		v.DisabledIntents = map[string]bool{}
		for _, id := range ids {
			v.DisabledIntents[id] = true
		}
		return nil
	case ApprovedFallbackMode:
		var m string
		if err := json.Unmarshal(raw, &m); err != nil {
			return err
		}
		if m != "" && m != "static" {
			return fmt.Errorf("approved_fallback_mode must be \"\" or \"static\"")
		}
		v.FallbackMode = m
		return nil
	case MaxConcurrentTurns:
		if err := json.Unmarshal(raw, &v.MaxConcurrentTurns); err != nil {
			return err
		}
		if v.MaxConcurrentTurns < 0 {
			return fmt.Errorf("max_concurrent_turns must be >= 0")
		}
		return nil
	}
	return fmt.Errorf("unknown flag %q", name)
}

// Set validates and stores a flag change with its audit references and the
// control event, in one transaction.
func Set(ctx context.Context, st store.Store, name string, value []byte, actor, approval, environment string) error {
	if actor == "" || approval == "" {
		return fmt.Errorf("actor and approval references are required")
	}
	v := Defaults()
	if err := apply(&v, name, value); err != nil {
		return err
	}
	var compact bytes.Buffer
	if err := json.Compact(&compact, value); err != nil {
		return err
	}
	now := time.Now().UTC()
	ev, err := events.KillSwitchChanged(name, actor, approval, environment, now)
	if err != nil {
		return err
	}
	return st.SetFlag(ctx, store.Flag{Name: name, Value: compact.Bytes(), ActorRef: actor, ApprovalRef: approval, UpdatedAt: now}, []store.OutboxEvent{ev})
}

// Loader caches the effective values and refreshes them.
type Loader struct {
	st  store.Store
	mu  sync.RWMutex
	v   Values
	ok  bool
	err error
}

func NewLoader(st store.Store) *Loader { return &Loader{st: st, v: FailClosed()} }

// Refresh reloads flags; on error the loader fails closed.
func (l *Loader) Refresh(ctx context.Context) error {
	fs, err := l.st.Flags(ctx)
	v := Defaults()
	if err == nil {
		for _, f := range fs {
			if aerr := apply(&v, f.Name, f.Value); aerr != nil {
				err = fmt.Errorf("flag %s: %w", f.Name, aerr)
				break
			}
		}
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if err != nil {
		l.v, l.ok, l.err = FailClosed(), false, err
		return err
	}
	l.v, l.ok, l.err = v, true, nil
	return nil
}

// Values returns the effective flags (fail-closed until the first refresh).
func (l *Loader) Values() Values {
	l.mu.RLock()
	defer l.mu.RUnlock()
	return l.v
}

// Healthy reports whether the last refresh succeeded.
func (l *Loader) Healthy() bool {
	l.mu.RLock()
	defer l.mu.RUnlock()
	return l.ok
}
