package registry

// ORG-07 / MDM-05 matching, stewardship decisions, MergeParties, SplitParty and ExplainMergeHistory.
//
// Merge and split are CRITICAL decisions and never self-approved: the principal who opened a case cannot decide
// it, and the principal who merged cannot split. The store enforces both under the case's row lock and the
// lineage table enforces them again, so neither depends on this layer.

import (
	"context"
	"fmt"

	"zoiko.io/tenant-entity-registry-svc/internal/domain"
)

func (s *Service) resolutionStore() (PartyResolutionStore, error) {
	rs, ok := s.store.(PartyResolutionStore)
	if !ok {
		return nil, ErrServiceUnavailable
	}
	return rs, nil
}

// MatchParties answers what a create would do for this request, and why. Tenant-scoped: another tenant's parties
// are never candidates. It writes nothing.
func (s *Service) MatchParties(ctx context.Context, req domain.MatchPartiesRequest) (*domain.MatchPartiesResult, error) {
	if err := req.Validate(); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalidInput, err)
	}
	rs, err := s.resolutionStore()
	if err != nil {
		return nil, err
	}
	out, err := rs.MatchParties(ctx, req)
	if err != nil {
		return nil, fmt.Errorf("store.MatchParties: %w", err)
	}
	return out, nil
}

// ListResolutionCases pages stewardship cases.
func (s *Service) ListResolutionCases(ctx context.Context, status, caseType string, limit int, after string) ([]domain.ResolutionCase, string, error) {
	switch status {
	case "", domain.CaseOpen, domain.CaseResolved, domain.CaseDismissed:
	default:
		return nil, "", fmt.Errorf("%w: status is not a case status", ErrInvalidInput)
	}
	switch caseType {
	case "", domain.CaseDuplicateReview, domain.CaseIdentifierConflict, domain.CaseFactConflict:
	default:
		return nil, "", fmt.Errorf("%w: case_type is not a case type", ErrInvalidInput)
	}
	rs, err := s.resolutionStore()
	if err != nil {
		return nil, "", err
	}
	out, next, err := rs.ListResolutionCases(ctx, status, caseType, limit, after)
	if err != nil {
		return nil, "", fmt.Errorf("store.ListResolutionCases: %w", err)
	}
	return out, next, nil
}

// DecideResolutionCase settles a stewardship case.
func (s *Service) DecideResolutionCase(ctx context.Context, caseID string, req domain.CaseDecisionRequest) (*CaseDecisionOutcome, error) {
	if err := req.Validate(); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalidInput, err)
	}
	if err := s.authorize(ctx, "party.resolution", "decide"); err != nil {
		return nil, err
	}
	if err := s.assertTenantMayTransact(ctx, ""); err != nil {
		return nil, err
	}
	rs, err := s.resolutionStore()
	if err != nil {
		return nil, err
	}
	out, err := rs.DecideResolutionCase(ctx, caseID, req, domain.PrincipalFromContext(ctx), s.expectedVersionOptional)
	if err != nil {
		return nil, fmt.Errorf("store.DecideResolutionCase: %w", err)
	}
	return out, nil
}

// MergeParties is MergeParties (ORG-07) / POST /v1/resolution/{case}:merge.
func (s *Service) MergeParties(ctx context.Context, caseID string, req domain.MergePartiesRequest) (*MergeOutcome, error) {
	if err := req.Validate(); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalidInput, err)
	}
	if err := s.authorize(ctx, "party", "merge"); err != nil {
		return nil, err
	}
	if err := s.assertTenantMayTransact(ctx, ""); err != nil {
		return nil, err
	}
	rs, err := s.resolutionStore()
	if err != nil {
		return nil, err
	}
	out, err := rs.MergeParties(ctx, caseID, req, domain.PrincipalFromContext(ctx))
	if err != nil {
		return nil, fmt.Errorf("store.MergeParties: %w", err)
	}
	return out, nil
}

// SplitParties is SplitParty / POST /v1/resolution/{case}:split.
func (s *Service) SplitParties(ctx context.Context, caseID string, req domain.SplitPartiesRequest) (*SplitOutcome, error) {
	if err := req.Validate(); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalidInput, err)
	}
	if err := s.authorize(ctx, "party", "split"); err != nil {
		return nil, err
	}
	if err := s.assertTenantMayTransact(ctx, ""); err != nil {
		return nil, err
	}
	rs, err := s.resolutionStore()
	if err != nil {
		return nil, err
	}
	out, err := rs.SplitParties(ctx, caseID, req, domain.PrincipalFromContext(ctx))
	if err != nil {
		return nil, fmt.Errorf("store.SplitParties: %w", err)
	}
	return out, nil
}

// ExplainMergeHistory answers where a party's id resolves and every merge and split that touched it.
func (s *Service) ExplainMergeHistory(ctx context.Context, partyID string) (*domain.MergeHistory, error) {
	rs, err := s.resolutionStore()
	if err != nil {
		return nil, err
	}
	h, err := rs.ExplainMergeHistory(ctx, partyID)
	if err != nil {
		return nil, fmt.Errorf("store.ExplainMergeHistory: %w", err)
	}
	if h == nil {
		return nil, ErrNotFound
	}
	return h, nil
}
