package handler_test

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"go.uber.org/zap"

	"zoiko.io/asset-management-svc/internal/clients"
	"zoiko.io/asset-management-svc/internal/domain"
	"zoiko.io/asset-management-svc/internal/handler"
	"zoiko.io/asset-management-svc/internal/middleware"
)

// ── stubs ─────────────────────────────────────────────────────────────────────

type stubStore struct {
	assets     map[string]*domain.FixedAsset
	components map[string][]domain.AssetComponent
	books      map[string][]domain.AssetBookAssignment

	assignBookErr error
	splitErr      error

	schedules         map[string]*domain.DepreciationSchedule // current version, keyed by schedule_id
	scheduleAssetBook map[string]string                       // "asset_id|book_id" -> schedule_id, for the UNIQUE(asset,book) simulation
	runs              map[string]*domain.DepreciationRun
	runByPeriod       map[string]string // "legal_entity_id|fiscal_period" -> run_id (live runs only)

	assetEvents map[string]*domain.AssetEvent
}

func newStubStore() *stubStore {
	return &stubStore{
		assets:            make(map[string]*domain.FixedAsset),
		components:        make(map[string][]domain.AssetComponent),
		books:             make(map[string][]domain.AssetBookAssignment),
		schedules:         make(map[string]*domain.DepreciationSchedule),
		scheduleAssetBook: make(map[string]string),
		runs:              make(map[string]*domain.DepreciationRun),
		runByPeriod:       make(map[string]string),
		assetEvents:       make(map[string]*domain.AssetEvent),
	}
}

// ── AST-03 (Asset Event) ─────────────────────────────────────────────────────

func (s *stubStore) CreateAssetEvent(_ context.Context, e *domain.AssetEvent) error {
	cp := *e
	s.assetEvents[e.EventID] = &cp
	return nil
}

func (s *stubStore) GetAssetEvent(_ context.Context, eventID string) (*domain.AssetEvent, error) {
	e, ok := s.assetEvents[eventID]
	if !ok {
		return nil, domain.ErrAssetEventNotFound
	}
	cp := *e
	return &cp, nil
}

func (s *stubStore) ListAssetEvents(_ context.Context, assetID string) ([]domain.AssetEvent, error) {
	var out []domain.AssetEvent
	for _, e := range s.assetEvents {
		if e.AssetID == assetID {
			out = append(out, *e)
		}
	}
	return out, nil
}

func (s *stubStore) ValidateAssetEvent(_ context.Context, eventID string, at time.Time) error {
	e, ok := s.assetEvents[eventID]
	if !ok || e.Status != domain.AssetEventStatusDraft {
		return domain.ErrInvalidAssetEventTransition
	}
	e.Status, e.ValidatedAt = domain.AssetEventStatusValidated, &at
	return nil
}

func (s *stubStore) ApproveAssetEvent(_ context.Context, eventID, principalID string, at time.Time) error {
	e, ok := s.assetEvents[eventID]
	if !ok || e.Status != domain.AssetEventStatusValidated {
		return domain.ErrInvalidAssetEventTransition
	}
	e.Status, e.ApprovedAt, e.ApprovedByPrincipalID = domain.AssetEventStatusApproved, &at, &principalID
	return nil
}

func (s *stubStore) ApplyAssetEvent(_ context.Context, eventID string, at time.Time, journalID *string) error {
	e, ok := s.assetEvents[eventID]
	if !ok || e.Status != domain.AssetEventStatusApproved {
		return domain.ErrInvalidAssetEventTransition
	}
	if e.EventType == domain.AssetEventTypeDisposal {
		asset, ok := s.assets[e.AssetID]
		if !ok || (asset.Status != domain.AssetStatusActive && asset.Status != domain.AssetStatusSuspended) {
			return domain.ErrAssetNotEligibleForDisposal
		}
		asset.Status = domain.AssetStatusDisposed
	}
	e.AppliedAt = &at
	if journalID != nil {
		e.Status, e.EmittedAt, e.JournalID = domain.AssetEventStatusAccountingEventEmitted, &at, journalID
	} else {
		e.Status = domain.AssetEventStatusApplied
	}
	return nil
}

func (s *stubStore) ReverseAssetEvent(_ context.Context, eventID, principalID, reason string, at time.Time) error {
	e, ok := s.assetEvents[eventID]
	if !ok || e.Status != domain.AssetEventStatusAccountingEventEmitted {
		return domain.ErrInvalidAssetEventTransition
	}
	if e.EventType == domain.AssetEventTypeDisposal {
		if asset, ok := s.assets[e.AssetID]; ok && asset.Status == domain.AssetStatusDisposed {
			asset.Status = domain.AssetStatusActive
		}
	}
	e.Status, e.ReversedAt, e.ReversedByPrincipalID, e.ReversalReason = domain.AssetEventStatusReversed, &at, &principalID, &reason
	return nil
}

func (s *stubStore) SupersedeAssetEvent(_ context.Context, eventID, principalID, reason string, at time.Time) error {
	e, ok := s.assetEvents[eventID]
	if !ok || e.Status != domain.AssetEventStatusAccountingEventEmitted {
		return domain.ErrInvalidAssetEventTransition
	}
	if e.EventType == domain.AssetEventTypeDisposal {
		if asset, ok := s.assets[e.AssetID]; ok && asset.Status == domain.AssetStatusDisposed {
			asset.Status = domain.AssetStatusActive
		}
	}
	e.Status, e.SupersededAt, e.SupersededByPrincipalID, e.SupersessionReason = domain.AssetEventStatusSuperseded, &at, &principalID, &reason
	return nil
}

func (s *stubStore) CreateDepreciationSchedule(_ context.Context, sch *domain.DepreciationSchedule) error {
	key := sch.AssetID + "|" + sch.BookID
	if _, exists := s.scheduleAssetBook[key]; exists {
		return domain.ErrDuplicateScheduleForAssetBook
	}
	cp := *sch
	s.schedules[sch.ScheduleID] = &cp
	s.scheduleAssetBook[key] = sch.ScheduleID
	return nil
}

func (s *stubStore) GetCurrentDepreciationSchedule(_ context.Context, scheduleID string) (*domain.DepreciationSchedule, error) {
	sch, ok := s.schedules[scheduleID]
	if !ok {
		return nil, domain.ErrScheduleNotFound
	}
	cp := *sch
	return &cp, nil
}

func (s *stubStore) RecalculateSchedule(_ context.Context, scheduleID string, newVersion *domain.DepreciationSchedule, at time.Time) error {
	current, ok := s.schedules[scheduleID]
	if !ok {
		return domain.ErrScheduleNotFound
	}
	newVersion.Version = current.Version + 1
	newVersion.ScheduleID = scheduleID
	newVersion.Status = domain.DepreciationScheduleStatusActive
	cp := *newVersion
	s.schedules[scheduleID] = &cp
	return nil
}

func (s *stubStore) CreateDepreciationRun(_ context.Context, r *domain.DepreciationRun) error {
	key := r.LegalEntityID + "|" + r.FiscalPeriod
	if _, exists := s.runByPeriod[key]; exists {
		return domain.ErrRunAlreadyExistsForPeriod
	}
	cp := *r
	s.runs[r.RunID] = &cp
	s.runByPeriod[key] = r.RunID
	return nil
}

func (s *stubStore) GetDepreciationRun(_ context.Context, runID string) (*domain.DepreciationRun, error) {
	r, ok := s.runs[runID]
	if !ok {
		return nil, domain.ErrRunNotFound
	}
	cp := *r
	return &cp, nil
}

func (s *stubStore) FreezeDepreciationPopulation(_ context.Context, runID, legalEntityID string, at time.Time) (int, error) {
	r, ok := s.runs[runID]
	if !ok || r.Status != domain.DepreciationRunStatusDraft {
		return 0, domain.ErrInvalidRunTransition
	}
	frozen := 0
	for _, sch := range s.schedules {
		if sch.LegalEntityID == legalEntityID && sch.Status == domain.DepreciationScheduleStatusActive {
			if asset, ok := s.assets[sch.AssetID]; ok && asset.Status == domain.AssetStatusActive {
				frozen++
			}
		}
	}
	r.Status, r.FrozenAt = domain.DepreciationRunStatusPopulationFrozen, &at
	return frozen, nil
}

func (s *stubStore) ValidateDepreciationRun(_ context.Context, runID string, at time.Time) (int, error) {
	r, ok := s.runs[runID]
	if !ok || r.Status != domain.DepreciationRunStatusPopulationFrozen {
		return 0, domain.ErrInvalidRunTransition
	}
	lineCount := 0
	for _, sch := range s.schedules {
		if sch.LegalEntityID != r.LegalEntityID || sch.Status != domain.DepreciationScheduleStatusActive {
			continue
		}
		asset, ok := s.assets[sch.AssetID]
		if !ok || asset.Status != domain.AssetStatusActive {
			continue
		}
		monthly := (sch.CostBasis - sch.ResidualValue) / float64(sch.UsefulLifeMonths)
		r.Lines = append(r.Lines, domain.DepreciationLine{
			LineID: "line-" + sch.ScheduleVersionID, RunID: runID, ScheduleVersionID: sch.ScheduleVersionID,
			AssetID: sch.AssetID, BookID: sch.BookID, PeriodAmount: monthly, AccumulatedDepreciationAfter: monthly, CreatedAt: at,
		})
		lineCount++
	}
	r.Status, r.ValidatedAt = domain.DepreciationRunStatusValidated, &at
	return lineCount, nil
}

func (s *stubStore) ApproveDepreciationRun(_ context.Context, runID, principalID string, at time.Time) error {
	r, ok := s.runs[runID]
	if !ok || r.Status != domain.DepreciationRunStatusValidated {
		return domain.ErrInvalidRunTransition
	}
	r.Status, r.ApprovedAt, r.ApprovedByPrincipalID = domain.DepreciationRunStatusApproved, &at, &principalID
	return nil
}

func (s *stubStore) MarkDepreciationRunEmitted(_ context.Context, runID, journalID string, at time.Time) error {
	r, ok := s.runs[runID]
	if !ok || r.Status != domain.DepreciationRunStatusApproved {
		return domain.ErrInvalidRunTransition
	}
	r.Status, r.EmittedAt, r.JournalID = domain.DepreciationRunStatusAccountingEventEmitted, &at, &journalID
	return nil
}

func (s *stubStore) SupersedeDepreciationRun(_ context.Context, runID, principalID string, at time.Time) error {
	r, ok := s.runs[runID]
	if !ok || r.Status != domain.DepreciationRunStatusAccountingEventEmitted {
		return domain.ErrInvalidRunTransition
	}
	r.Status, r.SupersededAt, r.SupersededByPrincipalID = domain.DepreciationRunStatusSuperseded, &at, &principalID
	delete(s.runByPeriod, r.LegalEntityID+"|"+r.FiscalPeriod)
	return nil
}

func (s *stubStore) CreateAsset(_ context.Context, a *domain.FixedAsset) error {
	cp := *a
	s.assets[a.AssetID] = &cp
	return nil
}

func (s *stubStore) GetAsset(_ context.Context, assetID string) (*domain.FixedAsset, error) {
	a, ok := s.assets[assetID]
	if !ok {
		return nil, domain.ErrAssetNotFound
	}
	cp := *a
	cp.Components = s.components[assetID]
	cp.BookAssignments = s.books[assetID]
	return &cp, nil
}

func (s *stubStore) ListAssets(_ context.Context, legalEntityID string) ([]domain.FixedAsset, error) {
	var out []domain.FixedAsset
	for _, a := range s.assets {
		if a.LegalEntityID == legalEntityID {
			out = append(out, *a)
		}
	}
	return out, nil
}

// GetNetBookValueTotal is a simplified stub: sums CostBasis for ACTIVE
// schedules in the given book, for ACTIVE assets — accumulated
// depreciation isn't modeled in this in-memory stub (that math is
// verified for real in internal/store's own Postgres tests). Enough to
// exercise the handler's own routing/authz/validation plumbing.
func (s *stubStore) GetNetBookValueTotal(_ context.Context, legalEntityID, bookID string) (float64, error) {
	var total float64
	for _, sch := range s.schedules {
		if sch.LegalEntityID != legalEntityID || sch.BookID != bookID || sch.Status != domain.DepreciationScheduleStatusActive {
			continue
		}
		if a, ok := s.assets[sch.AssetID]; ok && a.Status == domain.AssetStatusActive {
			total += sch.CostBasis
		}
	}
	return total, nil
}

func (s *stubStore) AddComponent(_ context.Context, c *domain.AssetComponent) error {
	s.components[c.AssetID] = append(s.components[c.AssetID], *c)
	return nil
}

func (s *stubStore) AssignBookProfile(_ context.Context, b *domain.AssetBookAssignment) error {
	if s.assignBookErr != nil {
		return s.assignBookErr
	}
	for _, existing := range s.books[b.AssetID] {
		if existing.BookID == b.BookID {
			return domain.ErrRetroactiveBookProfileChange
		}
	}
	s.books[b.AssetID] = append(s.books[b.AssetID], *b)
	return nil
}

func (s *stubStore) RegisterAsset(_ context.Context, assetID, principalID string, at time.Time) error {
	a, ok := s.assets[assetID]
	if !ok || a.Status != domain.AssetStatusCandidate {
		return domain.ErrInvalidAssetTransition
	}
	a.Status, a.RegisteredAt, a.RegisteredByPrincipalID = domain.AssetStatusRegistered, &at, &principalID
	return nil
}

func (s *stubStore) CapitalizeAsset(_ context.Context, assetID, principalID string, at time.Time) error {
	a, ok := s.assets[assetID]
	if !ok || a.Status != domain.AssetStatusRegistered {
		return domain.ErrInvalidAssetTransition
	}
	a.Status, a.CapitalizedAt, a.CapitalizedByPrincipalID = domain.AssetStatusActive, &at, &principalID
	return nil
}

func (s *stubStore) SuspendAsset(_ context.Context, assetID, principalID, reason string, at time.Time) error {
	a, ok := s.assets[assetID]
	if !ok || a.Status != domain.AssetStatusActive {
		return domain.ErrInvalidAssetTransition
	}
	a.Status, a.SuspendedAt, a.SuspendedByPrincipalID, a.SuspensionReason = domain.AssetStatusSuspended, &at, &principalID, &reason
	return nil
}

func (s *stubStore) ReactivateAsset(_ context.Context, assetID string) error {
	a, ok := s.assets[assetID]
	if !ok || a.Status != domain.AssetStatusSuspended {
		return domain.ErrInvalidAssetTransition
	}
	a.Status = domain.AssetStatusActive
	return nil
}

func (s *stubStore) UpdateMetadata(_ context.Context, assetID string, description, custodianID, locationID, tagSerial *string) error {
	a, ok := s.assets[assetID]
	if !ok {
		return domain.ErrAssetNotFound
	}
	if description != nil {
		a.Description = *description
	}
	if custodianID != nil {
		a.CustodianID = *custodianID
	}
	if locationID != nil {
		a.LocationID = *locationID
	}
	if tagSerial != nil {
		a.TagSerial = *tagSerial
	}
	return nil
}

func (s *stubStore) MergeAssets(_ context.Context, sourceAssetID, targetAssetID, principalID string, at time.Time) error {
	source, ok := s.assets[sourceAssetID]
	if !ok {
		return domain.ErrAssetNotFound
	}
	target, ok := s.assets[targetAssetID]
	if !ok {
		return domain.ErrAssetNotFound
	}
	if source.LegalEntityID != target.LegalEntityID {
		return domain.ErrMergeAcrossLegalEntities
	}
	if source.Status != domain.AssetStatusActive {
		return domain.ErrInvalidAssetTransition
	}
	source.Status, source.MergedIntoAssetID = domain.AssetStatusMerged, &targetAssetID
	return nil
}

func (s *stubStore) SplitAsset(_ context.Context, newAsset *domain.FixedAsset, sourceAssetID string, componentIDs []string) error {
	if s.splitErr != nil {
		return s.splitErr
	}
	existing := s.components[sourceAssetID]
	for _, id := range componentIDs {
		found := false
		for i := range existing {
			if existing[i].ComponentID == id {
				existing[i].MovedToAssetID = &newAsset.AssetID
				found = true
			}
		}
		if !found {
			return domain.ErrComponentNotOnAsset
		}
	}
	s.components[sourceAssetID] = existing
	cp := *newAsset
	s.assets[newAsset.AssetID] = &cp
	return nil
}

var _ handler.Store = (*stubStore)(nil)

type stubPublisher struct{ calls int }

func (p *stubPublisher) PublishAssetRegistered(_ context.Context, _, _ string, _ domain.FixedAsset) {
	p.calls++
}
func (p *stubPublisher) PublishAssetComponentAdded(_ context.Context, _, _ string, _ domain.FixedAsset, _ domain.AssetComponent) {
	p.calls++
}
func (p *stubPublisher) PublishAssetBookAssigned(_ context.Context, _, _ string, _ domain.FixedAsset, _ domain.AssetBookAssignment) {
	p.calls++
}
func (p *stubPublisher) PublishAssetCapitalizationRequested(_ context.Context, _, _ string, _ domain.FixedAsset) {
	p.calls++
}
func (p *stubPublisher) PublishAssetMetadataChanged(_ context.Context, _, _ string, _ domain.FixedAsset) {
	p.calls++
}
func (p *stubPublisher) PublishAssetSuspended(_ context.Context, _, _ string, _ domain.FixedAsset) {
	p.calls++
}

var _ handler.Publisher = (*stubPublisher)(nil)

type stubAuthZ struct{ err error }

func (a *stubAuthZ) CheckAllowed(_ context.Context, _, _, _ string) error { return a.err }

func newRouter(s *stubStore, pub *stubPublisher, authz *stubAuthZ) chi.Router {
	return newRouterWithLedger(s, pub, authz, &stubLedger{})
}

type stubLedger struct {
	postJournalID           string
	postErr                 error
	postCalls               int
	reverseErr              error
	reverseCalls            int
	lastPostedSourceEventID string

	checkPeriodErr error
}

func (l *stubLedger) PostDepreciationAccountingEvent(_ context.Context, _, _, _, _, _, sourceEventID, _ string, _ []clients.LedgerLine) (string, error) {
	l.postCalls++
	l.lastPostedSourceEventID = sourceEventID
	if l.postErr != nil {
		return "", l.postErr
	}
	if l.postJournalID != "" {
		return l.postJournalID, nil
	}
	return "journal-generated", nil
}

func (l *stubLedger) ReverseDepreciationJournal(_ context.Context, _, _, _, _ string) error {
	l.reverseCalls++
	return l.reverseErr
}

func (l *stubLedger) PostAssetEventAccountingEvent(_ context.Context, _, _, _, _, _, sourceEventID, _ string, _ []clients.LedgerLine) (string, error) {
	l.postCalls++
	l.lastPostedSourceEventID = sourceEventID
	if l.postErr != nil {
		return "", l.postErr
	}
	if l.postJournalID != "" {
		return l.postJournalID, nil
	}
	return "journal-generated", nil
}

func (l *stubLedger) ReverseAssetEventJournal(_ context.Context, _, _, _, _ string) error {
	l.reverseCalls++
	return l.reverseErr
}

func (l *stubLedger) CheckPeriodOpen(_ context.Context, _, _, _ string) error {
	return l.checkPeriodErr
}

func newRouterWithLedger(s *stubStore, pub *stubPublisher, authz *stubAuthZ, ledger *stubLedger) chi.Router {
	r := chi.NewRouter()
	r.Use(func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			req = req.WithContext(middleware.WithTenant(req.Context(), "tenant-abc"))
			next.ServeHTTP(w, req)
		})
	})
	h := handler.New(s, pub, authz, zap.NewNop()).WithLedgerClient(ledger)
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

func createRegisteredAsset(t *testing.T, s *stubStore, r chi.Router, legalEntityID string) string {
	t.Helper()
	req := domain.CreateAssetCandidateRequest{
		LegalEntityID: legalEntityID, AssetCategory: "IT Equipment", Description: "Server rack",
	}
	rr := doReq(r, http.MethodPost, "/v1/assets/", req, "preparer-1")
	if rr.Code != http.StatusCreated {
		t.Fatalf("create failed: %d %s", rr.Code, rr.Body.String())
	}
	var a domain.FixedAsset
	_ = json.NewDecoder(rr.Body).Decode(&a)

	approve := doReq(r, http.MethodPost, "/v1/assets/"+a.AssetID+"/approve", nil, "approver-1")
	if approve.Code != http.StatusOK {
		t.Fatalf("approve failed: %d %s", approve.Code, approve.Body.String())
	}
	return a.AssetID
}

func createActiveAsset(t *testing.T, s *stubStore, r chi.Router, legalEntityID string) string {
	t.Helper()
	id := createRegisteredAsset(t, s, r, legalEntityID)
	s.assets[id].AcquisitionSourceRef = "PO-1001"
	rr := doReq(r, http.MethodPost, "/v1/assets/"+id+"/request-capitalization", nil, "approver-1")
	if rr.Code != http.StatusOK {
		t.Fatalf("capitalization failed: %d %s", rr.Code, rr.Body.String())
	}
	return id
}

// ── CreateAssetCandidate ─────────────────────────────────────────────────────

func TestCreateAssetCandidate_MissingFields_Returns400(t *testing.T) {
	r := newRouter(newStubStore(), &stubPublisher{}, &stubAuthZ{})
	rr := doReq(r, http.MethodPost, "/v1/assets/", map[string]string{}, "preparer-1")
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d: %s", rr.Code, rr.Body.String())
	}
}

func TestCreateAssetCandidate_HappyPath(t *testing.T) {
	r := newRouter(newStubStore(), &stubPublisher{}, &stubAuthZ{})
	req := domain.CreateAssetCandidateRequest{LegalEntityID: "le-1", AssetCategory: "Vehicles", Description: "Delivery van"}
	rr := doReq(r, http.MethodPost, "/v1/assets/", req, "preparer-1")
	if rr.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d: %s", rr.Code, rr.Body.String())
	}
	var a domain.FixedAsset
	_ = json.NewDecoder(rr.Body).Decode(&a)
	if a.Status != domain.AssetStatusCandidate {
		t.Fatalf("expected CANDIDATE, got %q", a.Status)
	}
}

// ── ApproveAssetRegistration (self-approval SoD) ─────────────────────────────

func TestApproveAssetRegistration_SameCreator_Returns403(t *testing.T) {
	s := newStubStore()
	r := newRouter(s, &stubPublisher{}, &stubAuthZ{})
	req := domain.CreateAssetCandidateRequest{LegalEntityID: "le-1", AssetCategory: "IT", Description: "Laptop"}
	rr := doReq(r, http.MethodPost, "/v1/assets/", req, "preparer-1")
	var a domain.FixedAsset
	_ = json.NewDecoder(rr.Body).Decode(&a)

	approve := doReq(r, http.MethodPost, "/v1/assets/"+a.AssetID+"/approve", nil, "preparer-1")
	if approve.Code != http.StatusForbidden {
		t.Fatalf("expected 403 self-approval, got %d: %s", approve.Code, approve.Body.String())
	}
}

func TestApproveAssetRegistration_DifferentApprover_Allowed(t *testing.T) {
	s := newStubStore()
	r := newRouter(s, &stubPublisher{}, &stubAuthZ{})
	id := createRegisteredAsset(t, s, r, "le-1")
	if s.assets[id].Status != domain.AssetStatusRegistered {
		t.Fatalf("expected REGISTERED, got %q", s.assets[id].Status)
	}
}

// ── RequestCapitalization ("Asset capitalized without source evidence") ─────

func TestRequestCapitalization_NoAcquisitionEvidence_Returns422(t *testing.T) {
	s := newStubStore()
	r := newRouter(s, &stubPublisher{}, &stubAuthZ{})
	id := createRegisteredAsset(t, s, r, "le-1") // no acquisition_source_ref set

	rr := doReq(r, http.MethodPost, "/v1/assets/"+id+"/request-capitalization", nil, "approver-1")
	if rr.Code != http.StatusUnprocessableEntity {
		t.Fatalf("expected 422, got %d: %s", rr.Code, rr.Body.String())
	}
}

func TestRequestCapitalization_WithEvidence_Succeeds(t *testing.T) {
	s := newStubStore()
	r := newRouter(s, &stubPublisher{}, &stubAuthZ{})
	id := createActiveAsset(t, s, r, "le-1")
	if s.assets[id].Status != domain.AssetStatusActive {
		t.Fatalf("expected ACTIVE, got %q", s.assets[id].Status)
	}
}

// ── AssignAssetBookProfile ("Retroactive book profile change") ──────────────

func TestAssignAssetBookProfile_Duplicate_Returns422(t *testing.T) {
	s := newStubStore()
	r := newRouter(s, &stubPublisher{}, &stubAuthZ{})
	id := createActiveAsset(t, s, r, "le-1")

	req := domain.AssignAssetBookProfileRequest{BookID: "book-1"}
	first := doReq(r, http.MethodPost, "/v1/assets/"+id+"/book-profiles", req, "preparer-1")
	if first.Code != http.StatusCreated {
		t.Fatalf("first assignment failed: %d %s", first.Code, first.Body.String())
	}
	second := doReq(r, http.MethodPost, "/v1/assets/"+id+"/book-profiles", req, "preparer-1")
	if second.Code != http.StatusUnprocessableEntity {
		t.Fatalf("expected 422 on retroactive book profile change, got %d: %s", second.Code, second.Body.String())
	}
}

// ── SuspendAsset ──────────────────────────────────────────────────────────────

func TestSuspendAsset_MissingReason_Returns400(t *testing.T) {
	s := newStubStore()
	r := newRouter(s, &stubPublisher{}, &stubAuthZ{})
	id := createActiveAsset(t, s, r, "le-1")

	rr := doReq(r, http.MethodPost, "/v1/assets/"+id+"/suspend", map[string]string{}, "preparer-1")
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d: %s", rr.Code, rr.Body.String())
	}
}

func TestSuspendAsset_FromCandidate_Refused(t *testing.T) {
	s := newStubStore()
	r := newRouter(s, &stubPublisher{}, &stubAuthZ{})
	req := domain.CreateAssetCandidateRequest{LegalEntityID: "le-1", AssetCategory: "IT", Description: "Laptop"}
	rr := doReq(r, http.MethodPost, "/v1/assets/", req, "preparer-1")
	var a domain.FixedAsset
	_ = json.NewDecoder(rr.Body).Decode(&a)

	suspend := doReq(r, http.MethodPost, "/v1/assets/"+a.AssetID+"/suspend", domain.SuspendAssetRequest{Reason: "x"}, "preparer-1")
	if suspend.Code != http.StatusUnprocessableEntity {
		t.Fatalf("expected 422 suspending a CANDIDATE, got %d: %s", suspend.Code, suspend.Body.String())
	}
}

// ── MergeAssetControlled ("Physical asset merged across legal entities") ────

func TestMergeAssetControlled_AcrossLegalEntities_Returns422(t *testing.T) {
	s := newStubStore()
	r := newRouter(s, &stubPublisher{}, &stubAuthZ{})
	source := createActiveAsset(t, s, r, "le-1")
	target := createActiveAsset(t, s, r, "le-2")

	req := domain.MergeAssetRequest{TargetAssetID: target, Reason: "consolidating duplicate records"}
	rr := doReq(r, http.MethodPost, "/v1/assets/"+source+"/merge", req, "preparer-1")
	if rr.Code != http.StatusUnprocessableEntity {
		t.Fatalf("expected 422, got %d: %s", rr.Code, rr.Body.String())
	}
	if s.assets[source].Status != domain.AssetStatusActive {
		t.Fatalf("expected source asset to remain ACTIVE (unmerged), got %q", s.assets[source].Status)
	}
}

func TestMergeAssetControlled_SameLegalEntity_Succeeds(t *testing.T) {
	s := newStubStore()
	r := newRouter(s, &stubPublisher{}, &stubAuthZ{})
	source := createActiveAsset(t, s, r, "le-1")
	target := createActiveAsset(t, s, r, "le-1")

	req := domain.MergeAssetRequest{TargetAssetID: target, Reason: "consolidating duplicate records"}
	rr := doReq(r, http.MethodPost, "/v1/assets/"+source+"/merge", req, "preparer-1")
	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rr.Code, rr.Body.String())
	}
	if s.assets[source].Status != domain.AssetStatusMerged {
		t.Fatalf("expected MERGED, got %q", s.assets[source].Status)
	}
}

// ── SplitAssetControlled ─────────────────────────────────────────────────────

func TestSplitAssetControlled_NoComponentsNamed_Returns400(t *testing.T) {
	s := newStubStore()
	r := newRouter(s, &stubPublisher{}, &stubAuthZ{})
	id := createActiveAsset(t, s, r, "le-1")

	req := domain.SplitAssetRequest{Description: "new asset", Reason: "splitting off a component"}
	rr := doReq(r, http.MethodPost, "/v1/assets/"+id+"/split", req, "preparer-1")
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d: %s", rr.Code, rr.Body.String())
	}
}

func TestSplitAssetControlled_ComponentNotOnAsset_Returns422(t *testing.T) {
	s := newStubStore()
	r := newRouter(s, &stubPublisher{}, &stubAuthZ{})
	id := createActiveAsset(t, s, r, "le-1")

	req := domain.SplitAssetRequest{ComponentIDs: []string{"nonexistent-component"}, Description: "new asset", Reason: "splitting off a component"}
	rr := doReq(r, http.MethodPost, "/v1/assets/"+id+"/split", req, "preparer-1")
	if rr.Code != http.StatusUnprocessableEntity {
		t.Fatalf("expected 422, got %d: %s", rr.Code, rr.Body.String())
	}
}

// ── AmendNonFinancialMetadata (SoD: cannot alter accounting basis) ──────────

func TestAmendNonFinancialMetadata_NeverChangesStatus(t *testing.T) {
	s := newStubStore()
	r := newRouter(s, &stubPublisher{}, &stubAuthZ{})
	id := createActiveAsset(t, s, r, "le-1")

	req := domain.AmendNonFinancialMetadataRequest{CustodianID: strPtr("custodian-2")}
	rr := doReq(r, http.MethodPost, "/v1/assets/"+id+"/amend-metadata", req, "preparer-1")
	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rr.Code, rr.Body.String())
	}
	if s.assets[id].Status != domain.AssetStatusActive {
		t.Fatalf("expected status to remain ACTIVE after a metadata amend, got %q", s.assets[id].Status)
	}
	if s.assets[id].CustodianID != "custodian-2" {
		t.Fatalf("expected custodian updated, got %q", s.assets[id].CustodianID)
	}
}

func strPtr(s string) *string { return &s }

// ── Authorization ────────────────────────────────────────────────────────────

func TestCreateAssetCandidate_AuthorizationDenied_Returns403(t *testing.T) {
	r := newRouter(newStubStore(), &stubPublisher{}, &stubAuthZ{err: domain.ErrAuthorizationDenied})
	req := domain.CreateAssetCandidateRequest{LegalEntityID: "le-1", AssetCategory: "IT", Description: "Laptop"}
	rr := doReq(r, http.MethodPost, "/v1/assets/", req, "preparer-1")
	if rr.Code != http.StatusForbidden {
		t.Fatalf("expected 403, got %d: %s", rr.Code, rr.Body.String())
	}
}
