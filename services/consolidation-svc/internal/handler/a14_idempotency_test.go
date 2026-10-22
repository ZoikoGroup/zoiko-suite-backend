package handler_test

// A14: every mutating command in this service takes an Idempotency-Key, a
// retry under the same key replays the original result instead of doing the
// work twice, and a failed final CompleteRun is reported as the run's actual
// persisted state — never a 201 COMPLETED the store did not write.
//
// These tests exercise the handler against the stub store, which applies the
// same claim rules as command_idempotency (migration 000008). The store's own
// transaction-level behaviour is covered in idempotency_integration_test.go.

import (
	"encoding/json"
	"errors"
	"net/http"
	"testing"

	"zoiko.io/consolidation-svc/internal/domain"
)

func startRunBody(period string) map[string]any {
	return map[string]any{
		"group_legal_entity_id":  "le-group",
		"child_legal_entity_ids": []string{"le-us", "le-uk"},
		"fiscal_period":          period,
		"target_currency":        "USD",
	}
}

// TestStartRun_SameKey_ReplaysRunWithoutDoingTheWorkAgain is A14's central
// StartRun requirement: the retry returns the run the first request created,
// and none of the run's work (GL reads, elimination, snapshots) happens twice.
func TestStartRun_SameKey_ReplaysRunWithoutDoingTheWorkAgain(t *testing.T) {
	s := newStubStore()
	cl := &stubClients{glBalances: map[string]map[string]float64{
		"le-us": {"1000-Cash": 5000.0},
		"le-uk": {"1000-Cash": 3000.0},
	}}
	r := newRouter(s, &stubAuthZ{}, cl)
	key := "start-run-key-1"

	first := doReqWithKey(r, http.MethodPost, "/v1/consolidation/runs/", startRunBody("2024-Q1"), "principal-1", key)
	if first.Code != http.StatusCreated {
		t.Fatalf("first attempt: expected 201, got %d: %s", first.Code, first.Body.String())
	}
	var firstResp domain.ConsolidationRunResponse
	if err := json.NewDecoder(first.Body).Decode(&firstResp); err != nil {
		t.Fatalf("decode first response: %v", err)
	}
	if firstResp.Status != "COMPLETED" {
		t.Fatalf("first attempt: expected COMPLETED, got %q", firstResp.Status)
	}
	if len(s.runs) != 1 {
		t.Fatalf("first attempt: expected exactly one run, got %d", len(s.runs))
	}
	readsAfterFirst := cl.trialBalanceReads
	snapshotsAfterFirst := len(s.snapshots[firstResp.ConsolidationRunID])

	retry := doReqWithKey(r, http.MethodPost, "/v1/consolidation/runs/", startRunBody("2024-Q1"), "principal-1", key)
	if retry.Code != http.StatusOK {
		t.Fatalf("retry: expected 200 replay, got %d: %s", retry.Code, retry.Body.String())
	}
	if got := retry.Header().Get("Idempotent-Replay"); got != "true" {
		t.Fatalf("retry: expected Idempotent-Replay: true, got %q", got)
	}
	var retryResp domain.ConsolidationRunResponse
	if err := json.NewDecoder(retry.Body).Decode(&retryResp); err != nil {
		t.Fatalf("decode retry response: %v", err)
	}
	if retryResp.ConsolidationRunID != firstResp.ConsolidationRunID {
		t.Fatalf("retry returned run %q, first attempt created %q", retryResp.ConsolidationRunID, firstResp.ConsolidationRunID)
	}
	if retryResp.Status != "COMPLETED" {
		t.Errorf("retry: expected the persisted COMPLETED status, got %q", retryResp.Status)
	}
	if len(s.runs) != 1 {
		t.Fatalf("retry created a second run: %d runs", len(s.runs))
	}
	if cl.trialBalanceReads != readsAfterFirst {
		t.Errorf("retry re-read trial balances: %d reads, was %d", cl.trialBalanceReads, readsAfterFirst)
	}
	if got := len(s.snapshots[firstResp.ConsolidationRunID]); got != snapshotsAfterFirst {
		t.Errorf("retry recorded snapshots again: %d, was %d", got, snapshotsAfterFirst)
	}
}

// TestStartRun_SameKeyDifferentRequest_Conflicts proves a key is bound to the
// run it created: reusing it for another period is an error, not a replay of
// a run for different inputs.
func TestStartRun_SameKeyDifferentRequest_Conflicts(t *testing.T) {
	s := newStubStore()
	r := newRouter(s, &stubAuthZ{}, &stubClients{})
	key := "start-run-key-reused"

	first := doReqWithKey(r, http.MethodPost, "/v1/consolidation/runs/", startRunBody("2024-Q1"), "principal-1", key)
	if first.Code != http.StatusCreated {
		t.Fatalf("first attempt: expected 201, got %d: %s", first.Code, first.Body.String())
	}

	second := doReqWithKey(r, http.MethodPost, "/v1/consolidation/runs/", startRunBody("2024-Q2"), "principal-1", key)
	if second.Code != http.StatusConflict {
		t.Fatalf("expected 409 reusing the key for another period, got %d: %s", second.Code, second.Body.String())
	}
	if len(s.runs) != 1 {
		t.Fatalf("conflicting retry created a second run: %d runs", len(s.runs))
	}
}

// TestStartRun_CompleteRunFailure_ReportsPersistedState is A14's named bug:
// the run row was created RUNNING and the final write failed, so the response
// must not claim 201 COMPLETED. It reports 503 with the run's real status.
func TestStartRun_CompleteRunFailure_ReportsPersistedState(t *testing.T) {
	s := newStubStore()
	s.completeErr = errors.New("connection reset by peer")
	r := newRouter(s, &stubAuthZ{}, &stubClients{})

	rr := doReq(r, http.MethodPost, "/v1/consolidation/runs/", startRunBody("2024-Q1"), "principal-1")
	if rr.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected 503 when CompleteRun fails, got %d: %s", rr.Code, rr.Body.String())
	}
	var resp domain.RunNotCompletedResponse
	if err := json.NewDecoder(rr.Body).Decode(&resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if resp.ErrorCode != "run_not_completed" {
		t.Errorf("expected error_code run_not_completed, got %q", resp.ErrorCode)
	}
	if resp.Status != "RUNNING" {
		t.Errorf("expected the persisted RUNNING status, got %q", resp.Status)
	}
	if resp.ConsolidationRunID == "" {
		t.Fatal("expected the run id in the failure response")
	}
	run, ok := s.runs[resp.ConsolidationRunID]
	if !ok {
		t.Fatalf("response names run %q, which the store does not hold", resp.ConsolidationRunID)
	}
	if run.Status != "RUNNING" {
		t.Errorf("response said RUNNING but the row is %q", run.Status)
	}
	if s.completedStatus == "COMPLETED" {
		t.Error("stub recorded a COMPLETED write despite CompleteRun failing")
	}
}

// TestMutatingCommands_RequireIdempotencyKey covers every command A14 names,
// including the four adjustment commands. Each request is otherwise valid, so
// only the missing header can produce the 400.
func TestMutatingCommands_RequireIdempotencyKey(t *testing.T) {
	cases := []struct {
		name   string
		seed   func(*stubStore)
		method string
		path   string
		body   any
	}{
		{
			name: "start run", method: http.MethodPost, path: "/v1/consolidation/runs/",
			body: startRunBody("2024-Q1"),
		},
		{
			name:   "create adjustment",
			seed:   func(s *stubStore) { s.groupHasRun["group-1"] = true },
			method: http.MethodPost, path: "/v1/consolidation/adjustments",
			body: validAdjustmentReq(domain.AdjustmentTypeManual),
		},
		{
			name: "approve adjustment",
			seed: func(s *stubStore) {
				seedAdjustment(s, "adj-1", domain.AdjustmentStatusPendingApproval, "preparer-1")
			},
			method: http.MethodPost, path: "/v1/consolidation/adjustments/adj-1/approve",
		},
		{
			name: "post adjustment",
			seed: func(s *stubStore) {
				seedAdjustment(s, "adj-1", domain.AdjustmentStatusApproved, "preparer-1")
			},
			method: http.MethodPost, path: "/v1/consolidation/adjustments/adj-1/post",
		},
		{
			name: "reverse adjustment",
			seed: func(s *stubStore) {
				seedAdjustment(s, "adj-1", domain.AdjustmentStatusPosted, "preparer-1")
			},
			method: http.MethodPost, path: "/v1/consolidation/adjustments/adj-1/reverse",
			body: map[string]string{"reason": "correcting an error"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := newStubStore()
			if tc.seed != nil {
				tc.seed(s)
			}
			r := newRouter(s, &stubAuthZ{}, &stubClients{})

			rr := doReqWithKey(r, tc.method, tc.path, tc.body, "principal-1", "")
			if rr.Code != http.StatusBadRequest {
				t.Fatalf("expected 400 without Idempotency-Key, got %d: %s", rr.Code, rr.Body.String())
			}
			var resp map[string]string
			if err := json.NewDecoder(rr.Body).Decode(&resp); err != nil {
				t.Fatalf("decode response: %v", err)
			}
			if resp["error_code"] != "missing_idempotency_key" {
				t.Errorf("expected missing_idempotency_key, got %q", resp["error_code"])
			}
		})
	}
}

// TestCreateAdjustment_SameKey_ReplaysWithoutSecondAdjustment proves a retried
// create returns the original adjustment and stores only one.
func TestCreateAdjustment_SameKey_ReplaysWithoutSecondAdjustment(t *testing.T) {
	s := newStubStore()
	s.groupHasRun["group-1"] = true
	r := newRouter(s, &stubAuthZ{}, &stubClients{})
	key := "create-adj-key-1"

	first := doReqWithKey(r, http.MethodPost, "/v1/consolidation/adjustments", validAdjustmentReq(domain.AdjustmentTypeManual), "preparer-1", key)
	if first.Code != http.StatusCreated {
		t.Fatalf("first create: expected 201, got %d: %s", first.Code, first.Body.String())
	}
	var firstAdj domain.ConsolidationAdjustment
	if err := json.NewDecoder(first.Body).Decode(&firstAdj); err != nil {
		t.Fatalf("decode first response: %v", err)
	}

	retry := doReqWithKey(r, http.MethodPost, "/v1/consolidation/adjustments", validAdjustmentReq(domain.AdjustmentTypeManual), "preparer-1", key)
	if retry.Code != http.StatusOK {
		t.Fatalf("retry: expected 200 replay, got %d: %s", retry.Code, retry.Body.String())
	}
	if got := retry.Header().Get("Idempotent-Replay"); got != "true" {
		t.Fatalf("retry: expected Idempotent-Replay: true, got %q", got)
	}
	var retryAdj domain.ConsolidationAdjustment
	if err := json.NewDecoder(retry.Body).Decode(&retryAdj); err != nil {
		t.Fatalf("decode retry response: %v", err)
	}
	if retryAdj.ConsolidationAdjustmentID != firstAdj.ConsolidationAdjustmentID {
		t.Fatalf("retry returned %q, first create made %q", retryAdj.ConsolidationAdjustmentID, firstAdj.ConsolidationAdjustmentID)
	}
	if len(s.adjustments) != 1 {
		t.Fatalf("retry created a second adjustment: %d stored", len(s.adjustments))
	}
}

// TestApproveAdjustment_SameKey_ReplaysAfterStatusMoved proves the claim is
// consulted before the PENDING_APPROVAL-only guard: the retry succeeds with
// the adjustment as it now stands instead of 422-ing on its own first result.
func TestApproveAdjustment_SameKey_ReplaysAfterStatusMoved(t *testing.T) {
	s := newStubStore()
	seedAdjustment(s, "adj-1", domain.AdjustmentStatusPendingApproval, "preparer-1")
	r := newRouter(s, &stubAuthZ{}, &stubClients{})
	key := "approve-key-1"

	first := doReqWithKey(r, http.MethodPost, "/v1/consolidation/adjustments/adj-1/approve", nil, "approver-1", key)
	if first.Code != http.StatusOK {
		t.Fatalf("first approve: expected 200, got %d: %s", first.Code, first.Body.String())
	}

	retry := doReqWithKey(r, http.MethodPost, "/v1/consolidation/adjustments/adj-1/approve", nil, "approver-1", key)
	if retry.Code != http.StatusOK {
		t.Fatalf("retry: expected 200 replay, got %d: %s", retry.Code, retry.Body.String())
	}
	if got := retry.Header().Get("Idempotent-Replay"); got != "true" {
		t.Fatalf("retry: expected Idempotent-Replay: true, got %q", got)
	}
	var resp map[string]string
	if err := json.NewDecoder(retry.Body).Decode(&resp); err != nil {
		t.Fatalf("decode retry response: %v", err)
	}
	if resp["status"] != domain.AdjustmentStatusApproved {
		t.Errorf("expected APPROVED, got %q", resp["status"])
	}
}

// TestPostAdjustment_SameKey_DoesNotPostTheJournalTwice is the important
// adjustment replay: the retry must answer from the claim before the
// APPROVED-only guard and before general-ledger-svc is called.
func TestPostAdjustment_SameKey_DoesNotPostTheJournalTwice(t *testing.T) {
	s := newStubStore()
	seedAdjustment(s, "adj-1", domain.AdjustmentStatusApproved, "preparer-1")
	cl := &stubClients{postJournalID: "real-journal-1"}
	r := newRouter(s, &stubAuthZ{}, cl)
	key := "post-key-1"

	first := doReqWithKey(r, http.MethodPost, "/v1/consolidation/adjustments/adj-1/post", nil, "poster-1", key)
	if first.Code != http.StatusOK {
		t.Fatalf("first post: expected 200, got %d: %s", first.Code, first.Body.String())
	}
	if cl.postCalls != 1 {
		t.Fatalf("first post: expected one journal post, got %d", cl.postCalls)
	}

	retry := doReqWithKey(r, http.MethodPost, "/v1/consolidation/adjustments/adj-1/post", nil, "poster-1", key)
	if retry.Code != http.StatusOK {
		t.Fatalf("retry: expected 200 replay, got %d: %s", retry.Code, retry.Body.String())
	}
	if got := retry.Header().Get("Idempotent-Replay"); got != "true" {
		t.Fatalf("retry: expected Idempotent-Replay: true, got %q", got)
	}
	if cl.postCalls != 1 {
		t.Errorf("retry posted the journal again: %d posts", cl.postCalls)
	}
	var resp map[string]string
	if err := json.NewDecoder(retry.Body).Decode(&resp); err != nil {
		t.Fatalf("decode retry response: %v", err)
	}
	if resp["status"] != domain.AdjustmentStatusPosted {
		t.Errorf("expected POSTED, got %q", resp["status"])
	}
	if resp["consolidation_book_journal_id"] != "real-journal-1" {
		t.Errorf("expected the original journal id, got %q", resp["consolidation_book_journal_id"])
	}
}

// TestReverseAdjustment_SameKey_DoesNotReverseTheJournalTwice proves the
// pre-flight check protects general-ledger-svc's reversal endpoint, which has
// no idempotency key of its own.
func TestReverseAdjustment_SameKey_DoesNotReverseTheJournalTwice(t *testing.T) {
	s := newStubStore()
	seedAdjustment(s, "adj-1", domain.AdjustmentStatusPosted, "preparer-1")
	s.adjustments["adj-1"].ConsolidationBookJournalID = strPtr("journal-1")
	cl := &stubClients{}
	r := newRouter(s, &stubAuthZ{}, cl)
	key := "reverse-key-1"
	body := map[string]string{"reason": "correcting an error"}

	first := doReqWithKey(r, http.MethodPost, "/v1/consolidation/adjustments/adj-1/reverse", body, "reverser-1", key)
	if first.Code != http.StatusOK {
		t.Fatalf("first reverse: expected 200, got %d: %s", first.Code, first.Body.String())
	}
	if cl.reverseCalls != 1 {
		t.Fatalf("first reverse: expected one journal reversal, got %d", cl.reverseCalls)
	}

	retry := doReqWithKey(r, http.MethodPost, "/v1/consolidation/adjustments/adj-1/reverse", body, "reverser-1", key)
	if retry.Code != http.StatusOK {
		t.Fatalf("retry: expected 200 replay, got %d: %s", retry.Code, retry.Body.String())
	}
	if got := retry.Header().Get("Idempotent-Replay"); got != "true" {
		t.Fatalf("retry: expected Idempotent-Replay: true, got %q", got)
	}
	if cl.reverseCalls != 1 {
		t.Errorf("retry reversed the journal again: %d reversals", cl.reverseCalls)
	}
	var resp map[string]string
	if err := json.NewDecoder(retry.Body).Decode(&resp); err != nil {
		t.Fatalf("decode retry response: %v", err)
	}
	if resp["status"] != domain.AdjustmentStatusReversed {
		t.Errorf("expected REVERSED, got %q", resp["status"])
	}
}

// TestAdjustmentCommand_KeyUsedForAnotherCommand_Conflicts proves a key cannot
// be moved between commands on the same adjustment.
func TestAdjustmentCommand_KeyUsedForAnotherCommand_Conflicts(t *testing.T) {
	s := newStubStore()
	seedAdjustment(s, "adj-1", domain.AdjustmentStatusPendingApproval, "preparer-1")
	r := newRouter(s, &stubAuthZ{}, &stubClients{})
	key := "one-key-one-command"

	approve := doReqWithKey(r, http.MethodPost, "/v1/consolidation/adjustments/adj-1/approve", nil, "approver-1", key)
	if approve.Code != http.StatusOK {
		t.Fatalf("approve: expected 200, got %d: %s", approve.Code, approve.Body.String())
	}

	post := doReqWithKey(r, http.MethodPost, "/v1/consolidation/adjustments/adj-1/post", nil, "poster-1", key)
	if post.Code != http.StatusConflict {
		t.Fatalf("expected 409 reusing the approve key to post, got %d: %s", post.Code, post.Body.String())
	}
	if s.adjustments["adj-1"].Status != domain.AdjustmentStatusApproved {
		t.Errorf("conflicting post changed the adjustment to %q", s.adjustments["adj-1"].Status)
	}
}
