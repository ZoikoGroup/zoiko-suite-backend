package service_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"go.uber.org/zap"

	"zoiko.io/financial-statements-svc/internal/clients"
	"zoiko.io/financial-statements-svc/internal/domain"
	"zoiko.io/financial-statements-svc/internal/service"
)

// ── fakes ────────────────────────────────────────────────────────────────

type fakeStore struct {
	sets        map[string]*domain.FinancialStatementSet
	facts       map[string][]domain.StatementFact
	disclosures map[string][]domain.DisclosureInstance
}

func newFakeStore() *fakeStore {
	return &fakeStore{sets: map[string]*domain.FinancialStatementSet{}, facts: map[string][]domain.StatementFact{}, disclosures: map[string][]domain.DisclosureInstance{}}
}

func (s *fakeStore) CreateSet(ctx context.Context, tenantID string, set *domain.FinancialStatementSet, correlationID string) error {
	for _, existing := range s.sets {
		if existing.TenantID == tenantID && existing.LegalEntityID == set.LegalEntityID &&
			existing.ReportingPeriod == set.ReportingPeriod && existing.Framework == set.Framework &&
			existing.Status != domain.StateSuperseded {
			return domain.Errf(domain.CodeContextInvalid, "an active statement set already exists")
		}
	}
	cp := *set
	s.sets[set.StatementSetID] = &cp
	return nil
}

func (s *fakeStore) GetSet(ctx context.Context, tenantID, setID string) (*domain.FinancialStatementSet, error) {
	set, ok := s.sets[setID]
	if !ok || set.TenantID != tenantID {
		return nil, domain.ErrStatementSetNotFound
	}
	cp := *set
	return &cp, nil
}

func (s *fakeStore) ListSets(ctx context.Context, tenantID, legalEntityID, status string) ([]domain.FinancialStatementSet, error) {
	var out []domain.FinancialStatementSet
	for _, set := range s.sets {
		if set.TenantID == tenantID && set.LegalEntityID == legalEntityID && (status == "" || string(set.Status) == status) {
			out = append(out, *set)
		}
	}
	return out, nil
}

func (s *fakeStore) GetFacts(ctx context.Context, tenantID, setID string) ([]domain.StatementFact, error) {
	return s.facts[setID], nil
}

func (s *fakeStore) GetFact(ctx context.Context, tenantID, factID string) (*domain.StatementFact, error) {
	for _, facts := range s.facts {
		for _, f := range facts {
			if f.FactID == factID {
				return &f, nil
			}
		}
	}
	return nil, domain.ErrFactNotFound
}

func (s *fakeStore) checkVersionAndStatus(setID string, expectedVersion int64, cmd domain.Command) (*domain.FinancialStatementSet, error) {
	set, ok := s.sets[setID]
	if !ok {
		return nil, domain.ErrStatementSetNotFound
	}
	if set.Version != expectedVersion {
		return nil, domain.Errf(domain.CodeVersionConflict, "version conflict")
	}
	if _, err := domain.Next(cmd, set.Status); err != nil {
		return nil, err
	}
	return set, nil
}

func (s *fakeStore) RefreshFacts(ctx context.Context, tenantID, setID string, expectedVersion int64, facts []domain.StatementFact, actorID, correlationID string) error {
	set, err := s.checkVersionAndStatus(setID, expectedVersion, domain.CmdRefreshDraft)
	if err != nil {
		return err
	}
	s.facts[setID] = facts
	set.Version++
	now := time.Now().UTC()
	set.LastRefreshedAt = &now
	set.LastRefreshedByPrincipalID = &actorID
	return nil
}

func (s *fakeStore) Validate(ctx context.Context, tenantID, setID string, expectedVersion int64, validationErrors []string, correlationID string) error {
	set, err := s.checkVersionAndStatus(setID, expectedVersion, domain.CmdValidate)
	if err != nil {
		return err
	}
	set.Version++
	if len(validationErrors) == 0 {
		set.Status = domain.StateValidated
	}
	set.ValidationErrors = validationErrors
	return nil
}

func (s *fakeStore) SubmitForReview(ctx context.Context, tenantID, setID string, expectedVersion int64, actorID, correlationID string) error {
	set, err := s.checkVersionAndStatus(setID, expectedVersion, domain.CmdSubmitForReview)
	if err != nil {
		return err
	}
	set.Status = domain.StateInReview
	set.Version++
	return nil
}

func (s *fakeStore) RequestChanges(ctx context.Context, tenantID, setID string, expectedVersion int64, actorID, correlationID string) error {
	set, err := s.checkVersionAndStatus(setID, expectedVersion, domain.CmdRequestChanges)
	if err != nil {
		return err
	}
	set.Status = domain.StateDraft
	set.Version++
	return nil
}

func (s *fakeStore) Approve(ctx context.Context, tenantID, setID string, expectedVersion int64, actorID, correlationID string) error {
	set, err := s.checkVersionAndStatus(setID, expectedVersion, domain.CmdApprove)
	if err != nil {
		return err
	}
	if actorID == set.Preparer() || s.authoredDisclosure(setID, actorID) {
		return domain.Errf(domain.CodeSoDDenied, "preparer may not approve")
	}
	set.Status = domain.StateApproved
	set.Version++
	return nil
}

func (s *fakeStore) Certify(ctx context.Context, tenantID, setID string, expectedVersion int64, actorID, reason, correlationID string) error {
	set, err := s.checkVersionAndStatus(setID, expectedVersion, domain.CmdCertify)
	if err != nil {
		return err
	}
	if actorID == set.Preparer() || s.authoredDisclosure(setID, actorID) {
		return domain.Errf(domain.CodeSoDDenied, "preparer may not certify")
	}
	set.Status = domain.StateCertified
	set.Version++
	set.CertificationReason = &reason
	return nil
}

func (s *fakeStore) Publish(ctx context.Context, tenantID, setID string, expectedVersion int64, actorID, correlationID string) error {
	set, err := s.checkVersionAndStatus(setID, expectedVersion, domain.CmdPublish)
	if err != nil {
		return err
	}
	set.Status = domain.StatePublished
	set.Version++
	return nil
}

func (s *fakeStore) Supersede(ctx context.Context, tenantID, oldSetID string, expectedOldVersion int64, newSet *domain.FinancialStatementSet, actorID, correlationID string) error {
	old, ok := s.sets[oldSetID]
	if !ok {
		return domain.ErrStatementSetNotFound
	}
	if old.Version != expectedOldVersion {
		return domain.Errf(domain.CodeVersionConflict, "version conflict")
	}
	if !domain.CanSupersede(old.Status) {
		return domain.Errf(domain.CodeInvalidTransition, "cannot supersede from this state")
	}
	old.Status = domain.StateSuperseded
	old.Version++
	oldID := oldSetID
	old.SupersededBySetID = &newSet.StatementSetID
	_ = oldID
	cp := *newSet
	s.sets[newSet.StatementSetID] = &cp
	return nil
}

func (s *fakeStore) AddDisclosure(ctx context.Context, tenantID, setID string, expectedVersion int64, d *domain.DisclosureInstance, correlationID string) (*domain.DisclosureInstance, bool, error) {
	set, ok := s.sets[setID]
	if !ok || set.TenantID != tenantID {
		return nil, false, domain.ErrStatementSetNotFound
	}
	// Same order as the real store: replay first, then version, then state.
	if d.IdempotencyKey != "" {
		for _, prior := range s.disclosures[setID] {
			if prior.IdempotencyKey == d.IdempotencyKey {
				if !prior.SameContent(d.Title, d.Body) || prior.AddedByPrincipalID != d.AddedByPrincipalID {
					return nil, false, domain.Errf(domain.CodeContextInvalid, "idempotency key reused for different content")
				}
				cp := prior
				return &cp, true, nil
			}
		}
	}
	if set.Version != expectedVersion {
		return nil, false, domain.Errf(domain.CodeVersionConflict, "version conflict")
	}
	if _, err := domain.Next(domain.CmdAddDisclosure, set.Status); err != nil {
		return nil, false, err
	}
	d.Sequence = len(s.disclosures[setID]) + 1
	s.disclosures[setID] = append(s.disclosures[setID], *d)
	set.Version++
	cp := *d
	return &cp, false, nil
}

func (s *fakeStore) ListDisclosures(ctx context.Context, tenantID, setID string) ([]domain.DisclosureInstance, error) {
	return s.disclosures[setID], nil
}

// authoredDisclosure mirrors the real store's lock-held SoD re-check.
func (s *fakeStore) authoredDisclosure(setID, actorID string) bool {
	for _, d := range s.disclosures[setID] {
		if d.AddedByPrincipalID == actorID {
			return true
		}
	}
	return false
}

// fakeSnapshots is a canned clients.SnapshotReader.
type fakeSnapshots struct {
	snapshots map[string]*clients.Snapshot
}

func (f *fakeSnapshots) GetSnapshot(ctx context.Context, tenantID, principalID, snapshotID string) (*clients.Snapshot, error) {
	snap, ok := f.snapshots[snapshotID]
	if !ok {
		return nil, domain.Errf(domain.CodeSourceUnverified, "snapshot %q not found", snapshotID)
	}
	return snap, nil
}

// fakeAuthZ grants everything unless denyAction is set.
type fakeAuthZ struct{ denyAction string }

func (f *fakeAuthZ) CheckAllowed(ctx context.Context, principalID, legalEntityID, actionType string) error {
	if f.denyAction != "" && actionType == f.denyAction {
		return domain.Errf(domain.CodeForbidden, "denied")
	}
	return nil
}

func certifiedSnapshot(legalEntityID string) *clients.Snapshot {
	content := domain.SnapshotContent{
		Currency: "GBP",
		Accounts: []domain.SnapshotContentAccount{
			{AccountCode: "1000", AccountName: "Cash", Category: domain.CategoryAsset, Balance: "500.00"},
			{AccountCode: "2000", AccountName: "Payables", Category: domain.CategoryLiability, Balance: "100.00"},
			{AccountCode: "3000", AccountName: "Equity", Category: domain.CategoryEquity, Balance: "400.00"},
		},
	}
	raw, _ := json.Marshal(content)
	return &clients.Snapshot{
		SnapshotID:    "snap-1",
		LegalEntityID: legalEntityID,
		Content:       string(raw),
		ContentHash:   "sha256:abc",
		Status:        "CERTIFIED",
	}
}

func newService(snaps map[string]*clients.Snapshot, denyAction string) (*service.Service, *fakeStore) {
	st := newFakeStore()
	svc := service.New(st, &fakeSnapshots{snapshots: snaps}, &fakeAuthZ{denyAction: denyAction}, zap.NewNop())
	return svc, st
}

// ── tests ────────────────────────────────────────────────────────────────

func TestCreateStatementSet_BindsToVerifiedCertifiedSnapshot(t *testing.T) {
	snap := certifiedSnapshot("le-1")
	svc, _ := newService(map[string]*clients.Snapshot{"snap-1": snap}, "")

	set, err := svc.CreateStatementSet(context.Background(), "tenant-1", "preparer-1", "corr-1", service.CreateSetRequest{
		LegalEntityID: "le-1", ReportingPeriod: "2026-Q1", Framework: "IFRS",
		PresentationCurrency: "GBP", SourceSnapshotID: "snap-1",
	})
	if err != nil {
		t.Fatalf("CreateStatementSet: %v", err)
	}
	if set.Status != domain.StateDraft {
		t.Fatalf("expected DRAFT, got %s", set.Status)
	}
	if set.SourceSnapshotHash != "sha256:abc" {
		t.Fatalf("expected snapshot hash to be bound, got %q", set.SourceSnapshotHash)
	}
}

func TestCreateStatementSet_RejectsMismatchedLegalEntity(t *testing.T) {
	snap := certifiedSnapshot("le-OTHER")
	svc, _ := newService(map[string]*clients.Snapshot{"snap-1": snap}, "")

	_, err := svc.CreateStatementSet(context.Background(), "tenant-1", "preparer-1", "corr-1", service.CreateSetRequest{
		LegalEntityID: "le-1", ReportingPeriod: "2026-Q1", Framework: "IFRS",
		PresentationCurrency: "GBP", SourceSnapshotID: "snap-1",
	})
	if err == nil {
		t.Fatal("expected an error for a snapshot bound to a different legal entity")
	}
	de, ok := domain.AsError(err)
	if !ok || de.Code != domain.CodeSourceUnverified {
		t.Fatalf("expected CodeSourceUnverified, got %v", err)
	}
}

func TestFullLifecycle_HappyPath(t *testing.T) {
	snap := certifiedSnapshot("le-1")
	svc, _ := newService(map[string]*clients.Snapshot{"snap-1": snap}, "")
	ctx := context.Background()

	set, err := svc.CreateStatementSet(ctx, "t1", "preparer", "c1", service.CreateSetRequest{
		LegalEntityID: "le-1", ReportingPeriod: "2026-Q1", Framework: "IFRS", PresentationCurrency: "GBP", SourceSnapshotID: "snap-1",
	})
	if err != nil {
		t.Fatalf("CreateStatementSet: %v", err)
	}

	if _, err := svc.RefreshDraft(ctx, "t1", "preparer", "c1", set.StatementSetID); err != nil {
		t.Fatalf("RefreshDraft: %v", err)
	}
	res, err := svc.ValidateStatements(ctx, "t1", "preparer", "c1", set.StatementSetID)
	if err != nil {
		t.Fatalf("ValidateStatements: %v", err)
	}
	if len(res.Errors) != 0 {
		t.Fatalf("expected a balanced sheet to validate cleanly, got errors: %v", res.Errors)
	}
	if err := svc.SubmitForReview(ctx, "t1", "preparer", "c1", set.StatementSetID); err != nil {
		t.Fatalf("SubmitForReview: %v", err)
	}
	if err := svc.Approve(ctx, "t1", "reviewer", "c1", set.StatementSetID); err != nil {
		t.Fatalf("Approve: %v", err)
	}
	if err := svc.Certify(ctx, "t1", "cfo", "c1", set.StatementSetID, "quarter end"); err != nil {
		t.Fatalf("Certify: %v", err)
	}
	if err := svc.Publish(ctx, "t1", "cfo", "c1", set.StatementSetID); err != nil {
		t.Fatalf("Publish: %v", err)
	}

	final, err := svc.GetStatementSet(ctx, "t1", "preparer", set.StatementSetID)
	if err != nil {
		t.Fatalf("GetStatementSet: %v", err)
	}
	if final.Status != domain.StatePublished {
		t.Fatalf("expected PUBLISHED, got %s", final.Status)
	}
}

func TestApprove_PreparerCannotApproveOwnWork(t *testing.T) {
	snap := certifiedSnapshot("le-1")
	svc, st := newService(map[string]*clients.Snapshot{"snap-1": snap}, "")
	ctx := context.Background()

	set, _ := svc.CreateStatementSet(ctx, "t1", "preparer", "c1", service.CreateSetRequest{
		LegalEntityID: "le-1", ReportingPeriod: "2026-Q1", Framework: "IFRS", PresentationCurrency: "GBP", SourceSnapshotID: "snap-1",
	})
	svc.RefreshDraft(ctx, "t1", "preparer", "c1", set.StatementSetID)
	svc.ValidateStatements(ctx, "t1", "preparer", "c1", set.StatementSetID)
	svc.SubmitForReview(ctx, "t1", "preparer", "c1", set.StatementSetID)

	err := svc.Approve(ctx, "t1", "preparer", "c1", set.StatementSetID)
	if err == nil {
		t.Fatal("expected the preparer's own approval to be denied")
	}
	de, ok := domain.AsError(err)
	if !ok || de.Code != domain.CodeSoDDenied {
		t.Fatalf("expected CodeSoDDenied, got %v", err)
	}
	if st.sets[set.StatementSetID].Status != domain.StateInReview {
		t.Fatal("a denied approve must not change the set's state")
	}
}

func TestCertify_PreparerCannotCertifyOwnWork(t *testing.T) {
	snap := certifiedSnapshot("le-1")
	svc, _ := newService(map[string]*clients.Snapshot{"snap-1": snap}, "")
	ctx := context.Background()

	set, _ := svc.CreateStatementSet(ctx, "t1", "preparer", "c1", service.CreateSetRequest{
		LegalEntityID: "le-1", ReportingPeriod: "2026-Q1", Framework: "IFRS", PresentationCurrency: "GBP", SourceSnapshotID: "snap-1",
	})
	svc.RefreshDraft(ctx, "t1", "preparer", "c1", set.StatementSetID)
	svc.ValidateStatements(ctx, "t1", "preparer", "c1", set.StatementSetID)
	svc.SubmitForReview(ctx, "t1", "preparer", "c1", set.StatementSetID)
	svc.Approve(ctx, "t1", "reviewer", "c1", set.StatementSetID)

	err := svc.Certify(ctx, "t1", "preparer", "c1", set.StatementSetID, "reason")
	if err == nil {
		t.Fatal("expected the preparer's own certification to be denied")
	}
	de, ok := domain.AsError(err)
	if !ok || de.Code != domain.CodeSoDDenied {
		t.Fatalf("expected CodeSoDDenied, got %v", err)
	}
}

func TestValidateStatements_RequiresFactsFirst(t *testing.T) {
	snap := certifiedSnapshot("le-1")
	svc, _ := newService(map[string]*clients.Snapshot{"snap-1": snap}, "")
	ctx := context.Background()

	set, _ := svc.CreateStatementSet(ctx, "t1", "preparer", "c1", service.CreateSetRequest{
		LegalEntityID: "le-1", ReportingPeriod: "2026-Q1", Framework: "IFRS", PresentationCurrency: "GBP", SourceSnapshotID: "snap-1",
	})
	if _, err := svc.ValidateStatements(ctx, "t1", "preparer", "c1", set.StatementSetID); err == nil {
		t.Fatal("expected validating a set with no facts yet to fail")
	}
}

func TestRequestChanges_SendsBackToDraft(t *testing.T) {
	snap := certifiedSnapshot("le-1")
	svc, st := newService(map[string]*clients.Snapshot{"snap-1": snap}, "")
	ctx := context.Background()

	set, _ := svc.CreateStatementSet(ctx, "t1", "preparer", "c1", service.CreateSetRequest{
		LegalEntityID: "le-1", ReportingPeriod: "2026-Q1", Framework: "IFRS", PresentationCurrency: "GBP", SourceSnapshotID: "snap-1",
	})
	svc.RefreshDraft(ctx, "t1", "preparer", "c1", set.StatementSetID)
	svc.ValidateStatements(ctx, "t1", "preparer", "c1", set.StatementSetID)
	svc.SubmitForReview(ctx, "t1", "preparer", "c1", set.StatementSetID)

	if err := svc.RequestChanges(ctx, "t1", "reviewer", "c1", set.StatementSetID); err != nil {
		t.Fatalf("RequestChanges: %v", err)
	}
	if st.sets[set.StatementSetID].Status != domain.StateDraft {
		t.Fatalf("expected DRAFT after RequestChanges, got %s", st.sets[set.StatementSetID].Status)
	}
}

func TestSupersede_OnlyFromCertifiedOrPublished(t *testing.T) {
	snap := certifiedSnapshot("le-1")
	svc, _ := newService(map[string]*clients.Snapshot{"snap-1": snap}, "")
	ctx := context.Background()

	set, _ := svc.CreateStatementSet(ctx, "t1", "preparer", "c1", service.CreateSetRequest{
		LegalEntityID: "le-1", ReportingPeriod: "2026-Q1", Framework: "IFRS", PresentationCurrency: "GBP", SourceSnapshotID: "snap-1",
	})
	// Still DRAFT: superseding must be refused.
	if _, err := svc.SupersedeStatementSet(ctx, "t1", "preparer", "c1", set.StatementSetID, "snap-1"); err == nil {
		t.Fatal("expected superseding a DRAFT set to be refused")
	}
}

func TestAuthorizationDenied_PropagatesAsForbidden(t *testing.T) {
	snap := certifiedSnapshot("le-1")
	svc, _ := newService(map[string]*clients.Snapshot{"snap-1": snap}, service.ActionStatementSetCreate)

	_, err := svc.CreateStatementSet(context.Background(), "t1", "preparer", "c1", service.CreateSetRequest{
		LegalEntityID: "le-1", ReportingPeriod: "2026-Q1", Framework: "IFRS", PresentationCurrency: "GBP", SourceSnapshotID: "snap-1",
	})
	if err == nil {
		t.Fatal("expected authorization denial to propagate")
	}
	de, ok := domain.AsError(err)
	if !ok || de.Code != domain.CodeForbidden {
		t.Fatalf("expected CodeForbidden, got %v", err)
	}
}

// ── GetComparatives ──────────────────────────────────────────────────────

func seedSet(st *fakeStore, id, tenantID, legalEntityID, framework, period string, status domain.State, facts ...domain.StatementFact) {
	st.sets[id] = &domain.FinancialStatementSet{
		StatementSetID: id, TenantID: tenantID, LegalEntityID: legalEntityID, Framework: framework,
		ReportingPeriod: period, PresentationCurrency: "GBP", MappingVersion: domain.MappingVersionV1, Status: status,
	}
	for i := range facts {
		facts[i].StatementSetID = id
	}
	st.facts[id] = facts
}

func bsFact(id, code, amount string) domain.StatementFact {
	return domain.StatementFact{FactID: id, StatementType: domain.StatementBalanceSheet, Category: domain.CategoryAsset, AccountCode: code, Amount: amount}
}

func comparativeFixture(priorStatus domain.State) (*service.Service, *fakeStore) {
	svc, st := newService(nil, "")
	seedSet(st, "cur", "t1", "le-1", "IFRS", "2026-Q2", domain.StateDraft, bsFact("c1", "1000", "600"), bsFact("c2", "1100", "40"))
	seedSet(st, "pri", "t1", "le-1", "IFRS", "2026-Q1", priorStatus, bsFact("p1", "1000", "500"), bsFact("p2", "2000", "75"))
	return svc, st
}

func expectCode(t *testing.T, err error, want domain.Code) {
	t.Helper()
	de, ok := domain.AsError(err)
	if !ok || de.Code != want {
		t.Fatalf("expected %s, got %v", want, err)
	}
}

func TestGetComparatives_PairsCertifiedPriorIncludingOneSidedLines(t *testing.T) {
	for _, status := range []domain.State{domain.StateCertified, domain.StatePublished} {
		svc, _ := comparativeFixture(status)
		view, err := svc.GetComparatives(context.Background(), "t1", "viewer", "cur", "pri")
		if err != nil {
			t.Fatalf("%s prior: GetComparatives: %v", status, err)
		}
		if view.Current.StatementSetID != "cur" || view.Prior.StatementSetID != "pri" || view.Prior.ReportingPeriod != "2026-Q1" {
			t.Fatalf("unexpected set refs: %+v / %+v", view.Current, view.Prior)
		}
		lines := view.Statements[0].Lines
		if len(lines) != 3 {
			t.Fatalf("expected 1000 (both), 1100 (current only), 2000 (prior only); got %+v", lines)
		}
		if *lines[0].Change != "100" || lines[1].PriorAmount != nil || lines[2].CurrentAmount != nil {
			t.Fatalf("unexpected pairing: %+v", lines)
		}
	}
}

func TestGetComparatives_PriorNotCertified_SourceUnverified(t *testing.T) {
	for _, status := range []domain.State{domain.StateDraft, domain.StateApproved, domain.StateSuperseded} {
		svc, _ := comparativeFixture(status)
		_, err := svc.GetComparatives(context.Background(), "t1", "viewer", "cur", "pri")
		expectCode(t, err, domain.CodeSourceUnverified)
	}
}

func TestGetComparatives_PriorContextMismatch_ContextInvalid(t *testing.T) {
	cases := map[string]func(*domain.FinancialStatementSet){
		"legal entity": func(s *domain.FinancialStatementSet) { s.LegalEntityID = "le-2" },
		"framework":    func(s *domain.FinancialStatementSet) { s.Framework = "US_GAAP" },
		"currency":     func(s *domain.FinancialStatementSet) { s.PresentationCurrency = "USD" },
		"same period":  func(s *domain.FinancialStatementSet) { s.ReportingPeriod = "2026-Q2" },
	}
	for name, mutate := range cases {
		svc, st := comparativeFixture(domain.StateCertified)
		mutate(st.sets["pri"])
		_, err := svc.GetComparatives(context.Background(), "t1", "viewer", "cur", "pri")
		if err == nil {
			t.Fatalf("%s mismatch: expected an error", name)
		}
		expectCode(t, err, domain.CodeContextInvalid)
	}
}

func TestGetComparatives_NotFoundAndMissingParam(t *testing.T) {
	svc, _ := comparativeFixture(domain.StateCertified)
	ctx := context.Background()

	if _, err := svc.GetComparatives(ctx, "t1", "viewer", "nope", "pri"); err != domain.ErrStatementSetNotFound {
		t.Errorf("current missing: expected ErrStatementSetNotFound, got %v", err)
	}
	_, err := svc.GetComparatives(ctx, "t1", "viewer", "cur", "nope")
	expectCode(t, err, domain.CodeNotFound)
	_, err = svc.GetComparatives(ctx, "t1", "viewer", "cur", "")
	expectCode(t, err, domain.CodeContextInvalid)
	_, err = svc.GetComparatives(ctx, "t1", "viewer", "cur", "cur")
	expectCode(t, err, domain.CodeContextInvalid)
}

func TestGetComparatives_AuthorizationDenied_Forbidden(t *testing.T) {
	_, st := comparativeFixture(domain.StateCertified)
	svc := service.New(st, &fakeSnapshots{}, &fakeAuthZ{denyAction: service.ActionStatementSetView}, zap.NewNop())
	_, err := svc.GetComparatives(context.Background(), "t1", "viewer", "cur", "pri")
	expectCode(t, err, domain.CodeForbidden)
}

func TestGetComparatives_PriorInAnotherTenant_NotFound(t *testing.T) {
	// Another tenant's set is invisible — reported as not found, never as
	// a legal-entity mismatch that would confirm it exists.
	svc, st := comparativeFixture(domain.StateCertified)
	st.sets["pri"].TenantID = "t-other"
	_, err := svc.GetComparatives(context.Background(), "t1", "viewer", "cur", "pri")
	expectCode(t, err, domain.CodeNotFound)
}

// ── statement line aggregation (MappingVersionV2) ───────────────────────

func snapshotWith(legalEntityID string, accounts ...domain.SnapshotContentAccount) *clients.Snapshot {
	raw, _ := json.Marshal(domain.SnapshotContent{Currency: "GBP", Accounts: accounts})
	return &clients.Snapshot{SnapshotID: "snap-agg", LegalEntityID: legalEntityID, Content: string(raw), ContentHash: "sha256:agg", Status: "CERTIFIED"}
}

func twoCashSnapshot() *clients.Snapshot {
	return snapshotWith("le-1",
		domain.SnapshotContentAccount{AccountCode: "1000", AccountName: "Cash", Category: domain.CategoryAsset, Balance: "300.00"},
		domain.SnapshotContentAccount{AccountCode: "1010", AccountName: "Cash", Category: domain.CategoryAsset, Balance: "200.00"},
		domain.SnapshotContentAccount{AccountCode: "2000", AccountName: "Payables", Category: domain.CategoryLiability, Balance: "100.00"},
		domain.SnapshotContentAccount{AccountCode: "3000", AccountName: "Equity", Category: domain.CategoryEquity, Balance: "400.00"},
	)
}

func createAndRefresh(t *testing.T, svc *service.Service) *domain.FinancialStatementSet {
	t.Helper()
	ctx := context.Background()
	set, err := svc.CreateStatementSet(ctx, "t1", "preparer", "c1", service.CreateSetRequest{
		LegalEntityID: "le-1", ReportingPeriod: "2026-Q1", Framework: "IFRS", PresentationCurrency: "GBP", SourceSnapshotID: "snap-agg",
	})
	if err != nil {
		t.Fatalf("CreateStatementSet: %v", err)
	}
	if _, err := svc.RefreshDraft(ctx, "t1", "preparer", "c1", set.StatementSetID); err != nil {
		t.Fatalf("RefreshDraft: %v", err)
	}
	return set
}

func TestNewSet_UsesCurrentMappingAndAggregatesSameNamedAccounts(t *testing.T) {
	svc, st := newService(map[string]*clients.Snapshot{"snap-agg": twoCashSnapshot()}, "")
	set := createAndRefresh(t, svc)
	if set.MappingVersion != domain.MappingVersionV2 {
		t.Fatalf("expected new sets under %s, got %s", domain.MappingVersionV2, set.MappingVersion)
	}
	facts := st.facts[set.StatementSetID]
	if len(facts) != 3 {
		t.Fatalf("expected Cash (1000+1010), Payables, Equity; got %d facts: %+v", len(facts), facts)
	}
	// Aggregation regroups lines; the tie-out over them is unchanged.
	res, err := svc.ValidateStatements(context.Background(), "t1", "preparer", "c1", set.StatementSetID)
	if err != nil || len(res.Errors) != 0 {
		t.Fatalf("expected the aggregated set to tie out, got %v / %v", res, err)
	}
}

func TestExplainReportedFact_ListsEverySourceAccountOfAnAggregatedLine(t *testing.T) {
	svc, st := newService(map[string]*clients.Snapshot{"snap-agg": twoCashSnapshot()}, "")
	set := createAndRefresh(t, svc)

	var cashID, payID string
	for _, f := range st.facts[set.StatementSetID] {
		switch f.AccountName {
		case "Cash":
			cashID = f.FactID
		case "Payables":
			payID = f.FactID
		}
	}

	ex, err := svc.ExplainReportedFact(context.Background(), "t1", "viewer", cashID)
	if err != nil {
		t.Fatalf("ExplainReportedFact: %v", err)
	}
	if !ex.Aggregated || len(ex.SourceAccounts) != 2 {
		t.Fatalf("expected an aggregated fact with 2 source accounts, got %+v", ex)
	}
	if ex.SourceAccounts[0].AccountCode != "1000" || ex.SourceAccounts[0].Balance != "300.00" ||
		ex.SourceAccounts[1].AccountCode != "1010" || ex.SourceAccounts[1].Balance != "200.00" {
		t.Errorf("expected 1000 (300.00) and 1010 (200.00), got %+v", ex.SourceAccounts)
	}
	if ex.Fact.Amount != "500" || ex.SourceSnapshotHash != "sha256:agg" {
		t.Errorf("unexpected fact/provenance: %+v", ex)
	}

	single, err := svc.ExplainReportedFact(context.Background(), "t1", "viewer", payID)
	if err != nil {
		t.Fatalf("ExplainReportedFact (single): %v", err)
	}
	if single.Aggregated || len(single.SourceAccounts) != 1 || single.SourceAccounts[0].AccountCode != "2000" {
		t.Errorf("expected a single-source, non-aggregated fact, got %+v", single)
	}
}

func TestExplainReportedFact_PreAggregationFactExplainsItself(t *testing.T) {
	// A fact stored before source_accounts existed has none recorded.
	svc, st := newService(nil, "")
	seedSet(st, "old", "t1", "le-1", "IFRS", "2025-Q4", domain.StateCertified, bsFact("f-old", "1000", "42"))
	ex, err := svc.ExplainReportedFact(context.Background(), "t1", "viewer", "f-old")
	if err != nil {
		t.Fatalf("ExplainReportedFact: %v", err)
	}
	if ex.Aggregated || len(ex.SourceAccounts) != 1 || ex.SourceAccounts[0].AccountCode != "1000" || ex.SourceAccounts[0].Balance != "42" {
		t.Fatalf("expected the fact itself as its single source, got %+v", ex)
	}
}

func TestExplainReportedFact_SourcesNotSummingToAmount_Refused(t *testing.T) {
	svc, st := newService(map[string]*clients.Snapshot{"snap-agg": twoCashSnapshot()}, "")
	set := createAndRefresh(t, svc)
	var cashID string
	for i, f := range st.facts[set.StatementSetID] {
		if f.AccountName == "Cash" {
			cashID = f.FactID
			st.facts[set.StatementSetID][i].Amount = "501" // tampered total
		}
	}
	_, err := svc.ExplainReportedFact(context.Background(), "t1", "viewer", cashID)
	expectCode(t, err, domain.CodeValidationFailed)
}

func TestRefreshDraft_V1SetKeepsOneFactPerAccount(t *testing.T) {
	// A set created before aggregation keeps the mapping it recorded.
	svc, st := newService(map[string]*clients.Snapshot{"snap-agg": twoCashSnapshot()}, "")
	set := createAndRefresh(t, svc)
	st.sets[set.StatementSetID].MappingVersion = domain.MappingVersionV1
	if _, err := svc.RefreshDraft(context.Background(), "t1", "preparer", "c1", set.StatementSetID); err != nil {
		t.Fatalf("RefreshDraft: %v", err)
	}
	if n := len(st.facts[set.StatementSetID]); n != 4 {
		t.Fatalf("expected V1 to keep 4 per-account facts, got %d", n)
	}
}

func TestGetComparatives_V2SetsPairAggregatedLines(t *testing.T) {
	svc, st := newService(nil, "")
	cur := mustFactsV2(t, "cur",
		domain.SnapshotContentAccount{AccountCode: "1000", AccountName: "Cash", Category: domain.CategoryAsset, Balance: "70"},
		domain.SnapshotContentAccount{AccountCode: "1010", AccountName: "Cash", Category: domain.CategoryAsset, Balance: "30"})
	pri := mustFactsV2(t, "pri",
		domain.SnapshotContentAccount{AccountCode: "1000", AccountName: "Cash", Category: domain.CategoryAsset, Balance: "75"})
	seedSet(st, "cur", "t1", "le-1", "IFRS", "2026-Q2", domain.StateDraft, cur...)
	seedSet(st, "pri", "t1", "le-1", "IFRS", "2026-Q1", domain.StateCertified, pri...)
	st.sets["cur"].MappingVersion, st.sets["pri"].MappingVersion = domain.MappingVersionV2, domain.MappingVersionV2

	view, err := svc.GetComparatives(context.Background(), "t1", "viewer", "cur", "pri")
	if err != nil {
		t.Fatalf("GetComparatives: %v", err)
	}
	lines := view.Statements[0].Lines
	if len(lines) != 1 || *lines[0].Change != "25" || len(lines[0].CurrentSourceAccountCodes) != 2 || len(lines[0].PriorSourceAccountCodes) != 1 {
		t.Fatalf("expected one Cash line 100 vs 75 with composition 2 vs 1, got %+v", lines)
	}
}

func TestGetComparatives_V1AgainstV2_Refused(t *testing.T) {
	// Different granularities are never paired (B8): a per-account V1 set
	// cannot be compared line-for-line with an aggregated V2 set.
	svc, st := comparativeFixture(domain.StateCertified)
	st.sets["cur"].MappingVersion = domain.MappingVersionV2
	_, err := svc.GetComparatives(context.Background(), "t1", "viewer", "cur", "pri")
	expectCode(t, err, domain.CodeContextInvalid)
}

func mustFactsV2(t *testing.T, prefix string, accounts ...domain.SnapshotContentAccount) []domain.StatementFact {
	t.Helper()
	facts, err := domain.BuildFacts(domain.SnapshotContent{Currency: "GBP", Accounts: accounts}, domain.MappingVersionV2)
	if err != nil {
		t.Fatalf("BuildFacts: %v", err)
	}
	for i := range facts {
		facts[i].FactID = prefix + "-" + facts[i].AccountCode
	}
	return facts
}

// ── disclosures (DisclosureInstance) ─────────────────────────────────────

func TestAddDisclosure_HappyPath_AddsInSequenceAndLists(t *testing.T) {
	svc, st := newService(nil, "")
	seedSet(st, "s1", "t1", "le-1", "IFRS", "2026-Q1", domain.StateDraft)
	ctx := context.Background()

	d1, replayed, err := svc.AddDisclosure(ctx, "t1", "preparer", "c1", "", "s1", "  Basis of preparation ", " Prepared under IFRS. ")
	if err != nil || replayed {
		t.Fatalf("AddDisclosure: %v (replayed=%v)", err, replayed)
	}
	if d1.Sequence != 1 || d1.Title != "Basis of preparation" || d1.Body != "Prepared under IFRS." || d1.AddedByPrincipalID != "preparer" || d1.AddedAt.IsZero() {
		t.Fatalf("unexpected first disclosure: %+v", d1)
	}
	if _, _, err := svc.AddDisclosure(ctx, "t1", "preparer", "c1", "", "s1", "Going concern", "No material uncertainty."); err != nil {
		t.Fatalf("second AddDisclosure: %v", err)
	}
	list, err := svc.GetDisclosures(ctx, "t1", "viewer", "s1")
	if err != nil {
		t.Fatalf("GetDisclosures: %v", err)
	}
	if len(list) != 2 || list[0].Sequence != 1 || list[1].Sequence != 2 || list[1].Title != "Going concern" {
		t.Fatalf("expected notes 1 and 2 in order, got %+v", list)
	}
	if st.sets["s1"].Version != 2 {
		t.Errorf("each add is a content change and must bump the set version; got %d", st.sets["s1"].Version)
	}
}

func TestAddDisclosure_OnlyInDraft(t *testing.T) {
	// DRAFT is the only state report content may change in; every other
	// state, including VALIDATED (the one right before review), refuses.
	for _, status := range []domain.State{
		domain.StateValidated, domain.StateInReview, domain.StateApproved,
		domain.StateCertified, domain.StatePublished, domain.StateSuperseded,
	} {
		svc, st := newService(nil, "")
		seedSet(st, "s1", "t1", "le-1", "IFRS", "2026-Q1", status)
		_, _, err := svc.AddDisclosure(context.Background(), "t1", "preparer", "c1", "", "s1", "Note", "Text")
		expectCode(t, err, domain.CodeInvalidTransition)
		if len(st.disclosures["s1"]) != 0 {
			t.Fatalf("%s: a refused add must store nothing", status)
		}
	}
}

func TestAddDisclosure_AllowedAgainAfterRequestChanges(t *testing.T) {
	svc, st := newService(map[string]*clients.Snapshot{"snap-agg": twoCashSnapshot()}, "")
	set := createAndRefresh(t, svc)
	ctx := context.Background()
	if _, err := svc.ValidateStatements(ctx, "t1", "preparer", "c1", set.StatementSetID); err != nil {
		t.Fatalf("validate: %v", err)
	}
	if _, _, err := svc.AddDisclosure(ctx, "t1", "preparer", "c1", "", set.StatementSetID, "Note", "Text"); err == nil {
		t.Fatal("expected VALIDATED to refuse a new disclosure")
	}
	if err := svc.SubmitForReview(ctx, "t1", "preparer", "c1", set.StatementSetID); err != nil {
		t.Fatalf("submit: %v", err)
	}
	if err := svc.RequestChanges(ctx, "t1", "reviewer", "c1", set.StatementSetID); err != nil {
		t.Fatalf("request changes: %v", err)
	}
	if _, _, err := svc.AddDisclosure(ctx, "t1", "preparer", "c1", "", set.StatementSetID, "Note", "Text"); err != nil {
		t.Fatalf("expected DRAFT (after REQUEST_CHANGES) to accept a disclosure, got %v", err)
	}
	if len(st.disclosures[set.StatementSetID]) != 1 {
		t.Fatal("expected exactly one stored disclosure")
	}
}

func TestAddDisclosure_ValidatesText(t *testing.T) {
	cases := map[string][2]string{
		"empty title":    {"  ", "body"},
		"empty body":     {"title", ""},
		"body too long":  {"title", strings.Repeat("x", domain.MaxDisclosureBodyRunes+1)},
		"title too long": {strings.Repeat("t", domain.MaxDisclosureTitleRunes+1), "body"},
	}
	for name, tc := range cases {
		svc, st := newService(nil, "")
		seedSet(st, "s1", "t1", "le-1", "IFRS", "2026-Q1", domain.StateDraft)
		_, _, err := svc.AddDisclosure(context.Background(), "t1", "preparer", "c1", "", "s1", tc[0], tc[1])
		if err == nil {
			t.Fatalf("%s: expected an error", name)
		}
		expectCode(t, err, domain.CodeValidationFailed)
	}
}

func TestAddDisclosure_IdempotentReplay(t *testing.T) {
	svc, st := newService(nil, "")
	seedSet(st, "s1", "t1", "le-1", "IFRS", "2026-Q1", domain.StateDraft)
	ctx := context.Background()

	first, _, err := svc.AddDisclosure(ctx, "t1", "preparer", "c1", "key-1", "s1", "Note", "Text")
	if err != nil {
		t.Fatalf("first add: %v", err)
	}
	// The set moves on before the retry arrives: the retry must still get
	// the original note back, not INVALID_TRANSITION.
	st.sets["s1"].Status = domain.StateValidated
	again, replayed, err := svc.AddDisclosure(ctx, "t1", "preparer", "c1", "key-1", "s1", "Note", "Text")
	if err != nil || !replayed || again.DisclosureID != first.DisclosureID {
		t.Fatalf("expected a replay of %s, got %+v replayed=%v err=%v", first.DisclosureID, again, replayed, err)
	}
	if len(st.disclosures["s1"]) != 1 {
		t.Fatalf("a replay must not store a second note, got %d", len(st.disclosures["s1"]))
	}
	_, _, err = svc.AddDisclosure(ctx, "t1", "preparer", "c1", "key-1", "s1", "Note", "Different text")
	expectCode(t, err, domain.CodeContextInvalid)
}

func TestAddDisclosure_AuthorizationDenied(t *testing.T) {
	_, st := newService(nil, "")
	seedSet(st, "s1", "t1", "le-1", "IFRS", "2026-Q1", domain.StateDraft)
	svc := service.New(st, &fakeSnapshots{}, &fakeAuthZ{denyAction: service.ActionStatementSetDisclosureAdd}, zap.NewNop())
	_, _, err := svc.AddDisclosure(context.Background(), "t1", "preparer", "c1", "", "s1", "Note", "Text")
	expectCode(t, err, domain.CodeForbidden)

	viewDenied := service.New(st, &fakeSnapshots{}, &fakeAuthZ{denyAction: service.ActionStatementSetView}, zap.NewNop())
	_, err = viewDenied.GetDisclosures(context.Background(), "t1", "viewer", "s1")
	expectCode(t, err, domain.CodeForbidden)
}

func TestAddDisclosure_SetNotFound(t *testing.T) {
	svc, _ := newService(nil, "")
	if _, _, err := svc.AddDisclosure(context.Background(), "t1", "preparer", "c1", "", "missing", "Note", "Text"); err != domain.ErrStatementSetNotFound {
		t.Fatalf("expected ErrStatementSetNotFound, got %v", err)
	}
	if _, err := svc.GetDisclosures(context.Background(), "t1", "viewer", "missing"); err != domain.ErrStatementSetNotFound {
		t.Fatalf("expected ErrStatementSetNotFound, got %v", err)
	}
}

func TestDisclosureAuthor_MayNotApproveOrCertify(t *testing.T) {
	svc, _ := newService(map[string]*clients.Snapshot{"snap-agg": twoCashSnapshot()}, "")
	set := createAndRefresh(t, svc)
	ctx := context.Background()
	id := set.StatementSetID

	if _, _, err := svc.AddDisclosure(ctx, "t1", "writer", "c1", "", id, "Note", "Text"); err != nil {
		t.Fatalf("AddDisclosure: %v", err)
	}
	if _, err := svc.ValidateStatements(ctx, "t1", "preparer", "c1", id); err != nil {
		t.Fatalf("validate: %v", err)
	}
	if err := svc.SubmitForReview(ctx, "t1", "preparer", "c1", id); err != nil {
		t.Fatalf("submit: %v", err)
	}
	expectCode(t, svc.Approve(ctx, "t1", "writer", "c1", id), domain.CodeSoDDenied)
	if err := svc.Approve(ctx, "t1", "reviewer", "c1", id); err != nil {
		t.Fatalf("an independent reviewer must still be able to approve: %v", err)
	}
	expectCode(t, svc.Certify(ctx, "t1", "writer", "c1", id, "year end"), domain.CodeSoDDenied)
	if err := svc.Certify(ctx, "t1", "cfo", "c1", id, "year end"); err != nil {
		t.Fatalf("an independent certifier must still be able to certify: %v", err)
	}
	// Certified: the disclosure is now frozen with the set.
	_, _, err := svc.AddDisclosure(ctx, "t1", "writer", "c1", "", id, "Late note", "Text")
	expectCode(t, err, domain.CodeInvalidTransition)
}
