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

	"zoiko.io/consolidation-svc/internal/clients"
	"zoiko.io/consolidation-svc/internal/domain"
	"zoiko.io/consolidation-svc/internal/handler"
	"zoiko.io/consolidation-svc/internal/middleware"
)

// ── stubs ─────────────────────────────────────────────────────────────────────

type stubStore struct {
	runs          map[string]*domain.ConsolidationRun
	snapshots     map[string][]domain.BalanceSnapshot
	contributions map[string][]domain.BalanceContribution
	contribErr    error
}

func newStubStore() *stubStore {
	return &stubStore{
		runs:          make(map[string]*domain.ConsolidationRun),
		snapshots:     make(map[string][]domain.BalanceSnapshot),
		contributions: make(map[string][]domain.BalanceContribution),
	}
}

func (s *stubStore) CreateRun(_ context.Context, run *domain.ConsolidationRun) error {
	s.runs[run.ConsolidationRunID] = run
	return nil
}

func (s *stubStore) GetRun(_ context.Context, id string) (*domain.ConsolidationRun, error) {
	run, ok := s.runs[id]
	if !ok {
		return nil, domain.ErrRunNotFound
	}
	return run, nil
}

func (s *stubStore) ListRuns(_ context.Context, groupLegalEntityID string) ([]domain.ConsolidationRun, error) {
	var out []domain.ConsolidationRun
	for _, run := range s.runs {
		if groupLegalEntityID != "" && run.GroupLegalEntityID != groupLegalEntityID {
			continue
		}
		out = append(out, *run)
	}
	return out, nil
}

func (s *stubStore) CompleteRun(_ context.Context, id, status string, exceptionCount int, completedAt time.Time) error {
	run, ok := s.runs[id]
	if !ok {
		return domain.ErrRunNotFound
	}
	run.Status = status
	run.ExceptionCount = exceptionCount
	t := completedAt
	run.CompletedAt = &t
	return nil
}

func (s *stubStore) CreateBalanceSnapshots(_ context.Context, snapshots []domain.BalanceSnapshot) error {
	if len(snapshots) == 0 {
		return nil
	}
	runID := snapshots[0].ConsolidationRunID
	s.snapshots[runID] = append(s.snapshots[runID], snapshots...)
	return nil
}

func (s *stubStore) ListSnapshotsByRun(_ context.Context, runID string) ([]domain.BalanceSnapshot, error) {
	snaps, ok := s.snapshots[runID]
	if !ok {
		return []domain.BalanceSnapshot{}, nil
	}
	return snaps, nil
}

func (s *stubStore) CreateBalanceContributions(_ context.Context, contributions []domain.BalanceContribution) error {
	if s.contribErr != nil {
		return s.contribErr
	}
	if len(contributions) == 0 {
		return nil
	}
	runID := contributions[0].ConsolidationRunID
	s.contributions[runID] = append(s.contributions[runID], contributions...)
	return nil
}

func (s *stubStore) ListContributionsByRun(_ context.Context, runID string) ([]domain.BalanceContribution, error) {
	c, ok := s.contributions[runID]
	if !ok {
		return []domain.BalanceContribution{}, nil
	}
	return c, nil
}

type stubPublisher struct {
	started, completed, exceptions int
}

func (p *stubPublisher) PublishRunStarted(_ context.Context, _, _ string, _ domain.ConsolidationRun) {
	p.started++
}
func (p *stubPublisher) PublishCompleted(_ context.Context, _, _ string, _ domain.ConsolidationRun, _ int) {
	p.completed++
}
func (p *stubPublisher) PublishExceptionDetected(_ context.Context, _, _ string, _ domain.ConsolidationRun, _ []string) {
	p.exceptions++
}

type stubAuthZ struct{ err error }

func (a *stubAuthZ) CheckAllowed(_ context.Context, _, _, _ string) error { return a.err }

type stubClients struct {
	glBalances map[string]map[string]float64
	glErr      error

	intercompanyEntries []clients.IntercompanyEntry
	intercompanyErr     error
	principalIDReceived string // captures what FetchMatchedIntercompanyEntries was called with

	// journalLines maps journal_id -> its real posted lines, for the
	// elimination path (FetchJournalLines). journalLinesErr, keyed the
	// same way, lets a test simulate one specific journal's lookup failing.
	journalLines    map[string][]clients.JournalLine
	journalLinesErr map[string]error
}

func (c *stubClients) FetchTrialBalance(_ context.Context, _, legalEntityID, _ string) (map[string]float64, error) {
	if c.glErr != nil {
		return nil, c.glErr
	}
	if c.glBalances != nil {
		if bal, ok := c.glBalances[legalEntityID]; ok {
			return bal, nil
		}
	}
	return map[string]float64{"1000-Cash": 10000.0}, nil
}

func (c *stubClients) FetchMatchedIntercompanyEntries(_ context.Context, _, principalID string) ([]clients.IntercompanyEntry, error) {
	c.principalIDReceived = principalID
	if c.intercompanyErr != nil {
		return nil, c.intercompanyErr
	}
	return c.intercompanyEntries, nil
}

func (c *stubClients) FetchJournalLines(_ context.Context, _, journalID string) ([]clients.JournalLine, error) {
	if err, ok := c.journalLinesErr[journalID]; ok {
		return nil, err
	}
	return c.journalLines[journalID], nil
}

// ── router factory ─────────────────────────────────────────────────────────────

func newRouter(s *stubStore, pub *stubPublisher, authz *stubAuthZ, cl *stubClients) chi.Router {
	r := chi.NewRouter()
	r.Use(func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			req = req.WithContext(middleware.WithTenant(req.Context(), "tenant-abc"))
			next.ServeHTTP(w, req)
		})
	})
	h := handler.New(s, pub, authz, cl, zap.NewNop())
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

// ── StartRun Tests ────────────────────────────────────────────────────────────

func TestStartRun_MissingPrincipal(t *testing.T) {
	r := newRouter(newStubStore(), &stubPublisher{}, &stubAuthZ{}, &stubClients{})
	rr := doReq(r, http.MethodPost, "/v1/consolidation/runs/", map[string]any{
		"group_legal_entity_id":  "le-group",
		"child_legal_entity_ids": []string{"le-us", "le-uk"},
		"fiscal_period":          "2024-Q1",
		"target_currency":        "USD",
	}, "")
	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 got %d", rr.Code)
	}
}

func TestStartRun_AuthzDenied(t *testing.T) {
	r := newRouter(newStubStore(), &stubPublisher{}, &stubAuthZ{err: domain.ErrAuthorizationDenied}, &stubClients{})
	rr := doReq(r, http.MethodPost, "/v1/consolidation/runs/", map[string]any{
		"group_legal_entity_id":  "le-group",
		"child_legal_entity_ids": []string{"le-us", "le-uk"},
		"fiscal_period":          "2024-Q1",
		"target_currency":        "USD",
	}, "principal-1")
	if rr.Code != http.StatusForbidden {
		t.Fatalf("expected 403 got %d", rr.Code)
	}
}

func TestStartRun_MissingFields(t *testing.T) {
	r := newRouter(newStubStore(), &stubPublisher{}, &stubAuthZ{}, &stubClients{})
	rr := doReq(r, http.MethodPost, "/v1/consolidation/runs/", map[string]any{
		"group_legal_entity_id": "le-group",
	}, "principal-1")
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 got %d", rr.Code)
	}
}

func TestStartRun_HappyPath(t *testing.T) {
	s := newStubStore()
	pub := &stubPublisher{}
	cl := &stubClients{
		glBalances: map[string]map[string]float64{
			"le-us": {"1000-Cash": 5000.0, "2000-AP": -2000.0},
			"le-uk": {"1000-Cash": 3000.0, "2000-AP": -1000.0},
		},
	}
	r := newRouter(s, pub, &stubAuthZ{}, cl)
	rr := doReq(r, http.MethodPost, "/v1/consolidation/runs/", map[string]any{
		"group_legal_entity_id":  "le-group",
		"child_legal_entity_ids": []string{"le-us", "le-uk"},
		"fiscal_period":          "2024-Q1",
		"target_currency":        "USD",
	}, "principal-1")

	if rr.Code != http.StatusCreated {
		t.Fatalf("expected 201 got %d: %s", rr.Code, rr.Body.String())
	}

	var resp domain.ConsolidationRunResponse
	if err := json.NewDecoder(rr.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}

	if resp.Status != "COMPLETED" {
		t.Errorf("expected COMPLETED status, got %q", resp.Status)
	}
	if len(resp.Snapshots) != 2 {
		t.Fatalf("expected 2 balance snapshots, got %d", len(resp.Snapshots))
	}
	if pub.started != 1 {
		t.Errorf("expected 1 started event got %d", pub.started)
	}
	if pub.completed != 1 {
		t.Errorf("expected 1 completed event got %d", pub.completed)
	}
}

// TestStartRun_ElimatesMatchedIntercompanyEntry proves ACC-12 elimination
// actually happens, not just that the fetch is called: le-us posted a
// 500.0 intercompany receivable to a child, le-uk posted the offsetting
// 500.0 payable, and once matched, both legs' real net contribution must
// be subtracted from the group total — leaving only the genuinely
// external cash balances.
func TestStartRun_EliminatesMatchedIntercompanyEntry(t *testing.T) {
	s := newStubStore()
	pub := &stubPublisher{}
	tgtJournal := "journal-uk-1"
	cl := &stubClients{
		glBalances: map[string]map[string]float64{
			"le-us": {"1000-Cash": 5000.0, "1500-IC-Receivable": 500.0},
			"le-uk": {"1000-Cash": 3000.0, "2500-IC-Payable": -500.0},
		},
		intercompanyEntries: []clients.IntercompanyEntry{
			{
				IntercompanyEntryID: "ic-1",
				SourceLegalEntityID: "le-us",
				TargetLegalEntityID: "le-uk",
				SourceJournalID:     "journal-us-1",
				TargetJournalID:     &tgtJournal,
				Amount:              500.0,
				MatchStatus:         "MATCHED",
			},
		},
		journalLines: map[string][]clients.JournalLine{
			"journal-us-1": {{AccountCode: "1500-IC-Receivable", DebitAmount: 500.0, CreditAmount: 0}},
			"journal-uk-1": {{AccountCode: "2500-IC-Payable", DebitAmount: 0, CreditAmount: 500.0}},
		},
	}
	r := newRouter(s, pub, &stubAuthZ{}, cl)
	rr := doReq(r, http.MethodPost, "/v1/consolidation/runs/", map[string]any{
		"group_legal_entity_id":  "le-group",
		"child_legal_entity_ids": []string{"le-us", "le-uk"},
		"fiscal_period":          "2024-Q1",
		"target_currency":        "USD",
	}, "principal-1")

	if rr.Code != http.StatusCreated {
		t.Fatalf("expected 201 got %d: %s", rr.Code, rr.Body.String())
	}
	var resp domain.ConsolidationRunResponse
	if err := json.NewDecoder(rr.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.ExceptionCount != 0 {
		t.Fatalf("expected 0 elimination exceptions, got %d", resp.ExceptionCount)
	}

	byAccount := map[string]float64{}
	for _, snap := range resp.Snapshots {
		byAccount[snap.AccountCode] = snap.ConsolidatedBalance
	}
	// 500 (US receivable) - 500 (eliminated) = 0 — must NOT still show 500.
	if got := byAccount["1500-IC-Receivable"]; got != 0 {
		t.Errorf("expected 1500-IC-Receivable eliminated to 0, got %v", got)
	}
	// -500 (UK payable) - (-500) (eliminated) = 0 — must NOT still show -500.
	if got := byAccount["2500-IC-Payable"]; got != 0 {
		t.Errorf("expected 2500-IC-Payable eliminated to 0, got %v", got)
	}
	// Genuinely external cash must be untouched by elimination.
	if got := byAccount["1000-Cash"]; got != 8000.0 {
		t.Errorf("expected cash unaffected by elimination (8000), got %v", got)
	}
	if cl.principalIDReceived != "principal-1" {
		t.Errorf("expected the caller's own principal forwarded to intercompany-accounting-svc, got %q", cl.principalIDReceived)
	}
}

// TestStartRun_EliminationFetchFailure_RecordsVisibleException proves a
// failed elimination leg surfaces as a real, visible exception on the run
// — never a silent warning, and never a run that reports 0 exceptions
// while quietly having skipped elimination.
func TestStartRun_EliminationFetchFailure_RecordsVisibleException(t *testing.T) {
	s := newStubStore()
	pub := &stubPublisher{}
	tgtJournal := "journal-uk-1"
	cl := &stubClients{
		glBalances: map[string]map[string]float64{
			"le-us": {"1000-Cash": 5000.0},
			"le-uk": {"1000-Cash": 3000.0},
		},
		intercompanyEntries: []clients.IntercompanyEntry{
			{
				IntercompanyEntryID: "ic-1",
				SourceJournalID:     "journal-us-1",
				TargetJournalID:     &tgtJournal,
				Amount:              500.0,
				MatchStatus:         "MATCHED",
			},
		},
		journalLinesErr: map[string]error{
			"journal-us-1": domain.ErrGLServiceUnavailable,
		},
	}
	r := newRouter(s, pub, &stubAuthZ{}, cl)
	rr := doReq(r, http.MethodPost, "/v1/consolidation/runs/", map[string]any{
		"group_legal_entity_id":  "le-group",
		"child_legal_entity_ids": []string{"le-us", "le-uk"},
		"fiscal_period":          "2024-Q1",
		"target_currency":        "USD",
	}, "principal-1")

	if rr.Code != http.StatusCreated {
		t.Fatalf("expected 201 (run still completes) got %d: %s", rr.Code, rr.Body.String())
	}
	var resp domain.ConsolidationRunResponse
	if err := json.NewDecoder(rr.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.ExceptionCount != 1 {
		t.Fatalf("expected 1 visible elimination exception, got %d", resp.ExceptionCount)
	}
	if pub.exceptions != 1 {
		t.Errorf("expected PublishExceptionDetected to fire once, got %d calls", pub.exceptions)
	}
}

// TestStartRun_RecordsEntityToGroupProvenance proves ACC-13's missing
// provenance is now real: each child entity's own gross (pre-elimination)
// contribution to each account is persisted and independently retrievable,
// not just summed in memory and discarded.
func TestStartRun_RecordsEntityToGroupProvenance(t *testing.T) {
	s := newStubStore()
	pub := &stubPublisher{}
	cl := &stubClients{
		glBalances: map[string]map[string]float64{
			"le-us": {"1000-Cash": 5000.0},
			"le-uk": {"1000-Cash": 3000.0},
		},
	}
	r := newRouter(s, pub, &stubAuthZ{}, cl)
	rr := doReq(r, http.MethodPost, "/v1/consolidation/runs/", map[string]any{
		"group_legal_entity_id":  "le-group",
		"child_legal_entity_ids": []string{"le-us", "le-uk"},
		"fiscal_period":          "2024-Q1",
		"target_currency":        "USD",
	}, "principal-1")
	if rr.Code != http.StatusCreated {
		t.Fatalf("expected 201 got %d: %s", rr.Code, rr.Body.String())
	}
	var resp domain.ConsolidationRunResponse
	_ = json.NewDecoder(rr.Body).Decode(&resp)

	rrc := doReq(r, http.MethodGet, "/v1/consolidation/runs/"+resp.ConsolidationRunID+"/contributions", nil, "principal-1")
	if rrc.Code != http.StatusOK {
		t.Fatalf("expected 200 got %d: %s", rrc.Code, rrc.Body.String())
	}
	var contributions []domain.BalanceContribution
	if err := json.NewDecoder(rrc.Body).Decode(&contributions); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(contributions) != 2 {
		t.Fatalf("expected 2 contribution rows (one per child entity), got %d", len(contributions))
	}
	byEntity := map[string]float64{}
	for _, c := range contributions {
		if c.AccountCode != "1000-Cash" {
			t.Errorf("unexpected account code %q", c.AccountCode)
		}
		byEntity[c.SourceLegalEntityID] = c.GrossAmount
	}
	if byEntity["le-us"] != 5000.0 || byEntity["le-uk"] != 3000.0 {
		t.Errorf("expected per-entity gross amounts preserved, got %+v", byEntity)
	}
}

// TestStartRun_ContributionRecordingFails_RunFailsVisibly proves a failure
// to persist provenance is never silently ignored — the run itself fails,
// rather than completing and reporting COMPLETED balances nobody can trace
// back to a source entity.
func TestStartRun_ContributionRecordingFails_RunFailsVisibly(t *testing.T) {
	s := newStubStore()
	s.contribErr = domain.ErrStoreUnavailable
	cl := &stubClients{glBalances: map[string]map[string]float64{"le-us": {"1000-Cash": 5000.0}}}
	r := newRouter(s, &stubPublisher{}, &stubAuthZ{}, cl)
	rr := doReq(r, http.MethodPost, "/v1/consolidation/runs/", map[string]any{
		"group_legal_entity_id":  "le-group",
		"child_legal_entity_ids": []string{"le-us"},
		"fiscal_period":          "2024-Q1",
		"target_currency":        "USD",
	}, "principal-1")
	if rr.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected 503 got %d: %s", rr.Code, rr.Body.String())
	}
}

// ── ListRuns & Snapshots Tests ────────────────────────────────────────────────

func TestGetRun_NotFound(t *testing.T) {
	r := newRouter(newStubStore(), &stubPublisher{}, &stubAuthZ{}, &stubClients{})
	rr := doReq(r, http.MethodGet, "/v1/consolidation/runs/nonexistent-id", nil, "principal-1")
	if rr.Code != http.StatusNotFound {
		t.Fatalf("expected 404 got %d", rr.Code)
	}
}
