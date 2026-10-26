package handler_test

import (
	"net/http"
	"testing"

	"zoiko.io/consolidation-svc/internal/domain"
)

// An entity that is not the reporting entity of a consolidation group of the group book has nowhere to post: a clear 422, and the
// adjustment stays APPROVED.
func TestPostConsolidationAdjustment_NoGroupBook_Returns422AndStaysApproved(t *testing.T) {
	s := newStubStore()
	seedAdjustment(s, "adj-1", domain.AdjustmentStatusApproved, "preparer-1")
	cl := &stubClients{postErr: domain.ErrNoGroupBook}
	r := newRouter(s, &stubAuthZ{}, cl)

	resp := doReq(r, http.MethodPost, "/v1/consolidation/adjustments/adj-1/post", nil, "poster-1")
	if resp.Code != http.StatusUnprocessableEntity {
		t.Fatalf("expected 422, got %d: %s", resp.Code, resp.Body.String())
	}
	if s.adjustments["adj-1"].Status != domain.AdjustmentStatusApproved {
		t.Fatalf("a post that reached no group book changes nothing, got %q", s.adjustments["adj-1"].Status)
	}
}

// An adjustment already applied by a consolidation run of the group book is not withdrawn behind that run: 409, and it stays POSTED.
func TestReverseConsolidationAdjustment_AppliedByAGroupRun_Returns409(t *testing.T) {
	s := newStubStore()
	a := seedAdjustment(s, "adj-1", domain.AdjustmentStatusPosted, "preparer-1")
	a.ConsolidationBookJournalID = strPtr("gl-adjustment-1")
	cl := &stubClients{reverseErr: domain.ErrAdjustmentInGroupRun}
	r := newRouter(s, &stubAuthZ{}, cl)

	resp := doReq(r, http.MethodPost, "/v1/consolidation/adjustments/adj-1/reverse", map[string]string{"reason": "wrong"}, "reverser-1")
	if resp.Code != http.StatusConflict {
		t.Fatalf("expected 409, got %d: %s", resp.Code, resp.Body.String())
	}
	if s.adjustments["adj-1"].Status != domain.AdjustmentStatusPosted {
		t.Fatalf("got %q", s.adjustments["adj-1"].Status)
	}
}
