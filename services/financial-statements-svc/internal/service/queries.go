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

// GetComparatives pairs one statement set's facts with an explicitly named
// prior set's (ComparativePresentation, two periods only). The prior side
// must be CERTIFIED or PUBLISHED — an uncertified comparative is not a real
// comparative, the same fail-closed posture CreateStatementSet takes on its
// snapshot binding — and must describe the same legal entity, framework and
// presentation currency, since a side-by-side of anything else is
// meaningless. The current set may be in any state: preparing a draft
// against last period's certified figures is the main use.
func (s *Service) GetComparatives(ctx context.Context, tenantID, principalID, setID, priorSetID string) (*domain.ComparativeStatementView, error) {
	if priorSetID == "" {
		return nil, domain.Errf(domain.CodeContextInvalid, "prior_set_id is required")
	}
	if priorSetID == setID {
		return nil, domain.Errf(domain.CodeContextInvalid, "prior_set_id must name a different statement set")
	}
	current, err := s.store.GetSet(ctx, tenantID, setID)
	if err != nil {
		return nil, err
	}
	if err := s.authz.CheckAllowed(ctx, principalID, current.LegalEntityID, ActionStatementSetView); err != nil {
		return nil, err
	}
	prior, err := s.store.GetSet(ctx, tenantID, priorSetID)
	if err == domain.ErrStatementSetNotFound {
		return nil, domain.Errf(domain.CodeNotFound, "prior statement set %q not found", priorSetID)
	}
	if err != nil {
		return nil, err
	}
	// Authorize the prior set at its own entity before reporting any
	// mismatch, so a 422 never confirms a set exists in an entity the
	// caller cannot view.
	if err := s.authz.CheckAllowed(ctx, principalID, prior.LegalEntityID, ActionStatementSetView); err != nil {
		return nil, err
	}
	if prior.LegalEntityID != current.LegalEntityID {
		return nil, domain.Errf(domain.CodeContextInvalid, "prior set belongs to legal entity %q, not %q", prior.LegalEntityID, current.LegalEntityID)
	}
	if prior.Framework != current.Framework {
		return nil, domain.Errf(domain.CodeContextInvalid, "prior set is reported under framework %q, not %q", prior.Framework, current.Framework)
	}
	if prior.PresentationCurrency != current.PresentationCurrency {
		return nil, domain.Errf(domain.CodeContextInvalid, "prior set is presented in %q, not %q", prior.PresentationCurrency, current.PresentationCurrency)
	}
	if prior.MappingVersion != current.MappingVersion {
		return nil, domain.Errf(domain.CodeContextInvalid, "prior set uses presentation mapping %q, not %q", prior.MappingVersion, current.MappingVersion)
	}
	if prior.ReportingPeriod == current.ReportingPeriod {
		return nil, domain.Errf(domain.CodeContextInvalid, "prior set covers the same reporting period %q; a comparative needs a different period", prior.ReportingPeriod)
	}
	if prior.Status != domain.StateCertified && prior.Status != domain.StatePublished {
		msg := "prior set is %s; a comparative must be CERTIFIED or PUBLISHED"
		if prior.SupersededBySetID != nil {
			return nil, domain.Errf(domain.CodeSourceUnverified, msg+" (superseded by %q)", prior.Status, *prior.SupersededBySetID)
		}
		return nil, domain.Errf(domain.CodeSourceUnverified, msg, prior.Status)
	}

	currentFacts, err := s.store.GetFacts(ctx, tenantID, current.StatementSetID)
	if err != nil {
		return nil, err
	}
	priorFacts, err := s.store.GetFacts(ctx, tenantID, prior.StatementSetID)
	if err != nil {
		return nil, err
	}
	if len(priorFacts) == 0 {
		// A certified set always has facts (ValidateStatements requires
		// them); none here means the read is wrong, not that the prior
		// period was empty — refuse rather than show every line as new.
		return nil, domain.Errf(domain.CodeSourceUnverified, "prior set %q has no statement facts", prior.StatementSetID)
	}
	statements, err := domain.BuildComparative(currentFacts, priorFacts, current.MappingVersion)
	if err != nil {
		return nil, domain.Errf(domain.CodeValidationFailed, "cannot pair statement facts: %v", err)
	}
	return &domain.ComparativeStatementView{
		LegalEntityID:        current.LegalEntityID,
		Framework:            current.Framework,
		PresentationCurrency: current.PresentationCurrency,
		MappingVersion:       current.MappingVersion,
		Current:              domain.RefFor(current),
		Prior:                domain.RefFor(prior),
		Statements:           statements,
	}, nil
}

// ExplainedFact is ExplainReportedFact's response: the fact plus the
// evidence it is traceable to. Full ACC-18 lineage-graph integration is not
// built (see SPEC_DEVIATIONS.md) — this names the source snapshot, its hash,
// and every snapshot account summed into the fact, which is as far as this
// version's traceability goes.
type ExplainedFact struct {
	Fact               domain.StatementFact `json:"fact"`
	StatementSetID     string               `json:"statement_set_id"`
	SourceSnapshotID   string               `json:"source_snapshot_id"`
	SourceSnapshotHash string               `json:"source_snapshot_hash"`
	// Aggregated is true when the fact sums more than one source account.
	Aggregated bool `json:"aggregated"`
	// SourceAccounts is every snapshot account behind the fact, each with
	// its own balance — always at least one, never just the first of many.
	SourceAccounts []domain.SnapshotContentAccount `json:"source_accounts"`
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
	sources := fact.Sources()
	// The provenance claim is "these accounts make up this number". Check
	// it rather than assert it: a stored fact whose sources do not sum to
	// its amount is refused, not explained.
	balances := make([]string, len(sources))
	for i, a := range sources {
		balances[i] = a.Balance
	}
	sum, err := domain.SumAmounts(balances)
	if err != nil {
		return nil, domain.Errf(domain.CodeValidationFailed, "fact %q has an unparseable source balance: %v", factID, err)
	}
	amount, err := domain.ParseAmount(fact.Amount)
	if err != nil || sum.Cmp(amount) != 0 {
		return nil, domain.Errf(domain.CodeValidationFailed, "fact %q amount %s does not equal the sum of its source accounts %s", factID, fact.Amount, domain.CanonicalAmount(sum))
	}
	return &ExplainedFact{
		Fact:               *fact,
		StatementSetID:     set.StatementSetID,
		SourceSnapshotID:   set.SourceSnapshotID,
		SourceSnapshotHash: set.SourceSnapshotHash,
		Aggregated:         len(sources) > 1,
		SourceAccounts:     sources,
	}, nil
}
