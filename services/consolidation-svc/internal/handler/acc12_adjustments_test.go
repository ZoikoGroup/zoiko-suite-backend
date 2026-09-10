package handler_test

import (
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"zoiko.io/consolidation-svc/internal/clients"
	"zoiko.io/consolidation-svc/internal/domain"
)

func seedAdjustment(s *stubStore, id, status, createdBy string) *domain.ConsolidationAdjustment {
	a := &domain.ConsolidationAdjustment{
		ConsolidationAdjustmentID: id, TenantID: "tenant-abc",
		GroupLegalEntityID: "group-1", FiscalPeriod: "2026-07",
		AdjustmentType: domain.AdjustmentTypeManual, Description: "test",
		Status: status,
		Lines: []domain.ConsolidationAdjustmentLine{
			{AccountCode: "9000", DebitAmount: 100},
			{AccountCode: "9100", CreditAmount: 100},
		},
		CreatedAt: time.Now().UTC(), CreatedByPrincipalID: createdBy,
	}
	s.adjustments[id] = a
	return a
}

func stubClientsWithMatched(amount float64) *stubClients {
	return &stubClients{
		intercompanyEntries: []clients.IntercompanyEntry{
			{IntercompanyEntryID: "ic-1", Amount: amount, MatchStatus: "MATCHED"},
		},
	}
}

func validAdjustmentReq(adjType string) map[string]any {
	return map[string]any{
		"group_legal_entity_id": "group-1",
		"fiscal_period":         "2026-07",
		"adjustment_type":       adjType,
		"description":           "test adjustment",
		"lines": []map[string]any{
			{"account_code": "9000", "debit_amount": 100},
			{"account_code": "9100", "credit_amount": 100},
		},
	}
}

// ── CreateEliminationProposal ────────────────────────────────────────────────

func TestCreateEliminationProposal_TargetsStatutoryBook_Refused(t *testing.T) {
	s := newStubStore() // group-1 never appeared in any ConsolidationRun
	r := newRouter(s, &stubPublisher{}, &stubAuthZ{}, &stubClients{})

	resp := doReq(r, http.MethodPost, "/v1/consolidation/adjustments", validAdjustmentReq(domain.AdjustmentTypeManual), "preparer-1")
	if resp.Code != http.StatusUnprocessableEntity {
		t.Fatalf("expected 422 targeting an entity never used as a group entity, got %d: %s", resp.Code, resp.Body.String())
	}
}

func TestCreateEliminationProposal_ManualAdjustment_HappyPath(t *testing.T) {
	s := newStubStore()
	s.groupHasRun["group-1"] = true
	r := newRouter(s, &stubPublisher{}, &stubAuthZ{}, &stubClients{})

	resp := doReq(r, http.MethodPost, "/v1/consolidation/adjustments", validAdjustmentReq(domain.AdjustmentTypeManual), "preparer-1")
	if resp.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d: %s", resp.Code, resp.Body.String())
	}
	var got domain.ConsolidationAdjustment
	if err := json.Unmarshal(resp.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.Status != domain.AdjustmentStatusPendingApproval {
		t.Fatalf("expected PENDING_APPROVAL, got %q", got.Status)
	}
}

func TestCreateEliminationProposal_EliminationExceedsMatchedBalance_Refused(t *testing.T) {
	s := newStubStore()
	s.groupHasRun["group-1"] = true
	// Only 50 of matched intercompany balance exists; the proposal below asks for 100.
	r := newRouter(s, &stubPublisher{}, &stubAuthZ{}, stubClientsWithMatched(50))

	resp := doReq(r, http.MethodPost, "/v1/consolidation/adjustments", validAdjustmentReq(domain.AdjustmentTypeElimination), "preparer-1")
	if resp.Code != http.StatusUnprocessableEntity {
		t.Fatalf("expected 422 exceeding matched balance, got %d: %s", resp.Code, resp.Body.String())
	}
}

func TestCreateEliminationProposal_EliminationWithinMatchedBalance_Allowed(t *testing.T) {
	s := newStubStore()
	s.groupHasRun["group-1"] = true
	r := newRouter(s, &stubPublisher{}, &stubAuthZ{}, stubClientsWithMatched(500))

	resp := doReq(r, http.MethodPost, "/v1/consolidation/adjustments", validAdjustmentReq(domain.AdjustmentTypeElimination), "preparer-1")
	if resp.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d: %s", resp.Code, resp.Body.String())
	}
}

// ── ApproveConsolidationAdjustment ───────────────────────────────────────────

func TestApproveConsolidationAdjustment_SelfApproval_Refused(t *testing.T) {
	s := newStubStore()
	seedAdjustment(s, "adj-1", domain.AdjustmentStatusPendingApproval, "preparer-1")
	r := newRouter(s, &stubPublisher{}, &stubAuthZ{}, &stubClients{})

	resp := doReq(r, http.MethodPost, "/v1/consolidation/adjustments/adj-1/approve", nil, "preparer-1")
	if resp.Code != http.StatusForbidden {
		t.Fatalf("expected 403 self-approval, got %d: %s", resp.Code, resp.Body.String())
	}
}

func TestApproveConsolidationAdjustment_DifferentApprover_Allowed(t *testing.T) {
	s := newStubStore()
	seedAdjustment(s, "adj-1", domain.AdjustmentStatusPendingApproval, "preparer-1")
	r := newRouter(s, &stubPublisher{}, &stubAuthZ{}, &stubClients{})

	resp := doReq(r, http.MethodPost, "/v1/consolidation/adjustments/adj-1/approve", nil, "approver-1")
	if resp.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", resp.Code, resp.Body.String())
	}
	if s.adjustments["adj-1"].Status != domain.AdjustmentStatusApproved {
		t.Fatalf("expected APPROVED, got %q", s.adjustments["adj-1"].Status)
	}
}

// ── PostConsolidationAdjustment ───────────────────────────────────────────────

func TestPostConsolidationAdjustment_NotApproved_Refused(t *testing.T) {
	s := newStubStore()
	seedAdjustment(s, "adj-1", domain.AdjustmentStatusPendingApproval, "preparer-1")
	r := newRouter(s, &stubPublisher{}, &stubAuthZ{}, &stubClients{})

	resp := doReq(r, http.MethodPost, "/v1/consolidation/adjustments/adj-1/post", nil, "poster-1")
	if resp.Code != http.StatusUnprocessableEntity {
		t.Fatalf("expected 422, got %d: %s", resp.Code, resp.Body.String())
	}
}

func TestPostConsolidationAdjustment_Approved_PostsRealJournal(t *testing.T) {
	s := newStubStore()
	seedAdjustment(s, "adj-1", domain.AdjustmentStatusApproved, "preparer-1")
	cl := &stubClients{postJournalID: "real-journal-1"}
	r := newRouter(s, &stubPublisher{}, &stubAuthZ{}, cl)

	resp := doReq(r, http.MethodPost, "/v1/consolidation/adjustments/adj-1/post", nil, "poster-1")
	if resp.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", resp.Code, resp.Body.String())
	}
	if s.adjustments["adj-1"].Status != domain.AdjustmentStatusPosted {
		t.Fatalf("expected POSTED, got %q", s.adjustments["adj-1"].Status)
	}
	if s.adjustments["adj-1"].ConsolidationBookJournalID == nil || *s.adjustments["adj-1"].ConsolidationBookJournalID != "real-journal-1" {
		t.Fatalf("expected the real journal id recorded, got %+v", s.adjustments["adj-1"].ConsolidationBookJournalID)
	}
}

// ── ReverseConsolidationAdjustment ────────────────────────────────────────────

func TestReverseConsolidationAdjustment_MissingReason_Returns400(t *testing.T) {
	s := newStubStore()
	seedAdjustment(s, "adj-1", domain.AdjustmentStatusPosted, "preparer-1")
	r := newRouter(s, &stubPublisher{}, &stubAuthZ{}, &stubClients{})

	resp := doReq(r, http.MethodPost, "/v1/consolidation/adjustments/adj-1/reverse", map[string]string{}, "reverser-1")
	if resp.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d: %s", resp.Code, resp.Body.String())
	}
}

func TestReverseConsolidationAdjustment_AfterSnapshot_RequiresSupersession(t *testing.T) {
	s := newStubStore()
	seedAdjustment(s, "adj-1", domain.AdjustmentStatusPosted, "preparer-1")
	s.hasSnapshot["group-1|2026-07"] = true
	r := newRouter(s, &stubPublisher{}, &stubAuthZ{}, &stubClients{})

	resp := doReq(r, http.MethodPost, "/v1/consolidation/adjustments/adj-1/reverse", map[string]string{"reason": "correcting an error"}, "reverser-1")
	if resp.Code != http.StatusUnprocessableEntity {
		t.Fatalf("expected 422 requiring supersession, got %d: %s", resp.Code, resp.Body.String())
	}
}

func TestReverseConsolidationAdjustment_AfterSnapshot_WithSupersession_Allowed(t *testing.T) {
	s := newStubStore()
	seedAdjustment(s, "adj-1", domain.AdjustmentStatusPosted, "preparer-1")
	seedAdjustment(s, "adj-2", domain.AdjustmentStatusPendingApproval, "preparer-1")
	s.hasSnapshot["group-1|2026-07"] = true
	supersededBy := "adj-2"
	r := newRouter(s, &stubPublisher{}, &stubAuthZ{}, &stubClients{})

	resp := doReq(r, http.MethodPost, "/v1/consolidation/adjustments/adj-1/reverse", map[string]any{
		"reason": "correcting an error", "superseded_by_adjustment_id": supersededBy,
	}, "reverser-1")
	if resp.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", resp.Code, resp.Body.String())
	}
	if s.adjustments["adj-1"].Status != domain.AdjustmentStatusReversed {
		t.Fatalf("expected REVERSED, got %q", s.adjustments["adj-1"].Status)
	}
}

func TestReverseConsolidationAdjustment_NoSnapshotYet_PlainReversalAllowed(t *testing.T) {
	s := newStubStore()
	seedAdjustment(s, "adj-1", domain.AdjustmentStatusPosted, "preparer-1")
	// No snapshot recorded for group-1|2026-07.
	r := newRouter(s, &stubPublisher{}, &stubAuthZ{}, &stubClients{})

	resp := doReq(r, http.MethodPost, "/v1/consolidation/adjustments/adj-1/reverse", map[string]string{"reason": "never posted correctly"}, "reverser-1")
	if resp.Code != http.StatusOK {
		t.Fatalf("expected 200 (no snapshot exists yet), got %d: %s", resp.Code, resp.Body.String())
	}
}
