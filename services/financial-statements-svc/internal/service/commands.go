package service

import (
	"context"
	"encoding/json"
	"time"

	"github.com/google/uuid"

	"zoiko.io/financial-statements-svc/internal/domain"
)

// CreateSetRequest is CreateStatementSet's input.
type CreateSetRequest struct {
	LegalEntityID        string
	ReportingPeriod      string
	Framework            string
	PresentationCurrency string
	SourceSnapshotID     string
}

// CreateStatementSet verifies the named snapshot is CERTIFIED (ZS-SVC-H-001
// §4.1's first mandatory control) and creates a new set in DRAFT, bound to
// it.
func (s *Service) CreateStatementSet(ctx context.Context, tenantID, principalID, correlationID string, req CreateSetRequest) (*domain.FinancialStatementSet, error) {
	if req.LegalEntityID == "" || req.ReportingPeriod == "" || req.Framework == "" || req.PresentationCurrency == "" || req.SourceSnapshotID == "" {
		return nil, domain.Errf(domain.CodeContextInvalid, "legal_entity_id, reporting_period, framework, presentation_currency and source_snapshot_id are all required")
	}
	if err := s.authz.CheckAllowed(ctx, principalID, req.LegalEntityID, ActionStatementSetCreate); err != nil {
		return nil, err
	}

	snap, err := s.snapshots.GetSnapshot(ctx, tenantID, principalID, req.SourceSnapshotID)
	if err != nil {
		return nil, err
	}
	if snap.LegalEntityID != req.LegalEntityID {
		return nil, domain.Errf(domain.CodeSourceUnverified,
			"snapshot %q belongs to legal entity %q, not %q", req.SourceSnapshotID, snap.LegalEntityID, req.LegalEntityID)
	}

	now := time.Now().UTC()
	set := &domain.FinancialStatementSet{
		StatementSetID:       uuid.NewString(),
		TenantID:             tenantID,
		LegalEntityID:        req.LegalEntityID,
		ReportingPeriod:      req.ReportingPeriod,
		Framework:            req.Framework,
		PresentationCurrency: req.PresentationCurrency,
		SourceSnapshotID:     req.SourceSnapshotID,
		SourceSnapshotHash:   snap.ContentHash,
		MappingVersion:       domain.MappingVersionCurrent,
		Status:               domain.StateDraft,
		Version:              1,
		CreatedAt:            now,
		CreatedByPrincipalID: principalID,
	}
	if err := s.store.CreateSet(ctx, tenantID, set, correlationID); err != nil {
		return nil, err
	}
	return set, nil
}

// RefreshDraft re-reads the bound snapshot and regenerates every
// StatementFact. Only legal from DRAFT.
func (s *Service) RefreshDraft(ctx context.Context, tenantID, principalID, correlationID, setID string) (*domain.FinancialStatementSet, error) {
	set, err := s.store.GetSet(ctx, tenantID, setID)
	if err != nil {
		return nil, err
	}
	if err := s.authz.CheckAllowed(ctx, principalID, set.LegalEntityID, ActionStatementSetRefresh); err != nil {
		return nil, err
	}
	if _, err := domain.Next(domain.CmdRefreshDraft, set.Status); err != nil {
		return nil, err
	}

	snap, err := s.snapshots.GetSnapshot(ctx, tenantID, principalID, set.SourceSnapshotID)
	if err != nil {
		return nil, err
	}
	// A CERTIFIED snapshot's content is immutable; a hash mismatch here would
	// mean either corruption or a bug upstream, never a legitimate update —
	// fail closed rather than silently build facts off different data than
	// the set claims to be bound to.
	if snap.ContentHash != set.SourceSnapshotHash {
		return nil, domain.Errf(domain.CodeSourceUnverified,
			"snapshot %q content hash changed since this set was created; a CERTIFIED snapshot must never change", set.SourceSnapshotID)
	}

	var content domain.SnapshotContent
	if err := json.Unmarshal([]byte(snap.Content), &content); err != nil {
		return nil, domain.Errf(domain.CodeSourceUnverified, "snapshot %q content is not valid JSON for this service's schema: %v", set.SourceSnapshotID, err)
	}
	// Built under the set's OWN mapping version, never the current one: a
	// set created under V1 keeps one line per account for its whole life.
	facts, err := domain.BuildFacts(content, set.MappingVersion)
	if err != nil {
		return nil, err
	}
	now := time.Now().UTC()
	for i := range facts {
		facts[i].FactID = uuid.NewString()
		facts[i].TenantID = tenantID
		facts[i].StatementSetID = setID
		facts[i].CreatedAt = now
	}

	if err := s.store.RefreshFacts(ctx, tenantID, setID, set.Version, facts, principalID, correlationID); err != nil {
		return nil, err
	}
	set.Version++
	set.LastRefreshedAt = &now
	set.LastRefreshedByPrincipalID = &principalID
	return set, nil
}

// ValidationResult is ValidateStatements's response: either Errors is empty
// and the set moved to VALIDATED, or it names what failed and the set stayed
// in DRAFT.
type ValidationResult struct {
	Errors []string
}

// ValidateStatements runs the tie-out check over the set's current facts.
func (s *Service) ValidateStatements(ctx context.Context, tenantID, principalID, correlationID, setID string) (*ValidationResult, error) {
	set, err := s.store.GetSet(ctx, tenantID, setID)
	if err != nil {
		return nil, err
	}
	if err := s.authz.CheckAllowed(ctx, principalID, set.LegalEntityID, ActionStatementSetValidate); err != nil {
		return nil, err
	}
	if _, err := domain.Next(domain.CmdValidate, set.Status); err != nil {
		return nil, err
	}
	facts, err := s.store.GetFacts(ctx, tenantID, setID)
	if err != nil {
		return nil, err
	}
	if len(facts) == 0 {
		return nil, domain.Errf(domain.CodeValidationFailed, "statement set %q has no facts yet; call RefreshDraft first", setID)
	}
	errs, err := domain.ValidateTieOut(facts)
	if err != nil {
		return nil, err
	}
	if err := s.store.Validate(ctx, tenantID, setID, set.Version, errs, correlationID); err != nil {
		return nil, err
	}
	return &ValidationResult{Errors: errs}, nil
}

func (s *Service) SubmitForReview(ctx context.Context, tenantID, principalID, correlationID, setID string) error {
	set, err := s.store.GetSet(ctx, tenantID, setID)
	if err != nil {
		return err
	}
	if err := s.authz.CheckAllowed(ctx, principalID, set.LegalEntityID, ActionStatementSetReview); err != nil {
		return err
	}
	if _, err := domain.Next(domain.CmdSubmitForReview, set.Status); err != nil {
		return err
	}
	return s.store.SubmitForReview(ctx, tenantID, setID, set.Version, principalID, correlationID)
}

// RequestChanges is the reviewer's rejection path (SPEC_DEVIATIONS.md B2).
func (s *Service) RequestChanges(ctx context.Context, tenantID, principalID, correlationID, setID string) error {
	set, err := s.store.GetSet(ctx, tenantID, setID)
	if err != nil {
		return err
	}
	if err := s.authz.CheckAllowed(ctx, principalID, set.LegalEntityID, ActionStatementSetReview); err != nil {
		return err
	}
	if _, err := domain.Next(domain.CmdRequestChanges, set.Status); err != nil {
		return err
	}
	return s.store.RequestChanges(ctx, tenantID, setID, set.Version, principalID, correlationID)
}

// Approve enforces four-eyes: the approver may not be the preparer.
func (s *Service) Approve(ctx context.Context, tenantID, principalID, correlationID, setID string) error {
	set, err := s.store.GetSet(ctx, tenantID, setID)
	if err != nil {
		return err
	}
	if err := s.authz.CheckAllowed(ctx, principalID, set.LegalEntityID, ActionStatementSetApprove); err != nil {
		return err
	}
	if _, err := domain.Next(domain.CmdApprove, set.Status); err != nil {
		return err
	}
	if principalID == set.Preparer() {
		return domain.Errf(domain.CodeSoDDenied, "the preparer of a statement set may not also approve it")
	}
	return s.store.Approve(ctx, tenantID, setID, set.Version, principalID, correlationID)
}

// Certify enforces ZS-SVC-H-001 §4.1's own SoD rule: the certifier may not
// be the preparer.
func (s *Service) Certify(ctx context.Context, tenantID, principalID, correlationID, setID, reason string) error {
	set, err := s.store.GetSet(ctx, tenantID, setID)
	if err != nil {
		return err
	}
	if err := s.authz.CheckAllowed(ctx, principalID, set.LegalEntityID, ActionStatementSetCertify); err != nil {
		return err
	}
	if _, err := domain.Next(domain.CmdCertify, set.Status); err != nil {
		return err
	}
	if principalID == set.Preparer() {
		return domain.Errf(domain.CodeSoDDenied, "the preparer of a statement set may not also certify it")
	}
	return s.store.Certify(ctx, tenantID, setID, set.Version, principalID, reason, correlationID)
}

func (s *Service) Publish(ctx context.Context, tenantID, principalID, correlationID, setID string) error {
	set, err := s.store.GetSet(ctx, tenantID, setID)
	if err != nil {
		return err
	}
	if err := s.authz.CheckAllowed(ctx, principalID, set.LegalEntityID, ActionStatementSetPublish); err != nil {
		return err
	}
	if _, err := domain.Next(domain.CmdPublish, set.Status); err != nil {
		return err
	}
	return s.store.Publish(ctx, tenantID, setID, set.Version, principalID, correlationID)
}

// SupersedeStatementSet is REP-01's correction path: certified output is
// immutable, so a correction is always a NEW set, bound to a new snapshot,
// never an edit of the old one.
func (s *Service) SupersedeStatementSet(ctx context.Context, tenantID, principalID, correlationID, oldSetID, newSnapshotID string) (*domain.FinancialStatementSet, error) {
	old, err := s.store.GetSet(ctx, tenantID, oldSetID)
	if err != nil {
		return nil, err
	}
	if err := s.authz.CheckAllowed(ctx, principalID, old.LegalEntityID, ActionStatementSetCreate); err != nil {
		return nil, err
	}
	if !domain.CanSupersede(old.Status) {
		return nil, domain.Errf(domain.CodeInvalidTransition, "a statement set in state %s cannot be superseded", old.Status)
	}

	snap, err := s.snapshots.GetSnapshot(ctx, tenantID, principalID, newSnapshotID)
	if err != nil {
		return nil, err
	}
	if snap.LegalEntityID != old.LegalEntityID {
		return nil, domain.Errf(domain.CodeSourceUnverified,
			"replacement snapshot %q belongs to legal entity %q, not %q", newSnapshotID, snap.LegalEntityID, old.LegalEntityID)
	}

	now := time.Now().UTC()
	oldID := old.StatementSetID
	newSet := &domain.FinancialStatementSet{
		StatementSetID:       uuid.NewString(),
		TenantID:             tenantID,
		LegalEntityID:        old.LegalEntityID,
		ReportingPeriod:      old.ReportingPeriod,
		Framework:            old.Framework,
		PresentationCurrency: old.PresentationCurrency,
		SourceSnapshotID:     newSnapshotID,
		SourceSnapshotHash:   snap.ContentHash,
		MappingVersion:       domain.MappingVersionCurrent,
		Status:               domain.StateDraft,
		Version:              1,
		SupersedesSetID:      &oldID,
		CreatedAt:            now,
		CreatedByPrincipalID: principalID,
	}
	if err := s.store.Supersede(ctx, tenantID, oldSetID, old.Version, newSet, principalID, correlationID); err != nil {
		return nil, err
	}
	return newSet, nil
}
