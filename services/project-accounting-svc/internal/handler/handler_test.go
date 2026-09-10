package handler_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"go.uber.org/zap"

	"zoiko.io/project-accounting-svc/internal/clients"
	"zoiko.io/project-accounting-svc/internal/domain"
	"zoiko.io/project-accounting-svc/internal/handler"
	"zoiko.io/project-accounting-svc/internal/middleware"
)

// ── stubs ─────────────────────────────────────────────────────────────────────

type stubStore struct {
	projects map[string]*domain.Project

	workPackages map[string][]domain.WorkPackage // project_id -> work packages

	financialProfiles map[string]*domain.FinancialProfile // project_id -> current version

	costEntries     map[string]*domain.CostEntry
	entriesBySource map[string]string // "source_type|source_reference" -> entry_id

	estimates map[string]*domain.RecognitionEstimate // project_id -> current version
	runs      map[string]*domain.RecognitionRun

	projections map[string]*domain.ProfitabilityProjection // project_id -> live projection
	snapshots   map[string]*domain.ProfitabilitySnapshot
}

func newStubStore() *stubStore {
	return &stubStore{
		projects:          make(map[string]*domain.Project),
		workPackages:      make(map[string][]domain.WorkPackage),
		financialProfiles: make(map[string]*domain.FinancialProfile),
		costEntries:       make(map[string]*domain.CostEntry),
		entriesBySource:   make(map[string]string),
		estimates:         make(map[string]*domain.RecognitionEstimate),
		runs:              make(map[string]*domain.RecognitionRun),
		projections:       make(map[string]*domain.ProfitabilityProjection),
		snapshots:         make(map[string]*domain.ProfitabilitySnapshot),
	}
}

// ── PRJ-04 (Project Profitability) ───────────────────────────────────────────

func (s *stubStore) RefreshProfitabilityProjection(_ context.Context, projectID, principalID string, at time.Time) (*domain.ProfitabilityProjection, error) {
	var revenue, cost float64
	var runID *string
	var revenueWatermark *time.Time
	for _, r := range s.runs {
		if r.ProjectID != projectID || r.Status == domain.RecognitionRunStatusDraft || r.Status == domain.RecognitionRunStatusPopulationFrozen || r.Status == domain.RecognitionRunStatusSuperseded {
			continue
		}
		if r.CalculatedAt != nil && (revenueWatermark == nil || r.CalculatedAt.After(*revenueWatermark)) {
			revenueWatermark = r.CalculatedAt
			id := r.RunID
			runID = &id
			if r.CumulativeRecognizedRevenue != nil {
				revenue = *r.CumulativeRecognizedRevenue
			}
		}
	}
	costWatermark := at
	for _, e := range s.costEntries {
		if e.ProjectID == projectID && e.Status != domain.CostEntryStatusReversed {
			cost += e.Amount
		}
	}
	p := &domain.ProfitabilityProjection{
		ProjectionID: "projection-" + projectID, ProjectID: projectID, Status: domain.ProfitabilityProjectionStatusCurrent,
		Revenue: revenue, Cost: cost, Margin: revenue - cost, CostWatermarkAt: costWatermark, RevenueRunID: runID, RevenueWatermarkAt: revenueWatermark,
		RefreshedAt: at, RefreshedByPrincipalID: principalID, CreatedAt: at,
	}
	if pr, ok := s.projects[projectID]; ok {
		p.LegalEntityID = pr.LegalEntityID
	}
	s.projections[projectID] = p
	cp := *p
	return &cp, nil
}

func (s *stubStore) GetProjectProfitability(_ context.Context, projectID string) (*domain.ProfitabilityProjection, error) {
	p, ok := s.projections[projectID]
	if !ok {
		return nil, domain.ErrProjectionNotBuilt
	}
	cp := *p
	return &cp, nil
}

func (s *stubStore) BuildProfitabilitySnapshot(_ context.Context, projectID, principalID string, at time.Time) (*domain.ProfitabilitySnapshot, error) {
	p, ok := s.projections[projectID]
	if !ok {
		return nil, domain.ErrProjectionNotBuilt
	}
	if p.Status != domain.ProfitabilityProjectionStatusCurrent {
		return nil, domain.ErrProjectionStale
	}
	snapshotSeq++
	snap := &domain.ProfitabilitySnapshot{
		SnapshotID: fmt.Sprintf("snapshot-%d", snapshotSeq), LegalEntityID: p.LegalEntityID, ProjectID: projectID, Status: domain.ProfitabilitySnapshotStatusReconciled,
		Revenue: p.Revenue, Cost: p.Cost, Margin: p.Margin, BilledAmount: p.BilledAmount, UnbilledAmount: p.UnbilledAmount,
		CostWatermarkAt: p.CostWatermarkAt, RevenueRunID: p.RevenueRunID, RevenueWatermarkAt: p.RevenueWatermarkAt,
		BuiltAt: at, BuiltByPrincipalID: principalID,
	}
	s.snapshots[snap.SnapshotID] = snap
	cp := *snap
	return &cp, nil
}

func (s *stubStore) GetProfitabilitySnapshot(_ context.Context, snapshotID string) (*domain.ProfitabilitySnapshot, error) {
	snap, ok := s.snapshots[snapshotID]
	if !ok {
		return nil, domain.ErrSnapshotNotFound
	}
	cp := *snap
	return &cp, nil
}

func (s *stubStore) CertifyProfitabilitySnapshot(_ context.Context, snapshotID, principalID string, at time.Time) (*domain.ProfitabilitySnapshot, error) {
	snap, ok := s.snapshots[snapshotID]
	if !ok {
		return nil, domain.ErrSnapshotNotFound
	}
	if snap.Status != domain.ProfitabilitySnapshotStatusReconciled {
		return nil, domain.ErrInvalidSnapshotTransition
	}
	snap.Status, snap.CertifiedAt, snap.CertifiedByPrincipalID = domain.ProfitabilitySnapshotStatusCertified, &at, &principalID
	cp := *snap
	return &cp, nil
}

var snapshotSeq int

// ── PRJ-03 (Project Revenue & WIP) ───────────────────────────────────────────

func (s *stubStore) SetApprovedEstimate(_ context.Context, newVersion *domain.RecognitionEstimate) error {
	if current, ok := s.estimates[newVersion.ProjectID]; ok {
		et := newVersion.EffectiveFrom
		current.EffectiveTo = &et
	}
	cp := *newVersion
	s.estimates[newVersion.ProjectID] = &cp
	return nil
}

func (s *stubStore) GetCurrentEstimate(_ context.Context, projectID string) (*domain.RecognitionEstimate, error) {
	e, ok := s.estimates[projectID]
	if !ok {
		return nil, nil
	}
	cp := *e
	return &cp, nil
}

func (s *stubStore) CreateRecognitionRun(_ context.Context, r *domain.RecognitionRun) error {
	for _, existing := range s.runs {
		if existing.ProjectID == r.ProjectID && existing.FiscalPeriod == r.FiscalPeriod && existing.Status != domain.RecognitionRunStatusSuperseded {
			return domain.ErrRecognitionRunAlreadyExistsForPeriod
		}
	}
	cp := *r
	s.runs[r.RunID] = &cp
	return nil
}

func (s *stubStore) GetRecognitionRun(_ context.Context, runID string) (*domain.RecognitionRun, error) {
	r, ok := s.runs[runID]
	if !ok {
		return nil, domain.ErrRecognitionRunNotFound
	}
	cp := *r
	return &cp, nil
}

func (s *stubStore) FreezeAndCalculate(_ context.Context, runID string, at time.Time) error {
	r, ok := s.runs[runID]
	if !ok {
		return domain.ErrRecognitionRunNotFound
	}
	if r.Status != domain.RecognitionRunStatusDraft {
		return domain.ErrInvalidRecognitionRunTransition
	}
	est, ok := s.estimates[r.ProjectID]
	if !ok {
		return domain.ErrApprovedEstimateRequired
	}
	contractValue := 0.0
	if r.ContractValue != nil {
		contractValue = *r.ContractValue
	}
	billedToDate := 0.0
	if r.BilledToDate != nil {
		billedToDate = *r.BilledToDate
	}
	itd := 0.0
	percentComplete := 0.0
	if itd+est.EstimateToComplete > 0 {
		percentComplete = itd / (itd + est.EstimateToComplete)
	}
	cumulative := percentComplete * contractValue
	period := cumulative
	margin := cumulative - itd
	balance := cumulative - billedToDate
	balanceType := domain.BalanceTypeNone
	if balance > 0 {
		balanceType = domain.BalanceTypeContractAsset
	} else if balance < 0 {
		balanceType = domain.BalanceTypeContractLiability
	}
	r.Status, r.FrozenAt, r.CalculatedAt = domain.RecognitionRunStatusCalculated, &at, &at
	r.EstimateToComplete, r.ITDCostIncurred, r.PercentComplete = &est.EstimateToComplete, &itd, &percentComplete
	r.CumulativeRecognizedRevenue, r.PeriodRecognizedRevenue, r.RecognizedCost = &cumulative, &period, &itd
	r.Margin, r.BalanceAmount, r.BalanceType = &margin, &balance, &balanceType
	return nil
}

func (s *stubStore) ValidateRecognitionRun(_ context.Context, runID string, at time.Time) error {
	r, ok := s.runs[runID]
	if !ok || r.Status != domain.RecognitionRunStatusCalculated {
		return domain.ErrInvalidRecognitionRunTransition
	}
	r.Status, r.ValidatedAt = domain.RecognitionRunStatusReviewed, &at
	return nil
}

func (s *stubStore) ApproveRecognitionRun(_ context.Context, runID, principalID string, at time.Time) error {
	r, ok := s.runs[runID]
	if !ok || r.Status != domain.RecognitionRunStatusReviewed {
		return domain.ErrInvalidRecognitionRunTransition
	}
	r.Status, r.ApprovedAt, r.ApprovedByPrincipalID = domain.RecognitionRunStatusApproved, &at, &principalID
	return nil
}

func (s *stubStore) MarkRecognitionRunEmitted(_ context.Context, runID, journalID string, at time.Time) error {
	r, ok := s.runs[runID]
	if !ok || r.Status != domain.RecognitionRunStatusApproved {
		return domain.ErrInvalidRecognitionRunTransition
	}
	r.Status, r.EmittedAt, r.JournalID = domain.RecognitionRunStatusAccountingEventEmitted, &at, &journalID
	return nil
}

func (s *stubStore) SupersedeRecognitionRun(_ context.Context, runID, principalID string, at time.Time) error {
	r, ok := s.runs[runID]
	if !ok {
		return domain.ErrRecognitionRunNotFound
	}
	r.Status, r.SupersededAt, r.SupersededByPrincipalID = domain.RecognitionRunStatusSuperseded, &at, &principalID
	return nil
}

func (s *stubStore) GetPostedRevenueTotal(_ context.Context, legalEntityID, fiscalPeriod string) (float64, error) {
	var total float64
	for _, r := range s.runs {
		if r.LegalEntityID != legalEntityID || r.FiscalPeriod != fiscalPeriod || r.Status != domain.RecognitionRunStatusAccountingEventEmitted {
			continue
		}
		if r.PeriodRecognizedRevenue != nil {
			total += *r.PeriodRecognizedRevenue
		}
	}
	return total, nil
}

// ── PRJ-02 (Project Cost Capture) ────────────────────────────────────────────

func (s *stubStore) CaptureProjectCost(_ context.Context, e *domain.CostEntry) error {
	key := e.SourceType + "|" + e.SourceReference
	if existingID, ok := s.entriesBySource[key]; ok {
		*e = *s.costEntries[existingID]
		return nil
	}
	cp := *e
	s.costEntries[e.EntryID] = &cp
	s.entriesBySource[key] = e.EntryID
	return nil
}

func (s *stubStore) GetCostEntry(_ context.Context, entryID string) (*domain.CostEntry, error) {
	e, ok := s.costEntries[entryID]
	if !ok {
		return nil, domain.ErrCostEntryNotFound
	}
	cp := *e
	return &cp, nil
}

func (s *stubStore) ListCostEntries(_ context.Context, projectID, wbsID string) ([]domain.CostEntry, error) {
	var out []domain.CostEntry
	for _, e := range s.costEntries {
		if e.ProjectID != projectID {
			continue
		}
		if wbsID != "" && (e.WBSID == nil || *e.WBSID != wbsID) {
			continue
		}
		out = append(out, *e)
	}
	return out, nil
}

func (s *stubStore) ValidateProjectCost(_ context.Context, entryID string, at time.Time) error {
	e, ok := s.costEntries[entryID]
	if !ok || e.Status != domain.CostEntryStatusCaptured {
		return domain.ErrInvalidCostEntryTransition
	}
	e.Status, e.ValidatedAt = domain.CostEntryStatusAccepted, &at
	return nil
}

func (s *stubStore) MarkBillableEligibility(_ context.Context, entryID string, billable, capitalizable bool) error {
	e, ok := s.costEntries[entryID]
	if !ok {
		return domain.ErrCostEntryNotFound
	}
	e.Billable, e.Capitalizable = billable, capitalizable
	return nil
}

func (s *stubStore) CreateLinkedCostEntry(_ context.Context, originalEntryID, principalID, reason string, isReversal bool, newEntryID string, amountOverride *float64, costCategory *string, billable, capitalizable *bool, at time.Time) (*domain.CostEntry, error) {
	original, ok := s.costEntries[originalEntryID]
	if !ok {
		return nil, domain.ErrCostEntryNotFound
	}
	if original.CreatedByPrincipalID == principalID {
		if isReversal {
			return nil, domain.ErrSelfApprovalNotPermittedReversal
		}
		return nil, domain.ErrSelfApprovalNotPermittedReclassify
	}
	if isReversal && original.Status == domain.CostEntryStatusReversed {
		return nil, domain.ErrCostEntryAlreadyReversed
	}

	linked := &domain.CostEntry{
		EntryID: newEntryID, LegalEntityID: original.LegalEntityID, ProjectID: original.ProjectID, WBSID: original.WBSID,
		SourceType: original.SourceType, SourceReference: newEntryID, CostCategory: original.CostCategory,
		Quantity: original.Quantity, Currency: original.Currency, TransactionDate: at,
		Billable: original.Billable, Capitalizable: original.Capitalizable,
		Status: domain.CostEntryStatusAccepted, Reason: &reason,
		CreatedAt: at, CreatedByPrincipalID: principalID, ApprovedAt: &at, ApprovedByPrincipalID: &principalID,
	}
	if isReversal {
		linked.Amount = -original.Amount
		linked.ReversesEntryID = &originalEntryID
		original.Status = domain.CostEntryStatusReversed
	} else {
		linked.Amount = original.Amount
		if amountOverride != nil {
			linked.Amount = *amountOverride
		}
		if costCategory != nil {
			linked.CostCategory = *costCategory
		}
		if billable != nil {
			linked.Billable = *billable
		}
		if capitalizable != nil {
			linked.Capitalizable = *capitalizable
		}
		linked.ReclassifiesEntryID = &originalEntryID
	}
	s.costEntries[linked.EntryID] = linked
	cp := *linked
	return &cp, nil
}

func (s *stubStore) CertifyCostPopulation(_ context.Context, projectID, principalID string, at time.Time) (*domain.CostCertification, error) {
	var count int
	var total float64
	for _, e := range s.costEntries {
		if e.ProjectID == projectID && e.Status != domain.CostEntryStatusReversed {
			count++
			total += e.Amount
		}
	}
	return &domain.CostCertification{
		CertificationID: "cert-" + projectID, ProjectID: projectID, EntryCount: count, TotalAmount: total,
		CertifiedAt: at, CertifiedByPrincipalID: principalID,
	}, nil
}

func (s *stubStore) CreateProject(_ context.Context, p *domain.Project) error {
	for _, existing := range s.projects {
		if existing.LegalEntityID == p.LegalEntityID && existing.ProjectCode == p.ProjectCode {
			return domain.ErrDuplicateProjectCode
		}
	}
	cp := *p
	s.projects[p.ProjectID] = &cp
	return nil
}

func (s *stubStore) GetProject(_ context.Context, projectID string) (*domain.Project, error) {
	p, ok := s.projects[projectID]
	if !ok {
		return nil, domain.ErrProjectNotFound
	}
	cp := *p
	cp.WorkPackages = s.workPackages[projectID]
	return &cp, nil
}

func (s *stubStore) ListProjects(_ context.Context, legalEntityID string) ([]domain.Project, error) {
	var out []domain.Project
	for _, p := range s.projects {
		if p.LegalEntityID == legalEntityID {
			out = append(out, *p)
		}
	}
	return out, nil
}

func (s *stubStore) ApproveProject(_ context.Context, projectID, principalID string, at time.Time) error {
	p, ok := s.projects[projectID]
	if !ok || p.Status != domain.ProjectStatusDraft {
		return domain.ErrInvalidProjectTransition
	}
	p.Status, p.ApprovedAt, p.ApprovedByPrincipalID = domain.ProjectStatusApproved, &at, &principalID
	return nil
}

func (s *stubStore) ActivateProject(_ context.Context, projectID string, at time.Time) error {
	p, ok := s.projects[projectID]
	if !ok || p.Status != domain.ProjectStatusApproved {
		return domain.ErrInvalidProjectTransition
	}
	p.Status, p.ActivatedAt = domain.ProjectStatusActive, &at
	return nil
}

func (s *stubStore) SuspendProject(_ context.Context, projectID, principalID, reason string, at time.Time) error {
	p, ok := s.projects[projectID]
	if !ok || p.Status != domain.ProjectStatusActive {
		return domain.ErrInvalidProjectTransition
	}
	p.Status, p.SuspendedAt, p.SuspendedByPrincipalID, p.SuspensionReason = domain.ProjectStatusSuspended, &at, &principalID, &reason
	return nil
}

func (s *stubStore) CloseProject(_ context.Context, projectID, principalID, reason string, at time.Time) error {
	p, ok := s.projects[projectID]
	if !ok {
		return domain.ErrInvalidProjectTransition
	}
	if p.Status != domain.ProjectStatusActive && p.Status != domain.ProjectStatusSuspended {
		return domain.ErrInvalidProjectTransition
	}
	p.Status, p.ClosedAt, p.ClosedByPrincipalID, p.CloseReason = domain.ProjectStatusClosed, &at, &principalID, &reason
	return nil
}

func (s *stubStore) ReopenProjectControlled(_ context.Context, projectID, principalID, reason string, at time.Time) error {
	p, ok := s.projects[projectID]
	if !ok || p.Status != domain.ProjectStatusClosed {
		return domain.ErrInvalidProjectTransition
	}
	if p.ClosedByPrincipalID != nil && *p.ClosedByPrincipalID == principalID {
		return domain.ErrSelfReopenNotPermitted
	}
	p.Status, p.ReopenedAt, p.ReopenedByPrincipalID, p.ReopenReason = domain.ProjectStatusActive, &at, &principalID, &reason
	return nil
}

func (s *stubStore) LinkContract(_ context.Context, projectID, contractRef string) error {
	p, ok := s.projects[projectID]
	if !ok {
		return domain.ErrProjectNotFound
	}
	p.ContractRef = &contractRef
	return nil
}

func (s *stubStore) AddWorkPackage(_ context.Context, w *domain.WorkPackage) error {
	for _, existing := range s.workPackages[w.ProjectID] {
		if existing.WBSCode == w.WBSCode {
			return domain.ErrDuplicateWBSCode
		}
	}
	s.workPackages[w.ProjectID] = append(s.workPackages[w.ProjectID], *w)
	return nil
}

func (s *stubStore) AmendFinancialProfile(_ context.Context, projectID string, newVersion *domain.FinancialProfile) error {
	current, exists := s.financialProfiles[projectID]
	if exists {
		newVersion.ProfileID = current.ProfileID
		newVersion.Version = current.Version + 1
	} else {
		newVersion.ProfileID = "profile-" + projectID
		newVersion.Version = 1
	}
	cp := *newVersion
	s.financialProfiles[projectID] = &cp
	return nil
}

func (s *stubStore) GetCurrentFinancialProfile(_ context.Context, projectID string) (*domain.FinancialProfile, error) {
	f, ok := s.financialProfiles[projectID]
	if !ok {
		return nil, nil
	}
	cp := *f
	return &cp, nil
}

func (s *stubStore) GetFinancialProfileAsOf(_ context.Context, projectID string, _ time.Time) (*domain.FinancialProfile, error) {
	return s.GetCurrentFinancialProfile(context.Background(), projectID)
}

var _ handler.Store = (*stubStore)(nil)

type stubPublisher struct{ calls int }

func (p *stubPublisher) PublishProjectCreated(_ context.Context, _, _ string, _ domain.Project) {
	p.calls++
}
func (p *stubPublisher) PublishProjectApproved(_ context.Context, _, _ string, _ domain.Project) {
	p.calls++
}
func (p *stubPublisher) PublishProjectActivated(_ context.Context, _, _ string, _ domain.Project) {
	p.calls++
}
func (p *stubPublisher) PublishProjectFinancialProfileChanged(_ context.Context, _, _, _, _, _ string) {
	p.calls++
}
func (p *stubPublisher) PublishProjectClosed(_ context.Context, _, _ string, _ domain.Project) {
	p.calls++
}
func (p *stubPublisher) PublishProjectReopened(_ context.Context, _, _ string, _ domain.Project) {
	p.calls++
}
func (p *stubPublisher) PublishProjectCostCaptured(_ context.Context, _, _, _ string, _ domain.CostEntry) {
	p.calls++
}
func (p *stubPublisher) PublishProjectCostReclassified(_ context.Context, _, _, _ string, _ domain.CostEntry) {
	p.calls++
}
func (p *stubPublisher) PublishProjectCostReversed(_ context.Context, _, _, _ string, _ domain.CostEntry) {
	p.calls++
}
func (p *stubPublisher) PublishProjectCostPopulationCertified(_ context.Context, _, _, _, _ string, _ domain.CostCertification) {
	p.calls++
}
func (p *stubPublisher) PublishProjectRecognitionCalculated(_ context.Context, _, _, _ string, _ domain.RecognitionRun) {
	p.calls++
}
func (p *stubPublisher) PublishProjectRevenueApproved(_ context.Context, _, _, _ string, _ domain.RecognitionRun) {
	p.calls++
}
func (p *stubPublisher) PublishProjectRecognitionAccountingEventEmitted(_ context.Context, _, _, _, _, _, _ string) {
	p.calls++
}
func (p *stubPublisher) PublishProjectRecognitionSuperseded(_ context.Context, _, _, _ string, _ domain.RecognitionRun) {
	p.calls++
}
func (p *stubPublisher) PublishProjectProfitabilityRefreshed(_ context.Context, _, _, _ string, _ domain.ProfitabilityProjection) {
	p.calls++
}
func (p *stubPublisher) PublishProjectProfitabilitySnapshotCertified(_ context.Context, _, _, _ string, _ domain.ProfitabilitySnapshot) {
	p.calls++
}

var _ handler.Publisher = (*stubPublisher)(nil)

type stubAuthZ struct{ err error }

func (a *stubAuthZ) CheckAllowed(_ context.Context, _, _, _ string) error { return a.err }

type stubPeriodChecker struct{ err error }

func (c *stubPeriodChecker) CheckPeriodOpen(_ context.Context, _, _, _ string) error { return c.err }

var _ handler.PeriodChecker = (*stubPeriodChecker)(nil)

type stubLedger struct {
	postJournalID string
	postErr       error
	postCalls     int
	reverseErr    error
	reverseCalls  int
}

func (l *stubLedger) PostRecognitionAccountingEvent(_ context.Context, _, _, _, _, _, sourceEventID, _ string, _ []clients.LedgerLine) (string, error) {
	l.postCalls++
	if l.postErr != nil {
		return "", l.postErr
	}
	if l.postJournalID != "" {
		return l.postJournalID, nil
	}
	return "journal-" + sourceEventID, nil
}

func (l *stubLedger) ReverseRecognitionJournal(_ context.Context, _, _, _, _ string) error {
	l.reverseCalls++
	return l.reverseErr
}

var _ handler.RecognitionLedgerClient = (*stubLedger)(nil)

func newRouter(s *stubStore, pub *stubPublisher, authz *stubAuthZ) chi.Router {
	return newRouterWithPeriodChecker(s, pub, authz, &stubPeriodChecker{})
}

func newRouterWithPeriodChecker(s *stubStore, pub *stubPublisher, authz *stubAuthZ, pc *stubPeriodChecker) chi.Router {
	return newRouterFull(s, pub, authz, pc, &stubLedger{})
}

func newRouterWithLedger(s *stubStore, pub *stubPublisher, authz *stubAuthZ, ledger *stubLedger) chi.Router {
	return newRouterFull(s, pub, authz, &stubPeriodChecker{}, ledger)
}

func newRouterFull(s *stubStore, pub *stubPublisher, authz *stubAuthZ, pc *stubPeriodChecker, ledger *stubLedger) chi.Router {
	r := chi.NewRouter()
	r.Use(func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			req = req.WithContext(middleware.WithTenant(req.Context(), "tenant-abc"))
			next.ServeHTTP(w, req)
		})
	})
	h := handler.New(s, pub, authz, zap.NewNop()).WithPeriodChecker(pc).WithLedgerClient(ledger)
	handler.RegisterRoutes(r, h)
	return r
}

func doReq(r chi.Router, method, path string, body any, principalID string) *httptest.ResponseRecorder {
	var buf bytes.Buffer
	if body != nil {
		_ = json.NewEncoder(&buf).Encode(body)
	}
	req := httptest.NewRequest(method, path, &buf)
	req.Header.Set("Content-Type", "application/json")
	if principalID != "" {
		req.Header.Set("X-Principal-Id", principalID)
	}
	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, req)
	return rr
}

// ── helpers ──────────────────────────────────────────────────────────────────

func createDraftProject(t *testing.T, r chi.Router, legalEntityID, code string) domain.Project {
	t.Helper()
	req := domain.CreateProjectRequest{LegalEntityID: legalEntityID, ProjectCode: code, Name: "Test Project"}
	rr := doReq(r, http.MethodPost, "/v1/projects/", req, "creator-1")
	if rr.Code != http.StatusCreated {
		t.Fatalf("create failed: %d %s", rr.Code, rr.Body.String())
	}
	var p domain.Project
	_ = json.NewDecoder(rr.Body).Decode(&p)
	return p
}

func approveProject(t *testing.T, r chi.Router, projectID string) {
	t.Helper()
	rr := doReq(r, http.MethodPost, "/v1/projects/"+projectID+"/approve", nil, "approver-1")
	if rr.Code != http.StatusOK {
		t.Fatalf("approve failed: %d %s", rr.Code, rr.Body.String())
	}
}

func setFinancialProfile(t *testing.T, r chi.Router, projectID string) {
	t.Helper()
	future := time.Now().UTC().Add(time.Hour)
	req := domain.AmendFinancialProfileRequest{
		RecognitionMethod: domain.RecognitionMethodPercentageOfCompletion, BillingType: domain.BillingTypeFixedPrice,
		Currency: "USD", EffectiveFrom: &future,
	}
	rr := doReq(r, http.MethodPost, "/v1/projects/"+projectID+"/financial-profile", req, "manager-1")
	if rr.Code != http.StatusCreated {
		t.Fatalf("set financial profile failed: %d %s", rr.Code, rr.Body.String())
	}
}

func createActiveProject(t *testing.T, r chi.Router, legalEntityID, code string) string {
	t.Helper()
	p := createDraftProject(t, r, legalEntityID, code)
	approveProject(t, r, p.ProjectID)
	setFinancialProfile(t, r, p.ProjectID)
	rr := doReq(r, http.MethodPost, "/v1/projects/"+p.ProjectID+"/activate", nil, "manager-1")
	if rr.Code != http.StatusOK {
		t.Fatalf("activate failed: %d %s", rr.Code, rr.Body.String())
	}
	return p.ProjectID
}

// ── CreateProject ─────────────────────────────────────────────────────────────

func TestCreateProject_MissingFields_Returns400(t *testing.T) {
	r := newRouter(newStubStore(), &stubPublisher{}, &stubAuthZ{})
	rr := doReq(r, http.MethodPost, "/v1/projects/", map[string]string{}, "creator-1")
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d: %s", rr.Code, rr.Body.String())
	}
}

func TestCreateProject_DuplicateCode_Returns422(t *testing.T) {
	s := newStubStore()
	r := newRouter(s, &stubPublisher{}, &stubAuthZ{})
	createDraftProject(t, r, "le-1", "PRJ-DUP")

	req := domain.CreateProjectRequest{LegalEntityID: "le-1", ProjectCode: "PRJ-DUP", Name: "Another"}
	rr := doReq(r, http.MethodPost, "/v1/projects/", req, "creator-1")
	if rr.Code != http.StatusUnprocessableEntity {
		t.Fatalf("expected 422, got %d: %s", rr.Code, rr.Body.String())
	}
}

// ── ApproveProject (self-approval SoD) ───────────────────────────────────────

func TestApproveProject_SameCreator_Returns403(t *testing.T) {
	s := newStubStore()
	r := newRouter(s, &stubPublisher{}, &stubAuthZ{})
	p := createDraftProject(t, r, "le-1", "PRJ-1")

	rr := doReq(r, http.MethodPost, "/v1/projects/"+p.ProjectID+"/approve", nil, "creator-1")
	if rr.Code != http.StatusForbidden {
		t.Fatalf("expected 403 self-approval, got %d: %s", rr.Code, rr.Body.String())
	}
}

func TestApproveProject_DifferentApprover_Succeeds(t *testing.T) {
	s := newStubStore()
	r := newRouter(s, &stubPublisher{}, &stubAuthZ{})
	p := createDraftProject(t, r, "le-1", "PRJ-2")
	approveProject(t, r, p.ProjectID)
	if s.projects[p.ProjectID].Status != domain.ProjectStatusApproved {
		t.Fatalf("expected APPROVED, got %q", s.projects[p.ProjectID].Status)
	}
}

// ── ActivateProject ("Missing policy/currency/book blocks activation") ──────

func TestActivateProject_NoFinancialProfile_Returns422(t *testing.T) {
	s := newStubStore()
	r := newRouter(s, &stubPublisher{}, &stubAuthZ{})
	p := createDraftProject(t, r, "le-1", "PRJ-3")
	approveProject(t, r, p.ProjectID)

	rr := doReq(r, http.MethodPost, "/v1/projects/"+p.ProjectID+"/activate", nil, "manager-1")
	if rr.Code != http.StatusUnprocessableEntity {
		t.Fatalf("expected 422, got %d: %s", rr.Code, rr.Body.String())
	}
}

func TestActivateProject_WithFinancialProfile_Succeeds(t *testing.T) {
	s := newStubStore()
	r := newRouter(s, &stubPublisher{}, &stubAuthZ{})
	id := createActiveProject(t, r, "le-1", "PRJ-4")
	if s.projects[id].Status != domain.ProjectStatusActive {
		t.Fatalf("expected ACTIVE, got %q", s.projects[id].Status)
	}
}

// ── AmendFinancialProfile ("Recognition policy changed after run") ──────────

func TestAmendFinancialProfile_PastEffectiveDate_Returns422(t *testing.T) {
	s := newStubStore()
	r := newRouter(s, &stubPublisher{}, &stubAuthZ{})
	p := createDraftProject(t, r, "le-1", "PRJ-5")

	past := time.Now().UTC().Add(-time.Hour)
	req := domain.AmendFinancialProfileRequest{
		RecognitionMethod: domain.RecognitionMethodTimeAndMaterials, BillingType: domain.BillingTypeTimeAndMaterials,
		Currency: "USD", EffectiveFrom: &past,
	}
	rr := doReq(r, http.MethodPost, "/v1/projects/"+p.ProjectID+"/financial-profile", req, "manager-1")
	if rr.Code != http.StatusUnprocessableEntity {
		t.Fatalf("expected 422 retroactive profile change, got %d: %s", rr.Code, rr.Body.String())
	}
}

func TestAmendFinancialProfile_InvalidRecognitionMethod_Returns400(t *testing.T) {
	s := newStubStore()
	r := newRouter(s, &stubPublisher{}, &stubAuthZ{})
	p := createDraftProject(t, r, "le-1", "PRJ-6")

	future := time.Now().UTC().Add(time.Hour)
	req := domain.AmendFinancialProfileRequest{RecognitionMethod: "MADE_UP", BillingType: domain.BillingTypeFixedPrice, Currency: "USD", EffectiveFrom: &future}
	rr := doReq(r, http.MethodPost, "/v1/projects/"+p.ProjectID+"/financial-profile", req, "manager-1")
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d: %s", rr.Code, rr.Body.String())
	}
}

// ── CloseProject / ReopenProjectControlled ───────────────────────────────────

func TestCloseProject_MissingReason_Returns400(t *testing.T) {
	s := newStubStore()
	r := newRouter(s, &stubPublisher{}, &stubAuthZ{})
	id := createActiveProject(t, r, "le-1", "PRJ-7")

	rr := doReq(r, http.MethodPost, "/v1/projects/"+id+"/close", map[string]string{}, "manager-1")
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d: %s", rr.Code, rr.Body.String())
	}
}

func TestReopenProjectControlled_SameCloser_Returns403(t *testing.T) {
	s := newStubStore()
	r := newRouter(s, &stubPublisher{}, &stubAuthZ{})
	id := createActiveProject(t, r, "le-1", "PRJ-8")
	closeResp := doReq(r, http.MethodPost, "/v1/projects/"+id+"/close", domain.CloseProjectRequest{Reason: "done"}, "closer-1")
	if closeResp.Code != http.StatusOK {
		t.Fatalf("close failed: %d %s", closeResp.Code, closeResp.Body.String())
	}

	rr := doReq(r, http.MethodPost, "/v1/projects/"+id+"/reopen", domain.ReopenProjectRequest{Reason: "need more work"}, "closer-1")
	if rr.Code != http.StatusForbidden {
		t.Fatalf("expected 403 self-reopen, got %d: %s", rr.Code, rr.Body.String())
	}
}

func TestReopenProjectControlled_DifferentPrincipal_Succeeds(t *testing.T) {
	s := newStubStore()
	r := newRouter(s, &stubPublisher{}, &stubAuthZ{})
	id := createActiveProject(t, r, "le-1", "PRJ-9")
	closeResp := doReq(r, http.MethodPost, "/v1/projects/"+id+"/close", domain.CloseProjectRequest{Reason: "done"}, "closer-1")
	if closeResp.Code != http.StatusOK {
		t.Fatalf("close failed: %d %s", closeResp.Code, closeResp.Body.String())
	}

	rr := doReq(r, http.MethodPost, "/v1/projects/"+id+"/reopen", domain.ReopenProjectRequest{Reason: "need more work"}, "reviewer-2")
	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rr.Code, rr.Body.String())
	}
	if s.projects[id].Status != domain.ProjectStatusActive {
		t.Fatalf("expected ACTIVE after reopen, got %q", s.projects[id].Status)
	}
}

// ── AddWorkPackage (WBS deletion orphans historical cost — no delete path) ───

func TestAddWorkPackage_DuplicateCode_Returns422(t *testing.T) {
	s := newStubStore()
	r := newRouter(s, &stubPublisher{}, &stubAuthZ{})
	id := createActiveProject(t, r, "le-1", "PRJ-10")

	req := domain.AddWorkPackageRequest{WBSCode: "WBS-1"}
	first := doReq(r, http.MethodPost, "/v1/projects/"+id+"/work-packages", req, "manager-1")
	if first.Code != http.StatusCreated {
		t.Fatalf("first add failed: %d %s", first.Code, first.Body.String())
	}
	second := doReq(r, http.MethodPost, "/v1/projects/"+id+"/work-packages", req, "manager-1")
	if second.Code != http.StatusUnprocessableEntity {
		t.Fatalf("expected 422 duplicate wbs_code, got %d: %s", second.Code, second.Body.String())
	}
}

// ── LinkContract ──────────────────────────────────────────────────────────────

func TestLinkContract_MissingRef_Returns400(t *testing.T) {
	s := newStubStore()
	r := newRouter(s, &stubPublisher{}, &stubAuthZ{})
	id := createActiveProject(t, r, "le-1", "PRJ-11")

	rr := doReq(r, http.MethodPost, "/v1/projects/"+id+"/link-contract", map[string]string{}, "manager-1")
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d: %s", rr.Code, rr.Body.String())
	}
}

// ── Authorization ────────────────────────────────────────────────────────────

func TestCreateProject_AuthorizationDenied_Returns403(t *testing.T) {
	r := newRouter(newStubStore(), &stubPublisher{}, &stubAuthZ{err: domain.ErrAuthorizationDenied})
	req := domain.CreateProjectRequest{LegalEntityID: "le-1", ProjectCode: "PRJ-12", Name: "Test"}
	rr := doReq(r, http.MethodPost, "/v1/projects/", req, "creator-1")
	if rr.Code != http.StatusForbidden {
		t.Fatalf("expected 403, got %d: %s", rr.Code, rr.Body.String())
	}
}
