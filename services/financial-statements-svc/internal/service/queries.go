package service

import (
	"context"

	"zoiko.io/financial-statements-svc/internal/domain"
)

// GetStatementSet reads one set, scoped to the caller's view permission.
func (s *Service) GetStatementSet(ctx context.Context, tenantID, principalID, setID string) (*domain.FinancialStatementSet, error) {
	set, err := s.store.GetSet(ctx, tenantID, setID)
	if err != nil {
		return nil, err
	}
	if err := s.authz.CheckAllowed(ctx, principalID, set.LegalEntityID, ActionStatementSetView); err != nil {
		return nil, err
	}
	return set, nil
}

// ListStatementSets lists sets for one legal entity (authorization is
// checked at the entity the caller asked for, not per row — a caller with no
// view right for that entity gets a 403, not a silently filtered list).
func (s *Service) ListStatementSets(ctx context.Context, tenantID, principalID, legalEntityID, status string) ([]domain.FinancialStatementSet, error) {
	if legalEntityID == "" {
		return nil, domain.Errf(domain.CodeContextInvalid, "legal_entity_id is required")
	}
	if err := s.authz.CheckAllowed(ctx, principalID, legalEntityID, ActionStatementSetView); err != nil {
		return nil, err
	}
	return s.store.ListSets(ctx, tenantID, legalEntityID, status)
}

// GetStatementFacts lists every fact in one set.
func (s *Service) GetStatementFacts(ctx context.Context, tenantID, principalID, setID string) ([]domain.StatementFact, error) {
	set, err := s.store.GetSet(ctx, tenantID, setID)
	if err != nil {
		return nil, err
	}
	if err := s.authz.CheckAllowed(ctx, principalID, set.LegalEntityID, ActionStatementSetView); err != nil {
		return nil, err
	}
	return s.store.GetFacts(ctx, tenantID, setID)
}

// ExplainedFact is ExplainReportedFact's response: the fact plus the
// evidence it is traceable to. Full ACC-18 lineage-graph integration is not
// built (see SPEC_DEVIATIONS.md) — this names the source snapshot and its
// hash, which is as far as this version's traceability goes.
type ExplainedFact struct {
	Fact               domain.StatementFact `json:"fact"`
	StatementSetID     string               `json:"statement_set_id"`
	SourceSnapshotID   string               `json:"source_snapshot_id"`
	SourceSnapshotHash string               `json:"source_snapshot_hash"`
}

// ExplainReportedFact answers "where did this number come from."
func (s *Service) ExplainReportedFact(ctx context.Context, tenantID, principalID, factID string) (*ExplainedFact, error) {
	fact, err := s.store.GetFact(ctx, tenantID, factID)
	if err != nil {
		return nil, err
	}
	set, err := s.store.GetSet(ctx, tenantID, fact.StatementSetID)
	if err != nil {
		return nil, err
	}
	if err := s.authz.CheckAllowed(ctx, principalID, set.LegalEntityID, ActionStatementSetView); err != nil {
		return nil, err
	}
	return &ExplainedFact{
		Fact:               *fact,
		StatementSetID:     set.StatementSetID,
		SourceSnapshotID:   set.SourceSnapshotID,
		SourceSnapshotHash: set.SourceSnapshotHash,
	}, nil
}
