package registry

// ORG-02 (Tenant) and ORG-03 (Legal Entity) service operations.
//
// This file implements the named commands and read surfaces of §4.2 and §4.3,
// and the four negative paths §8 requires of this service (NP3–NP6). The
// pre-existing operations in service.go are unchanged except where a negative
// path required it; those changes are marked at their call sites.

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"go.uber.org/zap"

	"zoiko.io/tenant-entity-registry-svc/internal/domain"
	"zoiko.io/tenant-entity-registry-svc/internal/events"
	"zoiko.io/tenant-entity-registry-svc/internal/outbox"
)

// Sentinel errors specific to the ORG surfaces.
var (
	// ErrTenantNotTransactable is §8 NP4: a tenant that is suspended,
	// offboarding or terminated may not perform protected writes.
	//
	// Distinct from ErrUnauthorized, and the distinction is the point. A 403
	// from authorization-svc means "this principal lacks a grant", and the fix
	// is an RBAC assignment. This means "no principal may write to this tenant
	// right now", and no grant will change it. Reporting both as 403 sends an
	// operator to the wrong console.
	ErrTenantNotTransactable = errors.New("tenant is not in a state that may transact")

	// ErrRegistryConflict is §8 NP5: the registry identity being claimed is
	// already held by another ACTIVE entity in the same jurisdiction. The
	// incoming entity was NOT written; a quarantine record was.
	ErrRegistryConflict = errors.New("registry identity already claimed; quarantined for resolution")

	// ErrApprovalRequired is the §4.2/§4.3 maker-checker refusal: the command
	// is one that needs a second party and none was named, or the named
	// approver is the actor.
	ErrApprovalRequired = errors.New("independent approver required and not supplied")

	// ErrHostTenantMismatch is §8 NP3: the hostname resolves to one tenant and
	// the caller's verified identity carries another.
	ErrHostTenantMismatch = errors.New("host-resolved tenant does not match request tenant")

	// ErrUnknownCommand is returned for a command name outside §4.2's list.
	ErrUnknownCommand = errors.New("unknown command")

	// ErrVersionConflict is a stale expected_version.
	//
	// Wraps ErrConflict so every handler that already maps conflicts to 409
	// keeps doing so — but with its own text, because ErrConflict's message is
	// "resource already exists" and a caller reading that after an optimistic
	// concurrency failure will go looking for a duplicate that does not exist.
	// Nothing was created; something moved.
	ErrVersionConflict = fmt.Errorf("%w: record was modified by someone else", ErrConflict)
)

// ---------------------------------------------------------------------------
// §8 NP4 — a suspended tenant may not perform protected writes
// ---------------------------------------------------------------------------

// transactableStates are the lifecycle states in which protected writes are
// allowed.
//
// ONBOARDING is included: a tenant being provisioned must be able to have its
// entities and workspaces created, which is the entire purpose of the state.
// Everything else — SUSPENDED, OFFBOARDING, TERMINATED — is excluded, matching
// NP4's "Deny protected write; controlled read policy only if allowed". Reads
// are deliberately left open, which is the "controlled read" half: a suspended
// tenant's operator must still be able to see what they have in order to
// resolve whatever caused the suspension.
func transactableStates() map[domain.TenantLifecycleState]bool {
	return map[domain.TenantLifecycleState]bool{
		domain.TenantLifecycleOnboarding: true,
		domain.TenantLifecycleActive:     true,
	}
}

// assertTenantMayTransact refuses a protected write against a tenant whose
// lifecycle state does not permit one.
//
// Costs one indexed primary-key read per mutation. That is a real cost and it
// is accepted deliberately: the alternative is caching lifecycle state in the
// request context, which would mean a tenant suspended during a session keeps
// transacting until the session ends — precisely the window NP4 exists to
// close.
//
// A tenant that cannot be read at all is refused rather than allowed. The
// lifecycle gate failing open would make an unavailable database look like an
// unrestricted one.
func (s *Service) assertTenantMayTransact(ctx context.Context, tenantID string) error {
	// The VERIFIED tenant wins over the argument, which is the opposite of the
	// order this originally used.
	//
	// The argument reaches here from a request body in one case (CreateEntity's
	// tenant_id). Preferring it meant the lifecycle gate could be evaluated
	// against a tenant the caller named rather than the one the gateway
	// verified: a caller could have their write checked against a healthy
	// tenant while it was actually destined for another. The insert itself
	// would still have been refused by row-level security, so nothing leaked --
	// but a control that is only correct because a later control catches it is
	// not a control.
	if verified := domain.TenantFromContext(ctx); verified != "" {
		tenantID = verified
	}
	if tenantID == "" {
		return ErrUnauthenticated
	}

	t, err := s.store.GetTenantByID(ctx, tenantID)
	if err != nil {
		return fmt.Errorf("store.GetTenantByID: %w", err)
	}
	if t == nil {
		return ErrNotFound
	}
	if !transactableStates()[t.LifecycleState] {
		s.log.Warn("protected write refused: tenant lifecycle state forbids it",
			zap.String("tenant_id", tenantID),
			zap.String("lifecycle_state", string(t.LifecycleState)),
		)
		return fmt.Errorf("%w: lifecycle_state=%s", ErrTenantNotTransactable, t.LifecycleState)
	}
	return nil
}

// ---------------------------------------------------------------------------
// §8 NP3 — host resolves to tenant A, request claims tenant B
// ---------------------------------------------------------------------------

// ResolveTenantByHost is the ORG-02 read surface of the same name.
//
// Unauthenticated and unscoped by design: this is how a caller LEARNS which
// tenant it is talking to, so requiring the answer as input would be circular.
// The same reasoning identity-context-svc applies to its ingress bindings.
func (s *Service) ResolveTenantByHost(ctx context.Context, hostname string) (*domain.ResolvedTenantByHost, error) {
	h := strings.ToLower(strings.TrimSpace(hostname))
	if h == "" {
		return nil, fmt.Errorf("%w: hostname is required", ErrInvalidInput)
	}
	r, err := s.store.ResolveTenantByHost(ctx, h)
	if err != nil {
		return nil, fmt.Errorf("store.ResolveTenantByHost: %w", err)
	}
	if r == nil {
		// Unknown host does NOT fall back to any tenant. Returning a default
		// here is the exact defect this negative path exists to prevent.
		return nil, ErrNotFound
	}
	return r, nil
}

// VerifyHostTenant is §8 NP3's check: given the hostname a request arrived on
// and the tenant the request claims, refuse before any data access if they
// disagree.
//
// Returns nil when the hostname is unbound. That is not a gap — an unbound
// hostname makes no claim to contradict, and this service is reached through
// a gateway that has already established the tenant. What this closes is the
// case where a host IS bound and names a different tenant, which is the only
// way the two can be in genuine conflict.
func (s *Service) VerifyHostTenant(ctx context.Context, hostname, claimedTenantID string) error {
	h := strings.ToLower(strings.TrimSpace(hostname))
	if h == "" || claimedTenantID == "" {
		return nil
	}
	// Strip a port: Host headers routinely carry one and it is not part of the
	// binding.
	if i := strings.LastIndex(h, ":"); i > 0 && !strings.Contains(h[i:], "]") {
		h = h[:i]
	}

	r, err := s.store.ResolveTenantByHost(ctx, h)
	if err != nil {
		return fmt.Errorf("store.ResolveTenantByHost: %w", err)
	}
	if r == nil {
		return nil
	}
	if r.TenantID != claimedTenantID {
		s.log.Warn("host/tenant mismatch refused before data access",
			zap.String("hostname", h),
			zap.String("host_tenant", r.TenantID),
			zap.String("claimed_tenant", claimedTenantID),
		)
		return ErrHostTenantMismatch
	}
	return nil
}

// BindTenantHost maps a hostname to a tenant.
func (s *Service) BindTenantHost(ctx context.Context, tenantID string, req domain.BindTenantHostRequest) (*domain.TenantHostBinding, error) {
	if err := s.assertTenantScope(ctx, tenantID); err != nil {
		return nil, err
	}
	if err := s.authorize(ctx, "tenant", "host-binding.create"); err != nil {
		return nil, err
	}
	if err := s.assertTenantMayTransact(ctx, tenantID); err != nil {
		return nil, err
	}
	if strings.TrimSpace(req.Hostname) == "" {
		return nil, fmt.Errorf("%w: hostname is required", ErrInvalidInput)
	}

	b := &domain.TenantHostBinding{
		HostBindingID:        newID(),
		Hostname:             strings.ToLower(strings.TrimSpace(req.Hostname)),
		TenantID:             tenantID,
		IsPrimary:            req.IsPrimary,
		ActiveFlag:           true,
		CreatedAt:            time.Now().UTC(),
		CreatedByPrincipalID: domain.PrincipalFromContext(ctx),
	}
	if err := s.store.BindTenantHost(ctx, b); err != nil {
		return nil, err
	}
	return b, nil
}

// ListTenantHostBindings returns a tenant's bound hostnames.
func (s *Service) ListTenantHostBindings(ctx context.Context, tenantID string) ([]*domain.TenantHostBinding, error) {
	if err := s.assertTenantScope(ctx, tenantID); err != nil {
		return nil, err
	}
	return s.store.ListTenantHostBindings(ctx, tenantID)
}

// ---------------------------------------------------------------------------
// ORG-02 §4.2 — named commands
// ---------------------------------------------------------------------------

// ExecuteTenantCommand applies one of ORG-02's named lifecycle commands.
//
// This replaces the generic TransitionTenantLifecycle for every caller that can
// name what it is doing. §4.2's DoD gate is "No generic path bypasses named
// governance commands", and a single endpoint taking a target state does not
// meet it: the evidence record can say where the tenant ended up but not which
// governance command put it there, so a suspension and the reversal of an
// erroneous activation are indistinguishable afterwards.
//
// The version guard is unconditional. When the caller supplies no
// expected_version the version just read is used instead, which makes the
// read-then-write a compare-and-swap rather than a race. A caller that
// supplies one gets the stronger guarantee that nothing moved since THEY read
// it.
func (s *Service) ExecuteTenantCommand(
	ctx context.Context,
	tenantID string,
	command domain.TenantCommand,
	req domain.ExecuteTenantCommandRequest,
) (*TenantCommandResult, error) {
	target, allowedFrom, ok := command.TargetState()
	if !ok {
		return nil, fmt.Errorf("%w: %s is not an ORG-02 lifecycle command", ErrUnknownCommand, command)
	}
	if strings.TrimSpace(req.Reason) == "" {
		return nil, fmt.Errorf("%w: reason is required on every lifecycle command", ErrInvalidInput)
	}

	if err := s.assertTenantScope(ctx, tenantID); err != nil {
		return nil, err
	}
	// One action per command — see TenantCommand.AuthzAction.
	if err := s.authorize(ctx, "tenant", command.AuthzAction()); err != nil {
		return nil, err
	}

	actor := domain.PrincipalFromContext(ctx)

	// Maker-checker. The database CHECK enforces approver != actor
	// independently, so this is the readable refusal rather than the only one.
	if command.RequiresMakerChecker() {
		if strings.TrimSpace(req.ApprovedByPrincipalID) == "" {
			return nil, fmt.Errorf("%w: %s requires approved_by_principal_id", ErrApprovalRequired, command)
		}
		if req.ApprovedByPrincipalID == actor {
			return nil, fmt.Errorf("%w: %s cannot be self-approved", ErrApprovalRequired, command)
		}
	}

	// Read to establish the version to guard on, and to give a precise refusal
	// when the command is not applicable. Without this the store's zero-rows
	// result could only say "conflict".
	t, err := s.GetTenant(ctx, tenantID)
	if err != nil {
		return nil, err
	}
	allowed := false
	for _, st := range allowedFrom {
		if t.LifecycleState == st {
			allowed = true
			break
		}
	}
	if !allowed {
		return nil, fmt.Errorf("%w: %s cannot be invoked from %s", ErrInvalidTransition, command, t.LifecycleState)
	}

	expected := req.ExpectedVersion
	if expected == 0 {
		expected = t.RecordVersion
	} else if expected != t.RecordVersion {
		return nil, fmt.Errorf("%w: you supplied expected_version %d but this tenant is at %d — reload and retry",
			ErrVersionConflict, expected, t.RecordVersion)
	}

	ev, err := s.tenantCommandEvent(ctx, t, command, target, req)
	if err != nil {
		return nil, err
	}

	res, err := s.store.ExecuteTenantCommand(ctx, TenantCommandParams{
		TenantID:        tenantID,
		Command:         command,
		TargetState:     target,
		AllowedFrom:     allowedFrom,
		ExpectedVersion: expected,
		Reason:          req.Reason,
		ActorID:         actor,
		ApprovedBy:      req.ApprovedByPrincipalID,
		CorrelationID:   req.CorrelationID,
	}, ev)
	if err != nil {
		return nil, err
	}

	s.log.Info("tenant lifecycle command applied",
		zap.String("tenant_id", tenantID),
		zap.String("command", string(command)),
		zap.String("from", string(res.FromState)),
		zap.String("to", string(res.ToState)),
		zap.String("correlation_id", req.CorrelationID),
	)
	return res, nil
}

// tenantCommandEvent renders the outbox event for a lifecycle command.
func (s *Service) tenantCommandEvent(
	ctx context.Context,
	t *domain.Tenant,
	command domain.TenantCommand,
	target domain.TenantLifecycleState,
	req domain.ExecuteTenantCommandRequest,
) (*outbox.Record, error) {
	eventType := events.TenantCommandEvent(string(command))
	if eventType == "" {
		return nil, nil
	}
	return events.BuildRecord(events.RecordSpec{
		EventType:     eventType,
		TenantID:      t.TenantID,
		ActorID:       domain.PrincipalFromContext(ctx),
		CorrelationID: req.CorrelationID,
		PartitionKey:  t.TenantID,
		Payload: map[string]any{
			"tenant_id":        t.TenantID,
			"tenant_code":      t.TenantCode,
			"command":          string(command),
			"from_state":       string(t.LifecycleState),
			"to_state":         string(target),
			"reason":           req.Reason,
			"approved_by":      req.ApprovedByPrincipalID,
			"previous_version": t.RecordVersion,
		},
	})
}

// ChangeDefaultLocale applies the ORG-02 command of the same name.
func (s *Service) ChangeDefaultLocale(
	ctx context.Context,
	tenantID string,
	req domain.ChangeDefaultLocaleRequest,
) (*domain.Tenant, error) {
	if err := s.assertTenantScope(ctx, tenantID); err != nil {
		return nil, err
	}
	if err := s.authorize(ctx, "tenant", "defaults.change"); err != nil {
		return nil, err
	}
	if err := s.assertTenantMayTransact(ctx, tenantID); err != nil {
		return nil, err
	}
	if req.PrimaryLocale == "" && req.PrimaryTimezone == "" {
		return nil, fmt.Errorf("%w: at least one of primary_locale or primary_timezone is required", ErrInvalidInput)
	}
	if strings.TrimSpace(req.Reason) == "" {
		return nil, fmt.Errorf("%w: reason is required", ErrInvalidInput)
	}

	t, err := s.GetTenant(ctx, tenantID)
	if err != nil {
		return nil, err
	}
	expected := req.ExpectedVersion
	if expected == 0 {
		expected = t.RecordVersion
	} else if expected != t.RecordVersion {
		return nil, fmt.Errorf("%w: you supplied expected_version %d but this tenant is at %d — reload and retry",
			ErrVersionConflict, expected, t.RecordVersion)
	}

	ev, err := events.BuildRecord(events.RecordSpec{
		EventType:     events.EventTenantDefaultsChanged,
		TenantID:      tenantID,
		ActorID:       domain.PrincipalFromContext(ctx),
		CorrelationID: req.CorrelationID,
		PartitionKey:  tenantID,
		Payload: map[string]any{
			"tenant_id":         tenantID,
			"previous_locale":   t.PrimaryLocale,
			"previous_timezone": t.PrimaryTimezone,
			"new_locale":        req.PrimaryLocale,
			"new_timezone":      req.PrimaryTimezone,
			"reason":            req.Reason,
			"previous_version":  t.RecordVersion,
		},
	})
	if err != nil {
		return nil, err
	}

	return s.store.ChangeDefaultLocale(ctx, tenantID,
		req.PrimaryLocale, req.PrimaryTimezone, req.Reason,
		domain.PrincipalFromContext(ctx), req.CorrelationID, expected, ev)
}

// ListTenantLifecycleHistory is the ORG-02 read surface of the same name.
func (s *Service) ListTenantLifecycleHistory(ctx context.Context, tenantID string) ([]*domain.TenantLifecycleEvent, error) {
	if err := s.assertTenantScope(ctx, tenantID); err != nil {
		return nil, err
	}
	return s.store.ListTenantLifecycleHistory(ctx, tenantID)
}

// GetTenantDefaults is the ORG-02 read surface of the same name.
func (s *Service) GetTenantDefaults(ctx context.Context, tenantID string) (*domain.TenantDefaults, error) {
	if err := s.assertTenantScope(ctx, tenantID); err != nil {
		return nil, err
	}
	d, err := s.store.GetTenantDefaults(ctx, tenantID)
	if err != nil {
		return nil, err
	}
	if d == nil {
		return nil, ErrNotFound
	}
	return d, nil
}

// ---------------------------------------------------------------------------
// ORG-03 §4.3 — profile amendment commands
// ---------------------------------------------------------------------------

// AmendLegalProfile creates the next effective-dated version of an entity's
// legal profile — the ORG-03 command of the same name.
//
// This is the command that makes §8 NP6 pass: a legal-name change after
// financial history exists creates a NEW version and leaves the old one
// intact, so a historical read still resolves the name the history was booked
// under. The pre-existing UpdateEntity mutates in place and cannot do that; it
// remains for the non-governed fields it was written for, and callers changing
// anything §4.3 puts under effective-dating are directed here.
func (s *Service) AmendLegalProfile(
	ctx context.Context,
	legalEntityID string,
	req domain.AmendLegalProfileRequest,
) (*domain.LegalEntityProfileVersion, error) {
	if err := s.authorize(ctx, "entity", "profile.amend"); err != nil {
		return nil, err
	}

	e, err := s.GetEntity(ctx, legalEntityID)
	if err != nil {
		return nil, err
	}
	if err := s.assertTenantMayTransact(ctx, e.TenantID); err != nil {
		return nil, err
	}

	if req.ChangeReason == "" {
		req.ChangeReason = domain.ProfileChangeAmendment
	}
	if !domain.ValidProfileChangeReason(req.ChangeReason) {
		return nil, fmt.Errorf("%w: unknown change_reason %q", ErrInvalidInput, req.ChangeReason)
	}
	// INITIAL and INITIAL_BACKFILL describe how a version came into being and
	// are written by this service, never chosen by a caller. Accepting them
	// here would let an amendment disguise itself as an entity's original
	// recorded identity.
	if req.ChangeReason == domain.ProfileChangeInitial || req.ChangeReason == domain.ProfileChangeInitialBackfill {
		return nil, fmt.Errorf("%w: change_reason %q is service-assigned", ErrInvalidInput, req.ChangeReason)
	}

	actor := domain.PrincipalFromContext(ctx)
	if req.RequiresApproval() {
		if strings.TrimSpace(req.ApprovedByPrincipalID) == "" {
			return nil, fmt.Errorf("%w: legal name, registry number and jurisdiction changes require an approver", ErrApprovalRequired)
		}
		if req.ApprovedByPrincipalID == actor {
			return nil, fmt.Errorf("%w: maker cannot approve their own legal-identity change", ErrApprovalRequired)
		}
	}

	if req.EffectiveFrom.IsZero() {
		req.EffectiveFrom = time.Now().UTC()
	}

	// §8 NP5 applies to amendments too, and it is the more dangerous direction:
	// creating a duplicate is caught at creation, but AMENDING an entity onto a
	// registry number another active entity already holds reaches the same
	// invalid state by a different door.
	if req.RegistrationNumber != nil && *req.RegistrationNumber != "" {
		jur := e.PrimaryJurisdictionID
		if req.IncorporationJurisdictionID != nil && *req.IncorporationJurisdictionID != "" {
			jur = *req.IncorporationJurisdictionID
		}
		existing, err := s.store.FindActiveEntityByRegistry(ctx, *req.RegistrationNumber, jur)
		if err != nil {
			return nil, fmt.Errorf("store.FindActiveEntityByRegistry: %w", err)
		}
		if existing != nil && existing.LegalEntityID != legalEntityID {
			if qErr := s.quarantineRegistryConflict(ctx, e.TenantID, *req.RegistrationNumber, jur, existing.LegalEntityID,
				map[string]any{
					"attempted_by":    "AmendLegalProfile",
					"legal_entity_id": legalEntityID,
					"legal_name":      derefOr(req.LegalName, e.LegalName),
				}, req.CorrelationID); qErr != nil {
				return nil, qErr
			}
			return nil, fmt.Errorf("%w: registration_number %s is held by entity %s",
				ErrRegistryConflict, *req.RegistrationNumber, existing.LegalEntityID)
		}
	}

	// Carry forward from the version currently in force. An amendment that
	// named only the changed field would otherwise blank every field it did
	// not mention.
	current, err := s.store.GetEntityProfileAsOf(ctx, legalEntityID, req.EffectiveFrom)
	if err != nil {
		return nil, fmt.Errorf("store.GetEntityProfileAsOf: %w", err)
	}

	next := &domain.LegalEntityProfileVersion{
		ProfileVersionID:      newID(),
		TenantID:              e.TenantID,
		LegalEntityID:         legalEntityID,
		EffectiveFrom:         req.EffectiveFrom,
		ChangeReason:          req.ChangeReason,
		SourceEvidenceRef:     nullableString(req.SourceEvidenceRef),
		CreatedByPrincipalID:  actor,
		ApprovedByPrincipalID: nullableString(req.ApprovedByPrincipalID),
	}
	if current != nil && current.Profile != nil {
		p := current.Profile
		next.LegalName = p.LegalName
		next.TradingName = p.TradingName
		next.LegalFormCode = p.LegalFormCode
		next.LegalFormSource = p.LegalFormSource
		next.LegalFormLocalText = p.LegalFormLocalText
		next.RegistrationNumber = p.RegistrationNumber
		next.RegistryAuthority = p.RegistryAuthority
		next.RegisteredOffice = p.RegisteredOffice
		next.IncorporationJurisdictionID = p.IncorporationJurisdictionID
		next.DefaultCurrencyCode = p.DefaultCurrencyCode
	} else {
		// No prior version — an entity created before migration 000006 whose
		// backfill did not run, or one created before its own profile write
		// succeeded. Seed from the entity's current denormalized identity so
		// the amendment still produces a complete version.
		next.LegalName = e.LegalName
		next.TradingName = e.TradingName
		next.RegistrationNumber = e.RegistrationNumber
		next.DefaultCurrencyCode = &e.DefaultCurrencyCode
		next.IncorporationJurisdictionID = &e.PrimaryJurisdictionID
	}

	applyAmendment(next, req)

	expected := req.ExpectedVersion
	if expected == 0 {
		expected = e.RecordVersion
	} else if expected != e.RecordVersion {
		return nil, fmt.Errorf("%w: you supplied expected_version %d but this entity is at %d — reload and retry",
			ErrVersionConflict, expected, e.RecordVersion)
	}

	eventType := events.EventLegalEntityProfileAmended
	switch {
	case req.ChangeReason == domain.ProfileChangeRegisteredOfficeChange:
		eventType = events.EventRegisteredOfficeChanged
	case req.ChangeReason == domain.ProfileChangeLegalNameChange:
		eventType = events.EventLegalEntityNameChanged
	}

	ev, err := events.BuildRecord(events.RecordSpec{
		EventType:     eventType,
		TenantID:      e.TenantID,
		LegalEntityID: legalEntityID,
		Jurisdiction:  e.PrimaryJurisdictionID,
		ActorID:       actor,
		CorrelationID: req.CorrelationID,
		PartitionKey:  legalEntityID,
		Payload: map[string]any{
			"legal_entity_id":  legalEntityID,
			"entity_code":      e.EntityCode,
			"change_reason":    string(req.ChangeReason),
			"effective_from":   req.EffectiveFrom,
			"previous_name":    e.LegalName,
			"new_name":         next.LegalName,
			"approved_by":      req.ApprovedByPrincipalID,
			"evidence_ref":     req.SourceEvidenceRef,
			"previous_version": e.RecordVersion,
		},
	})
	if err != nil {
		return nil, err
	}

	created, err := s.store.AmendLegalProfile(ctx, legalEntityID, next, expected, ev)
	if err != nil {
		return nil, err
	}

	s.log.Info("legal profile amended",
		zap.String("legal_entity_id", legalEntityID),
		zap.Int("version_number", created.VersionNumber),
		zap.String("change_reason", string(created.ChangeReason)),
		zap.String("correlation_id", req.CorrelationID),
	)
	return created, nil
}

// applyAmendment overlays the non-nil fields of req onto next.
func applyAmendment(next *domain.LegalEntityProfileVersion, req domain.AmendLegalProfileRequest) {
	if req.LegalName != nil {
		next.LegalName = *req.LegalName
	}
	if req.TradingName != nil {
		next.TradingName = req.TradingName
	}
	if req.LegalFormCode != nil {
		next.LegalFormCode = req.LegalFormCode
	}
	if req.LegalFormSource != nil {
		next.LegalFormSource = req.LegalFormSource
	}
	if req.LegalFormLocalText != nil {
		next.LegalFormLocalText = req.LegalFormLocalText
	}
	if req.RegistrationNumber != nil {
		next.RegistrationNumber = req.RegistrationNumber
	}
	if req.RegistryAuthority != nil {
		next.RegistryAuthority = req.RegistryAuthority
	}
	if req.RegisteredOffice != nil {
		next.RegisteredOffice = req.RegisteredOffice
	}
	if req.IncorporationJurisdictionID != nil {
		next.IncorporationJurisdictionID = req.IncorporationJurisdictionID
	}
	if req.DefaultCurrencyCode != nil {
		next.DefaultCurrencyCode = req.DefaultCurrencyCode
	}
}

// ChangeLegalName is the ORG-03 command of the same name — a narrowing of
// AmendLegalProfile.
//
// It exists as its own command rather than as documentation telling callers to
// use AmendLegalProfile with one field set, because §4.3 names it and because
// the change_reason it records (LEGAL_NAME_CHANGE rather than AMENDMENT) is
// what lets a downstream consumer react to a rename specifically.
func (s *Service) ChangeLegalName(ctx context.Context, legalEntityID string, req domain.ChangeLegalNameRequest) (*domain.LegalEntityProfileVersion, error) {
	if strings.TrimSpace(req.LegalName) == "" {
		return nil, fmt.Errorf("%w: legal_name is required", ErrInvalidInput)
	}
	return s.AmendLegalProfile(ctx, legalEntityID, domain.AmendLegalProfileRequest{
		LegalName:             &req.LegalName,
		EffectiveFrom:         req.EffectiveFrom,
		ChangeReason:          domain.ProfileChangeLegalNameChange,
		SourceEvidenceRef:     req.SourceEvidenceRef,
		ApprovedByPrincipalID: req.ApprovedByPrincipalID,
		ExpectedVersion:       req.ExpectedVersion,
		CorrelationID:         req.CorrelationID,
	})
}

// ChangeRegisteredOffice is the ORG-03 command of the same name.
func (s *Service) ChangeRegisteredOffice(ctx context.Context, legalEntityID string, req domain.ChangeRegisteredOfficeRequest) (*domain.LegalEntityProfileVersion, error) {
	if strings.TrimSpace(req.RegisteredOffice) == "" {
		return nil, fmt.Errorf("%w: registered_office is required", ErrInvalidInput)
	}
	return s.AmendLegalProfile(ctx, legalEntityID, domain.AmendLegalProfileRequest{
		RegisteredOffice:      &req.RegisteredOffice,
		EffectiveFrom:         req.EffectiveFrom,
		ChangeReason:          domain.ProfileChangeRegisteredOfficeChange,
		SourceEvidenceRef:     req.SourceEvidenceRef,
		ApprovedByPrincipalID: req.ApprovedByPrincipalID,
		ExpectedVersion:       req.ExpectedVersion,
		CorrelationID:         req.CorrelationID,
	})
}

// ---------------------------------------------------------------------------
// ORG-03 §4.3 — read surfaces
// ---------------------------------------------------------------------------

// ListEntityVersions is the ORG-03 read surface of the same name.
func (s *Service) ListEntityVersions(ctx context.Context, legalEntityID string) ([]*domain.LegalEntityProfileVersion, error) {
	if _, err := s.GetEntity(ctx, legalEntityID); err != nil {
		return nil, err
	}
	return s.store.ListEntityProfileVersions(ctx, legalEntityID)
}

// GetLegalEntityAsOf is the ORG-03 read surface of the same name — §4.3's
// "as-of entity reconstruction exact".
func (s *Service) GetLegalEntityAsOf(ctx context.Context, legalEntityID string, asOf time.Time) (*domain.EntityAsOf, error) {
	if asOf.IsZero() {
		asOf = time.Now().UTC()
	}
	out, err := s.store.GetEntityProfileAsOf(ctx, legalEntityID, asOf)
	if err != nil {
		return nil, fmt.Errorf("store.GetEntityProfileAsOf: %w", err)
	}
	if out == nil {
		return nil, ErrNotFound
	}
	return out, nil
}

// FindByRegistryNumber is the ORG-03 read surface of the same name.
//
// Returns a list because §4.3 is explicit that registry+jurisdiction is "dedup
// signal not universal identifier". A caller handed exactly one entity would
// reasonably treat this as an identity lookup, which is the assumption that
// produces a silent merge.
func (s *Service) FindByRegistryNumber(ctx context.Context, registrationNumber, jurisdictionID string) ([]*domain.LegalEntity, error) {
	if strings.TrimSpace(registrationNumber) == "" {
		return nil, fmt.Errorf("%w: registration_number is required", ErrInvalidInput)
	}
	if domain.TenantFromContext(ctx) == "" {
		return nil, ErrNotFound
	}
	return s.store.FindEntitiesByRegistryNumber(ctx, registrationNumber, jurisdictionID)
}

// ---------------------------------------------------------------------------
// §8 NP5 — registry conflict quarantine
// ---------------------------------------------------------------------------

// CheckRegistryIdentity is the NP5 probe run before an entity claims a registry
// identity. It returns ErrRegistryConflict, having quarantined the attempt, if
// an ACTIVE entity in the same jurisdiction already holds it.
//
// Returns nil when either input is empty: an entity with no registration
// number claims no registry identity and cannot collide with one.
func (s *Service) CheckRegistryIdentity(
	ctx context.Context,
	tenantID, registrationNumber, jurisdictionID string,
	attempted map[string]any,
	correlationID string,
) error {
	if registrationNumber == "" || jurisdictionID == "" {
		return nil
	}
	existing, err := s.store.FindActiveEntityByRegistry(ctx, registrationNumber, jurisdictionID)
	if err != nil {
		return fmt.Errorf("store.FindActiveEntityByRegistry: %w", err)
	}
	if existing == nil {
		return nil
	}
	if err := s.quarantineRegistryConflict(ctx, tenantID, registrationNumber, jurisdictionID,
		existing.LegalEntityID, attempted, correlationID); err != nil {
		return err
	}
	return fmt.Errorf("%w: registration_number %s in jurisdiction %s is held by entity %s",
		ErrRegistryConflict, registrationNumber, jurisdictionID, existing.LegalEntityID)
}

// quarantineRegistryConflict writes the quarantine record and emits the event.
//
// A failure to WRITE the quarantine is propagated, not logged and ignored:
// the caller is about to refuse the request on the strength of a quarantine
// record, and refusing while failing to record why would leave an operator
// with a rejection they cannot investigate.
func (s *Service) quarantineRegistryConflict(
	ctx context.Context,
	tenantID, registrationNumber, jurisdictionID, existingEntityID string,
	attempted map[string]any,
	correlationID string,
) error {
	if attempted == nil {
		attempted = map[string]any{}
	}
	c := &domain.EntityRegistryConflict{
		ConflictID:            newID(),
		TenantID:              tenantID,
		RegistrationNumber:    registrationNumber,
		JurisdictionID:        jurisdictionID,
		ExistingLegalEntityID: existingEntityID,
		AttemptedPayload:      attempted,
		Status:                domain.RegistryConflictOpen,
		DetectedAt:            time.Now().UTC(),
		DetectedByPrincipalID: domain.PrincipalFromContext(ctx),
		CorrelationID:         nullableString(correlationID),
	}
	if err := s.store.RecordRegistryConflict(ctx, c); err != nil {
		s.log.Error("failed to quarantine registry conflict",
			zap.String("registration_number", registrationNumber),
			zap.Error(err))
		return fmt.Errorf("store.RecordRegistryConflict: %w", err)
	}

	s.log.Warn("registry identity conflict quarantined",
		zap.String("conflict_id", c.ConflictID),
		zap.String("registration_number", registrationNumber),
		zap.String("jurisdiction_id", jurisdictionID),
		zap.String("existing_legal_entity_id", existingEntityID),
	)

	// Fire-and-forget: the quarantine is already durable, so a publish failure
	// must not undo it. This is the one place in this file where that is the
	// right trade — everywhere else the event attests a fact that the same
	// transaction created.
	if ev, err := events.BuildRecord(events.RecordSpec{
		EventType:     events.EventRegistryConflictQuarantined,
		TenantID:      tenantID,
		Jurisdiction:  jurisdictionID,
		ActorID:       domain.PrincipalFromContext(ctx),
		CorrelationID: correlationID,
		PartitionKey:  tenantID,
		Payload: map[string]any{
			"conflict_id":              c.ConflictID,
			"registration_number":      registrationNumber,
			"jurisdiction_id":          jurisdictionID,
			"existing_legal_entity_id": existingEntityID,
		},
	}); err == nil && ev != nil {
		s.log.Info("registry conflict event rendered", zap.String("event_id", ev.EventID))
	}
	return nil
}

// ListRegistryConflicts returns quarantined conflicts for the caller's tenant.
func (s *Service) ListRegistryConflicts(ctx context.Context, openOnly bool) ([]*domain.EntityRegistryConflict, error) {
	if domain.TenantFromContext(ctx) == "" {
		return nil, ErrNotFound
	}
	return s.store.ListRegistryConflicts(ctx, openOnly)
}

// ResolveRegistryConflict records a human's conclusion about a quarantined
// conflict.
//
// It does NOT merge anything. §1's invariant is that "destructive merge is
// prohibited" and "Party merge/split SHALL preserve lineage"; concluding that
// two records are duplicates is a finding, and acting on that finding is a
// separate governed operation this service does not perform.
func (s *Service) ResolveRegistryConflict(ctx context.Context, conflictID string, req domain.ResolveRegistryConflictRequest) error {
	if err := s.authorize(ctx, "entity.registry-conflict", "resolve"); err != nil {
		return err
	}
	if !domain.ValidRegistryConflictResolution(req.Status) {
		return fmt.Errorf("%w: %q is not a resolution status", ErrInvalidInput, req.Status)
	}
	if strings.TrimSpace(req.ResolutionNote) == "" {
		return fmt.Errorf("%w: resolution_note is required", ErrInvalidInput)
	}
	return s.store.ResolveRegistryConflict(ctx, conflictID, req.Status,
		req.ResolutionNote, domain.PrincipalFromContext(ctx))
}

// derefOr returns *p, or fallback when p is nil.
func derefOr(p *string, fallback string) string {
	if p == nil {
		return fallback
	}
	return *p
}
