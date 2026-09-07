package handler_test

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/go-chi/chi/v5"
	"go.uber.org/zap"

	"zoiko.io/general-ledger-svc/internal/domain"
	"zoiko.io/general-ledger-svc/internal/handler"
	svcmiddleware "zoiko.io/general-ledger-svc/internal/middleware"
)

// ── ACC-03 (Journal Entry: proposal/approval lifecycle) ─────────────────────

// createDraftJournal POSTs a valid journal and returns its id. Every new
// journal starts DRAFT — see insertJournal's own fallback.
func createDraftJournal(t *testing.T, r chi.Router) string {
	t.Helper()
	rec := doRequest(r, http.MethodPost, "/v1/journals/", validCreateReq(), "preparer-1")
	if rec.Code != http.StatusCreated {
		t.Fatalf("create failed: %d %s", rec.Code, rec.Body.String())
	}
	var got domain.JournalWithLines
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.ApprovalStatus != domain.ApprovalStatusDraft {
		t.Fatalf("expected a new journal to start DRAFT, got %q", got.ApprovalStatus)
	}
	return got.JournalID
}

// submitAndApprove drives a DRAFT journal to APPROVED with two distinct
// principals — SoD is universal in this v1 (see
// domain.ErrSelfApprovalNotPermitted's own doc comment), so a helper that
// used the SAME principal for both steps would build fixtures no real
// approval could ever reach.
func submitAndApprove(t *testing.T, r chi.Router, journalID string) {
	t.Helper()
	if rec := doRequest(r, http.MethodPost, "/v1/journals/"+journalID+"/submit", nil, "preparer-1"); rec.Code != http.StatusOK {
		t.Fatalf("submit failed: %d %s", rec.Code, rec.Body.String())
	}
	if rec := doRequest(r, http.MethodPost, "/v1/journals/"+journalID+"/approve", nil, "approver-1"); rec.Code != http.StatusOK {
		t.Fatalf("approve failed: %d %s", rec.Code, rec.Body.String())
	}
}

func TestJournalLifecycle_DraftSubmitApproveRequestPosting(t *testing.T) {
	s := newStubStore()
	r := newRouter(s, &stubPublisher{}, &stubAuthZ{})
	journalID := createDraftJournal(t, r)
	submitAndApprove(t, r, journalID)

	rec := doRequest(r, http.MethodPost, "/v1/journals/"+journalID+"/request-posting", nil, "approver-1")
	if rec.Code != http.StatusOK {
		t.Fatalf("request-posting failed: %d %s", rec.Code, rec.Body.String())
	}
	if s.journals[journalID].ApprovalStatus != domain.ApprovalStatusPostingRequested {
		t.Errorf("expected POSTING_REQUESTED, got %q", s.journals[journalID].ApprovalStatus)
	}
}

// The spec's own negative path #1 (verbatim): "Debit/credit imbalance."
// Caught at SubmitJournal — before an approver's time is spent on a
// proposal that could never post — not only at ledger-validate time.
func TestSubmitJournal_UnbalancedLines_Returns422AndStaysDraft(t *testing.T) {
	s := newStubStore()
	s.debitTotal, s.creditTotal = 10000, 9000
	r := newRouter(s, &stubPublisher{}, &stubAuthZ{})
	journalID := createDraftJournal(t, r)

	rec := doRequest(r, http.MethodPost, "/v1/journals/"+journalID+"/submit", nil, "preparer-1")
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("expected 422, got %d: %s", rec.Code, rec.Body.String())
	}
	if s.journals[journalID].ApprovalStatus != domain.ApprovalStatusDraft {
		t.Errorf("an unbalanced submit must leave the journal DRAFT, got %q", s.journals[journalID].ApprovalStatus)
	}
}

// The spec's own negative path #2 (verbatim): "Journal changed after
// approval." Satisfied structurally — AmendDraftJournal has no status it
// succeeds from once APPROVED.
func TestAmendDraftJournal_AfterApproval_Refused(t *testing.T) {
	s := newStubStore()
	r := newRouter(s, &stubPublisher{}, &stubAuthZ{})
	journalID := createDraftJournal(t, r)
	submitAndApprove(t, r, journalID)

	req := validCreateReq()
	amendReq := domain.AmendDraftJournalRequest{
		Description: "changed after approval", JournalType: req.JournalType,
		TransactionDate: req.TransactionDate, PostingDate: req.PostingDate, CurrencyCode: req.CurrencyCode,
		Lines: req.Lines,
	}
	rec := doRequest(r, http.MethodPost, "/v1/journals/"+journalID+"/amend", amendReq, "preparer-1")
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("expected 422, got %d: %s", rec.Code, rec.Body.String())
	}
	if s.journals[journalID].ApprovalStatus != domain.ApprovalStatusApproved {
		t.Errorf("the journal must be left untouched, got %q", s.journals[journalID].ApprovalStatus)
	}
}

// AmendDraftJournal on a PENDING_APPROVAL journal is legal — but withdraws
// the pending approval back to DRAFT, per ValidApprovalTransitions's own
// doc comment: editing invalidates a stale approval request rather than
// leaving one pending against content an approver never actually saw.
func TestAmendDraftJournal_WhilePendingApproval_WithdrawsToDraft(t *testing.T) {
	s := newStubStore()
	r := newRouter(s, &stubPublisher{}, &stubAuthZ{})
	journalID := createDraftJournal(t, r)
	doRequest(r, http.MethodPost, "/v1/journals/"+journalID+"/submit", nil, "preparer-1")

	req := validCreateReq()
	amendReq := domain.AmendDraftJournalRequest{
		Description: "caught a mistake before approval", JournalType: req.JournalType,
		TransactionDate: req.TransactionDate, PostingDate: req.PostingDate, CurrencyCode: req.CurrencyCode,
		Lines: req.Lines,
	}
	rec := doRequest(r, http.MethodPost, "/v1/journals/"+journalID+"/amend", amendReq, "preparer-1")
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	if s.journals[journalID].ApprovalStatus != domain.ApprovalStatusDraft {
		t.Errorf("expected the amend to withdraw approval back to DRAFT, got %q", s.journals[journalID].ApprovalStatus)
	}
}

// The spec's own negative path #3 (verbatim): "Preparer self-approves
// protected journal." No journal-class config exists, so this v1 makes
// maker/checker universal.
func TestApproveJournal_SamePrincipalAsSubmitter_Returns403(t *testing.T) {
	s := newStubStore()
	r := newRouter(s, &stubPublisher{}, &stubAuthZ{})
	journalID := createDraftJournal(t, r)
	doRequest(r, http.MethodPost, "/v1/journals/"+journalID+"/submit", nil, "preparer-1")

	rec := doRequest(r, http.MethodPost, "/v1/journals/"+journalID+"/approve", nil, "preparer-1")
	if rec.Code != http.StatusForbidden {
		t.Fatalf("expected 403, got %d: %s", rec.Code, rec.Body.String())
	}
	if s.journals[journalID].ApprovalStatus != domain.ApprovalStatusPendingApproval {
		t.Errorf("a refused self-approval must leave the journal PENDING_APPROVAL, got %q", s.journals[journalID].ApprovalStatus)
	}
}

func TestApproveJournal_DifferentPrincipal_SucceedsAndRecordsFingerprint(t *testing.T) {
	s := newStubStore()
	r := newRouter(s, &stubPublisher{}, &stubAuthZ{})
	journalID := createDraftJournal(t, r)
	submitAndApprove(t, r, journalID)

	h := s.journals[journalID]
	if h.ApprovalStatus != domain.ApprovalStatusApproved {
		t.Fatalf("expected APPROVED, got %q", h.ApprovalStatus)
	}
	if h.ApprovalFingerprint == nil || *h.ApprovalFingerprint == "" {
		t.Error("expected a real approval fingerprint to be recorded")
	}
	if h.ApprovedByPrincipalID == nil || *h.ApprovedByPrincipalID != "approver-1" {
		t.Errorf("expected approver-1 recorded as approver, got %v", h.ApprovedByPrincipalID)
	}
}

// The spec's own negative path #4 (verbatim): "Attempt edit after
// posting." Satisfied structurally by the same AmendDraftJournal guard —
// POSTED is not DRAFT or PENDING_APPROVAL.
func TestAmendDraftJournal_AfterPosting_Refused(t *testing.T) {
	s := newStubStore()
	r := newRouter(s, &stubPublisher{}, &stubAuthZ{})
	journalID := createDraftJournal(t, r)
	submitAndApprove(t, r, journalID)
	doRequest(r, http.MethodPost, "/v1/journals/"+journalID+"/request-posting", nil, "approver-1")
	// ACC-03's own governance (Draft->...->PostingRequested) is a
	// SEPARATE lifecycle from the ledger's own Tri-Phase Commit — a
	// journal must still be independently VALIDATED before ACC-04 will
	// finalize it.
	if rec := doRequest(r, http.MethodPost, "/v1/journals/"+journalID+"/validate", nil, "svc-ap"); rec.Code != http.StatusOK {
		t.Fatalf("validate failed: %d %s", rec.Code, rec.Body.String())
	}

	// Drive the journal all the way to FINALIZED/POSTED through the real
	// posting engine, proving the ACC-03/ACC-04 integration end to end.
	rec := doRequest(r, http.MethodPost, "/v1/postings/journals",
		domain.PostApprovedJournalRequest{JournalID: journalID}, "svc-ap")
	if rec.Code != http.StatusOK {
		t.Fatalf("PostApprovedJournal failed: %d %s", rec.Code, rec.Body.String())
	}
	if s.journals[journalID].ApprovalStatus != domain.ApprovalStatusPosted {
		t.Fatalf("expected POSTED, got %q", s.journals[journalID].ApprovalStatus)
	}

	req := validCreateReq()
	amendReq := domain.AmendDraftJournalRequest{
		Description: "attempted edit after posting", JournalType: req.JournalType,
		TransactionDate: req.TransactionDate, PostingDate: req.PostingDate, CurrencyCode: req.CurrencyCode,
		Lines: req.Lines,
	}
	amendRec := doRequest(r, http.MethodPost, "/v1/journals/"+journalID+"/amend", amendReq, "preparer-1")
	if amendRec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("expected 422, got %d: %s", amendRec.Code, amendRec.Body.String())
	}
}

func TestRejectJournal_RequiresReason(t *testing.T) {
	s := newStubStore()
	r := newRouter(s, &stubPublisher{}, &stubAuthZ{})
	journalID := createDraftJournal(t, r)
	doRequest(r, http.MethodPost, "/v1/journals/"+journalID+"/submit", nil, "preparer-1")

	rec := doRequest(r, http.MethodPost, "/v1/journals/"+journalID+"/reject", domain.RejectJournalRequest{}, "approver-1")
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestRejectJournal_WithReason_IsTerminal(t *testing.T) {
	s := newStubStore()
	r := newRouter(s, &stubPublisher{}, &stubAuthZ{})
	journalID := createDraftJournal(t, r)
	doRequest(r, http.MethodPost, "/v1/journals/"+journalID+"/submit", nil, "preparer-1")

	rec := doRequest(r, http.MethodPost, "/v1/journals/"+journalID+"/reject",
		domain.RejectJournalRequest{Reason: "wrong accounts"}, "approver-1")
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	if s.journals[journalID].ApprovalStatus != domain.ApprovalStatusRejected {
		t.Errorf("expected REJECTED, got %q", s.journals[journalID].ApprovalStatus)
	}
	// Terminal: nothing can move it further.
	submitAgain := doRequest(r, http.MethodPost, "/v1/journals/"+journalID+"/submit", nil, "preparer-1")
	if submitAgain.Code != http.StatusUnprocessableEntity {
		t.Errorf("expected a rejected journal to refuse re-submission, got %d", submitAgain.Code)
	}
}

func TestRequestCorrection_OnlyFromPostedJournal(t *testing.T) {
	s := newStubStore()
	r := newRouter(s, &stubPublisher{}, &stubAuthZ{})
	journalID := createDraftJournal(t, r) // still DRAFT, never posted

	req := validCreateReq()
	correctionReq := domain.RequestCorrectionRequest{Reason: "wrong amount", Description: "correction", Lines: req.Lines}
	rec := doRequest(r, http.MethodPost, "/v1/journals/"+journalID+"/correct", correctionReq, "preparer-1")
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("expected 422, got %d: %s", rec.Code, rec.Body.String())
	}
}

// "Corrections create new journals" (verbatim state model) — never an
// in-place edit of the posted original.
func TestRequestCorrection_FromPostedJournal_CreatesNewDraftWithCorrectionChain(t *testing.T) {
	s := newStubStore()
	r := newRouter(s, &stubPublisher{}, &stubAuthZ{})
	journalID := createDraftJournal(t, r)
	submitAndApprove(t, r, journalID)
	doRequest(r, http.MethodPost, "/v1/journals/"+journalID+"/request-posting", nil, "approver-1")
	if rec := doRequest(r, http.MethodPost, "/v1/journals/"+journalID+"/validate", nil, "svc-ap"); rec.Code != http.StatusOK {
		t.Fatalf("validate failed: %d %s", rec.Code, rec.Body.String())
	}
	postRec := doRequest(r, http.MethodPost, "/v1/postings/journals", domain.PostApprovedJournalRequest{JournalID: journalID}, "svc-ap")
	if postRec.Code != http.StatusOK {
		t.Fatalf("PostApprovedJournal failed: %d %s", postRec.Code, postRec.Body.String())
	}

	req := validCreateReq()
	correctionReq := domain.RequestCorrectionRequest{Reason: "wrong amount", Description: "correction", Lines: req.Lines}
	rec := doRequest(r, http.MethodPost, "/v1/journals/"+journalID+"/correct", correctionReq, "preparer-1")
	if rec.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d: %s", rec.Code, rec.Body.String())
	}
	var correction domain.JournalHeader
	if err := json.Unmarshal(rec.Body.Bytes(), &correction); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if correction.JournalID == journalID {
		t.Fatal("a correction must be a NEW journal, not the original")
	}
	if correction.CorrectionOfJournalID == nil || *correction.CorrectionOfJournalID != journalID {
		t.Errorf("expected correction_of_journal_id to point at the original, got %v", correction.CorrectionOfJournalID)
	}
	if correction.ApprovalStatus != domain.ApprovalStatusDraft {
		t.Errorf("expected the correction to start DRAFT and go through the full lifecycle again, got %q", correction.ApprovalStatus)
	}
	// The original is untouched — never edited in place.
	if s.journals[journalID].ApprovalStatus != domain.ApprovalStatusPosted {
		t.Errorf("the original must remain POSTED, unedited, got %q", s.journals[journalID].ApprovalStatus)
	}
}

func TestGetAvailableActions_ReflectsRealTransitions(t *testing.T) {
	s := newStubStore()
	r := newRouter(s, &stubPublisher{}, &stubAuthZ{})
	journalID := createDraftJournal(t, r)

	rec := doRequest(r, http.MethodGet, "/v1/journals/"+journalID+"/available-actions", nil, "preparer-1")
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	var actions domain.AvailableActions
	_ = json.Unmarshal(rec.Body.Bytes(), &actions)
	foundAmend, foundSubmit := false, false
	for _, a := range actions.Actions {
		if a == "amend" {
			foundAmend = true
		}
		if a == "submit" {
			foundSubmit = true
		}
	}
	if !foundAmend || !foundSubmit {
		t.Errorf("expected DRAFT to offer amend and submit, got %v", actions.Actions)
	}
}

func TestGetJournalHistory_BuildsFromRealTimestamps(t *testing.T) {
	s := newStubStore()
	r := newRouter(s, &stubPublisher{}, &stubAuthZ{})
	journalID := createDraftJournal(t, r)
	submitAndApprove(t, r, journalID)

	rec := doRequest(r, http.MethodGet, "/v1/journals/"+journalID+"/history", nil, "preparer-1")
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	var history []domain.JournalHistoryEntry
	_ = json.Unmarshal(rec.Body.Bytes(), &history)
	events := map[string]bool{}
	for _, e := range history {
		events[e.Event] = true
	}
	for _, want := range []string{"created", "submitted", "approved"} {
		if !events[want] {
			t.Errorf("expected history to include %q, got %+v", want, history)
		}
	}
	// Not-yet-happened events must not be fabricated.
	if events["rejected"] || events["posted"] {
		t.Errorf("history must not include events that never happened, got %+v", history)
	}
}

// newRouterWithRecordingAuthZACC03 lets a test assert the exact action
// name checked, same pattern as posting_engine_test.go's own helper.
func newRouterWithRecordingAuthZACC03(s *stubStore, authz *recordingAuthZ) chi.Router {
	r := chi.NewRouter()
	r.Use(svcmiddleware.TenantContext())
	h := handler.New(s, &stubPublisher{}, authz, &stubClose{}, zap.NewNop())
	handler.RegisterRoutes(r, h)
	return r
}

func TestApproveJournal_UsesDistinctAuthorizationActionFromSubmit(t *testing.T) {
	authz := &recordingAuthZ{}
	s := newStubStore()
	r := newRouterWithRecordingAuthZACC03(s, authz)
	journalID := createDraftJournal(t, r)
	doRequest(r, http.MethodPost, "/v1/journals/"+journalID+"/submit", nil, "preparer-1")
	authz.seen = nil // only care about ApproveJournal's own check

	doRequest(r, http.MethodPost, "/v1/journals/"+journalID+"/approve", nil, "approver-1")
	if len(authz.seen) == 0 || authz.seen[0] != "GL_JOURNAL_APPROVE" {
		t.Fatalf("expected GL_JOURNAL_APPROVE, got %v", authz.seen)
	}
}
