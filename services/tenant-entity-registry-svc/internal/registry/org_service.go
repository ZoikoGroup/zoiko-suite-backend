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
	// A hostname→tenant mapping is a hard isolation identifier (ORG-02 §4.2
	// "tenant admin cannot change hard isolation identifiers"): it decides
	// which tenant every request on that host is resolved to. Evaluated in the
	// PLATFORM scope, so no tenant-scope grant — a tenant admin's — can make
	// one.
	if err := s.authorizeIn(ctx, s.platformScopeID, "tenant", "host-binding.create"); err != nil {
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

	// A body-supplied approver is refused outright, on EVERY command. On a
	// maker-checker command it was the self-asserted approval; on any other it
	// was worse — written into lifecycle history as an approver of record for
	// a command nobody approved. Checked before anything is read so the
	// refusal does not depend on the tenant's state.
	if s.legacyBodyApprover {
		if command.RequiresMakerChecker() {
			if err := s.legacyApproverCheck(req.ApprovedByPrincipalID, actor, string(command)); err != nil {
				return nil, err
			}
		}
	} else if strings.TrimSpace(req.ApprovedByPrincipalID) != "" {
		return nil, errBodyApprover
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

	// RetryProvisioning re-runs the failed provisioning step rather than
	// only moving the state.
	if command == domain.TenantCommandRetryProvisioning {
		return s.retryProvisioning(ctx, t, expected, req)
	}

	// §4.2 maker-checker on creation: the tenant may not leave ONBOARDING
	// until a second principal has approved its creation.
	if command == domain.TenantCommandActivate && t.LifecycleState == domain.TenantLifecycleOnboarding {
		if err := s.requireCreationApproval(ctx, t); err != nil {
			return nil, err
		}
	}

	// §4.2 maker-checker on termination: file, do not execute. The command
	// runs when a different verified principal approves it.
	if command.RequiresMakerChecker() && !s.legacyBodyApprover {
		req.ExpectedVersion = expected
		req.ApprovedByPrincipalID = ""
		a, err := s.propose(ctx, proposal{
			subjectType:     domain.ApprovalSubjectTenantCommand,
			tenantID:        tenantID,
			subjectID:       tenantID,
			command:         string(command),
			expectedVersion: expected,
			reason:          req.Reason,
			payload:         req,
			correlationID:   req.CorrelationID,
		})
		if err != nil {
			return nil, err
		}
		return nil, &PendingApprovalError{Request: a}
	}

	return s.applyTenantCommand(ctx, t, command, target, allowedFrom, expected, req, actor, nil)
}

// applyTenantCommand writes a validated lifecycle command.
//
// maker is the principal of record for the command. d, when non-nil, is the
// verified approval that released it: its decider becomes the approver of
// record and the approval is marked APPROVED in the same transaction. When d
// is nil the approver is the legacy body field, which is empty outside legacy
// mode for every command that reaches here.
func (s *Service) applyTenantCommand(
	ctx context.Context,
	t *domain.Tenant,
	command domain.TenantCommand,
	target domain.TenantLifecycleState,
	allowedFrom []domain.TenantLifecycleState,
	expected int64,
	req domain.ExecuteTenantCommandRequest,
	maker string,
	d *domain.ApprovalDecision,
) (*TenantCommandResult, error) {
	approver := req.ApprovedByPrincipalID
	approvalID := ""
	if d != nil {
		approver = d.DecidedByPrincipalID
		approvalID = d.ApprovalRequestID
	}

	ev, err := s.tenantCommandEvent(t, command, target, req, maker, approver, approvalID)
	if err != nil {
		return nil, err
	}

	res, err := s.store.ExecuteTenantCommand(ctx, TenantCommandParams{
		TenantID:        t.TenantID,
		Command:         command,
		TargetState:     target,
		AllowedFrom:     allowedFrom,
		ExpectedVersion: expected,
		Reason:          req.Reason,
		ActorID:         maker,
		ApprovedBy:      approver,
		CorrelationID:   req.CorrelationID,
		Approval:        d,
	}, ev)
	if err != nil {
		return nil, err
	}

	s.log.Info("tenant lifecycle command applied",
		zap.String("tenant_id", t.TenantID),
		zap.String("command", string(command)),
		zap.String("from", string(res.FromState)),
		zap.String("to", string(res.ToState)),
		zap.String("approval_request_id", approvalID),
		zap.String("correlation_id", req.CorrelationID),
	)
	return res, nil
}

// tenantCommandEvent renders the outbox event for a lifecycle command.
func (s *Service) tenantCommandEvent(
	t *domain.Tenant,
	command domain.TenantCommand,
	target domain.TenantLifecycleState,
	req domain.ExecuteTenantCommandRequest,
	maker, approver, approvalID string,
) (*outbox.Record, error) {
	eventType := events.TenantCommandEvent(string(command))
	if eventType == "" {
		return nil, nil
	}
	payload := map[string]any{
		"tenant_id":        t.TenantID,
		"tenant_code":      t.TenantCode,
		"command":          string(command),
		"from_state":       string(t.LifecycleState),
		"to_state":         string(target),
		"reason":           req.Reason,
		"approved_by":      approver,
		"previous_version": t.RecordVersion,
	}
	if approvalID != "" {
		payload["approval_request_id"] = approvalID
	}
	if t.OnboardingRequestRef != nil {
		payload["onboarding_request_ref"] = *t.OnboardingRequestRef
	}
	if t.ExternalCustomerKey != nil {
		payload["external_customer_key"] = *t.ExternalCustomerKey
	}
	return events.BuildRecord(events.RecordSpec{
		EventType:     eventType,
		TenantID:      t.TenantID,
		ActorID:       maker,
		CorrelationID: req.CorrelationID,
		PartitionKey:  t.TenantID,
		Payload:       payload,
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

	if req.EffectiveFrom.IsZero() {
		req.EffectiveFrom = time.Now().UTC()
	}
	if req.LEI != nil || req.LEISource != nil || req.LEIStatus != nil {
		if req.LEI != nil {
			v := strings.TrimSpace(*req.LEI)
			req.LEI = &v
		}
		if err := validateLEI(req.LEI, req.LEISource, req.LEIStatus); err != nil {
			return nil, err
		}
	}

	actor := domain.PrincipalFromContext(ctx)
	// As for tenant commands: the body approver is refused on every
	// amendment, SoD or not, so it can never become false evidence.
	if !s.legacyBodyApprover && strings.TrimSpace(req.ApprovedByPrincipalID) != "" {
		return nil, errBodyApprover
	}
	if req.RequiresApproval() {
		if s.legacyBodyApprover {
			if err := s.legacyApproverCheck(req.ApprovedByPrincipalID, actor, "legal name, registry number and jurisdiction changes"); err != nil {
				return nil, err
			}
		} else {
			return nil, s.proposeAmendment(ctx, e, req)
		}
	}

	return s.executeAmendment(ctx, e, req, actor, nil)
}

// proposeAmendment files a §4.3 SoD amendment for independent approval.
//
// The registry-collision probe runs now as well as at execution, so a
// proposal that could never apply is refused (and quarantined) before anyone
// is asked to review it.
func (s *Service) proposeAmendment(ctx context.Context, e *domain.LegalEntity, req domain.AmendLegalProfileRequest) error {
	expected := req.ExpectedVersion
	if expected == 0 {
		expected = e.RecordVersion
	} else if expected != e.RecordVersion {
		return fmt.Errorf("%w: you supplied expected_version %d but this entity is at %d — reload and retry",
			ErrVersionConflict, expected, e.RecordVersion)
	}
	if err := s.checkAmendmentRegistry(ctx, e, req); err != nil {
		return err
	}
	req.ExpectedVersion = expected
	req.ApprovedByPrincipalID = ""
	reason := req.SourceEvidenceRef
	if reason == "" {
		reason = string(req.ChangeReason)
	}
	a, err := s.propose(ctx, proposal{
		subjectType:     domain.ApprovalSubjectLegalProfileAmendment,
		tenantID:        e.TenantID,
		subjectID:       e.LegalEntityID,
		command:         amendmentCommandName(req.ChangeReason),
		expectedVersion: expected,
		reason:          reason,
		payload:         req,
		correlationID:   req.CorrelationID,
	})
	if err != nil {
		return err
	}
	return &PendingApprovalError{Request: a}
}

// amendmentCommandName is the §4.3 named command an amendment represents.
func amendmentCommandName(r domain.ProfileChangeReason) string {
	switch r {
	case domain.ProfileChangeLegalNameChange:
		return "ChangeLegalName"
	case domain.ProfileChangeRegisteredOfficeChange:
		return "ChangeRegisteredOffice"
	}
	return "AmendLegalProfile"
}

// checkAmendmentRegistry is §8 NP5 for amendments.
func (s *Service) checkAmendmentRegistry(ctx context.Context, e *domain.LegalEntity, req domain.AmendLegalProfileRequest) error {
	if req.RegistrationNumber == nil || *req.RegistrationNumber == "" {
		return nil
	}
	jur := e.PrimaryJurisdictionID
	if req.IncorporationJurisdictionID != nil && *req.IncorporationJurisdictionID != "" {
		jur = *req.IncorporationJurisdictionID
	}
	existing, err := s.store.FindActiveEntityByRegistry(ctx, *req.RegistrationNumber, jur)
	if err != nil {
		return fmt.Errorf("store.FindActiveEntityByRegistry: %w", err)
	}
	if existing != nil && existing.LegalEntityID != e.LegalEntityID {
		if qErr := s.quarantineRegistryConflict(ctx, e.TenantID, *req.RegistrationNumber, jur, existing.LegalEntityID,
			map[string]any{
				"attempted_by":    "AmendLegalProfile",
				"legal_entity_id": e.LegalEntityID,
				"legal_name":      derefOr(req.LegalName, e.LegalName),
			}, req.CorrelationID); qErr != nil {
			return qErr
		}
		return fmt.Errorf("%w: registration_number %s is held by entity %s",
			ErrRegistryConflict, *req.RegistrationNumber, existing.LegalEntityID)
	}
	return nil
}

// executeAmendment writes the next profile version.
//
// maker is the principal of record. d, when non-nil, is the verified approval
// that released the amendment; its decider is the approver of record and the
// approval is marked APPROVED in the same transaction as the new version.
func (s *Service) executeAmendment(
	ctx context.Context,
	e *domain.LegalEntity,
	req domain.AmendLegalProfileRequest,
	maker string,
	d *domain.ApprovalDecision,
) (*domain.LegalEntityProfileVersion, error) {
	legalEntityID := e.LegalEntityID
	approver := req.ApprovedByPrincipalID
	var approvalID *string
	if d != nil {
		approver = d.DecidedByPrincipalID
		approvalID = &d.ApprovalRequestID
	}

	// §8 NP5 applies to amendments too, and it is the more dangerous direction:
	// creating a duplicate is caught at creation, but AMENDING an entity onto a
	// registry number another active entity already holds reaches the same
	// invalid state by a different door. Re-run at execution because an
	// approved proposal can be released after another entity took the number.
	if err := s.checkAmendmentRegistry(ctx, e, req); err != nil {
		return nil, err
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
		CreatedByPrincipalID:  maker,
		ApprovedByPrincipalID: nullableString(approver),
		ApprovalRequestID:     approvalID,
		Approval:              d,
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
		next.LEI = p.LEI
		next.LEISource = p.LEISource
		next.LEIStatus = p.LEIStatus
		next.LEIVerifiedAt = p.LEIVerifiedAt
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
		ActorID:       maker,
		CorrelationID: req.CorrelationID,
		PartitionKey:  legalEntityID,
		Payload: map[string]any{
			"legal_entity_id":  legalEntityID,
			"entity_code":      e.EntityCode,
			"change_reason":    string(req.ChangeReason),
			"effective_from":   req.EffectiveFrom,
			"previous_name":    e.LegalName,
			"new_name":         next.LegalName,
			"approved_by":      approver,
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
	if req.LEI != nil {
		next.LEI = req.LEI
		next.LEISource = req.LEISource
		next.LEIStatus = req.LEIStatus
		next.LEIVerifiedAt = req.LEIVerifiedAt
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
	if s.legacyBodyApprover {
		return s.store.ResolveRegistryConflict(ctx, conflictID, req.Status,
			req.ResolutionNote, domain.PrincipalFromContext(ctx))
	}

	// §4.3 "no self-approval of merge". The resolution is filed, not applied;
	// a second principal — neither this resolver nor the one whose claim was
	// quarantined — releases it through /approve.
	c, err := s.store.GetRegistryConflict(ctx, conflictID)
	if err != nil {
		return err
	}
	if c == nil {
		return ErrNotFound
	}
	if c.Status != domain.RegistryConflictOpen {
		return fmt.Errorf("%w: conflict is already %s", ErrConflict, c.Status)
	}
	a, err := s.propose(ctx, proposal{
		subjectType:   domain.ApprovalSubjectRegistryConflictResolution,
		tenantID:      c.TenantID,
		subjectID:     c.ConflictID,
		command:       "ResolveRegistryConflict",
		reason:        req.ResolutionNote,
		payload:       req,
		correlationID: req.CorrelationID,
	})
	if err != nil {
		return err
	}
	return &PendingApprovalError{Request: a}
}

// derefOr returns *p, or fallback when p is nil.
func derefOr(p *string, fallback string) string {
	if p == nil {
		return fallback
	}
	return *p
}
