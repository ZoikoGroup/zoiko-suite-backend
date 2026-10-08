package handler_test

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"zoiko.io/financial-close-svc/internal/domain"
)

// newOpenPeriod builds a period in OPEN state with full bank/subledger
// gates already satisfied, so hard-close tests only exercise the state
// machine, not the readiness checks covered elsewhere.
func newOpenPeriod(s *stubStore, id string) *domain.FiscalPeriod {
	fp := &domain.FiscalPeriod{
		FiscalPeriodID: id, TenantID: testTenantID, LegalEntityID: "le-1",
		PeriodName: "2026-10", PeriodStart: octStart, PeriodEnd: octEnd, CloseStatus: domain.PeriodOpen,
	}
	s.periods[id] = fp
	bothMatched(s)
	return fp
}

func closeClients() *stubClients {
	return &stubClients{bankAccounts: []domain.BankAccountRef{barclays},
		bankRecon: []domain.BankAccountReconStatus{certified("acct-barclays", "2026-10-30")}}
}

// ── The six-state machine ────────────────────────────────────────────────

func TestPeriodStateMachine_FullHappyPath(t *testing.T) {
	s := newStubStore()
	newOpenPeriod(s, "fp-1")
	cl := closeClients()
	r := newGatedRouter(s, cl)

	// OPEN -> SOFT_CLOSE
	rr := doReq(r, http.MethodPost, "/v1/close/periods/fp-1/soft-close", nil, "accountant-1")
	if rr.Code != http.StatusOK || s.periods["fp-1"].CloseStatus != domain.PeriodSoftClose {
		t.Fatalf("soft-close: %d %s", rr.Code, rr.Body.String())
	}

	// SOFT_CLOSE -> CLOSE_REVIEW
	rr = doReq(r, http.MethodPost, "/v1/close/periods/fp-1/close-review", nil, "accountant-1")
	if rr.Code != http.StatusOK || s.periods["fp-1"].CloseStatus != domain.PeriodCloseReview {
		t.Fatalf("close-review: %d %s", rr.Code, rr.Body.String())
	}

	// CLOSE_REVIEW -> HARD_CLOSED
	rr = doReq(r, http.MethodPost, "/v1/close/periods/fp-1/hard-close", nil, "controller-1")
	if rr.Code != http.StatusOK || s.periods["fp-1"].CloseStatus != domain.PeriodHardClosed {
		t.Fatalf("hard-close: %d %s", rr.Code, rr.Body.String())
	}

	// Requesting and approving a reopen moves to AUTHORIZED_REOPEN.
	until := time.Now().UTC().Add(48 * time.Hour)
	rr = doReq(r, http.MethodPost, "/v1/close/periods/fp-1/reopen-requests",
		domain.RequestReopenRequest{Reason: "misposting found", ReopenUntil: until}, "accountant-1")
	if rr.Code != http.StatusCreated {
		t.Fatalf("reopen request: %d %s", rr.Code, rr.Body.String())
	}
	var reqResp domain.ReopenRequest
	_ = json.Unmarshal(rr.Body.Bytes(), &reqResp)

	rr = doReq(r, http.MethodPost, "/v1/close/reopen-requests/"+reqResp.RequestID+"/approve", nil, "cfo-1")
	if rr.Code != http.StatusOK || s.periods["fp-1"].CloseStatus != domain.PeriodAuthorizedReopen {
		t.Fatalf("approve reopen: %d %s", rr.Code, rr.Body.String())
	}

	// AUTHORIZED_REOPEN -> RECLOSED, reperformed after the reopen.
	at := s.periods["fp-1"].ReopenedAt.Add(time.Hour)
	bothMatchedAt(s, at)
	cl.bankRecon = []domain.BankAccountReconStatus{certifiedAt("acct-barclays", "2026-10-30", at)}
	rr = doReq(r, http.MethodPost, "/v1/close/periods/fp-1/reclose", nil, "controller-1")
	if rr.Code != http.StatusOK || s.periods["fp-1"].CloseStatus != domain.PeriodReclosed {
		t.Fatalf("reclose: %d %s", rr.Code, rr.Body.String())
	}
}

func TestPeriodStateMachine_OutOfOrderTransitionsRefused(t *testing.T) {
	s := newStubStore()
	newOpenPeriod(s, "fp-1")
	r := newGatedRouter(s, closeClients())

	// Cannot enter close-review before soft-close.
	rr := doReq(r, http.MethodPost, "/v1/close/periods/fp-1/close-review", nil, "accountant-1")
	if rr.Code != http.StatusConflict {
		t.Fatalf("close-review from OPEN: got %d, want 409", rr.Code)
	}
	// Cannot hard-close an OPEN period.
	rr = doReq(r, http.MethodPost, "/v1/close/periods/fp-1/hard-close", nil, "controller-1")
	if rr.Code != http.StatusConflict {
		t.Fatalf("hard-close from OPEN: got %d, want 409", rr.Code)
	}
	if s.periods["fp-1"].CloseStatus != domain.PeriodOpen {
		t.Fatal("a refused transition must not move the period")
	}
}

// The legacy /lock endpoint is hard close: it still requires CLOSE_REVIEW.
func TestLegacyLockEndpoint_IsHardClose(t *testing.T) {
	s := newStubStore()
	newOpenPeriod(s, "fp-1")
	s.periods["fp-1"].CloseStatus = domain.PeriodCloseReview
	r := newGatedRouter(s, closeClients())
	rr := doReq(r, http.MethodPost, "/v1/close/periods/fp-1/lock", nil, "controller-1")
	if rr.Code != http.StatusOK || s.periods["fp-1"].CloseStatus != domain.PeriodHardClosed {
		t.Fatalf("legacy lock: %d %s", rr.Code, rr.Body.String())
	}
}

// ── The old one-step reopen is retired ──────────────────────────────────

func TestRetiredReopen_Returns410(t *testing.T) {
	s := newStubStore()
	newOpenPeriod(s, "fp-1")
	s.periods["fp-1"].CloseStatus = domain.PeriodHardClosed
	r := newGatedRouter(s, closeClients())
	rr := doReq(r, http.MethodPost, "/v1/close/periods/fp-1/reopen", domain.ReopenPeriodRequest{Reason: "x"}, "accountant-1")
	if rr.Code != http.StatusGone {
		t.Fatalf("got %d, want 410", rr.Code)
	}
	if s.periods["fp-1"].CloseStatus != domain.PeriodHardClosed {
		t.Fatal("the retired endpoint must not reopen anything")
	}
}

// ── Reopen: two-person approval ──────────────────────────────────────────

func hardClosedPeriod(s *stubStore, id string) {
	newOpenPeriod(s, id)
	s.periods[id].CloseStatus = domain.PeriodHardClosed
}

func TestReopenRequest_RequiresReasonAndBoundedWindow(t *testing.T) {
	s := newStubStore()
	hardClosedPeriod(s, "fp-1")
	r := newGatedRouter(s, closeClients())

	cases := []struct {
		name string
		req  domain.RequestReopenRequest
	}{
		{"no reason", domain.RequestReopenRequest{ReopenUntil: time.Now().Add(24 * time.Hour)}},
		{"window in the past", domain.RequestReopenRequest{Reason: "x", ReopenUntil: time.Now().Add(-time.Hour)}},
		{"window beyond 30 days", domain.RequestReopenRequest{Reason: "x", ReopenUntil: time.Now().Add(31 * 24 * time.Hour)}},
	}
	for _, c := range cases {
		rr := doReq(r, http.MethodPost, "/v1/close/periods/fp-1/reopen-requests", c.req, "accountant-1")
		if rr.Code != http.StatusBadRequest {
			t.Errorf("%s: got %d, want 400", c.name, rr.Code)
		}
	}
	if len(s.reopenRequests) != 0 {
		t.Fatal("an invalid request was stored")
	}
}

func TestReopenRequest_OnlyFromClosedStates(t *testing.T) {
	s := newStubStore()
	newOpenPeriod(s, "fp-1") // still OPEN
	r := newGatedRouter(s, closeClients())
	rr := doReq(r, http.MethodPost, "/v1/close/periods/fp-1/reopen-requests",
		domain.RequestReopenRequest{Reason: "x", ReopenUntil: time.Now().Add(24 * time.Hour)}, "accountant-1")
	if rr.Code != http.StatusConflict {
		t.Fatalf("got %d, want 409", rr.Code)
	}
}

func TestReopenRequest_OnlyOnePending(t *testing.T) {
	s := newStubStore()
	hardClosedPeriod(s, "fp-1")
	r := newGatedRouter(s, closeClients())
	first := domain.RequestReopenRequest{Reason: "first", ReopenUntil: time.Now().Add(24 * time.Hour)}
	if rr := doReq(r, http.MethodPost, "/v1/close/periods/fp-1/reopen-requests", first, "accountant-1"); rr.Code != http.StatusCreated {
		t.Fatalf("first request: %d", rr.Code)
	}
	second := domain.RequestReopenRequest{Reason: "second", ReopenUntil: time.Now().Add(24 * time.Hour)}
	rr := doReq(r, http.MethodPost, "/v1/close/periods/fp-1/reopen-requests", second, "accountant-1")
	if rr.Code != http.StatusConflict {
		t.Fatalf("second pending request: got %d, want 409", rr.Code)
	}
}

// The spec's negative path #3: a requester cannot approve their own request.
func TestApproveReopen_RequesterCannotApproveOwnRequest(t *testing.T) {
	s := newStubStore()
	hardClosedPeriod(s, "fp-1")
	r := newGatedRouter(s, closeClients())
	rr := doReq(r, http.MethodPost, "/v1/close/periods/fp-1/reopen-requests",
		domain.RequestReopenRequest{Reason: "x", ReopenUntil: time.Now().Add(24 * time.Hour)}, "accountant-1")
	var req domain.ReopenRequest
	_ = json.Unmarshal(rr.Body.Bytes(), &req)

	self := doReq(r, http.MethodPost, "/v1/close/reopen-requests/"+req.RequestID+"/approve", nil, "accountant-1")
	if self.Code != http.StatusForbidden {
		t.Fatalf("self-approval: got %d, want 403", self.Code)
	}
	if s.periods["fp-1"].CloseStatus != domain.PeriodHardClosed {
		t.Fatal("self-approval must not reopen the period")
	}

	other := doReq(r, http.MethodPost, "/v1/close/reopen-requests/"+req.RequestID+"/approve", nil, "cfo-1")
	if other.Code != http.StatusOK || s.periods["fp-1"].CloseStatus != domain.PeriodAuthorizedReopen {
		t.Fatalf("approval by a different principal: %d %s", other.Code, other.Body.String())
	}
}

func TestRejectReopen_RequiresReasonAndLeavesPeriodClosed(t *testing.T) {
	s := newStubStore()
	hardClosedPeriod(s, "fp-1")
	r := newGatedRouter(s, closeClients())
	rr := doReq(r, http.MethodPost, "/v1/close/periods/fp-1/reopen-requests",
		domain.RequestReopenRequest{Reason: "x", ReopenUntil: time.Now().Add(24 * time.Hour)}, "accountant-1")
	var req domain.ReopenRequest
	_ = json.Unmarshal(rr.Body.Bytes(), &req)

	noReason := doReq(r, http.MethodPost, "/v1/close/reopen-requests/"+req.RequestID+"/reject", nil, "cfo-1")
	if noReason.Code != http.StatusBadRequest {
		t.Fatalf("reject without reason: got %d, want 400", noReason.Code)
	}
	rejected := doReq(r, http.MethodPost, "/v1/close/reopen-requests/"+req.RequestID+"/reject",
		domain.DecideReopenRequest{Reason: "not material enough"}, "cfo-1")
	if rejected.Code != http.StatusOK || s.periods["fp-1"].CloseStatus != domain.PeriodHardClosed {
		t.Fatalf("reject: %d %s", rejected.Code, rejected.Body.String())
	}

	// A new request can be made after rejection.
	again := doReq(r, http.MethodPost, "/v1/close/periods/fp-1/reopen-requests",
		domain.RequestReopenRequest{Reason: "y", ReopenUntil: time.Now().Add(24 * time.Hour)}, "accountant-1")
	if again.Code != http.StatusCreated {
		t.Fatalf("new request after rejection: %d", again.Code)
	}
}

func TestApproveReopen_AlreadyDecidedIsRefused(t *testing.T) {
	s := newStubStore()
	hardClosedPeriod(s, "fp-1")
	r := newGatedRouter(s, closeClients())
	rr := doReq(r, http.MethodPost, "/v1/close/periods/fp-1/reopen-requests",
		domain.RequestReopenRequest{Reason: "x", ReopenUntil: time.Now().Add(24 * time.Hour)}, "accountant-1")
	var req domain.ReopenRequest
	_ = json.Unmarshal(rr.Body.Bytes(), &req)
	doReq(r, http.MethodPost, "/v1/close/reopen-requests/"+req.RequestID+"/approve", nil, "cfo-1")

	twice := doReq(r, http.MethodPost, "/v1/close/reopen-requests/"+req.RequestID+"/approve", nil, "cfo-2")
	if twice.Code != http.StatusConflict {
		t.Fatalf("approving an already-decided request: got %d, want 409", twice.Code)
	}
}

// ── Posting policy during the state machine ──────────────────────────────

func TestPostingPolicy_AcrossStates(t *testing.T) {
	now := time.Now().UTC()
	cases := []struct {
		state  string
		expiry *time.Time
		want   string
	}{
		{domain.PeriodOpen, nil, domain.PostingOpen},
		{domain.PeriodSoftClose, nil, domain.PostingRestricted},
		{domain.PeriodCloseReview, nil, domain.PostingCloseJournalsOnly},
		{domain.PeriodHardClosed, nil, domain.PostingClosed},
		{domain.PeriodReclosed, nil, domain.PostingClosed},
	}
	for _, c := range cases {
		fp := &domain.FiscalPeriod{CloseStatus: c.state}
		if got := fp.PostingPolicy(now); got != c.want {
			t.Errorf("%s: policy = %s, want %s", c.state, got, c.want)
		}
	}
	future := now.Add(time.Hour)
	reopened := &domain.FiscalPeriod{CloseStatus: domain.PeriodAuthorizedReopen, ReopenExpiresAt: &future}
	if got := reopened.PostingPolicy(now); got != domain.PostingReopened {
		t.Errorf("inside reopen window: %s, want %s", got, domain.PostingReopened)
	}
	expired := &domain.FiscalPeriod{CloseStatus: domain.PeriodAuthorizedReopen, ReopenExpiresAt: &now}
	if got := expired.PostingPolicy(now.Add(time.Second)); got != domain.PostingClosed {
		t.Errorf("after the window ends: %s, want %s — posting must stop automatically", got, domain.PostingClosed)
	}
}

// Four consumers (general-ledger, asset/inventory/project-accounting) read
// only close_status and treat anything but LOCKED/CLOSED as open. They must
// never see OPEN for a state that restricts posting.
func TestLegacyCloseStatus_NeverFalselyOpen(t *testing.T) {
	now := time.Now().UTC()
	future := now.Add(time.Hour)
	for _, state := range []string{domain.PeriodSoftClose, domain.PeriodCloseReview, domain.PeriodHardClosed, domain.PeriodReclosed} {
		fp := &domain.FiscalPeriod{CloseStatus: state}
		if got := fp.LegacyCloseStatus(now); got != "LOCKED" {
			t.Errorf("%s must read LOCKED to legacy consumers, got %s", state, got)
		}
	}
	reopened := &domain.FiscalPeriod{CloseStatus: domain.PeriodAuthorizedReopen, ReopenExpiresAt: &future}
	if got := reopened.LegacyCloseStatus(now); got != "OPEN" {
		t.Errorf("inside a reopen window legacy consumers must see OPEN, got %s", got)
	}
	expired := &domain.FiscalPeriod{CloseStatus: domain.PeriodAuthorizedReopen, ReopenExpiresAt: &now}
	if got := expired.LegacyCloseStatus(now.Add(time.Second)); got != "LOCKED" {
		t.Errorf("after the window ends legacy consumers must see LOCKED, got %s", got)
	}
}

// ── Reclose requires reperformance ────────────────────────────────────────

func reopenedPeriod(s *stubStore, id string, reopenedAt time.Time) {
	hardClosedPeriod(s, id)
	until := reopenedAt.Add(48 * time.Hour)
	s.periods[id].CloseStatus = domain.PeriodAuthorizedReopen
	s.periods[id].ReopenedAt = &reopenedAt
	s.periods[id].ReopenExpiresAt = &until
}

// The negative path #4: evidence from before the reopen does not satisfy a
// reclose.
func TestReclose_StaleControlRunBlocks(t *testing.T) {
	s := newStubStore()
	reopenAt := time.Date(2026, 11, 20, 10, 0, 0, 0, time.UTC)
	reopenedPeriod(s, "fp-1", reopenAt)
	bothMatchedAt(s, reopenAt.Add(-time.Hour)) // before the reopen
	cl := closeClients()
	cl.bankRecon = []domain.BankAccountReconStatus{certifiedAt("acct-barclays", "2026-10-30", reopenAt.Add(-time.Hour))}
	r := newGatedRouter(s, cl)
	rr := doReq(r, http.MethodPost, "/v1/close/periods/fp-1/reclose", nil, "controller-1")
	if rr.Code != http.StatusUnprocessableEntity {
		t.Fatalf("stale evidence must block reclose, got %d: %s", rr.Code, rr.Body.String())
	}
	body := rr.Body.String()
	if !strings.Contains(body, "not_reperformed") {
		t.Fatalf("blocker must say reperformance is required: %s", body)
	}
	if s.periods["fp-1"].CloseStatus != domain.PeriodAuthorizedReopen {
		t.Fatal("a blocked reclose must not move the period")
	}
}

func TestReclose_ReperformedEvidenceCloses(t *testing.T) {
	s := newStubStore()
	reopenAt := time.Date(2026, 11, 20, 10, 0, 0, 0, time.UTC)
	reopenedPeriod(s, "fp-1", reopenAt)
	after := reopenAt.Add(time.Hour)
	bothMatchedAt(s, after)
	cl := closeClients()
	cl.bankRecon = []domain.BankAccountReconStatus{certifiedAt("acct-barclays", "2026-10-30", after)}
	r := newGatedRouter(s, cl)
	rr := doReq(r, http.MethodPost, "/v1/close/periods/fp-1/reclose", nil, "controller-1")
	if rr.Code != http.StatusOK || s.periods["fp-1"].CloseStatus != domain.PeriodReclosed {
		t.Fatalf("reclose with fresh evidence: %d %s", rr.Code, rr.Body.String())
	}
	if s.periods["fp-1"].ReopenedAt != nil {
		t.Fatal("reclose must clear the reopen window")
	}
}

// ── History and available actions ────────────────────────────────────────

func TestCloseHistory_RecordsEveryTransition(t *testing.T) {
	s := newStubStore()
	newOpenPeriod(s, "fp-1")
	r := newGatedRouter(s, closeClients())
	doReq(r, http.MethodPost, "/v1/close/periods/fp-1/soft-close", nil, "accountant-1")
	doReq(r, http.MethodPost, "/v1/close/periods/fp-1/close-review", nil, "accountant-1")
	doReq(r, http.MethodPost, "/v1/close/periods/fp-1/hard-close", nil, "controller-1")

	rr := doReq(r, http.MethodGet, "/v1/close/periods/fp-1/history", nil, "auditor-1")
	var hist domain.CloseHistory
	if err := json.Unmarshal(rr.Body.Bytes(), &hist); err != nil || rr.Code != http.StatusOK {
		t.Fatalf("history: %d %s", rr.Code, rr.Body.String())
	}
	if len(hist.Transitions) != 3 {
		t.Fatalf("transitions = %d, want 3: %+v", len(hist.Transitions), hist.Transitions)
	}
	if hist.Transitions[0].FromState != domain.PeriodOpen || hist.Transitions[0].ToState != domain.PeriodSoftClose {
		t.Fatalf("first transition %+v", hist.Transitions[0])
	}
	if hist.Transitions[2].ToState != domain.PeriodHardClosed || hist.Transitions[2].PrincipalID != "controller-1" {
		t.Fatalf("last transition %+v", hist.Transitions[2])
	}
}

func TestAvailableCloseActions_MatchesState(t *testing.T) {
	s := newStubStore()
	newOpenPeriod(s, "fp-1")
	r := newGatedRouter(s, closeClients())
	rr := doReq(r, http.MethodGet, "/v1/close/periods/fp-1/available-actions", nil, "accountant-1")
	var actions domain.AvailableCloseActions
	_ = json.Unmarshal(rr.Body.Bytes(), &actions)
	if len(actions.Actions) != 1 || actions.Actions[0] != "soft-close" {
		t.Fatalf("OPEN actions = %v, want [soft-close]", actions.Actions)
	}
}

// ── Permissions ───────────────────────────────────────────────────────────

func TestHardClose_RequiresCloseApprovePermission(t *testing.T) {
	s := newStubStore()
	newOpenPeriod(s, "fp-1")
	s.periods["fp-1"].CloseStatus = domain.PeriodCloseReview
	authz := &actionAuthZ{deny: map[string]bool{"PERIOD_CLOSE_APPROVE": true}}
	r := checklistRouterWithClients(s, authz, closeClients())
	rr := doReq(r, http.MethodPost, "/v1/close/periods/fp-1/hard-close", nil, "controller-1")
	if rr.Code != http.StatusForbidden {
		t.Fatalf("got %d, want 403", rr.Code)
	}
}

func TestReopenApprove_RequiresReopenApprovePermission(t *testing.T) {
	s := newStubStore()
	hardClosedPeriod(s, "fp-1")
	authz := &actionAuthZ{deny: map[string]bool{"PERIOD_REOPEN_APPROVE": true}}
	r := checklistRouterWithClients(s, authz, closeClients())
	rr := doReq(r, http.MethodPost, "/v1/close/periods/fp-1/reopen-requests",
		domain.RequestReopenRequest{Reason: "x", ReopenUntil: time.Now().Add(24 * time.Hour)}, "accountant-1")
	var req domain.ReopenRequest
	_ = json.Unmarshal(rr.Body.Bytes(), &req)
	approve := doReq(r, http.MethodPost, "/v1/close/reopen-requests/"+req.RequestID+"/approve", nil, "cfo-1")
	if approve.Code != http.StatusForbidden {
		t.Fatalf("got %d, want 403", approve.Code)
	}
}
