package service

import (
	"context"
	"time"

	"zoiko.io/currency-registry-svc/internal/domain"
	"zoiko.io/currency-registry-svc/internal/events"
)

// Command names used for idempotency scoping and metrics.
const (
	CommandActivate = "activate"
	CommandRestrict = "restrict"
	CommandRetire   = "retire"
)

// TransitionInput is ActivatePlatformCurrency / RestrictCurrency / RetireCurrency.
type TransitionInput struct {
	Meta
	CurrencyID      string
	Command         string // activate | restrict | retire
	ExpectedVersion int64
	// EffectiveAt is honoured for retire only (it becomes valid_to); it must lie
	// within [valid_from, now]. Other transitions take effect at recorded time.
	EffectiveAt *time.Time
	// ApproverID is the asserted approver (recorded as evidence, not verified).
	ApproverID string
}

// TargetStatus maps a command to its target status.
func TargetStatus(command string) (domain.Status, bool) {
	switch command {
	case CommandActivate:
		return domain.StatusSupported, true
	case CommandRestrict:
		return domain.StatusRestricted, true
	case CommandRetire:
		return domain.StatusRetired, true
	}
	return "", false
}

// Transition applies a named lifecycle command to a currency.
//
// Check order is deliberate and each step has its own typed error:
// context -> existence (NOT_FOUND) -> expected_version (VERSION_CONFLICT) ->
// lifecycle (INVALID_TRANSITION) -> segregation of duties (SOD_DENIED).
func (s *Service) Transition(ctx context.Context, in TransitionInput) (*domain.Currency, bool, error) {
	if err := in.Meta.check(); err != nil {
		return nil, false, err
	}
	target, ok := TargetStatus(in.Command)
	if !ok {
		return nil, false, domain.Errf(domain.CodeContextInvalid, "unknown command %q", in.Command)
	}
	if in.CurrencyID == "" {
		return nil, false, domain.Errf(domain.CodeContextInvalid, "currency id is required")
	}
	if in.ExpectedVersion < 1 {
		return nil, false, domain.Errf(domain.CodeContextInvalid, "expected_version is required and must be >= 1")
	}

	var res *domain.Currency
	var replayed bool
	err := s.store.InTx(ctx, in.TenantID, func(tx Tx) error {
		out, rep, err := idempotent(ctx, tx, in.Meta, in.Command+":"+in.CurrencyID, func() (*domain.Currency, error) {
			return s.doTransition(ctx, tx, in, target)
		})
		if err != nil {
			return err
		}
		res, replayed = out, rep
		return nil
	})
	if err != nil {
		return nil, false, err
	}
	return res, replayed, nil
}

func (s *Service) doTransition(ctx context.Context, tx Tx, in TransitionInput, target domain.Status) (*domain.Currency, error) {
	c, err := tx.GetCurrencyByID(ctx, in.CurrencyID, true)
	if err != nil {
		return nil, err
	}
	if c == nil {
		return nil, domain.Errf(domain.CodeNotFound, "currency %s not found", in.CurrencyID)
	}
	if c.Version != in.ExpectedVersion {
		return nil, domain.Errf(domain.CodeVersionConflict, "expected_version %d does not match current version %d", in.ExpectedVersion, c.Version)
	}
	if err := domain.ValidateTransition(c.Status, target); err != nil {
		return nil, err
	}
	// SoD: whoever applied the import that introduced or last changed this
	// currency may not make it (or keep it) usable for the platform.
	if target == domain.StatusSupported && c.LastImportActor != "" && c.LastImportActor == in.Actor {
		return nil, domain.Errf(domain.CodeSoDDenied,
			"the actor who applied import %s may not activate this currency; a different actor must", c.LastImportID)
	}

	now := s.now()
	effective := now
	if target == domain.StatusRetired && in.EffectiveAt != nil {
		effective = in.EffectiveAt.UTC()
		if effective.Before(c.ValidFrom) || effective.After(now) {
			return nil, domain.Errf(domain.CodeContextInvalid, "effective_at must be within [valid_from, now]")
		}
	}

	prevStatus, prevVersion := c.Status, c.Version
	c.Status = target
	c.Version++
	c.RecordedAt = now
	if target == domain.StatusRetired {
		t := effective
		c.ValidTo = &t
	}
	if err := tx.UpdateCurrency(ctx, c, prevVersion); err != nil {
		if err == domain.ErrVersionConflictStore {
			return nil, domain.Errf(domain.CodeVersionConflict, "currency changed concurrently; re-read and retry")
		}
		return nil, err
	}
	if err := tx.InsertStatusHistory(ctx, &domain.StatusHistoryEntry{
		HistoryID: s.newID(), CurrencyID: c.CurrencyID, FromStatus: prevStatus, ToStatus: target,
		Actor: in.Actor, Approver: in.ApproverID, Reason: in.Reason, EffectiveAt: effective, RecordedAt: now,
		ResultingVersion: c.Version, CorrelationID: in.CorrelationID,
	}); err != nil {
		return nil, err
	}

	evType := events.EventCurrencySupportChanged
	if target == domain.StatusRetired {
		evType = events.EventCurrencyRetired
	}
	if err := s.enqueue(ctx, tx, events.Event{
		Type: evType, TenantID: in.TenantID, Scope: events.ScopeGlobal,
		ObjectType: events.ObjectTypeCurrency, ObjectID: c.CurrencyID, ObjectVersion: c.Version,
		EffectiveAt: effective, RecordedAt: now, Actor: in.Actor, CorrelationID: in.CorrelationID, CausationID: in.CausationID,
		Data: map[string]any{
			"alpha_code": c.AlphaCode, "numeric_code": c.NumericCode,
			"previous_status": string(prevStatus), "new_status": string(target),
			"reason": in.Reason, "approver": in.ApproverID,
		},
	}); err != nil {
		return nil, err
	}
	if err := s.attachMinorUnit(ctx, tx, c, now); err != nil {
		return nil, err
	}
	return c, nil
}

// TenantSupportInput is the tenant overlay command (enable / disable).
type TenantSupportInput struct {
	Meta
	// TargetTenantID is the tenant whose overlay changes. The handler has
	// already required it to equal Meta.TenantID.
	TargetTenantID string
	AlphaCode      string
	Enable         bool
	// ExpectedVersion is the overlay row's version; 0 means "no overlay row yet".
	ExpectedVersion int64
}

// SetTenantSupport enables or disables a currency for one tenant. A tenant may
// only enable a currency that is globally SUPPORTED.
func (s *Service) SetTenantSupport(ctx context.Context, in TenantSupportInput) (*domain.TenantSupport, bool, error) {
	if err := in.Meta.check(); err != nil {
		return nil, false, err
	}
	if in.TargetTenantID == "" || in.TargetTenantID != in.TenantID {
		return nil, false, domain.Errf(domain.CodeContextInvalid, "tenant overlay may only be changed within the caller's own tenant context")
	}
	if in.AlphaCode == "" || in.ExpectedVersion < 0 {
		return nil, false, domain.Errf(domain.CodeContextInvalid, "currency code and a non-negative expected_version are required")
	}
	op := "tenant-disable:"
	if in.Enable {
		op = "tenant-enable:"
	}

	var res *domain.TenantSupport
	var replayed bool
	err := s.store.InTx(ctx, in.TenantID, func(tx Tx) error {
		out, rep, err := idempotent(ctx, tx, in.Meta, op+in.AlphaCode, func() (*domain.TenantSupport, error) {
			return s.doTenantSupport(ctx, tx, in)
		})
		if err != nil {
			return err
		}
		res, replayed = out, rep
		return nil
	})
	if err != nil {
		return nil, false, err
	}
	return res, replayed, nil
}

func (s *Service) doTenantSupport(ctx context.Context, tx Tx, in TenantSupportInput) (*domain.TenantSupport, error) {
	cs, err := tx.FindCurrenciesByAlpha(ctx, in.AlphaCode)
	if err != nil {
		return nil, err
	}
	if len(cs) == 0 {
		return nil, domain.Errf(domain.CodeNotFound, "currency %q is not in the registry", in.AlphaCode)
	}
	c := pickCurrent(cs)
	if in.Enable {
		if c.Status == domain.StatusRetired {
			return nil, domain.Errf(domain.CodeReferenceRetired, "currency %q is retired and cannot be enabled for a tenant", in.AlphaCode)
		}
		if c.Status != domain.StatusSupported {
			return nil, domain.Errf(domain.CodeContextInvalid, "currency %q is %s, not globally SUPPORTED; a tenant cannot enable it", in.AlphaCode, c.Status)
		}
	}
	cur, err := tx.GetTenantSupport(ctx, in.TenantID, c.CurrencyID)
	if err != nil {
		return nil, err
	}
	var curVersion int64
	if cur != nil {
		curVersion = cur.Version
	}
	if in.ExpectedVersion != curVersion {
		return nil, domain.Errf(domain.CodeVersionConflict, "expected_version %d does not match current overlay version %d", in.ExpectedVersion, curVersion)
	}
	if in.Enable && cur != nil && cur.Enabled {
		return nil, domain.Errf(domain.CodeInvalidTransition, "currency %q is already enabled for this tenant", in.AlphaCode)
	}
	if !in.Enable && (cur == nil || !cur.Enabled) {
		return nil, domain.Errf(domain.CodeInvalidTransition, "currency %q is not enabled for this tenant", in.AlphaCode)
	}

	now := s.now()
	ts := &domain.TenantSupport{
		TenantID: in.TenantID, CurrencyID: c.CurrencyID, AlphaCode: c.AlphaCode, Enabled: in.Enable,
		Version: curVersion + 1, Reason: in.Reason, Actor: in.Actor, CreatedAt: now, UpdatedAt: now,
	}
	if cur != nil {
		ts.CreatedAt = cur.CreatedAt
	}
	if err := tx.UpsertTenantSupport(ctx, ts, curVersion); err != nil {
		if err == domain.ErrVersionConflictStore {
			return nil, domain.Errf(domain.CodeVersionConflict, "tenant overlay changed concurrently; re-read and retry")
		}
		return nil, err
	}
	if err := s.enqueue(ctx, tx, events.Event{
		Type: events.EventCurrencySupportChanged, TenantID: in.TenantID, Scope: events.ScopeTenant,
		ObjectType: events.ObjectTypeTenantCurrencySupport, ObjectID: c.CurrencyID, ObjectVersion: ts.Version,
		EffectiveAt: now, RecordedAt: now, Actor: in.Actor, CorrelationID: in.CorrelationID, CausationID: in.CausationID,
		Data: map[string]any{
			"alpha_code": c.AlphaCode, "numeric_code": c.NumericCode, "enabled": in.Enable,
			"currency_version": c.Version, "reason": in.Reason,
		},
	}); err != nil {
		return nil, err
	}
	return ts, nil
}
