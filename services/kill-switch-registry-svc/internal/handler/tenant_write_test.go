package handler

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"zoiko.io/kill-switch-registry-svc/internal/domain"
)

// Body tenant_id on engage/disengage is never a source of tenant identity: it
// may only agree with the verified X-Tenant-Id. With no verified tenant it used
// to pass the handler's check, reach authorization and then fail at the
// database's row-level-security WITH CHECK as a 500.

func engageBody(tenant string) domain.EngageKillSwitchRequest {
	return domain.EngageKillSwitchRequest{
		Domain: "AUTOMATION_ACTION", TenantID: tenant, Reason: "tenant write test",
		ReconciliationProcedureRef: "runbook:test", ApprovedByPrincipalID: "sre-lead-approver-2",
	}
}

func disengageBody(tenant string) domain.DisengageKillSwitchRequest {
	return domain.DisengageKillSwitchRequest{
		Domain: "AUTOMATION_ACTION", TenantID: tenant, Reason: "tenant write test",
		ApprovedByPrincipalID: "sre-lead-approver-2",
	}
}

func do(t *testing.T, h *Handler, path string, body interface{}, verifiedTenant string) *httptest.ResponseRecorder {
	t.Helper()
	req := buildRequest(http.MethodPost, path, body)
	if verifiedTenant != "" {
		req.Header.Set("X-Tenant-Id", verifiedTenant)
	}
	w := httptest.NewRecorder()
	newTestRouter(h).ServeHTTP(w, req)
	return w
}

func assertNoInternals(t *testing.T, body string) {
	t.Helper()
	for _, leak := range []string{"row-level", "row level", "policy", "postgres", "pgx", "SQLSTATE", "violates"} {
		if strings.Contains(strings.ToLower(body), strings.ToLower(leak)) {
			t.Fatalf("response leaks database detail %q: %s", leak, body)
		}
	}
}

func TestEngage_BodyTenantWithoutVerifiedTenant_Is403_NothingWritten(t *testing.T) {
	h, st, _ := newTestHandler()
	w := do(t, h, "/v1/kill-switches/engage", engageBody(testTenantID1), "")
	if w.Code != http.StatusForbidden {
		t.Fatalf("expected 403, got %d — %s", w.Code, w.Body.String())
	}
	assertNoInternals(t, w.Body.String())
	if len(st.events) != 0 || len(st.outboxEvents) != 0 {
		t.Fatalf("nothing may be stored: %d events, %d outbox", len(st.events), len(st.outboxEvents))
	}
}

func TestDisengage_BodyTenantWithoutVerifiedTenant_Is403_NothingWritten(t *testing.T) {
	h, st, _ := newTestHandler()
	// An ENGAGE exists for that tenant, so a 409 here would mean the request got past the tenant check.
	st.events = append(st.events, domain.KillSwitchEvent{
		Domain: strp("AUTOMATION_ACTION"), TenantID: strp(testTenantID1), Action: domain.KillSwitchActionEngage,
	})
	w := do(t, h, "/v1/kill-switches/disengage", disengageBody(testTenantID1), "")
	if w.Code != http.StatusForbidden {
		t.Fatalf("expected 403, got %d — %s", w.Code, w.Body.String())
	}
	assertNoInternals(t, w.Body.String())
	if len(st.events) != 1 || len(st.outboxEvents) != 0 {
		t.Fatalf("nothing may be stored: %d events, %d outbox", len(st.events), len(st.outboxEvents))
	}
}

func TestEngageThenDisengage_MatchingVerifiedTenant_Succeeds(t *testing.T) {
	h, st, _ := newTestHandler()
	if w := do(t, h, "/v1/kill-switches/engage", engageBody(testTenantID1), testTenantID1); w.Code != http.StatusCreated {
		t.Fatalf("engage: expected 201, got %d — %s", w.Code, w.Body.String())
	}
	if w := do(t, h, "/v1/kill-switches/disengage", disengageBody(testTenantID1), testTenantID1); w.Code != http.StatusOK {
		t.Fatalf("disengage: expected 200, got %d — %s", w.Code, w.Body.String())
	}
	if len(st.events) != 2 || len(st.outboxEvents) != 2 {
		t.Fatalf("expected 2 events and 2 outbox events, got %d / %d", len(st.events), len(st.outboxEvents))
	}
}

func TestEngageAndDisengage_ConflictingBodyTenant_Is403(t *testing.T) {
	h, st, _ := newTestHandler()
	if w := do(t, h, "/v1/kill-switches/engage", engageBody(testTenantID2), testTenantID1); w.Code != http.StatusForbidden {
		t.Fatalf("engage: expected 403, got %d — %s", w.Code, w.Body.String())
	}
	if w := do(t, h, "/v1/kill-switches/disengage", disengageBody(testTenantID2), testTenantID1); w.Code != http.StatusForbidden {
		t.Fatalf("disengage: expected 403, got %d — %s", w.Code, w.Body.String())
	}
	if len(st.events) != 0 {
		t.Fatalf("nothing may be stored, got %d events", len(st.events))
	}
}

func TestEngageAndDisengage_InvalidBodyTenant_Is400_WithAndWithoutVerifiedTenant(t *testing.T) {
	for _, verified := range []string{"", testTenantID1} {
		h, st, _ := newTestHandler()
		if w := do(t, h, "/v1/kill-switches/engage", engageBody("not-a-uuid"), verified); w.Code != http.StatusBadRequest {
			t.Fatalf("engage (verified=%q): expected 400, got %d — %s", verified, w.Code, w.Body.String())
		}
		if w := do(t, h, "/v1/kill-switches/disengage", disengageBody("not-a-uuid"), verified); w.Code != http.StatusBadRequest {
			t.Fatalf("disengage (verified=%q): expected 400, got %d — %s", verified, w.Code, w.Body.String())
		}
		if len(st.events) != 0 {
			t.Fatalf("nothing may be stored, got %d events", len(st.events))
		}
	}
}

// A genuinely platform-wide switch has no tenant_id and needs no tenant header;
// a tenant-bearing caller may also engage one (authorized at platform scope).
func TestEngage_PlatformWide_WithAndWithoutVerifiedTenant_Succeeds(t *testing.T) {
	for _, verified := range []string{"", testTenantID1} {
		h, st, _ := newTestHandler()
		if w := do(t, h, "/v1/kill-switches/engage", engageBody(""), verified); w.Code != http.StatusCreated {
			t.Fatalf("verified=%q: expected 201, got %d — %s", verified, w.Code, w.Body.String())
		}
		if len(st.events) != 1 || st.events[0].TenantID != nil {
			t.Fatalf("expected one platform-wide event, got %+v", st.events)
		}
	}
}

// The new refusal comes before authorization: a body tenant with no verified
// tenant must not even reach authorization-svc.
func TestEngage_BodyTenantWithoutVerifiedTenant_DoesNotReachAuthorization(t *testing.T) {
	h, _, _ := newTestHandler()
	az := &countingAuthz{}
	h.authz = az
	if w := do(t, h, "/v1/kill-switches/engage", engageBody(testTenantID1), ""); w.Code != http.StatusForbidden {
		t.Fatalf("expected 403, got %d", w.Code)
	}
	if az.calls != 0 {
		t.Fatalf("authorization must not be consulted, got %d calls", az.calls)
	}
}

func strp(s string) *string { return &s }

type countingAuthz struct{ calls int }

func (c *countingAuthz) CheckAllowed(_ context.Context, _, _, _ string) error {
	c.calls++
	return nil
}
