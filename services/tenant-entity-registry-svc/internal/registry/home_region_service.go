package registry

import (
	"context"
	"fmt"
	"strings"

	"github.com/google/uuid"
	"go.uber.org/zap"

	"zoiko.io/tenant-entity-registry-svc/internal/domain"
	"zoiko.io/tenant-entity-registry-svc/internal/events"
)

// ORG-02 §4.2 ChangeHomeRegion (migration 000012).
//
// §4.2 lists TenantHomeRegionReference among this service's authoritative
// facts and says "home-region changes require maker-checker", but names no
// command, so until 28 Sep 2026 the control could not be exercised at all.
//
// The home region is the region of the tenant's default residency policy —
// a hard isolation identifier, which §4.2 says a tenant admin cannot change.
// So the command is authorized in the PLATFORM scope (like host binding), is
// always filed for independent approval, and needs the home-region decision
// it implements as evidence.

// ChangeHomeRegion files the command for approval. It never applies directly.
func (s *Service) ChangeHomeRegion(ctx context.Context, tenantID string, req domain.ChangeHomeRegionRequest) error {
	if err := s.assertTenantScope(ctx, tenantID); err != nil {
		return err
	}
	if err := s.authorizeIn(ctx, s.platformScopeID, "tenant", domain.TenantCommandChangeHomeRegion.AuthzAction()); err != nil {
		return err
	}
	req.ResidencyRegionID = strings.TrimSpace(req.ResidencyRegionID)
	if _, err := uuid.Parse(req.ResidencyRegionID); err != nil {
		return fmt.Errorf("%w: residency_region_id must be a UUID", ErrInvalidInput)
	}
	if strings.TrimSpace(req.HomeRegionDecisionRef) == "" {
		return fmt.Errorf("%w: home_region_decision_ref is required — a home-region change implements a recorded residency decision", ErrSourceUnverified)
	}
	if strings.TrimSpace(req.Reason) == "" {
		return fmt.Errorf("%w: reason is required", ErrInvalidInput)
	}

	t, err := s.GetTenant(ctx, tenantID)
	if err != nil {
		return err
	}
	switch t.LifecycleState {
	case domain.TenantLifecycleOffboarding, domain.TenantLifecycleTerminated:
		return fmt.Errorf("%w: a %s tenant's home region cannot change", ErrInvalidTransition, t.LifecycleState)
	}
	expected, err := s.checkExpected(req.ExpectedVersion, t.RecordVersion, "tenant")
	if err != nil {
		return err
	}
	req.ExpectedVersion = expected
	if req.CorrelationID == "" {
		req.CorrelationID = uuid.NewString()
	}

	a, err := s.propose(ctx, proposal{
		subjectType:     domain.ApprovalSubjectTenantHomeRegion,
		tenantID:        tenantID,
		subjectID:       tenantID,
		command:         string(domain.TenantCommandChangeHomeRegion),
		expectedVersion: expected,
		reason:          req.Reason,
		payload:         req,
		correlationID:   req.CorrelationID,
	})
	if err != nil {
		return err
	}
	return &PendingApprovalError{Request: a}
}

// approveHomeRegion executes an approved ChangeHomeRegion.
func (s *Service) approveHomeRegion(ctx context.Context, a *domain.ApprovalRequest, d domain.ApprovalDecision) (any, error) {
	var req domain.ChangeHomeRegionRequest
	if err := decodeVerified(a, &req); err != nil {
		return nil, err
	}
	t, err := s.GetTenant(ctx, a.SubjectID)
	if err != nil {
		return nil, err
	}
	if t.RecordVersion != a.ExpectedVersion {
		return nil, fmt.Errorf("%w: proposed against version %d, tenant is now at %d",
			ErrVersionConflict, a.ExpectedVersion, t.RecordVersion)
	}

	ev, err := events.BuildRecord(events.RecordSpec{
		EventType:     events.EventTenantHomeRegionChanged,
		TenantID:      t.TenantID,
		ActorID:       a.RequestedByPrincipalID,
		CorrelationID: req.CorrelationID,
		ObjectID:      t.TenantID,
		ObjectVersion: a.ExpectedVersion + 1, // stamped again by the store
		EvidenceRef:   req.HomeRegionDecisionRef,
		PartitionKey:  t.TenantID,
		Payload: map[string]any{
			"tenant_id":                t.TenantID,
			"to_region_id":             req.ResidencyRegionID,
			"home_region_decision_ref": req.HomeRegionDecisionRef,
			"reason":                   req.Reason,
			"approved_by":              d.DecidedByPrincipalID,
			"approval_request_id":      a.ApprovalRequestID,
			"previous_version":         t.RecordVersion,
		},
	})
	if err != nil {
		return nil, err
	}

	res, err := s.store.ChangeHomeRegion(ctx, HomeRegionChange{
		TenantID:          t.TenantID,
		ResidencyRegionID: req.ResidencyRegionID,
		DecisionRef:       req.HomeRegionDecisionRef,
		Reason:            req.Reason,
		ActorID:           a.RequestedByPrincipalID,
		ExpectedVersion:   a.ExpectedVersion,
		CorrelationID:     req.CorrelationID,
		Approval:          d,
	}, ev)
	if err != nil {
		return nil, err
	}
	s.log.Info("tenant home region changed",
		zap.String("tenant_id", t.TenantID),
		zap.String("to_region_id", res.ToRegionID),
		zap.String("approved_by", d.DecidedByPrincipalID))
	return res, nil
}
