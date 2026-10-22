package handler

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"zoiko.io/retention-registry-svc/internal/domain"
)

// A body tenant_id on create-policy / create-hold is never a source of tenant
// identity: it may only agree with the verified X-Tenant-Id. With no verified
// tenant it used to pass the handler's check, reach authorization and then fail
// at the database's row-level-security WITH CHECK as an opaque 500.

type countingAuthz struct{ calls int }

func (c *countingAuthz) CheckAllowed(_ context.Context, _, _, _ string) error {
	c.calls++
	return nil
}

type tenantWriteCase struct {
	name, path string
	body       func(tenant string) interface{}
}

func tenantWriteCases() []tenantWriteCase {
	return []tenantWriteCase{
		{"retention policy", "/v1/retention-policies", func(tenant string) interface{} {
			return domain.CreateRetentionPolicyRequest{
				RecordClass: "FINANCIAL_LEDGER", LegalRegulatoryBasis: "Tax compliance",
				EffectiveFrom: "2026-01-01T00:00:00Z", MinRetentionDays: 365, TenantID: tenant,
			}
		}},
		{"legal hold", "/v1/legal-holds", func(tenant string) interface{} {
			return domain.CreateLegalHoldRequest{ScopeDescription: "matter 42", Authority: "High Court", TenantID: tenant}
		}},
	}
}

func post(t *testing.T, h *Handler, path string, body interface{}, verifiedTenant string) *httptest.ResponseRecorder {
	t.Helper()
	w := httptest.NewRecorder()
	newTestRouter(h).ServeHTTP(w, buildRequestAs(http.MethodPost, path, body, verifiedTenant))
	return w
}

func stored(st *stubStore) int { return len(st.policies) + len(st.holds) }

func assertNoDBDetail(t *testing.T, body string) {
	t.Helper()
	for _, leak := range []string{"row-level", "row level", "policy", "postgres", "pgx", "sqlstate", "violates"} {
		// "retention policy" is domain vocabulary, not a leak; only check the error text.
		if leak == "policy" {
			continue
		}
		if strings.Contains(strings.ToLower(body), leak) {
			t.Fatalf("response leaks database detail %q: %s", leak, body)
		}
	}
}

func TestTenantWrite_BodyTenantWithoutVerifiedTenant_Is403_NothingStored(t *testing.T) {
	for _, c := range tenantWriteCases() {
		t.Run(c.name, func(t *testing.T) {
			h, st, _ := newTestHandler()
			w := post(t, h, c.path, c.body(tenantA), "")
			if w.Code != http.StatusForbidden {
				t.Fatalf("expected 403, got %d — %s", w.Code, w.Body.String())
			}
			assertNoDBDetail(t, w.Body.String())
			if stored(st) != 0 || len(st.outboxEvents) != 0 {
				t.Fatalf("nothing may be stored: %d rows, %d outbox events", stored(st), len(st.outboxEvents))
			}
		})
	}
}

func TestTenantWrite_MalformedBodyTenant_Is400_WithAndWithoutVerifiedTenant(t *testing.T) {
	for _, c := range tenantWriteCases() {
		for _, verified := range []string{"", tenantA} {
			t.Run(c.name+"/verified="+verified, func(t *testing.T) {
				h, st, _ := newTestHandler()
				w := post(t, h, c.path, c.body("not-a-uuid"), verified)
				if w.Code != http.StatusBadRequest {
					t.Fatalf("expected 400, got %d — %s", w.Code, w.Body.String())
				}
				if stored(st) != 0 {
					t.Fatalf("nothing may be stored, got %d", stored(st))
				}
			})
		}
	}
}

func TestTenantWrite_MatchingBodyTenant_Succeeds(t *testing.T) {
	for _, c := range tenantWriteCases() {
		t.Run(c.name, func(t *testing.T) {
			h, st, _ := newTestHandler()
			w := post(t, h, c.path, c.body(tenantA), tenantA)
			if w.Code != http.StatusCreated {
				t.Fatalf("expected 201, got %d — %s", w.Code, w.Body.String())
			}
			if stored(st) != 1 || len(st.outboxEvents) != 1 {
				t.Fatalf("expected 1 row and 1 outbox event, got %d / %d", stored(st), len(st.outboxEvents))
			}
		})
	}
}

func TestTenantWrite_ConflictingBodyTenant_Is403_NothingStored(t *testing.T) {
	for _, c := range tenantWriteCases() {
		t.Run(c.name, func(t *testing.T) {
			h, st, _ := newTestHandler()
			w := post(t, h, c.path, c.body(tenantB), tenantA)
			if w.Code != http.StatusForbidden {
				t.Fatalf("expected 403, got %d — %s", w.Code, w.Body.String())
			}
			if stored(st) != 0 {
				t.Fatalf("nothing may be stored, got %d", stored(st))
			}
		})
	}
}

// A platform-wide request has no body tenant_id; it needs no tenant header and
// a tenant-bearing caller may also make one (authorized at platform scope).
func TestTenantWrite_PlatformWide_WithAndWithoutVerifiedTenant_Succeeds(t *testing.T) {
	for _, c := range tenantWriteCases() {
		for _, verified := range []string{"", tenantA} {
			t.Run(c.name+"/verified="+verified, func(t *testing.T) {
				h, st, _ := newTestHandler()
				w := post(t, h, c.path, c.body(""), verified)
				if w.Code != http.StatusCreated {
					t.Fatalf("expected 201, got %d — %s", w.Code, w.Body.String())
				}
				if stored(st) != 1 {
					t.Fatalf("expected 1 row, got %d", stored(st))
				}
				if len(st.policies) == 1 && st.policies[0].TenantID != nil || len(st.holds) == 1 && st.holds[0].TenantID != nil {
					t.Fatalf("a platform-wide row must carry no tenant_id")
				}
			})
		}
	}
}

// The refusal comes before authorization and before any store call: a body
// tenant with no verified tenant must not reach authorization-svc or the database.
func TestTenantWrite_BodyTenantWithoutVerifiedTenant_NeverReachesAuthorizationOrStore(t *testing.T) {
	for _, c := range tenantWriteCases() {
		t.Run(c.name, func(t *testing.T) {
			h, st, _ := newTestHandler()
			az := &countingAuthz{}
			h.authz = az
			if w := post(t, h, c.path, c.body(tenantA), ""); w.Code != http.StatusForbidden {
				t.Fatalf("expected 403, got %d", w.Code)
			}
			if az.calls != 0 || stored(st) != 0 {
				t.Fatalf("authorization calls=%d stored=%d, both must be 0", az.calls, stored(st))
			}
		})
	}
}
