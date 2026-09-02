package handler

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"go.uber.org/zap"

	authzpkg "zoiko.io/retention-registry-svc/internal/authz"
	"zoiko.io/retention-registry-svc/internal/domain"
)

// TestGetLegalHold_Authorized proves the fix for the self-documented gap:
// GetLegalHold used to skip authorization entirely, so any caller holding a
// legal_hold_id could read scope_description, custodians and authority for
// any hold. A granting authz check must still let a legitimate read through.
func TestGetLegalHold_Authorized(t *testing.T) {
	h, _, _ := newTestHandler()
	r := newTestRouter(h)

	wHold := httptest.NewRecorder()
	r.ServeHTTP(wHold, buildRequest(http.MethodPost, "/v1/legal-holds", domain.CreateLegalHoldRequest{
		ScopeDescription: "Litigation hold — case #9001",
		Authority:        "Legal Counsel — Case #9001",
	}))
	var hld domain.LegalHold
	_ = json.Unmarshal(wHold.Body.Bytes(), &hld)

	wGet := httptest.NewRecorder()
	r.ServeHTTP(wGet, buildRequest(http.MethodGet, "/v1/legal-holds/"+hld.LegalHoldID, nil))
	if wGet.Code != http.StatusOK {
		t.Fatalf("expected 200 for an authorized read, got %d — %s", wGet.Code, wGet.Body.String())
	}
	var got domain.LegalHold
	if err := json.Unmarshal(wGet.Body.Bytes(), &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got.LegalHoldID != hld.LegalHoldID {
		t.Fatalf("expected the requested hold, got %+v", got)
	}
}

// TestGetLegalHold_AuthorizationDenied403 is the regression test for the
// fix itself: a denied authorization check must now stop the read, where
// previously there was no check at all to deny.
func TestGetLegalHold_AuthorizationDenied403(t *testing.T) {
	logger, _ := zap.NewDevelopment()
	st := &stubStore{}
	h := New(st, &stubPublisher{}, &stubAuthz{}, logger)
	r := newTestRouter(h)

	wHold := httptest.NewRecorder()
	r.ServeHTTP(wHold, buildRequest(http.MethodPost, "/v1/legal-holds", domain.CreateLegalHoldRequest{
		ScopeDescription: "Litigation hold — case #9002",
		Authority:        "Legal Counsel — Case #9002",
	}))
	var hld domain.LegalHold
	_ = json.Unmarshal(wHold.Body.Bytes(), &hld)

	// Swap in a denying authz client for the read only.
	h.authz = &stubAuthz{err: authzpkg.ErrAuthorizationDenied}

	wGet := httptest.NewRecorder()
	r.ServeHTTP(wGet, buildRequest(http.MethodGet, "/v1/legal-holds/"+hld.LegalHoldID, nil))
	if wGet.Code != http.StatusForbidden {
		t.Fatalf("FABRICATION: expected 403 on denied authorization, got %d — %s", wGet.Code, wGet.Body.String())
	}
}

// TestGetLegalHold_MissingPrincipal_Returns401 pins that the route now
// requires a resolved principal at all, where previously it required none.
func TestGetLegalHold_MissingPrincipal_Returns401(t *testing.T) {
	h, st, _ := newTestHandler()
	st.holds = append(st.holds, domain.LegalHold{LegalHoldID: "hold-no-principal", HoldStatus: "ACTIVE"})
	r := newTestRouter(h)

	req := httptest.NewRequest(http.MethodGet, "/v1/legal-holds/hold-no-principal", nil)
	// Deliberately no X-Principal-Id.
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 with no principal, got %d — %s", w.Code, w.Body.String())
	}
}
