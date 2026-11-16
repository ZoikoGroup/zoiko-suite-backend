package handler_test

// SPEC_DEVIATIONS A2: GET /v1/intercompany/entries used to skip
// authorization entirely when called with neither source_legal_entity_id
// nor target_legal_entity_id, returning every entry in the tenant to any
// caller with an X-Principal-Id. It now requires at least one of the two.

import (
	"net/http"
	"testing"

	"zoiko.io/intercompany-accounting-svc/internal/domain"
)

func TestListEntries_NoEntityFilter_Refused(t *testing.T) {
	s := newStubStoreReal()
	rr := doReq(newRouter(s, &stubAuthZ{}, ledgerWithSourceJournal("le-us", "USD")), http.MethodGet, "/v1/intercompany/entries/", nil, "p-1")
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("unfiltered list: want 400, got %d: %s", rr.Code, rr.Body.String())
	}
}

func TestListEntries_SourceOnlyFilter_Authorized(t *testing.T) {
	s := newStubStoreReal()
	r := newRouter(s, &stubAuthZ{}, ledgerWithSourceJournal("le-us", "USD"))
	rr := doReq(r, http.MethodPost, "/v1/intercompany/entries/", createBody(map[string]any{"fiscal_period": "2026-09"}), "p-1")
	if rr.Code != http.StatusCreated {
		t.Fatalf("create: %d %s", rr.Code, rr.Body.String())
	}
	rr = doReq(r, http.MethodGet, "/v1/intercompany/entries/?source_legal_entity_id=le-us", nil, "p-1")
	if rr.Code != http.StatusOK {
		t.Fatalf("filtered list: want 200, got %d: %s", rr.Code, rr.Body.String())
	}
}

func TestListEntries_TargetOnlyFilter_Authorized(t *testing.T) {
	s := newStubStoreReal()
	r := newRouter(s, &stubAuthZ{}, ledgerWithSourceJournal("le-us", "USD"))
	rr := doReq(r, http.MethodPost, "/v1/intercompany/entries/", createBody(map[string]any{"fiscal_period": "2026-09"}), "p-1")
	if rr.Code != http.StatusCreated {
		t.Fatalf("create: %d %s", rr.Code, rr.Body.String())
	}
	rr = doReq(r, http.MethodGet, "/v1/intercompany/entries/?target_legal_entity_id=le-uk", nil, "p-1")
	if rr.Code != http.StatusOK {
		t.Fatalf("filtered list: want 200, got %d: %s", rr.Code, rr.Body.String())
	}
}

func TestListEntries_AuthzDenied_Refused(t *testing.T) {
	s := newStubStoreReal()
	r := newRouter(s, &stubAuthZ{err: domain.ErrAuthorizationDenied}, ledgerWithSourceJournal("le-us", "USD"))
	rr := doReq(r, http.MethodGet, "/v1/intercompany/entries/?source_legal_entity_id=le-us", nil, "p-1")
	if rr.Code != http.StatusForbidden {
		t.Fatalf("denied entity: want 403, got %d: %s", rr.Code, rr.Body.String())
	}
}
