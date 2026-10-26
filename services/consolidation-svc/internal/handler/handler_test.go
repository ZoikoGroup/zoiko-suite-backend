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
	"github.com/google/uuid"
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
	eliminations  map[string][]domain.Elimination
	elimErr       error

	adjustments map[string]*domain.ConsolidationAdjustment
	histories   map[string]*domain.AdjustmentHistory
	groupHasRun map[string]bool
	hasSnapshot map[string]bool

	// commandKeys mirrors command_idempotency by Idempotency-Key, so the
	// handler's replay path is exercised against the same rule the store
	// applies: same command and target replays, anything else conflicts.
	commandKeys map[string]stubCommandKey

	// completeErr makes CompleteRun fail, for the A14 test that the response
	// reports the run's persisted state instead of 201 COMPLETED.
	completeErr error

	// What the handler handed the store for the run's events; the enqueue
	// itself happens inside PgStore and is proven in outbox_events_test.go.
	createRunActor      string
	completedStatus     string
	completedActor      string
	completedSnapshots  int
	completedExceptions []string
}

// stubCommandKey is one command_idempotency row.
type stubCommandKey struct {
	command      string
	runID        string
	adjustmentID string
}

func newStubStore() *stubStore {
	return &stubStore{
		runs:          make(map[string]*domain.ConsolidationRun),
		snapshots:     make(map[string][]domain.BalanceSnapshot),
		contributions: make(map[string][]domain.BalanceContribution),
		eliminations:  make(map[string][]domain.Elimination),
		adjustments:   make(map[string]*domain.ConsolidationAdjustment),
		histories:     make(map[string]*domain.AdjustmentHistory),
		groupHasRun:   make(map[string]bool),
		hasSnapshot:   make(map[string]bool),
		commandKeys:   make(map[string]stubCommandKey),
	}
}

func (s *stubStore) recordCommandKey(key, command, runID, adjustmentID string) {
	s.commandKeys[key] = stubCommandKey{command: command, runID: runID, adjustmentID: adjustmentID}
}

func (s *stubStore) CreateRun(_ context.Context, run *domain.ConsolidationRun, idempotencyKey, actorID, _ string) (*domain.ConsolidationRun, bool, error) {
	if idempotencyKey == "" {
		return nil, false, domain.ErrIdempotencyKeyRequired
	}
	if rec, ok := s.commandKeys[idempotencyKey]; ok {
		if rec.command != domain.CommandStartRun || rec.runID == "" {
			return nil, false, domain.ErrIdempotencyKeyConflict
		}
		existing, ok := s.runs[rec.runID]
		if !ok {
			return nil, false, domain.ErrRunNotFound
		}
		if existing.GroupLegalEntityID != run.GroupLegalEntityID ||
			existing.FiscalPeriod != run.FiscalPeriod ||
			existing.TargetCurrency != run.TargetCurrency {
			return nil, false, domain.ErrIdempotencyKeyConflict
		}
		return existing, true, nil
	}
	s.runs[run.ConsolidationRunID] = run
	s.createRunActor = actorID
	s.recordCommandKey(idempotencyKey, domain.CommandStartRun, run.ConsolidationRunID, "")
	return run, false, nil
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

func (s *stubStore) CompleteRun(_ context.Context, id, status string, exceptionCount int, completedAt time.Time, actorID, _ string, snapshotCount int, exceptions []string) error {
	if s.completeErr != nil {
		return s.completeErr
	}
	run, ok := s.runs[id]
	if !ok {
		return domain.ErrRunNotFound
	}
	s.completedStatus, s.completedActor = status, actorID
	s.completedSnapshots, s.completedExceptions = snapshotCount, exceptions
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

func (s *stubStore) CreateEliminations(_ context.Context, rows []domain.Elimination) error {
	if s.elimErr != nil {
		return s.elimErr
	}
	for _, e := range rows {
		s.eliminations[e.ConsolidationRunID] = append(s.eliminations[e.ConsolidationRunID], e)
	}
	return nil
}

func (s *stubStore) ListEliminationsByRun(_ context.Context, runID string) ([]domain.Elimination, error) {
	return s.eliminations[runID], nil
}

func (s *stubStore) GroupEntityHasRun(_ context.Context, groupLegalEntityID string) (bool, error) {
	return s.groupHasRun[groupLegalEntityID], nil
}

func (s *stubStore) HasSnapshotForPeriod(_ context.Context, groupLegalEntityID, fiscalPeriod string) (bool, error) {
	return s.hasSnapshot[groupLegalEntityID+"|"+fiscalPeriod], nil
}

func (s *stubStore) CreateAdjustment(_ context.Context, a *domain.ConsolidationAdjustment, idempotencyKey, _ string) (*domain.ConsolidationAdjustment, bool, error) {
	if idempotencyKey == "" {
		return nil, false, domain.ErrIdempotencyKeyRequired
	}
	if rec, ok := s.commandKeys[idempotencyKey]; ok {
		if rec.command != domain.CommandCreateAdjustment || rec.adjustmentID == "" {
			return nil, false, domain.ErrIdempotencyKeyConflict
		}
		existing, ok := s.adjustments[rec.adjustmentID]
		if !ok {
			return nil, false, domain.ErrAdjustmentNotFound
		}
		if existing.GroupLegalEntityID != a.GroupLegalEntityID ||
			existing.FiscalPeriod != a.FiscalPeriod ||
			existing.AdjustmentType != a.AdjustmentType {
			return nil, false, domain.ErrIdempotencyKeyConflict
		}
		return existing, true, nil
	}
	s.adjustments[a.ConsolidationAdjustmentID] = a
	s.recordCommandKey(idempotencyKey, domain.CommandCreateAdjustment, "", a.ConsolidationAdjustmentID)
	return a, false, nil
}

func (s *stubStore) GetAdjustment(_ context.Context, id string) (*domain.ConsolidationAdjustment, error) {
	a, ok := s.adjustments[id]
	if !ok {
		return nil, domain.ErrAdjustmentNotFound
	}
	return a, nil
}

func (s *stubStore) GetAdjustmentHistory(_ context.Context, id string) (*domain.AdjustmentHistory, error) {
	h, ok := s.histories[id]
	if !ok {
		return nil, domain.ErrAdjustmentNotFound
	}
	return h, nil
}

func (s *stubStore) ListAdjustments(_ context.Context, groupLegalEntityID, fiscalPeriod string) ([]domain.ConsolidationAdjustment, error) {
	var out []domain.ConsolidationAdjustment
	for _, a := range s.adjustments {
		if groupLegalEntityID != "" && a.GroupLegalEntityID != groupLegalEntityID {
			continue
		}
		if fiscalPeriod != "" && a.FiscalPeriod != fiscalPeriod {
			continue
		}
		out = append(out, *a)
	}
	return out, nil
}

func (s *stubStore) ApproveAdjustment(_ context.Context, id, principalID, idempotencyKey, _ string) (*domain.ConsolidationAdjustment, bool, error) {
	if idempotencyKey == "" {
		return nil, false, domain.ErrIdempotencyKeyRequired
	}
	a, ok := s.adjustments[id]
	if !ok {
		return nil, false, domain.ErrAdjustmentNotFound
	}
	if rec, ok := s.commandKeys[idempotencyKey]; ok {
		if rec.command != domain.CommandApproveAdjustment || rec.adjustmentID != id {
			return nil, false, domain.ErrIdempotencyKeyConflict
		}
		return a, true, nil
	}
	if a.Status != domain.AdjustmentStatusPendingApproval {
		return nil, false, domain.ErrInvalidAdjustmentTransition
	}
	now := time.Now().UTC()
	a.Status, a.ApprovedAt, a.ApprovedByPrincipalID = domain.AdjustmentStatusApproved, &now, &principalID
	s.recordCommandKey(idempotencyKey, domain.CommandApproveAdjustment, "", id)
	return a, false, nil
}

func (s *stubStore) MarkAdjustmentPosted(_ context.Context, id, principalID, journalID, idempotencyKey, _ string) (*domain.ConsolidationAdjustment, bool, error) {
	if idempotencyKey == "" {
		return nil, false, domain.ErrIdempotencyKeyRequired
	}
	a, ok := s.adjustments[id]
	if !ok {
		return nil, false, domain.ErrAdjustmentNotFound
	}
	if rec, ok := s.commandKeys[idempotencyKey]; ok {
		if rec.command != domain.CommandPostAdjustment || rec.adjustmentID != id {
			return nil, false, domain.ErrIdempotencyKeyConflict
		}
		return a, true, nil
	}
	if a.Status != domain.AdjustmentStatusApproved {
		return nil, false, domain.ErrInvalidAdjustmentTransition
	}
	now := time.Now().UTC()
	a.Status, a.PostedAt, a.PostedByPrincipalID, a.ConsolidationBookJournalID = domain.AdjustmentStatusPosted, &now, &principalID, &journalID
	s.recordCommandKey(idempotencyKey, domain.CommandPostAdjustment, "", id)
	return a, false, nil
}

func (s *stubStore) ReverseAdjustment(_ context.Context, id, principalID, reason string, supersededBy *string, idempotencyKey, _ string) (*domain.ConsolidationAdjustment, bool, error) {
	if idempotencyKey == "" {
		return nil, false, domain.ErrIdempotencyKeyRequired
	}
	a, ok := s.adjustments[id]
	if !ok {
		return nil, false, domain.ErrAdjustmentNotFound
	}
	if rec, ok := s.commandKeys[idempotencyKey]; ok {
		if rec.command != domain.CommandReverseAdjustment || rec.adjustmentID != id {
			return nil, false, domain.ErrIdempotencyKeyConflict
		}
		return a, true, nil
	}
	if a.Status != domain.AdjustmentStatusPosted {
		return nil, false, domain.ErrInvalidAdjustmentTransition
	}
	now := time.Now().UTC()
	a.Status, a.ReversedAt, a.ReversedByPrincipalID, a.ReversalReason, a.SupersededByAdjustmentID =
		domain.AdjustmentStatusReversed, &now, &principalID, &reason, supersededBy
	s.recordCommandKey(idempotencyKey, domain.CommandReverseAdjustment, "", id)
	return a, false, nil
}

func (s *stubStore) ReplayedAdjustmentCommand(_ context.Context, id, command, idempotencyKey string) (bool, error) {
	if idempotencyKey == "" {
		return false, domain.ErrIdempotencyKeyRequired
	}
	rec, ok := s.commandKeys[idempotencyKey]
	if !ok {
		return false, nil
	}
	if rec.command != command || rec.adjustmentID != id {
		return false, domain.ErrIdempotencyKeyConflict
	}
	return true, nil
}

// Compile-time proof the stub still satisfies the contract the handler
// depends on.
var _ handler.Store = (*stubStore)(nil)

type stubAuthZ struct{ err error }

func (a *stubAuthZ) CheckAllowed(_ context.Context, _, _, _ string) error { return a.err }

type stubClients struct {
	glBalances map[string]map[string]float64
	glErr      error

	// trialBalanceReads counts FetchTrialBalance calls, so an A14 replay can
	// prove it did not run the run's GL work a second time.
	trialBalanceReads int

	intercompanyEntries []clients.IntercompanyEntry
	intercompanyErr     error
	principalIDReceived string // captures what FetchEliminableIntercompanyEntries was called with

	reciprocalBalances *domain.ReciprocalBalances
	reciprocalErr      error
	reciprocalEntityA  string
	reciprocalEntityB  string
	reciprocalPeriod   string

	// journals maps journal_id -> the journal general-ledger-svc serves,
	// for the elimination path (FetchJournal). journalErr, keyed the same
	// way, lets a test simulate one specific journal's lookup failing.
	journals     map[string]*domain.PostedJournal
	journalErr   map[string]error
	journalReads []string

	postJournalID           string
	postErr                 error
	postCalls               int
	reverseErr              error
	reverseCalls            int
	lastPostedLegalEntityID string
}

func (c *stubClients) FetchTrialBalance(_ context.Context, _, legalEntityID, _ string) (map[string]float64, error) {
	c.trialBalanceReads++
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

func (c *stubClients) FetchEliminableIntercompanyEntries(_ context.Context, _, principalID string) ([]clients.IntercompanyEntry, error) {
	c.principalIDReceived = principalID
	if c.intercompanyErr != nil {
		return nil, c.intercompanyErr
	}
	return c.intercompanyEntries, nil
}

func (c *stubClients) FetchReciprocalBalances(_ context.Context, _, principalID, entityA, entityB, fiscalPeriod string) (*domain.ReciprocalBalances, error) {
	c.principalIDReceived = principalID
	c.reciprocalEntityA, c.reciprocalEntityB, c.reciprocalPeriod = entityA, entityB, fiscalPeriod
	if c.reciprocalErr != nil {
		return nil, c.reciprocalErr
	}
	return c.reciprocalBalances, nil
}

func (c *stubClients) FetchJournal(_ context.Context, _, journalID string) (*domain.PostedJournal, error) {
	c.journalReads = append(c.journalReads, journalID)
	if err, ok := c.journalErr[journalID]; ok {
		return nil, err
	}
	if j, ok := c.journals[journalID]; ok {
		return j, nil
	}
	return nil, domain.ErrGLServiceUnavailable
}

// finalized is a FINALIZED journal of entity in period with lines.
func finalized(id, entity, period string, lines ...domain.PostedLine) *domain.PostedJournal {
	return &domain.PostedJournal{JournalID: id, LegalEntityID: entity, FiscalPeriod: period, Status: "FINALIZED", Lines: lines}
}

func strPtr(s string) *string { return &s }

func (c *stubClients) PostConsolidationAdjustmentJournal(_ context.Context, _, _, _, legalEntityID, _, _, _, _ string, _ []clients.JournalLine) (string, error) {
	c.postCalls++
	c.lastPostedLegalEntityID = legalEntityID
	if c.postErr != nil {
		return "", c.postErr
	}
	if c.postJournalID != "" {
		return c.postJournalID, nil
	}
	return "journal-generated", nil
}

func (c *stubClients) ReverseConsolidationAdjustmentJournal(_ context.Context, _, _, _, _, _ string) error {
	c.reverseCalls++
	return c.reverseErr
}

// ── router factory ─────────────────────────────────────────────────────────────

func newRouter(s *stubStore, authz *stubAuthZ, cl *stubClients) chi.Router {
	r := chi.NewRouter()
	r.Use(func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			req = req.WithContext(middleware.WithTenant(req.Context(), "tenant-abc"))
			next.ServeHTTP(w, req)
		})
	})
	h := handler.New(s, authz, cl, zap.NewNop())
	handler.RegisterRoutes(r, h)
	return r
}

// doReq sends a command with a fresh Idempotency-Key: every mutating route in
// this service requires one. Tests that need to retry under the same key use
// doReqWithKey; the missing-key path uses doReqWithKey with "".
func doReq(r chi.Router, method, path string, body any, principalID string) *httptest.ResponseRecorder {
	return doReqWithKey(r, method, path, body, principalID, uuid.NewString())
}

func doReqWithKey(r chi.Router, method, path string, body any, principalID, idempotencyKey string) *httptest.ResponseRecorder {
	var buf bytes.Buffer
	if body != nil {
		_ = json.NewEncoder(&buf).Encode(body)
	}
	req := httptest.NewRequest(method, path, &buf)
	req.Header.Set("Content-Type", "application/json")
	if idempotencyKey != "" {
		req.Header.Set("Idempotency-Key", idempotencyKey)
	}
	if principalID != "" {
		req.Header.Set("X-Principal-Id", principalID)
	}
	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, req)
	return rr
}

// ── StartRun Tests ────────────────────────────────────────────────────────────

func TestStartRun_MissingPrincipal(t *testing.T) {
	r := newRouter(newStubStore(), &stubAuthZ{}, &stubClients{})
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
	r := newRouter(newStubStore(), &stubAuthZ{err: domain.ErrAuthorizationDenied}, &stubClients{})
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
	r := newRouter(newStubStore(), &stubAuthZ{}, &stubClients{})
	rr := doReq(r, http.MethodPost, "/v1/consolidation/runs/", map[string]any{
		"group_legal_entity_id": "le-group",
	}, "principal-1")
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 got %d", rr.Code)
	}
}

func TestStartRun_HappyPath(t *testing.T) {
	s := newStubStore()
	cl := &stubClients{
		glBalances: map[string]map[string]float64{
			"le-us": {"1000-Cash": 5000.0, "2000-AP": -2000.0},
			"le-uk": {"1000-Cash": 3000.0, "2000-AP": -1000.0},
		},
	}
	r := newRouter(s, &stubAuthZ{}, cl)
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
	// started/completed enqueue: TestPgStore_CreateRun_EnqueuesStartedEvent and
	// TestPgStore_CompleteRun_EnqueuesCompletedEvent (outbox_events_test.go).
	if s.createRunActor != "principal-1" || s.completedActor != "principal-1" {
		t.Errorf("expected the caller's principal handed to the store as event actor, got create=%q complete=%q", s.createRunActor, s.completedActor)
	}
	if s.completedStatus != "COMPLETED" || s.completedSnapshots != 2 {
		t.Errorf("expected CompleteRun(COMPLETED, 2 snapshots), got (%q, %d)", s.completedStatus, s.completedSnapshots)
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
				Amount:              "500",
				CounterpartyAmount:  "500",
				FiscalPeriod:        strPtr("2024-Q1"),
				MatchStatus:         "MATCHED",
			},
		},
		journals: map[string]*domain.PostedJournal{
			"journal-us-1": finalized("journal-us-1", "le-us", "2024-Q1", domain.PostedLine{LineNumber: 1, AccountCode: "1500-IC-Receivable", DebitAmount: 500.0}),
			"journal-uk-1": finalized("journal-uk-1", "le-uk", "2024-Q1", domain.PostedLine{LineNumber: 1, AccountCode: "2500-IC-Payable", CreditAmount: 500.0}),
		},
	}
	r := newRouter(s, &stubAuthZ{}, cl)
	rr := doReq(r, http.MethodPost, "/v1/consolidation/runs/", map[string]any{
		"group_legal_entity_id":      "le-group",
		"child_legal_entity_ids":     []string{"le-us", "le-uk"},
		"fiscal_period":              "2024-Q1",
		"target_currency":            "USD",
		"intercompany_account_codes": []string{"1500-IC-Receivable", "2500-IC-Payable"},
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
// leg that cannot be read leaves the whole pair uneliminated: the pair is a
// BLOCKED elimination row and a visible exception on the run, and the
// readable leg is not subtracted on its own (no half-eliminated pair).
func TestStartRun_EliminationFetchFailure_RecordsVisibleException(t *testing.T) {
	s := newStubStore()
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
				Amount:              "500",
				CounterpartyAmount:  "500",
				FiscalPeriod:        strPtr("2024-Q1"),
				MatchStatus:         "MATCHED",
			},
		},
		journals: map[string]*domain.PostedJournal{
			"journal-us-1": finalized("journal-us-1", "le-us", "2024-Q1", domain.PostedLine{LineNumber: 1, AccountCode: "1500-IC-Receivable", DebitAmount: 500.0}),
		},
		journalErr: map[string]error{
			"journal-uk-1": domain.ErrGLServiceUnavailable,
		},
	}
	r := newRouter(s, &stubAuthZ{}, cl)
	rr := doReq(r, http.MethodPost, "/v1/consolidation/runs/", map[string]any{
		"group_legal_entity_id":      "le-group",
		"child_legal_entity_ids":     []string{"le-us", "le-uk"},
		"fiscal_period":              "2024-Q1",
		"target_currency":            "USD",
		"intercompany_account_codes": []string{"1500-IC-Receivable", "2500-IC-Payable"},
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
	// exception.detected enqueue: TestPgStore_CompleteRun_WithExceptions_EnqueuesExceptionDetectedEvent.
	if len(s.completedExceptions) != 1 {
		t.Errorf("expected the 1 elimination exception handed to CompleteRun, got %v", s.completedExceptions)
	}
	for _, snap := range resp.Snapshots {
		if snap.AccountCode == "1500-IC-Receivable" && snap.ConsolidatedBalance != 500 {
			t.Errorf("the readable source leg was eliminated on its own: receivable = %v", snap.ConsolidatedBalance)
		}
	}
	rows := s.eliminations[resp.ConsolidationRunID]
	if len(rows) != 1 || rows[0].Outcome != domain.EliminationOutcomeBlocked || rows[0].BlockedReason == nil || len(rows[0].Lines) != 0 {
		t.Fatalf("expected one BLOCKED elimination row with no lines, got %+v", rows)
	}
}

// TestStartRun_RecordsEntityToGroupProvenance proves ACC-13's missing
// provenance is now real: each child entity's own gross (pre-elimination)
// contribution to each account is persisted and independently retrievable,
// not just summed in memory and discarded.
func TestStartRun_RecordsEntityToGroupProvenance(t *testing.T) {
	s := newStubStore()
	cl := &stubClients{
		glBalances: map[string]map[string]float64{
			"le-us": {"1000-Cash": 5000.0},
			"le-uk": {"1000-Cash": 3000.0},
		},
	}
	r := newRouter(s, &stubAuthZ{}, cl)
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
	r := newRouter(s, &stubAuthZ{}, cl)
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
	r := newRouter(newStubStore(), &stubAuthZ{}, &stubClients{})
	rr := doReq(r, http.MethodGet, "/v1/consolidation/runs/nonexistent-id", nil, "principal-1")
	if rr.Code != http.StatusNotFound {
		t.Fatalf("expected 404 got %d", rr.Code)
	}
}
