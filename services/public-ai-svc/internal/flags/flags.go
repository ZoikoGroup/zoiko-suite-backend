// Package flags holds feature flags and the kill switch (Doc 04 §20,
// Doc 08 §26). Every change needs an actor and an approval reference and
// emits publicai.kill_switch.changed.v1. Reads fail closed.
package flags

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
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
	// StructuredResponseEnabled turns on GET /menu and the structured turn
	// fields (outcome, followups, cta, resolved citations). Off by default
	// so existing clients see the unchanged contract (ADR-008).
	StructuredResponseEnabled = "structured_response_enabled"
	// Doc 10 App. B commercial-extension gates: secure lead intake, CRM and
	// booking sync, and third-party tracking. They exist so the decision is
	// recorded, and stay off in R0: turning one on is refused until the
	// receiving-service contract, consent model and approval exist (Doc 10).
	SecureLeadIntakeEnabled   = "secure_lead_intake_enabled"
	CRMSyncEnabled            = "crm_sync_enabled"
	BookingSyncEnabled        = "booking_sync_enabled"
	ThirdPartyTrackingEnabled = "third_party_tracking_enabled"
	// CommercialRoutesEnabled gates official-route CTAs (demo, trial,
	// contact) in answers (Doc 10 App. B). On by default; turning it off
	// removes commercial routes without touching the answers themselves.
	CommercialRoutesEnabled = "commercial_routes_enabled"
)

// dualControl are the flags whose re-enable after a disable needs a second
// person: the actor turning it back on must differ from the one who turned
// it off (Doc 09 ADM-T046, Doc 08 §04 dual control).
var dualControl = map[string]bool{PublicAIEnabled: true, ModelGenerationEnabled: true}

// ErrSameActor refuses a re-enable by the actor who disabled the flag.
var ErrSameActor = errors.New("re-enabling needs a different actor from the one who disabled it (dual control)")

// hardOff are the gates that may only be recorded as false in R0.
var hardOff = map[string]bool{StreamEnabled: true, SecureLeadIntakeEnabled: true, CRMSyncEnabled: true,
	BookingSyncEnabled: true, ThirdPartyTrackingEnabled: true}

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
	StructuredResponse     bool
	// CommercialRoutes allows official-route CTAs in answers (Doc 10 App. B).
	CommercialRoutes bool
}

// Defaults apply when a flag was never set.
func Defaults() Values {
	return Values{PublicAIEnabled: true, ModelGenerationEnabled: true, PricingClaimsEnabled: true, TrialCTAEnabled: true,
		CommercialRoutes: true, DisabledIntents: map[string]bool{}, MaxConcurrentTurns: 200}
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
	case StreamEnabled, SecureLeadIntakeEnabled, CRMSyncEnabled, BookingSyncEnabled, ThirdPartyTrackingEnabled:
		var b bool
		if err := json.Unmarshal(raw, &b); err != nil {
			return err
		}
		if b && hardOff[name] {
			return fmt.Errorf("%s cannot be turned on in R0", name)
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
	case StructuredResponseEnabled:
		return json.Unmarshal(raw, &v.StructuredResponse)
	case CommercialRoutesEnabled:
		return json.Unmarshal(raw, &v.CommercialRoutes)
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
	if name == MaxConcurrentTurns && v.MaxConcurrentTurns == 0 && (environment == "staging" || environment == "production") {
		// 0 means "no cap"; outside dev/test every limit is set (SEC-P0-09).
		return fmt.Errorf("max_concurrent_turns must be > 0 in %s", environment)
	}
	if dualControl[name] && string(compact.Bytes()) == "true" {
		if err := checkDualControl(ctx, st, name, actor); err != nil {
			return err
		}
	}
	now := time.Now().UTC()
	ev, err := events.KillSwitchChanged(name, actor, approval, environment, now)
	if err != nil {
		return err
	}
	return st.SetFlag(ctx, store.Flag{Name: name, Value: compact.Bytes(), ActorRef: actor, ApprovalRef: approval, UpdatedAt: now, Environment: environment}, []store.OutboxEvent{ev})
}

// auditScan bounds how far back the control audit is read for the disable.
const auditScan = 1000

// checkDualControl refuses re-enabling name when its current value is false
// and the last control_audit entry that set it was made by actor. The
// ledger is append-only, so its newest flag.set entry for name is the one
// that produced the current value; the flag row's actor is the fallback when
// the entry is beyond the scan window.
func checkDualControl(ctx context.Context, st store.Store, name, actor string) error {
	fs, err := st.Flags(ctx)
	if err != nil {
		return fmt.Errorf("read flags for dual control: %w", err)
	}
	var cur *store.Flag
	for i := range fs {
		if fs[i].Name == name {
			cur = &fs[i]
		}
	}
	if cur == nil || string(cur.Value) != "false" {
		return nil // never disabled, or already on: not a re-enable
	}
	disabledBy := cur.ActorRef
	audit, err := st.ListAudit(ctx, auditScan)
	if err != nil {
		return fmt.Errorf("read control audit for dual control: %w", err)
	}
	for _, e := range audit { // newest first
		if e.Action == "flag.set" && e.Target == name {
			disabledBy = e.Actor
			break
		}
	}
	if disabledBy == actor {
		return fmt.Errorf("%s: %w (disabled by %s)", name, ErrSameActor, disabledBy)
	}
	return nil
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
