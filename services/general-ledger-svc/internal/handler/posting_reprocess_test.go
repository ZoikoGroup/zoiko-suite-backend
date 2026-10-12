package handler_test

import (
	"encoding/json"
	"errors"
	"net/http"
	"testing"
	"time"

	"zoiko.io/general-ledger-svc/internal/domain"
)

// failBeforeJournal posts an accounting event while the journal write fails —
// the one way an execution ends FAILED with no journal — and returns it.
func failBeforeJournal(t *testing.T, s *stubStore, req domain.PostAccountingEventRequest) *domain.PostingExecution {
	t.Helper()
	s.createErr = errors.New("connection reset by peer")
	rec := doRequest(newRouter(s, &stubPublisher{}, &stubAuthZ{}), http.MethodPost, "/v1/postings/events", req, "svc-ar")
	s.createErr = nil
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("setup: expected the post to fail with 503, got %d: %s", rec.Code, rec.Body.String())
	}
	exec, err := s.GetPostingExecutionBySource(t.Context(), testTenantID, req.SourceEventID)
	if err != nil {
		t.Fatalf("setup: no execution recorded: %v", err)
	}
	if exec.Status != domain.PostingExecutionStatusFailed || exec.JournalID != nil {
		t.Fatalf("setup: want FAILED with no journal, got %s journal=%v", exec.Status, exec.JournalID)
	}
	return exec
}

func journalsForSource(s *stubStore, sourceEventID string) int {
	n := 0
	for _, h := range s.journals {
		if h.SourceEventID != nil && *h.SourceEventID == sourceEventID {
			n++
		}
	}
	return n
}

func reprocess(s *stubStore, executionID string) (int, string) {
	rec := doRequest(newRouter(s, &stubPublisher{}, &stubAuthZ{}), http.MethodPost,
		"/v1/postings/"+executionID+"/reprocess", nil, "accountant-1")
	return rec.Code, rec.Body.String()
}

// The accepted request is kept on the execution, exactly as accepted.
func TestPostAccountingEvent_CapturesTheRequestForReplay(t *testing.T) {
	s := newStubStore()
	req := validPostEventReq()
	rec := doRequest(newRouter(s, &stubPublisher{}, &stubAuthZ{}), http.MethodPost, "/v1/postings/events", req, "svc-ar")
	if rec.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d: %s", rec.Code, rec.Body.String())
	}
	exec, _ := s.GetPostingExecutionBySource(t.Context(), testTenantID, req.SourceEventID)
	var captured domain.PostAccountingEventRequest
	if err := json.Unmarshal(exec.RequestPayload, &captured); err != nil {
		t.Fatalf("captured request does not decode: %v", err)
	}
	if captured.SourceEventID != req.SourceEventID || len(captured.Lines) != 2 || captured.Lines[0].DebitAmount != 100 {
		t.Fatalf("captured %+v, want the accepted request", captured)
	}
	// The PostingDate default is applied before capture, so a replay posts
	// on the same date the original would have.
	if captured.PostingDate != req.DocumentDate {
		t.Fatalf("captured posting date %v, want the defaulted %v", captured.PostingDate, req.DocumentDate)
	}
	if rec.Body.String() != "" && json.Valid(rec.Body.Bytes()) {
		var out map[string]any
		_ = json.Unmarshal(rec.Body.Bytes(), &out)
		if _, leaked := out["request_payload"]; leaked {
			t.Fatal("the captured request must not be exposed in the API response")
		}
	}
}

// The case that used to be a dead end: failed before any journal, so reprocess
// refused it and resubmission returned the prior FAILED result.
func TestReprocess_FailedBeforeJournal_ReplaysAndCommits(t *testing.T) {
	s := newStubStore()
	req := validPostEventReq()
	exec := failBeforeJournal(t, s, req)

	code, body := reprocess(s, exec.ExecutionID)
	if code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", code, body)
	}
	got := s.postingExecutions[exec.ExecutionID]
	if got.Status != domain.PostingExecutionStatusCommitted || got.JournalID == nil {
		t.Fatalf("want COMMITTED with a journal, got %s %v", got.Status, got.JournalID)
	}
	if s.journals[*got.JournalID].Status != domain.JournalStatusFinalized {
		t.Fatalf("replayed journal is %s, want FINALIZED", s.journals[*got.JournalID].Status)
	}
	if n := journalsForSource(s, req.SourceEventID); n != 1 {
		t.Fatalf("source event posted %d times, want exactly once", n)
	}

	// Duplicate source event now returns the committed result.
	rec := doRequest(newRouter(s, &stubPublisher{}, &stubAuthZ{}), http.MethodPost, "/v1/postings/events", req, "svc-ar")
	var again domain.PostingExecution
	_ = json.Unmarshal(rec.Body.Bytes(), &again)
	if again.Status != domain.PostingExecutionStatusCommitted {
		t.Fatalf("resubmission after a successful replay should return COMMITTED, got %s", again.Status)
	}
}

// The usual root cause is a mapping that did not exist; the replay must use
// the mapping as it is NOW, and record that resolution as its evidence.
func TestReprocess_ReplayUsesCurrentMappingsAndRecordsTrace(t *testing.T) {
	s := newStubStore()
	req := validPostEventReq()
	key := "REVENUE:SUBSCRIPTIONS"
	req.Lines[1].AccountCode = nil
	req.Lines[1].MappingKey = &key
	s.accountsByCode["t1|4100-Subs"] = &domain.Account{AccountCode: "4100-Subs", Status: "ACTIVE"}
	s.currentMappings["t1|"+key] = &domain.AccountMapping{TenantID: "t1", MappingKey: key, AccountCode: "4100-Subs"}
	exec := failBeforeJournal(t, s, req)

	// Remapped between the failure and the replay.
	s.accountsByCode["t1|4200-Subs-New"] = &domain.Account{AccountCode: "4200-Subs-New", Status: "ACTIVE"}
	s.currentMappings["t1|"+key] = &domain.AccountMapping{TenantID: "t1", MappingKey: key, AccountCode: "4200-Subs-New"}

	if code, body := reprocess(s, exec.ExecutionID); code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", code, body)
	}
	got := s.postingExecutions[exec.ExecutionID]
	lines := s.lines[*got.JournalID]
	if lines[1].AccountCode != "4200-Subs-New" {
		t.Fatalf("replay posted to %s, want the current mapping 4200-Subs-New", lines[1].AccountCode)
	}
	if !containsAny(got.CalculationTrace, "4200-Subs-New") {
		t.Fatalf("calculation trace %q does not record the resolution that posted", got.CalculationTrace)
	}
}

// If the failed attempt's journal did commit (acknowledgement lost), replaying
// blindly would post the fact twice. It is adopted instead.
func TestReprocess_AdoptsAJournalTheFailedAttemptDidWrite(t *testing.T) {
	s := newStubStore()
	req := validPostEventReq()
	exec := failBeforeJournal(t, s, req)
	src := req.SourceEventID
	s.journals["j-orphan"] = &domain.JournalHeader{
		JournalID: "j-orphan", TenantID: testTenantID, LegalEntityID: req.LegalEntityID,
		FiscalPeriod: req.FiscalPeriod, Status: domain.JournalStatusPending, SourceEventID: &src,
		ApprovalStatus: domain.ApprovalStatusPostingRequested,
	}

	if code, body := reprocess(s, exec.ExecutionID); code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", code, body)
	}
	if n := journalsForSource(s, src); n != 1 {
		t.Fatalf("source event now has %d journals, want the one that already existed", n)
	}
	got := s.postingExecutions[exec.ExecutionID]
	if got.JournalID == nil || *got.JournalID != "j-orphan" {
		t.Fatalf("execution linked to %v, want j-orphan", got.JournalID)
	}
	if s.journals["j-orphan"].Status != domain.JournalStatusFinalized {
		t.Fatalf("adopted journal is %s, want FINALIZED", s.journals["j-orphan"].Status)
	}
}

func TestReprocess_SeveralExistingJournalsIsRefused(t *testing.T) {
	s := newStubStore()
	req := validPostEventReq()
	exec := failBeforeJournal(t, s, req)
	src := req.SourceEventID
	for _, id := range []string{"j-a", "j-b"} {
		s.journals[id] = &domain.JournalHeader{JournalID: id, TenantID: testTenantID, SourceEventID: &src, Status: domain.JournalStatusPending}
	}
	code, body := reprocess(s, exec.ExecutionID)
	if code != http.StatusConflict {
		t.Fatalf("expected 409, got %d: %s", code, body)
	}
	if got := s.postingExecutions[exec.ExecutionID]; got.Status != domain.PostingExecutionStatusFailed {
		t.Fatalf("a refused replay must leave the execution FAILED (still in the backlog), got %s", got.Status)
	}
	if n := journalsForSource(s, src); n != 2 {
		t.Fatalf("a refused replay must post nothing, journals now %d", n)
	}
}

// The cause is not fixed yet: the replay fails cleanly and the execution goes
// back to FAILED with the new reason — it stays in the posting backlog.
func TestReprocess_StillBrokenGoesBackToFailedWithTheNewReason(t *testing.T) {
	s := newStubStore()
	req := validPostEventReq()
	exec := failBeforeJournal(t, s, req)
	s.createErr = errors.New("disk full")

	code, _ := reprocess(s, exec.ExecutionID)
	if code != http.StatusServiceUnavailable {
		t.Fatalf("expected 503, got %d", code)
	}
	got := s.postingExecutions[exec.ExecutionID]
	if got.Status != domain.PostingExecutionStatusFailed || got.FailureReason == nil || *got.FailureReason != "disk full" {
		t.Fatalf("want FAILED with reason %q, got %s %v", "disk full", got.Status, got.FailureReason)
	}
}

// Two reprocesses at once: only one may replay, or the fact posts twice.
func TestReprocess_ConcurrentClaimIsRefused(t *testing.T) {
	s := newStubStore()
	exec := failBeforeJournal(t, s, validPostEventReq())
	s.postingExecutions[exec.ExecutionID].Status = domain.PostingExecutionStatusValidating
	s.claimedAt[exec.ExecutionID] = time.Now().UTC()

	if code, body := reprocess(s, exec.ExecutionID); code != http.StatusConflict {
		t.Fatalf("expected 409 while another reprocess holds the claim, got %d: %s", code, body)
	}
	if len(s.journals) != 0 {
		t.Fatal("a refused claim must post nothing")
	}
}

// A claim abandoned by a crashed process is taken over after the timeout.
func TestReprocess_StaleClaimIsTakenOver(t *testing.T) {
	s := newStubStore()
	exec := failBeforeJournal(t, s, validPostEventReq())
	s.postingExecutions[exec.ExecutionID].Status = domain.PostingExecutionStatusValidating
	s.claimedAt[exec.ExecutionID] = time.Now().UTC().Add(-time.Hour)

	if code, body := reprocess(s, exec.ExecutionID); code != http.StatusOK {
		t.Fatalf("expected the stale claim to be taken over, got %d: %s", code, body)
	}
}

func containsAny(s string, subs ...string) bool {
	for _, sub := range subs {
		if len(sub) > 0 && len(s) >= len(sub) {
			for i := 0; i+len(sub) <= len(s); i++ {
				if s[i:i+len(sub)] == sub {
					return true
				}
			}
		}
	}
	return false
}
