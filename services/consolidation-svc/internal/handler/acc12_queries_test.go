package handler_test

import (
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"zoiko.io/consolidation-svc/internal/domain"
)

func reciprocalFixture(status string, usd domain.Decimal) *domain.ReciprocalBalances {
	return &domain.ReciprocalBalances{
		EntityA: "le-a", EntityB: "le-b", FiscalPeriod: "2026-07",
		ByMatchStatus: []domain.ReciprocalStatusBalances{{
			MatchStatus: status,
			EntityAToEntityB: []domain.ReciprocalCurrencyTotal{
				{CurrencyCode: "USD", Amount: usd},
			},
			EntityBToEntityA: []domain.ReciprocalCurrencyTotal{},
			Net: []domain.ReciprocalCurrencyTotal{
				{CurrencyCode: "USD", Amount: usd},
			},
		}},
	}
}

const explanationPath = "/v1/consolidation/eliminations/explanation" +
	"?group_legal_entity_id=group-1&entity_a=le-a&entity_b=le-b&fiscal_period=2026-07"

func TestExplainElimination_Matched(t *testing.T) {
	cl := &stubClients{reciprocalBalances: reciprocalFixture("MATCHED", "1234.5678")}
	r := newRouter(newStubStore(), &stubAuthZ{}, cl)

	rr := doReq(r, http.MethodGet, explanationPath, nil, "principal-1")
	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200 got %d: %s", rr.Code, rr.Body.String())
	}

	var got domain.EliminationExplanation
	if err := json.NewDecoder(rr.Body).Decode(&got); err != nil {
		t.Fatal(err)
	}
	if !got.Eliminable || got.Rule != domain.EliminationRuleMatchedOrClosed {
		t.Fatalf("unexpected explanation: %+v", got)
	}
	if len(got.MatchedStatuses) != 1 || got.MatchedStatuses[0] != "MATCHED" {
		t.Errorf("matched_statuses = %v", got.MatchedStatuses)
	}
	if got.ReciprocalBalance == nil ||
		got.ReciprocalBalance.ByMatchStatus[0].EntityAToEntityB[0].Amount != "1234.5678" {
		t.Errorf("reciprocal balance not returned intact: %+v", got.ReciprocalBalance)
	}
	if cl.reciprocalEntityA != "le-a" || cl.reciprocalEntityB != "le-b" || cl.reciprocalPeriod != "2026-07" {
		t.Errorf("pair/period not forwarded: %q %q %q",
			cl.reciprocalEntityA, cl.reciprocalEntityB, cl.reciprocalPeriod)
	}
}

func TestExplainElimination_Unmatched(t *testing.T) {
	cl := &stubClients{reciprocalBalances: reciprocalFixture("UNMATCHED", "10")}
	r := newRouter(newStubStore(), &stubAuthZ{}, cl)

	rr := doReq(r, http.MethodGet, explanationPath, nil, "principal-1")
	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200 got %d: %s", rr.Code, rr.Body.String())
	}
	var got domain.EliminationExplanation
	if err := json.NewDecoder(rr.Body).Decode(&got); err != nil {
		t.Fatal(err)
	}
	if got.Eliminable {
		t.Fatalf("UNMATCHED must not be eliminable: %+v", got)
	}
}

func TestExplainElimination_MissingFields(t *testing.T) {
	r := newRouter(newStubStore(), &stubAuthZ{}, &stubClients{})
	rr := doReq(r, http.MethodGet,
		"/v1/consolidation/eliminations/explanation?group_legal_entity_id=group-1&entity_a=le-a&entity_b=le-b",
		nil, "principal-1")
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 got %d: %s", rr.Code, rr.Body.String())
	}
}

func TestExplainElimination_SameEntity(t *testing.T) {
	r := newRouter(newStubStore(), &stubAuthZ{}, &stubClients{})
	rr := doReq(r, http.MethodGet,
		"/v1/consolidation/eliminations/explanation?group_legal_entity_id=group-1&entity_a=le-a&entity_b=le-a&fiscal_period=2026-07",
		nil, "principal-1")
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 got %d: %s", rr.Code, rr.Body.String())
	}
}

func TestExplainElimination_MissingPrincipal(t *testing.T) {
	r := newRouter(newStubStore(), &stubAuthZ{}, &stubClients{})
	rr := doReq(r, http.MethodGet, explanationPath, nil, "")
	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 got %d: %s", rr.Code, rr.Body.String())
	}
}

func TestExplainElimination_AuthzDenied(t *testing.T) {
	r := newRouter(newStubStore(), &stubAuthZ{err: domain.ErrAuthorizationDenied}, &stubClients{})
	rr := doReq(r, http.MethodGet, explanationPath, nil, "principal-1")
	if rr.Code != http.StatusForbidden {
		t.Fatalf("expected 403 got %d: %s", rr.Code, rr.Body.String())
	}
}

func TestExplainElimination_ClientUnavailable(t *testing.T) {
	cl := &stubClients{reciprocalErr: domain.ErrIntercompanyUnavailable}
	r := newRouter(newStubStore(), &stubAuthZ{}, cl)
	rr := doReq(r, http.MethodGet, explanationPath, nil, "principal-1")
	if rr.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected 503 got %d: %s", rr.Code, rr.Body.String())
	}
}

func TestExplainElimination_IntercompanyForbidden(t *testing.T) {
	cl := &stubClients{reciprocalErr: domain.ErrAuthorizationDenied}
	r := newRouter(newStubStore(), &stubAuthZ{}, cl)
	rr := doReq(r, http.MethodGet, explanationPath, nil, "principal-1")
	if rr.Code != http.StatusForbidden {
		t.Fatalf("expected 403 got %d: %s", rr.Code, rr.Body.String())
	}
}

func historyFixture() *domain.AdjustmentHistory {
	pending := domain.AdjustmentStatusPendingApproval
	approved := domain.AdjustmentStatusApproved
	posted := domain.AdjustmentStatusPosted
	reason := "correction"
	journalID := "11111111-1111-1111-1111-111111111111"
	return &domain.AdjustmentHistory{
		ConsolidationAdjustmentID: "adj-1",
		GroupLegalEntityID:        "group-1",
		FiscalPeriod:              "2026-07",
		AdjustmentType:            domain.AdjustmentTypeManual,
		Status:                    domain.AdjustmentStatusReversed,
		Transitions: []domain.AdjustmentTransition{
			{
				TransitionID: "t1", ConsolidationAdjustmentID: "adj-1", Sequence: 1,
				ToStatus: domain.AdjustmentStatusPendingApproval, ActorPrincipalID: "preparer-1",
				OccurredAt: time.Now().UTC().Add(-4 * time.Hour),
			},
			{
				TransitionID: "t2", ConsolidationAdjustmentID: "adj-1", Sequence: 2,
				FromStatus: &pending, ToStatus: domain.AdjustmentStatusApproved,
				ActorPrincipalID: "approver-1", OccurredAt: time.Now().UTC().Add(-3 * time.Hour),
			},
			{
				TransitionID: "t3", ConsolidationAdjustmentID: "adj-1", Sequence: 3,
				FromStatus: &approved, ToStatus: domain.AdjustmentStatusPosted,
				ActorPrincipalID: "poster-1", OccurredAt: time.Now().UTC().Add(-2 * time.Hour),
				Details: map[string]any{"consolidation_book_journal_id": journalID},
			},
			{
				TransitionID: "t4", ConsolidationAdjustmentID: "adj-1", Sequence: 4,
				FromStatus: &posted, ToStatus: domain.AdjustmentStatusReversed,
				ActorPrincipalID: "reverser-1", OccurredAt: time.Now().UTC().Add(-time.Hour),
				Reason: &reason,
			},
		},
	}
}

func TestGetAdjustmentHistory_Happy(t *testing.T) {
	s := newStubStore()
	s.histories["adj-1"] = historyFixture()
	r := newRouter(s, &stubAuthZ{}, &stubClients{})

	rr := doReq(r, http.MethodGet, "/v1/consolidation/adjustments/adj-1/history", nil, "principal-1")
	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200 got %d: %s", rr.Code, rr.Body.String())
	}
	var got domain.AdjustmentHistory
	if err := json.NewDecoder(rr.Body).Decode(&got); err != nil {
		t.Fatal(err)
	}
	if len(got.Transitions) != 4 {
		t.Fatalf("expected four transitions, got %+v", got.Transitions)
	}
	if got.Transitions[0].Sequence != 1 || got.Transitions[3].Sequence != 4 {
		t.Errorf("history not in sequence order: %+v", got.Transitions)
	}
	if got.Status != domain.AdjustmentStatusReversed {
		t.Errorf("current status = %q", got.Status)
	}
}

func TestGetAdjustmentHistory_NotFound(t *testing.T) {
	r := newRouter(newStubStore(), &stubAuthZ{}, &stubClients{})
	rr := doReq(r, http.MethodGet, "/v1/consolidation/adjustments/missing/history", nil, "principal-1")
	if rr.Code != http.StatusNotFound {
		t.Fatalf("expected 404 got %d: %s", rr.Code, rr.Body.String())
	}
}

func TestGetAdjustmentHistory_AuthzDenied(t *testing.T) {
	s := newStubStore()
	s.histories["adj-1"] = historyFixture()
	r := newRouter(s, &stubAuthZ{err: domain.ErrAuthorizationDenied}, &stubClients{})
	rr := doReq(r, http.MethodGet, "/v1/consolidation/adjustments/adj-1/history", nil, "principal-1")
	if rr.Code != http.StatusForbidden {
		t.Fatalf("expected 403 got %d: %s", rr.Code, rr.Body.String())
	}
}

func TestGetAdjustmentHistory_MissingPrincipal(t *testing.T) {
	r := newRouter(newStubStore(), &stubAuthZ{}, &stubClients{})
	rr := doReq(r, http.MethodGet, "/v1/consolidation/adjustments/adj-1/history", nil, "")
	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 got %d: %s", rr.Code, rr.Body.String())
	}
}
