package registry

// The remaining ORG-02 / ORG-03 gaps from the 23 Sep 2026 audit (migration
// 000008): onboarding idempotency and evidence, FailedProvisioning with
// compensating cleanup, LEI, Draft → Verified → Active, non-destructive
// merge, the hard-isolation-identifier guard and sensitive-identifier scoping.

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"go.uber.org/zap"

	"zoiko.io/tenant-entity-registry-svc/internal/domain"
	"zoiko.io/tenant-entity-registry-svc/internal/envelope"
	"zoiko.io/tenant-entity-registry-svc/internal/events"
	"zoiko.io/tenant-entity-registry-svc/internal/outbox"
)

var (
	// ErrOnboardingKeyExists is the store's signal that a provisioning request
	// reused an onboarding key. The service turns it into a replay or a 409.
	ErrOnboardingKeyExists = errors.New("onboarding key already used")

	// ErrOnboardingKeyRequired — §4.2 "Create by approved onboarding
	// correlation/external customer key". 422: well-formed, but not creatable
	// as submitted.
	ErrOnboardingKeyRequired = errors.New("external_customer_key is required: tenant creation is keyed by the approved onboarding correlation / external customer key")

	// ErrEntityNotOperational — the entity is DRAFT, VERIFIED or merged into
	// another, and may not yet (or any longer) be transacted against. 409, for
	// the same reason ErrTenantNotTransactable is: no grant will change it.
	ErrEntityNotOperational = errors.New("legal entity is not operational")
)

// ConfigureCompatibility sets the two dev-only migration aids for breaking
// changes in 000008. Config refuses both in staging and production.
//
// entityCreateActive restores entities created straight into ACTIVE (no
// Draft → Verified gate). onboardingKeyOptional lets ProvisionTenant run
// without external_customer_key, deduplicating only when one is sent.
func (s *Service) ConfigureCompatibility(entityCreateActive, onboardingKeyOptional bool) {
	s.legacyEntityCreateActive = entityCreateActive
	s.onboardingKeyOptional = onboardingKeyOptional
	if entityCreateActive {
		s.log.Warn("LEGACY_ENTITY_CREATE_ACTIVE is on: legal entities skip Draft → Verified. Development use only.")
	}
	if onboardingKeyOptional {
		s.log.Warn("ONBOARDING_KEY_OPTIONAL is on: tenants may be provisioned without an onboarding key. Development use only.")
	}
}

// ---------------------------------------------------------------------------
// ORG-02 — provisioning: idempotency, evidence, FailedProvisioning
// ---------------------------------------------------------------------------

// replayProvisioning answers a provisioning request whose onboarding key was
// already used: the original tenant for the same request, 409 for a different
// one.
func (s *Service) replayProvisioning(ctx context.Context, req domain.ProvisionTenantRequest, fingerprint string) (*domain.Tenant, error) {
	tenantID, storedFP, err := s.store.ResolveOnboardingKey(ctx, req.ExternalCustomerKey)
	if err != nil {
		return nil, fmt.Errorf("store.ResolveOnboardingKey: %w", err)
	}
	if tenantID == "" {
		// The key collided and then vanished — cannot happen without a
		// delete, which nothing performs. Refuse rather than guess.
		return nil, fmt.Errorf("%w: onboarding key collided but resolves to nothing", ErrConflict)
	}
	if storedFP != fingerprint {
		return nil, fmt.Errorf("%w: external_customer_key %q was already used for a different onboarding request",
			ErrConflict, req.ExternalCustomerKey)
	}
	tctx := domain.WithTenant(ctx, tenantID)
	t, err := s.store.GetTenantByID(tctx, tenantID)
	if err != nil {
		return nil, fmt.Errorf("store.GetTenantByID: %w", err)
	}
	if t == nil {
		return nil, ErrNotFound
	}
	if a, err := s.store.LatestApprovalForSubject(tctx, domain.ApprovalSubjectTenantCreation, tenantID); err == nil && a != nil {
		t.CreationApprovalRequestID = &a.ApprovalRequestID
	}
	t.IdempotentReplay = true
	s.log.Info("provisioning replay returned the original tenant",
		zap.String("tenant_id", tenantID))
	return t, nil
}

// tenantCreatedEvent is tenant.created, now enqueued transactionally with the
// creation approval instead of fired and forgotten after commit.
func tenantCreatedEvent(t *domain.Tenant, correlationID string) (*outbox.Record, error) {
	return events.BuildRecord(events.RecordSpec{
		EventType:     events.EventTenantCreated,
		TenantID:      t.TenantID,
		ActorID:       t.CreatedByPrincipalID,
		CorrelationID: correlationID,
		PartitionKey:  t.TenantID,
		Payload: map[string]any{
			"tenant_id":              t.TenantID,
			"tenant_code":            t.TenantCode,
			"legal_name":             t.LegalName,
			"lifecycle_state":        t.LifecycleState,
			"external_customer_key":  t.ExternalCustomerKey,
			"onboarding_request_ref": t.OnboardingRequestRef,
		},
	})
}

// creationApproval builds (without filing) the tenant's creation approval.
func (s *Service) creationApproval(ctx context.Context, t *domain.Tenant, correlationID string) (*domain.ApprovalRequest, error) {
	return s.buildProposal(ctx, proposal{
		subjectType: domain.ApprovalSubjectTenantCreation,
		tenantID:    t.TenantID,
		subjectID:   t.TenantID,
		command:     string(domain.TenantCommandCreate),
		reason:      "ORG-02 tenant creation requires independent approval before activation",
		payload: domain.TenantCreationPayload{
			TenantCode:           t.TenantCode,
			LegalName:            t.LegalName,
			TradingName:          t.TradingName,
			DefaultCurrencyCode:  t.DefaultCurrencyCode,
			ExternalCustomerKey:  t.ExternalCustomerKey,
			OnboardingRequestRef: t.OnboardingRequestRef,
		},
		correlationID: correlationID,
		requestedBy:   t.CreatedByPrincipalID,
	})
}

// completeProvisioning runs the follow-on provisioning step. On failure the
// tenant is marked FAILED_PROVISIONING — §4.2 "never expose partially isolated
// tenant as Active" — rather than left looking like one still in progress.
func (s *Service) completeProvisioning(ctx context.Context, t *domain.Tenant, correlationID string) {
	ctx = domain.WithTenant(ctx, t.TenantID)
	var a *domain.ApprovalRequest
	var stepErr error
	if !s.legacyBodyApprover {
		a, stepErr = s.creationApproval(ctx, t, correlationID)
	}
	var ev *outbox.Record
	if stepErr == nil {
		ev, stepErr = tenantCreatedEvent(t, correlationID)
	}
	if stepErr == nil {
		stepErr = s.store.CompleteProvisioning(ctx, ProvisioningCompletion{
			TenantID: t.TenantID, Approval: a, Event: ev, CorrelationID: correlationID,
		})
	}
	if stepErr == nil {
		if a != nil {
			t.CreationApprovalRequestID = &a.ApprovalRequestID
		}
		return
	}

	reason := stepErr.Error()
	s.log.Error("tenant provisioning step failed; tenant marked FAILED_PROVISIONING",
		zap.String("tenant_id", t.TenantID), zap.Error(stepErr))
	if err := s.store.MarkProvisioningFailed(ctx, t.TenantID, reason, t.CreatedByPrincipalID); err != nil {
		// The tenant stays ONBOARDING. The activation gate still refuses it
		// (no creation approval exists), so it is not exposed as Active.
		s.log.Error("could not mark tenant FAILED_PROVISIONING", zap.String("tenant_id", t.TenantID), zap.Error(err))
		return
	}
	now := time.Now().UTC()
	t.LifecycleState = domain.TenantLifecycleFailedProvisioning
	t.ProvisioningFailureReason = &reason
	t.ProvisioningFailedAt = &now
	t.RecordVersion++
}

// retryProvisioning is the RetryProvisioning command: the failed step again,
// and FAILED_PROVISIONING → ONBOARDING, atomically.
func (s *Service) retryProvisioning(ctx context.Context, t *domain.Tenant, expected int64, req domain.ExecuteTenantCommandRequest) (*TenantCommandResult, error) {
	var a *domain.ApprovalRequest
	var err error
	if !s.legacyBodyApprover {
		if a, err = s.creationApproval(ctx, t, req.CorrelationID); err != nil {
			return nil, err
		}
	}
	ev, err := tenantCreatedEvent(t, req.CorrelationID)
	if err != nil {
		return nil, err
	}
	if err := s.store.CompleteProvisioning(ctx, ProvisioningCompletion{
		TenantID: t.TenantID, Approval: a, Event: ev, FromFailed: true,
		ExpectedVersion: expected, ActorID: domain.PrincipalFromContext(ctx),
		Reason: req.Reason, CorrelationID: req.CorrelationID,
	}); err != nil {
		return nil, err
	}
	return &TenantCommandResult{
		FromState: domain.TenantLifecycleFailedProvisioning, ToState: domain.TenantLifecycleOnboarding,
		NewVersion: expected + 1, Status: t.Status,
	}, nil
}

// ---------------------------------------------------------------------------
// ORG-03 — LEI
// ---------------------------------------------------------------------------

// validateLEI enforces the ORG-03 control: an LEI is well-formed ISO 17442
// and carried with its source and GLEIF status.
func validateLEI(lei, source, status *string) error {
	if lei == nil {
		if source != nil || status != nil {
			return fmt.Errorf("%w: lei_source and lei_status describe an LEI and cannot be set without one", ErrInvalidInput)
		}
		return nil
	}
	if !domain.ValidLEI(*lei) {
		return fmt.Errorf("%w: %q is not a valid ISO 17442 LEI (20 characters, MOD 97-10 check digits)", ErrInvalidInput, *lei)
	}
	if source == nil || strings.TrimSpace(*source) == "" {
		return fmt.Errorf("%w: an LEI must carry lei_source", ErrInvalidInput)
	}
	if status == nil || !domain.ValidLEIStatus(domain.LEIStatus(*status)) {
		return fmt.Errorf("%w: an LEI must carry a GLEIF lei_status (ISSUED, LAPSED, RETIRED, …)", ErrInvalidInput)
	}
	return nil
}

// ---------------------------------------------------------------------------
// ORG-03 — Draft → Verified → Active
// ---------------------------------------------------------------------------

// assertEntityOperational refuses to transact against an entity that is not
// yet verified and activated, or that has been merged into another.
func (s *Service) assertEntityOperational(ctx context.Context, legalEntityID string) error {
	if legalEntityID == "" {
		return nil
	}
	e, err := s.GetEntity(ctx, legalEntityID)
	if err != nil {
		return err
	}
	switch {
	case e.MergedIntoLegalEntityID != nil:
		return fmt.Errorf("%w: entity %s has been merged into %s", ErrEntityNotOperational, e.LegalEntityID, *e.MergedIntoLegalEntityID)
	case e.EntityStatus == domain.EntityStatusDraft, e.EntityStatus == domain.EntityStatusVerified:
		return fmt.Errorf("%w: entity %s is %s — it must be verified and activated first", ErrEntityNotOperational, e.LegalEntityID, e.EntityStatus)
	}
	return nil
}

func entityExpectedVersion(e *domain.LegalEntity, supplied int64) (int64, error) {
	if supplied == 0 {
		return e.RecordVersion, nil
	}
	if supplied != e.RecordVersion {
		return 0, fmt.Errorf("%w: you supplied expected_version %d but this entity is at %d — reload and retry",
			ErrVersionConflict, supplied, e.RecordVersion)
	}
	return supplied, nil
}

func entityStatusEvent(e *domain.LegalEntity, actor, correlationID string, from, to domain.EntityStatus, extra map[string]any) (*outbox.Record, error) {
	payload := map[string]any{
		"legal_entity_id": e.LegalEntityID,
		"entity_code":     e.EntityCode,
		"from_status":     string(from),
		"to_status":       string(to),
	}
	for k, v := range extra {
		payload[k] = v
	}
	return events.BuildRecord(events.RecordSpec{
		EventType:     events.EventLegalEntityStatusChanged,
		TenantID:      e.TenantID,
		LegalEntityID: e.LegalEntityID,
		Jurisdiction:  e.PrimaryJurisdictionID,
		ActorID:       actor,
		CorrelationID: correlationID,
		PartitionKey:  e.LegalEntityID,
		Payload:       payload,
	})
}

// RequestEntityVerification files VerifyLegalEntity for independent approval.
func (s *Service) RequestEntityVerification(ctx context.Context, legalEntityID string, req domain.RequestEntityVerificationRequest) error {
	if err := s.authorize(ctx, "entity", "verify"); err != nil {
		return err
	}
	if strings.TrimSpace(req.VerificationEvidenceRef) == "" {
		return fmt.Errorf("%w: verification_evidence_ref is required", ErrInvalidInput)
	}
	e, err := s.GetEntity(ctx, legalEntityID)
	if err != nil {
		return err
	}
	if err := s.assertTenantMayTransact(ctx, e.TenantID); err != nil {
		return err
	}
	if e.EntityStatus != domain.EntityStatusDraft {
		return fmt.Errorf("%w: only a DRAFT entity can be verified; this one is %s", ErrInvalidTransition, e.EntityStatus)
	}
	expected, err := entityExpectedVersion(e, req.ExpectedVersion)
	if err != nil {
		return err
	}
	req.ExpectedVersion = expected
	reason := req.Reason
	if reason == "" {
		reason = "verify against " + req.VerificationEvidenceRef
	}
	a, err := s.propose(ctx, proposal{
		subjectType: domain.ApprovalSubjectLegalEntityVerification, tenantID: e.TenantID,
		subjectID: e.LegalEntityID, command: "VerifyLegalEntity", expectedVersion: expected,
		reason: reason, payload: req, correlationID: req.CorrelationID,
	})
	if err != nil {
		return err
	}
	return &PendingApprovalError{Request: a}
}

func (s *Service) approveEntityVerification(ctx context.Context, a *domain.ApprovalRequest, d domain.ApprovalDecision) (any, error) {
	var req domain.RequestEntityVerificationRequest
	if err := decodeVerified(a, &req); err != nil {
		return nil, err
	}
	e, err := s.GetEntity(ctx, a.SubjectID)
	if err != nil {
		return nil, err
	}
	// Independent of the entity's CREATOR as well as of the requester: the
	// creator asserted these facts, and verification is someone else
	// checking them.
	if d.DecidedByPrincipalID == e.CreatedByPrincipalID {
		return nil, fmt.Errorf("%w: the principal who created an entity cannot verify it", ErrSelfApproval)
	}
	if e.EntityStatus != domain.EntityStatusDraft || e.RecordVersion != a.ExpectedVersion {
		return nil, fmt.Errorf("%w: entity is %s at version %d; verification was proposed against DRAFT version %d",
			ErrConflict, e.EntityStatus, e.RecordVersion, a.ExpectedVersion)
	}
	ev, err := entityStatusEvent(e, a.RequestedByPrincipalID, req.CorrelationID,
		domain.EntityStatusDraft, domain.EntityStatusVerified,
		map[string]any{"verified_by": d.DecidedByPrincipalID, "verification_evidence_ref": req.VerificationEvidenceRef,
			"approval_request_id": a.ApprovalRequestID})
	if err != nil {
		return nil, err
	}
	if err := s.store.VerifyLegalEntity(ctx, EntityVerification{
		LegalEntityID: e.LegalEntityID, VerifiedBy: d.DecidedByPrincipalID,
		EvidenceRef: req.VerificationEvidenceRef, ExpectedVersion: a.ExpectedVersion, Decision: d,
	}, ev); err != nil {
		return nil, err
	}
	return s.GetEntity(ctx, e.LegalEntityID)
}

// ActivateLegalEntity is VERIFIED → ACTIVE. Verification was the independent
// gate; activation itself needs only the permission.
func (s *Service) ActivateLegalEntity(ctx context.Context, legalEntityID string, req domain.ActivateLegalEntityRequest) (*domain.LegalEntity, error) {
	if err := s.authorize(ctx, "entity", "activate"); err != nil {
		return nil, err
	}
	e, err := s.GetEntity(ctx, legalEntityID)
	if err != nil {
		return nil, err
	}
	if err := s.assertTenantMayTransact(ctx, e.TenantID); err != nil {
		return nil, err
	}
	if e.EntityStatus != domain.EntityStatusVerified {
		return nil, fmt.Errorf("%w: only a VERIFIED entity can be activated; this one is %s", ErrInvalidTransition, e.EntityStatus)
	}
	expected, err := entityExpectedVersion(e, req.ExpectedVersion)
	if err != nil {
		return nil, err
	}
	actor := domain.PrincipalFromContext(ctx)
	ev, err := entityStatusEvent(e, actor, req.CorrelationID, domain.EntityStatusVerified, domain.EntityStatusActive,
		map[string]any{"reason": req.Reason})
	if err != nil {
		return nil, err
	}
	if err := s.store.ActivateLegalEntity(ctx, legalEntityID, actor, expected, ev); err != nil {
		return nil, err
	}
	return s.GetEntity(ctx, legalEntityID)
}

// ---------------------------------------------------------------------------
// ORG-03 — MergeDuplicateCandidate (non-destructive)
// ---------------------------------------------------------------------------

// MergeDuplicateCandidate files a merge of duplicate into survivor for
// independent approval. Nothing is deleted or re-pointed when it lands: the
// duplicate becomes DORMANT with merged_into lineage.
func (s *Service) MergeDuplicateCandidate(ctx context.Context, duplicateID string, req domain.MergeDuplicateCandidateRequest) error {
	if err := s.authorize(ctx, "entity", "merge"); err != nil {
		return err
	}
	if strings.TrimSpace(req.Reason) == "" {
		return fmt.Errorf("%w: reason is required", ErrInvalidInput)
	}
	dup, surv, err := s.loadMergePair(ctx, duplicateID, req.SurvivorLegalEntityID)
	if err != nil {
		return err
	}
	if err := s.assertTenantMayTransact(ctx, dup.TenantID); err != nil {
		return err
	}
	expected, err := entityExpectedVersion(dup, req.ExpectedVersion)
	if err != nil {
		return err
	}
	req.ExpectedVersion = expected
	a, err := s.propose(ctx, proposal{
		subjectType: domain.ApprovalSubjectLegalEntityMerge, tenantID: dup.TenantID,
		subjectID: dup.LegalEntityID, command: "MergeDuplicateCandidate", expectedVersion: expected,
		reason: req.Reason + " (into " + surv.LegalEntityID + ")", payload: req, correlationID: req.CorrelationID,
	})
	if err != nil {
		return err
	}
	return &PendingApprovalError{Request: a}
}

// loadMergePair validates a duplicate/survivor pair.
func (s *Service) loadMergePair(ctx context.Context, duplicateID, survivorID string) (*domain.LegalEntity, *domain.LegalEntity, error) {
	if survivorID == "" {
		return nil, nil, fmt.Errorf("%w: survivor_legal_entity_id is required", ErrInvalidInput)
	}
	if survivorID == duplicateID {
		return nil, nil, fmt.Errorf("%w: an entity cannot be merged into itself", ErrInvalidInput)
	}
	dup, err := s.GetEntity(ctx, duplicateID)
	if err != nil {
		return nil, nil, err
	}
	surv, err := s.GetEntity(ctx, survivorID)
	if err != nil {
		return nil, nil, err
	}
	if dup.MergedIntoLegalEntityID != nil {
		return nil, nil, fmt.Errorf("%w: entity is already merged into %s", ErrConflict, *dup.MergedIntoLegalEntityID)
	}
	switch dup.EntityStatus {
	case domain.EntityStatusActive, domain.EntityStatusDormant, domain.EntityStatusSuspended:
	default:
		return nil, nil, fmt.Errorf("%w: a %s entity cannot be merged", ErrInvalidTransition, dup.EntityStatus)
	}
	if surv.EntityStatus != domain.EntityStatusActive || surv.MergedIntoLegalEntityID != nil {
		return nil, nil, fmt.Errorf("%w: the survivor must be an ACTIVE, unmerged entity", ErrConflict)
	}
	return dup, surv, nil
}

func (s *Service) approveMerge(ctx context.Context, a *domain.ApprovalRequest, d domain.ApprovalDecision) (any, error) {
	var req domain.MergeDuplicateCandidateRequest
	if err := decodeVerified(a, &req); err != nil {
		return nil, err
	}
	dup, _, err := s.loadMergePair(ctx, a.SubjectID, req.SurvivorLegalEntityID)
	if err != nil {
		return nil, err
	}
	if dup.RecordVersion != a.ExpectedVersion {
		return nil, fmt.Errorf("%w: merge proposed against version %d, entity is now at %d",
			ErrVersionConflict, a.ExpectedVersion, dup.RecordVersion)
	}
	m := &domain.EntityMergeRecord{
		MergeRecordID: newID(), TenantID: dup.TenantID,
		DuplicateLegalEntityID: dup.LegalEntityID, SurvivorLegalEntityID: req.SurvivorLegalEntityID,
		Reason: req.Reason, EvidenceRef: nullableString(req.EvidenceRef),
		MergedByPrincipalID: a.RequestedByPrincipalID, MergeApprovedByPrincipalID: d.DecidedByPrincipalID,
		MergeApprovalRequestID: a.ApprovalRequestID,
	}
	ev, err := entityStatusEvent(dup, a.RequestedByPrincipalID, req.CorrelationID, dup.EntityStatus, domain.EntityStatusDormant,
		map[string]any{"merged_into_legal_entity_id": req.SurvivorLegalEntityID, "approved_by": d.DecidedByPrincipalID,
			"approval_request_id": a.ApprovalRequestID})
	if err != nil {
		return nil, err
	}
	if err := s.store.MergeEntities(ctx, m, d, a.ExpectedVersion, ev); err != nil {
		return nil, err
	}
	return m, nil
}

// UnmergeEntity files the reversal of a merge for independent approval.
func (s *Service) UnmergeEntity(ctx context.Context, duplicateID string, req domain.UnmergeEntityRequest) error {
	if err := s.authorize(ctx, "entity", "unmerge"); err != nil {
		return err
	}
	if strings.TrimSpace(req.Reason) == "" {
		return fmt.Errorf("%w: reason is required", ErrInvalidInput)
	}
	e, err := s.GetEntity(ctx, duplicateID)
	if err != nil {
		return err
	}
	if e.MergedIntoLegalEntityID == nil {
		return fmt.Errorf("%w: entity is not merged", ErrConflict)
	}
	expected, err := entityExpectedVersion(e, req.ExpectedVersion)
	if err != nil {
		return err
	}
	req.ExpectedVersion = expected
	a, err := s.propose(ctx, proposal{
		subjectType: domain.ApprovalSubjectLegalEntityUnmerge, tenantID: e.TenantID,
		subjectID: e.LegalEntityID, command: "UnmergeEntity", expectedVersion: expected,
		reason: req.Reason, payload: req, correlationID: req.CorrelationID,
	})
	if err != nil {
		return err
	}
	return &PendingApprovalError{Request: a}
}

func (s *Service) approveUnmerge(ctx context.Context, a *domain.ApprovalRequest, d domain.ApprovalDecision) (any, error) {
	var req domain.UnmergeEntityRequest
	if err := decodeVerified(a, &req); err != nil {
		return nil, err
	}
	e, err := s.GetEntity(ctx, a.SubjectID)
	if err != nil {
		return nil, err
	}
	if e.MergedIntoLegalEntityID == nil || e.RecordVersion != a.ExpectedVersion {
		return nil, fmt.Errorf("%w: entity is no longer merged at version %d", ErrConflict, a.ExpectedVersion)
	}
	ev, err := entityStatusEvent(e, a.RequestedByPrincipalID, req.CorrelationID, e.EntityStatus, "",
		map[string]any{"unmerged_from_legal_entity_id": *e.MergedIntoLegalEntityID, "approved_by": d.DecidedByPrincipalID,
			"approval_request_id": a.ApprovalRequestID})
	if err != nil {
		return nil, err
	}
	if err := s.store.UnmergeEntity(ctx, e.LegalEntityID, a.RequestedByPrincipalID, req.Reason, d, a.ExpectedVersion, ev); err != nil {
		return nil, err
	}
	return s.GetEntity(ctx, e.LegalEntityID)
}

// ListEntityMergeRecords is the merge lineage read surface.
func (s *Service) ListEntityMergeRecords(ctx context.Context, legalEntityID string) ([]*domain.EntityMergeRecord, error) {
	if _, err := s.GetEntity(ctx, legalEntityID); err != nil {
		return nil, err
	}
	return s.store.ListEntityMergeRecords(ctx, legalEntityID)
}

// ---------------------------------------------------------------------------
// Sensitive identifiers (ORG-03 "sensitive identifier access scoped")
// ---------------------------------------------------------------------------

// sensitiveClassification reports whether a tax identity bundle's
// classification puts it under scoped access.
func sensitiveClassification(c string) bool {
	switch strings.ToUpper(c) {
	case "RESTRICTED", "CONFIDENTIAL":
		return true
	}
	return false
}

// authorizeSensitiveRead is the scoped-access gate: a dedicated permission AND
// a declared purpose. Both, because the permission says who MAY read and the
// purpose records why they DID — the second is what makes the access
// reviewable afterwards.
func (s *Service) authorizeSensitiveRead(ctx context.Context) error {
	if err := s.authorize(ctx, "entity.sensitive-identifier", "read"); err != nil {
		return err
	}
	env, ok := envelope.FromContext(ctx)
	if !ok || strings.TrimSpace(env.PurposeContext) == "" {
		return fmt.Errorf("%w: X-Purpose-Context is required to read a sensitive tax identity bundle", ErrUnauthorized)
	}
	return nil
}
