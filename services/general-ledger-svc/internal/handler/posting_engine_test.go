package handler_test

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
	"go.uber.org/zap"

	"zoiko.io/general-ledger-svc/internal/domain"
	"zoiko.io/general-ledger-svc/internal/handler"
	svcmiddleware "zoiko.io/general-ledger-svc/internal/middleware"
)

// ── ACC-04 Posting Engine ────────────────────────────────────────────────────

// recordingAuthZ captures which action was checked, so a test can assert the
// posting engine's own actions are distinct from the journal lifecycle's —
// the spec requires reversal in particular be a separately-authorized
// correction command, and an action name nothing asserts is one a later
// refactor can silently collapse into another.
type recordingAuthZ struct {
	seen []string
	err  error
}

func (a *recordingAuthZ) CheckAllowed(_ context.Context, _, _, actionType string) error {
	a.seen = append(a.seen, actionType)
	return a.err
}

// countingClose fails the Nth CheckPeriodOpen call and every one after it —
// the only way to reproduce the spec's own "closed period race during
// commit": the period is open when the journal is created and locked by the
// time it is actually posted.
type countingClose struct {
	calls    int
	failFrom int
	err      error
}

func (c *countingClose) CheckPeriodOpen(_ context.Context, _, _, _ string) error {
	c.calls++
	if c.failFrom > 0 && c.calls >= c.failFrom {
		return c.err
	}
	return nil
}

func newRouterWithRecordingAuthZ(s *stubStore, p *stubPublisher, a *recordingAuthZ) chi.Router {
	r := chi.NewRouter()
	r.Use(svcmiddleware.TenantContext())
	h := handler.New(s, p, a, &stubClose{}, zap.NewNop())
	handler.RegisterRoutes(r, h)
	return r
}

func validPostEventReq() domain.PostAccountingEventRequest {
	code1, code2 := "1000", "4000"
	return domain.PostAccountingEventRequest{
		LegalEntityID: "e1",
		FiscalPeriod:  "2026-07",
		Description:   "revenue recognized",
		SourceEventID: "src-evt-1",
		CorrelationID: "corr-post-1",
		Lines: []domain.PostingEventLineInput{
			{AccountCode: &code1, DebitAmount: 100},
			{AccountCode: &code2, CreditAmount: 100},
		},
	}
}

func TestPostAccountingEvent_CommitsAndFinalizesTheJournal(t *testing.T) {
	s := newStubStore()
	r := newRouter(s, &stubPublisher{}, &stubAuthZ{})

	rec := doRequest(r, http.MethodPost, "/v1/postings/events", validPostEventReq(), "svc-ap")
	if rec.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d: %s", rec.Code, rec.Body.String())
	}
	var exec domain.PostingExecution
	if err := json.Unmarshal(rec.Body.Bytes(), &exec); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if exec.Status != domain.PostingExecutionStatusCommitted {
		t.Errorf("expected COMMITTED, got %q (failure_reason=%v)", exec.Status, exec.FailureReason)
	}
	if exec.JournalID == nil {
		t.Fatal("expected the execution to name the journal it committed")
	}
	// The whole point of ACC-04: the ledger entry itself is FINALIZED, not
	// left as a draft for someone else to post.
	journal := s.journals[*exec.JournalID]
	if journal == nil || journal.Status != domain.JournalStatusFinalized {
		t.Fatalf("expected the journal to be FINALIZED, got %+v", journal)
	}
	if journal.SourceEventID == nil || *journal.SourceEventID != "src-evt-1" {
		t.Errorf("expected the source event carried onto the journal for lineage, got %v", journal.SourceEventID)
	}
}

// Negative path #1 (verbatim): "Duplicate source event ... no unauthorized
// or duplicate accounting consequence."
func TestPostAccountingEvent_DuplicateSourceEvent_ReturnsPriorResultAndPostsOnce(t *testing.T) {
	s := newStubStore()
	r := newRouter(s, &stubPublisher{}, &stubAuthZ{})

	first := doRequest(r, http.MethodPost, "/v1/postings/events", validPostEventReq(), "svc-ap")
	if first.Code != http.StatusCreated {
		t.Fatalf("first post failed: %d %s", first.Code, first.Body.String())
	}
	var firstExec domain.PostingExecution
	_ = json.Unmarshal(first.Body.Bytes(), &firstExec)

	replay := doRequest(r, http.MethodPost, "/v1/postings/events", validPostEventReq(), "svc-ap")
	if replay.Code != http.StatusOK {
		t.Fatalf("expected 200 on replay, got %d: %s", replay.Code, replay.Body.String())
	}
	var replayExec domain.PostingExecution
	_ = json.Unmarshal(replay.Body.Bytes(), &replayExec)
	if replayExec.ExecutionID != firstExec.ExecutionID {
		t.Errorf("expected the PRIOR execution back, got a new one (%s vs %s)", replayExec.ExecutionID, firstExec.ExecutionID)
	}
	if len(s.postingExecutions) != 1 {
		t.Errorf("expected exactly 1 posting execution, got %d", len(s.postingExecutions))
	}
	if len(s.journals) != 1 {
		t.Errorf("expected exactly 1 journal — a duplicate source event must not post twice, got %d", len(s.journals))
	}
}

// Negative path #2 (verbatim): "Posting rule ambiguity."
func TestPostAccountingEvent_LineNamingBothAccountCodeAndMappingKey_Returns422(t *testing.T) {
	s := newStubStore()
	r := newRouter(s, &stubPublisher{}, &stubAuthZ{})

	req := validPostEventReq()
	key := "EXPENSE_CATEGORY:MEALS"
	req.Lines[0].MappingKey = &key // names BOTH — which rule wins is not for this service to guess

	rec := doRequest(r, http.MethodPost, "/v1/postings/events", req, "svc-ap")
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("expected 422, got %d: %s", rec.Code, rec.Body.String())
	}
	if len(s.journals) != 0 {
		t.Errorf("an ambiguous posting rule must post nothing, got %d journals", len(s.journals))
	}
}

func TestPostAccountingEvent_LineNamingNeitherAccountCodeNorMappingKey_Returns422(t *testing.T) {
	r := newRouter(newStubStore(), &stubPublisher{}, &stubAuthZ{})
	req := validPostEventReq()
	req.Lines[0].AccountCode = nil

	rec := doRequest(r, http.MethodPost, "/v1/postings/events", req, "svc-ap")
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("expected 422, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestPostAccountingEvent_UnresolvableMappingKey_Returns422(t *testing.T) {
	s := newStubStore()
	r := newRouter(s, &stubPublisher{}, &stubAuthZ{})
	req := validPostEventReq()
	key := "EXPENSE_CATEGORY:NEVER_MAPPED"
	req.Lines[0].AccountCode = nil
	req.Lines[0].MappingKey = &key

	rec := doRequest(r, http.MethodPost, "/v1/postings/events", req, "svc-ap")
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("expected 422, got %d: %s", rec.Code, rec.Body.String())
	}
	if len(s.journals) != 0 {
		t.Errorf("a mapping_key that resolves to nothing must post nothing, got %d journals", len(s.journals))
	}
}

// Real ACC-02 integration: a caller may post against a business concept and
// let the posting engine resolve it, which is the whole reason ACC-02's
// mapping registry exists.
func TestPostAccountingEvent_ResolvesMappingKeyThroughACC02(t *testing.T) {
	s := newStubStore()
	s.accountsByCode["t1|6200-Meals"] = &domain.Account{AccountCode: "6200-Meals", Status: "ACTIVE"}
	s.currentMappings["t1|EXPENSE_CATEGORY:MEALS"] = &domain.AccountMapping{
		TenantID: "t1", MappingKey: "EXPENSE_CATEGORY:MEALS", AccountCode: "6200-Meals",
	}
	r := newRouter(s, &stubPublisher{}, &stubAuthZ{})

	req := validPostEventReq()
	key := "EXPENSE_CATEGORY:MEALS"
	req.Lines[0].AccountCode = nil
	req.Lines[0].MappingKey = &key

	rec := doRequest(r, http.MethodPost, "/v1/postings/events", req, "svc-ap")
	if rec.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d: %s", rec.Code, rec.Body.String())
	}
	var exec domain.PostingExecution
	_ = json.Unmarshal(rec.Body.Bytes(), &exec)

	lines := s.lines[*exec.JournalID]
	if len(lines) == 0 || lines[0].AccountCode != "6200-Meals" {
		t.Fatalf("expected the mapping to resolve to 6200-Meals on the posted line, got %+v", lines)
	}
	// ExplainPosting's own data: the trace must say HOW the account was reached.
	if !strings.Contains(exec.CalculationTrace, "mapping_key:EXPENSE_CATEGORY:MEALS -\\u003e account_code:6200-Meals") &&
		!strings.Contains(exec.CalculationTrace, "mapping_key:EXPENSE_CATEGORY:MEALS -> account_code:6200-Meals") {
		t.Errorf("expected the calculation trace to record the rule resolution, got %q", exec.CalculationTrace)
	}
}

// Negative path #4 (verbatim): "Closed period race during commit." The
// period is open when the journal is created and LOCKED by the time the
// posting is actually committed.
func TestPostAccountingEvent_ClosedPeriodRaceDuringCommit_BlocksAndRecordsFailure(t *testing.T) {
	s := newStubStore()
	// Call 1 = the pre-create check (open); call 2 = the re-check immediately
	// before FINALIZED (locked).
	closeClient := &countingClose{failFrom: 2, err: domain.ErrPeriodLocked}
	r := newRouterWithClose(s, &stubPublisher{}, &stubAuthZ{}, closeClient)

	rec := doRequest(r, http.MethodPost, "/v1/postings/events", validPostEventReq(), "svc-ap")
	if rec.Code != http.StatusPreconditionFailed {
		t.Fatalf("expected 412 period_locked, got %d: %s", rec.Code, rec.Body.String())
	}
	if closeClient.calls < 2 {
		t.Fatalf("expected the period to be re-checked before the commit, got %d checks", closeClient.calls)
	}
	// No journal may be left FINALIZED, and the execution must say plainly
	// that it failed — never left silently in SUBMITTED.
	for _, j := range s.journals {
		if j.Status == domain.JournalStatusFinalized {
			t.Errorf("a journal was finalized into a locked period: %+v", j)
		}
	}
	if len(s.postingExecutions) != 1 {
		t.Fatalf("expected 1 execution recorded, got %d", len(s.postingExecutions))
	}
	for _, e := range s.postingExecutions {
		if e.Status != domain.PostingExecutionStatusFailed && e.Status != domain.PostingExecutionStatusQuarantined {
			t.Errorf("expected the execution to be FAILED/QUARANTINED, got %q", e.Status)
		}
		if e.FailureReason == nil {
			t.Error("expected a recorded failure reason")
		}
	}
}

func TestPostAccountingEvent_UnbalancedLines_QuarantinesRatherThanPosts(t *testing.T) {
	s := newStubStore()
	s.debitTotal, s.creditTotal = 10000, 9000 // the store's own exact-cents disagreement
	r := newRouter(s, &stubPublisher{}, &stubAuthZ{})

	rec := doRequest(r, http.MethodPost, "/v1/postings/events", validPostEventReq(), "svc-ap")
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("expected 422 unbalanced, got %d: %s", rec.Code, rec.Body.String())
	}
	for _, e := range s.postingExecutions {
		if e.Status != domain.PostingExecutionStatusQuarantined {
			t.Errorf("an unbalanced posting needs a human, so it must QUARANTINE, got %q", e.Status)
		}
	}
	for _, j := range s.journals {
		if j.Status == domain.JournalStatusFinalized {
			t.Error("an unbalanced journal must never reach FINALIZED")
		}
	}
}

func TestPostApprovedJournal_PendingJournal_RefusedRatherThanSilentlyValidated(t *testing.T) {
	s := newStubStore()
	s.journals["j-pending"] = &domain.JournalHeader{
		JournalID: "j-pending", TenantID: testTenantID, LegalEntityID: "e1",
		FiscalPeriod: "2026-07", Status: domain.JournalStatusPending,
	}
	r := newRouter(s, &stubPublisher{}, &stubAuthZ{})

	rec := doRequest(r, http.MethodPost, "/v1/postings/journals",
		domain.PostApprovedJournalRequest{JournalID: "j-pending"}, "svc-ap")
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("expected 422, got %d: %s", rec.Code, rec.Body.String())
	}
	if s.journals["j-pending"].Status != domain.JournalStatusPending {
		t.Errorf("the journal must be left untouched, got %q", s.journals["j-pending"].Status)
	}
}

func TestPostApprovedJournal_ValidatedJournal_CommitsWithExecutionRecord(t *testing.T) {
	s := newStubStore()
	s.journals["j-validated"] = &domain.JournalHeader{
		JournalID: "j-validated", TenantID: testTenantID, LegalEntityID: "e1",
		FiscalPeriod: "2026-07", Status: domain.JournalStatusValidated,
		// Completed ACC-03's own approval workflow — the real precondition
		// PostApprovedJournal now checks, not just JournalStatus.
		ApprovalStatus: domain.ApprovalStatusPostingRequested,
	}
	r := newRouter(s, &stubPublisher{}, &stubAuthZ{})

	rec := doRequest(r, http.MethodPost, "/v1/postings/journals",
		domain.PostApprovedJournalRequest{JournalID: "j-validated"}, "svc-ap")
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	if s.journals["j-validated"].Status != domain.JournalStatusFinalized {
		t.Errorf("expected FINALIZED, got %q", s.journals["j-validated"].Status)
	}
	// The ACC-03/ACC-04 integration point: committing the ledger entry
	// must also close the loop back to ACC-03's own lifecycle.
	if s.journals["j-validated"].ApprovalStatus != domain.ApprovalStatusPosted {
		t.Errorf("expected ApprovalStatus POSTED, got %q", s.journals["j-validated"].ApprovalStatus)
	}
	if len(s.postingExecutions) != 1 {
		t.Fatalf("expected the commit to leave a posting execution record, got %d", len(s.postingExecutions))
	}
}

// The real ACC-03/ACC-04 integration point: a VALIDATED journal that never
// went through ACC-03's RequestPosting step must be refused, not silently
// posted. Before this gate existed, PostApprovedJournal accepted ANY
// VALIDATED journal — the spec's own "Dependencies: ... ACC-04" line
// requires the posting engine actually depend on ACC-03 having run.
func TestPostApprovedJournal_ValidatedButNeverRequestedPosting_Refused(t *testing.T) {
	s := newStubStore()
	s.journals["j-validated"] = &domain.JournalHeader{
		JournalID: "j-validated", TenantID: testTenantID, LegalEntityID: "e1",
		FiscalPeriod: "2026-07", Status: domain.JournalStatusValidated,
		ApprovalStatus: domain.ApprovalStatusApproved, // approved, but RequestPosting never called
	}
	r := newRouter(s, &stubPublisher{}, &stubAuthZ{})

	rec := doRequest(r, http.MethodPost, "/v1/postings/journals",
		domain.PostApprovedJournalRequest{JournalID: "j-validated"}, "svc-ap")
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("expected 422, got %d: %s", rec.Code, rec.Body.String())
	}
	if s.journals["j-validated"].Status != domain.JournalStatusValidated {
		t.Error("the journal must be left untouched")
	}
}

// The spec's own words: "reversal requires authorized correction command."
func TestCreateReversalPosting_UsesItsOwnCorrectionAction(t *testing.T) {
	s := newStubStore()
	s.journals["j-final"] = &domain.JournalHeader{
		JournalID: "j-final", TenantID: testTenantID, LegalEntityID: "e1",
		FiscalPeriod: "2026-07", Status: domain.JournalStatusFinalized,
	}
	s.lines["j-final"] = []domain.JournalLine{
		{AccountCode: "1000", DebitAmount: 100},
		{AccountCode: "4000", CreditAmount: 100},
	}
	authz := &recordingAuthZ{}
	r := newRouterWithRecordingAuthZ(s, &stubPublisher{}, authz)

	rec := doRequest(r, http.MethodPost, "/v1/postings/reversals",
		domain.CreateReversalPostingRequest{OriginalJournalID: "j-final", Reason: "wrong account"}, "principal-1")
	if rec.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d: %s", rec.Code, rec.Body.String())
	}
	if len(authz.seen) == 0 || authz.seen[0] != "GL_POSTING_REVERSE" {
		t.Fatalf("expected GL_POSTING_REVERSE to gate a reversal, got %v", authz.seen)
	}
	// The reversing journal must be the exact debit/credit inverse.
	var exec domain.PostingExecution
	_ = json.Unmarshal(rec.Body.Bytes(), &exec)
	reversingLines := s.lines[*exec.JournalID]
	if len(reversingLines) != 2 || reversingLines[0].CreditAmount != 100 || reversingLines[1].DebitAmount != 100 {
		t.Fatalf("expected an exact inverse, got %+v", reversingLines)
	}
}

func TestPostAccountingEvent_UsesTheExecuteActionNotTheJournalCreateAction(t *testing.T) {
	authz := &recordingAuthZ{}
	r := newRouterWithRecordingAuthZ(newStubStore(), &stubPublisher{}, authz)

	doRequest(r, http.MethodPost, "/v1/postings/events", validPostEventReq(), "svc-ap")
	if len(authz.seen) == 0 || authz.seen[0] != "GL_POSTING_EXECUTE" {
		t.Fatalf("expected GL_POSTING_EXECUTE, got %v", authz.seen)
	}
}

func TestReprocessFailedPosting_AlreadyCommitted_Refused(t *testing.T) {
	s := newStubStore()
	journalID := "j-1"
	s.postingExecutions["x-1"] = &domain.PostingExecution{
		ExecutionID: "x-1", TenantID: testTenantID, LegalEntityID: "e1",
		Status: domain.PostingExecutionStatusCommitted, JournalID: &journalID,
	}
	r := newRouter(s, &stubPublisher{}, &stubAuthZ{})

	rec := doRequest(r, http.MethodPost, "/v1/postings/x-1/reprocess", nil, "principal-1")
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("expected 422, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestReprocessFailedPosting_FailedWithJournal_RetriesTheCommit(t *testing.T) {
	s := newStubStore()
	journalID := "j-validated"
	s.journals[journalID] = &domain.JournalHeader{
		JournalID: journalID, TenantID: testTenantID, LegalEntityID: "e1",
		FiscalPeriod: "2026-07", Status: domain.JournalStatusValidated,
	}
	reason := "period was locked at commit time"
	s.postingExecutions["x-1"] = &domain.PostingExecution{
		ExecutionID: "x-1", TenantID: testTenantID, LegalEntityID: "e1",
		Status: domain.PostingExecutionStatusFailed, JournalID: &journalID, FailureReason: &reason,
	}
	r := newRouter(s, &stubPublisher{}, &stubAuthZ{})

	rec := doRequest(r, http.MethodPost, "/v1/postings/x-1/reprocess", nil, "principal-1")
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	if s.journals[journalID].Status != domain.JournalStatusFinalized {
		t.Errorf("expected the retry to finalize the journal, got %q", s.journals[journalID].Status)
	}
	if s.postingExecutions["x-1"].Status != domain.PostingExecutionStatusCommitted {
		t.Errorf("expected COMMITTED after a successful reprocess, got %q", s.postingExecutions["x-1"].Status)
	}
}

func TestReprocessFailedPosting_NoJournalEverCreated_RefusesRatherThanGuessing(t *testing.T) {
	s := newStubStore()
	reason := "store was down"
	s.postingExecutions["x-1"] = &domain.PostingExecution{
		ExecutionID: "x-1", TenantID: testTenantID, LegalEntityID: "e1",
		Status: domain.PostingExecutionStatusFailed, FailureReason: &reason,
	}
	r := newRouter(s, &stubPublisher{}, &stubAuthZ{})

	rec := doRequest(r, http.MethodPost, "/v1/postings/x-1/reprocess", nil, "principal-1")
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("expected 422, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestVerifyPostingUniqueness_ReportsWhetherASourceEventAlreadyPosted(t *testing.T) {
	s := newStubStore()
	r := newRouter(s, &stubPublisher{}, &stubAuthZ{})

	before := doRequest(r, http.MethodGet, "/v1/postings/verify-uniqueness?source_event_id=src-evt-1", nil, "svc-ap")
	if before.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", before.Code)
	}
	var body map[string]bool
	_ = json.Unmarshal(before.Body.Bytes(), &body)
	if body["exists"] {
		t.Error("expected exists=false before anything was posted")
	}

	doRequest(r, http.MethodPost, "/v1/postings/events", validPostEventReq(), "svc-ap")

	after := doRequest(r, http.MethodGet, "/v1/postings/verify-uniqueness?source_event_id=src-evt-1", nil, "svc-ap")
	_ = json.Unmarshal(after.Body.Bytes(), &body)
	if !body["exists"] {
		t.Error("expected exists=true once the source event had posted")
	}
}

func TestGetPostingBySource_ReturnsTheExecutionForASourceEvent(t *testing.T) {
	s := newStubStore()
	r := newRouter(s, &stubPublisher{}, &stubAuthZ{})
	doRequest(r, http.MethodPost, "/v1/postings/events", validPostEventReq(), "svc-ap")

	rec := doRequest(r, http.MethodGet, "/v1/postings/by-source?source_event_id=src-evt-1", nil, "svc-ap")
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	var exec domain.PostingExecution
	_ = json.Unmarshal(rec.Body.Bytes(), &exec)
	if exec.SourceEventID == nil || *exec.SourceEventID != "src-evt-1" {
		t.Fatalf("expected the execution for src-evt-1, got %+v", exec)
	}
}

func TestGetPostingExecution_NotFound_Returns404(t *testing.T) {
	r := newRouter(newStubStore(), &stubPublisher{}, &stubAuthZ{})
	rec := doRequest(r, http.MethodGet, "/v1/postings/does-not-exist", nil, "principal-1")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d", rec.Code)
	}
}
