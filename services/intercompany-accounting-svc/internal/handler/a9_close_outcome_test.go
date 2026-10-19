package handler_test

// SPEC_DEVIATIONS A9: the terminal CLOSED state, the resolution outcome and
// the pair's transition history.

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"zoiko.io/intercompany-accounting-svc/internal/domain"
)

func TestResolveMismatch_OutcomeRequired(t *testing.T) {
	for _, outcome := range []string{"", "accepted", "IGNORED"} {
		s := newStubStoreReal()
		seedEntry(s, "e1", domain.MatchStatusDisputed)
		resp := doReq(newRouter(s, &stubAuthZ{}, &stubLedger{}), http.MethodPost, "/v1/intercompany/entries/e1/resolve",
			map[string]string{"resolution_note": "n", "outcome": outcome}, "user-1")
		if resp.Code != http.StatusBadRequest {
			t.Errorf("outcome %q: expected 400, got %d: %s", outcome, resp.Code, resp.Body.String())
		}
		if s.entries["e1"].MatchStatus != domain.MatchStatusDisputed {
			t.Errorf("outcome %q: pair moved to %s", outcome, s.entries["e1"].MatchStatus)
		}
	}
}

func TestResolveMismatch_RecordsOutcome(t *testing.T) {
	s := newStubStoreReal()
	seedEntry(s, "e1", domain.MatchStatusDisputed)
	resp := doReq(newRouter(s, &stubAuthZ{}, &stubLedger{}), http.MethodPost, "/v1/intercompany/entries/e1/resolve",
		map[string]string{"resolution_note": "written off below materiality", "outcome": domain.ResolutionWrittenOff}, "user-1")
	if resp.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", resp.Code, resp.Body.String())
	}
	if got := s.entries["e1"].ResolutionOutcome; got == nil || *got != domain.ResolutionWrittenOff {
		t.Fatalf("expected WRITTEN_OFF recorded, got %v", got)
	}
}

func TestClosePair_FromMatched(t *testing.T) {
	s := newStubStoreReal()
	seedEntry(s, "e1", domain.MatchStatusMatched)
	resp := doReq(newRouter(s, &stubAuthZ{}, &stubLedger{}), http.MethodPost, "/v1/intercompany/entries/e1/close",
		map[string]string{"reason": "period 2026-09 consolidated"}, "user-1")
	if resp.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", resp.Code, resp.Body.String())
	}
	e := s.entries["e1"]
	if e.MatchStatus != domain.MatchStatusClosed || e.CloseReason == nil || e.ClosedByPrincipalID == nil || *e.ClosedByPrincipalID != "user-1" {
		t.Fatalf("close not recorded: %+v", e)
	}
}

func TestClosePair_RefusedOutsideMatched(t *testing.T) {
	for _, status := range []string{domain.MatchStatusUnmatched, domain.MatchStatusMismatch, domain.MatchStatusDisputed,
		domain.MatchStatusResolved, domain.MatchStatusClosed} {
		s := newStubStoreReal()
		seedEntry(s, "e1", status)
		resp := doReq(newRouter(s, &stubAuthZ{}, &stubLedger{}), http.MethodPost, "/v1/intercompany/entries/e1/close",
			map[string]string{"reason": "r"}, "user-1")
		if resp.Code != http.StatusUnprocessableEntity {
			t.Errorf("%s: expected 422, got %d: %s", status, resp.Code, resp.Body.String())
		}
	}
}

func TestClosePair_ReasonRequired(t *testing.T) {
	s := newStubStoreReal()
	seedEntry(s, "e1", domain.MatchStatusMatched)
	resp := doReq(newRouter(s, &stubAuthZ{}, &stubLedger{}), http.MethodPost, "/v1/intercompany/entries/e1/close",
		map[string]string{}, "user-1")
	if resp.Code != http.StatusBadRequest || s.entries["e1"].MatchStatus != domain.MatchStatusMatched {
		t.Fatalf("expected 400 and the pair left MATCHED, got %d / %s", resp.Code, s.entries["e1"].MatchStatus)
	}
}

// entityAuthZ denies one entity and records every entity it was asked about.
type entityAuthZ struct {
	deny    string
	checked []string
}

func (a *entityAuthZ) CheckAllowed(_ context.Context, _, entityID, _ string) error {
	a.checked = append(a.checked, entityID)
	if entityID == a.deny {
		return domain.ErrAuthorizationDenied
	}
	return nil
}

// A terminal close takes the pair out of both sides' open set, so a
// principal entitled on only one side cannot close it.
func TestClosePair_AuthorizedOnBothEntities(t *testing.T) {
	for _, deny := range []string{"entity-a", "entity-b"} {
		s := newStubStoreReal()
		seedEntry(s, "e1", domain.MatchStatusMatched)
		authz := &entityAuthZ{deny: deny}
		r := newRouter(s, authz, &stubLedger{})
		resp := doReq(r, http.MethodPost, "/v1/intercompany/entries/e1/close", map[string]string{"reason": "r"}, "user-1")
		if resp.Code != http.StatusForbidden {
			t.Errorf("deny %s: expected 403, got %d", deny, resp.Code)
		}
		if s.entries["e1"].MatchStatus != domain.MatchStatusMatched {
			t.Errorf("deny %s: pair was closed anyway", deny)
		}
	}
}

func TestMatchEntry_ClosedPair_Refused(t *testing.T) {
	s := newStubStoreReal()
	seedEntry(s, "e1", domain.MatchStatusClosed)
	resp := doReq(newRouter(s, &stubAuthZ{}, &stubLedger{}), http.MethodPost, "/v1/intercompany/entries/e1/match",
		map[string]string{"target_journal_id": "j"}, "user-1")
	if resp.Code != http.StatusUnprocessableEntity {
		t.Fatalf("expected 422, got %d", resp.Code)
	}
}

func TestListTransitions(t *testing.T) {
	s := newStubStoreReal()
	seedEntry(s, "e1", domain.MatchStatusMatched)
	from := domain.MatchStatusUnmatched
	s.transitions = map[string][]domain.PairTransition{"e1": {
		{TransitionID: 1, IntercompanyEntryID: "e1", ToState: domain.MatchStatusUnmatched, Actor: "u"},
		{TransitionID: 2, IntercompanyEntryID: "e1", FromState: &from, ToState: domain.MatchStatusMatched, Actor: "u"},
	}}
	r := newRouter(s, &stubAuthZ{}, &stubLedger{})

	resp := doReq(r, http.MethodGet, "/v1/intercompany/entries/e1/transitions", nil, "user-1")
	if resp.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", resp.Code, resp.Body.String())
	}
	var got []domain.PairTransition
	_ = json.NewDecoder(resp.Body).Decode(&got)
	if len(got) != 2 || got[0].FromState != nil || got[1].ToState != domain.MatchStatusMatched {
		t.Fatalf("unexpected transitions: %+v", got)
	}

	if resp := doReq(r, http.MethodGet, "/v1/intercompany/entries/missing/transitions", nil, "user-1"); resp.Code != http.StatusNotFound {
		t.Fatalf("expected 404 for an unknown pair, got %d", resp.Code)
	}
}
