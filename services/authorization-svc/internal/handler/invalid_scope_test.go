package handler_test

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"zoiko.io/authorization-svc/internal/handler"
)

// A mistyped scope is a 400, not an outage.
//
// ── THE DEFECT ──────────────────────────────────────────────────────────────
//
// legal_entity_id and tenant_id are UUID in every table this service compares
// them against. Passing a non-UUID to a uuid comparison is a DRIVER error,
// which the store wraps as ErrStoreUnavailable and every handler answers 503
// for. From a calling service that 503 is indistinguishable from
// authorization-svc being down — so a caller that mistyped an entity id was
// told the platform's authorization plane had failed.
//
// known-gaps.md recorded this and put the remedy on the CALLERS: "callers must
// therefore validate the scope themselves before asking". That is the wrong
// place — the workaround has to be written 111 times, each copy has to know
// which of this service's columns are uuid, and any caller that forgets reports
// an outage instead of a typo.
//
// The tenant case is the one that was easiest to miss, and it is the worse of
// the two: withRLS installs the raw value into app.tenant_id, and the POLICY
// does the `::uuid` cast. So a malformed tenant does not fail in a query this
// service wrote — it fails inside row security, on every table.
//
// ── WHY THESE TESTS EXIST AS A GROUP ────────────────────────────────────────
//
// Every route that takes a scope needs one, and there are seven of them across
// three code paths (resolveTenantScope, requireTenant, resolvePlatformScope).
// A route added without validation regresses silently: the failure only appears
// against a real database, as a 503, on somebody else's typo.

func decodeError(t *testing.T, w *httptest.ResponseRecorder) map[string]string {
	t.Helper()
	var body map[string]string
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("unmarshal %q: %v", w.Body.String(), err)
	}
	return body
}

const goodTenant = "11111111-1111-4111-8111-111111111111"
const goodEntity = "11111111-1111-4111-8111-aaaaaaaaaaa1"

// ── /v1/authorize ───────────────────────────────────────────────────────────

func TestAuthorize_MalformedEntityIsBadRequestNotOutage(t *testing.T) {
	store := grantingStore()
	r := newTestRouter(store)

	body := `{"principal_id":"p-1","legal_entity_id":"not-a-uuid","action_type":"PAYMENT_APPROVE","tenant_id":"` + goodTenant + `"}`
	req := httptest.NewRequest(http.MethodPost, handler.AuthorizePath, bytes.NewBufferString(body))
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d: %s — a 503 here reads to the caller as this service being down", w.Code, w.Body.String())
	}
	got := decodeError(t, w)
	if got["error"] != "invalid_scope" {
		t.Errorf("error = %q, want invalid_scope", got["error"])
	}
	if got["field"] != "legal_entity_id" {
		t.Errorf("field = %q, want legal_entity_id — the caller has to know which value was wrong", got["field"])
	}
	// Nothing was evaluated, so nothing may be recorded: a decision artifact
	// for a request that could not be evaluated would be evidence of a
	// decision nobody made.
	if store.recordedParams.ActionType != "" {
		t.Errorf("a decision was recorded for an unevaluable request: %+v", store.recordedParams)
	}
	if store.grantedTenantArg != "" {
		t.Error("the store was consulted with a value that would have produced a driver error")
	}
}

func TestAuthorize_MalformedTenantHeaderIsBadRequest(t *testing.T) {
	store := grantingStore()
	r := newTestRouter(store)

	body := `{"principal_id":"p-1","legal_entity_id":"` + goodEntity + `","action_type":"PAYMENT_APPROVE"}`
	req := httptest.NewRequest(http.MethodPost, handler.AuthorizePath, bytes.NewBufferString(body))
	req.Header.Set("X-Tenant-Id", "tenant-one")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d: %s", w.Code, w.Body.String())
	}
	got := decodeError(t, w)
	// The HEADER is named, not "tenant_id": a caller that set a header and is
	// told a body field is wrong looks for a field it never sent.
	if got["field"] != "X-Tenant-Id" {
		t.Errorf("field = %q, want X-Tenant-Id", got["field"])
	}
}

// The body fallback path — the pre-header calling convention that ~86 of this
// endpoint's callers still use. It gets the same check, and the message names
// the BODY field, because that is where their value came from.
func TestAuthorize_MalformedTenantBodyNamesTheBodyField(t *testing.T) {
	r := newTestRouter(grantingStore())

	body := `{"principal_id":"p-1","legal_entity_id":"` + goodEntity + `","action_type":"PAYMENT_APPROVE","tenant_id":"acme"}`
	req := httptest.NewRequest(http.MethodPost, handler.AuthorizePath, bytes.NewBufferString(body))
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d: %s", w.Code, w.Body.String())
	}
	if got := decodeError(t, w); got["field"] != "tenant_id" {
		t.Errorf("field = %q, want tenant_id — this caller set a body field, not a header", got["field"])
	}
}

// A body tenant that DISAGREES with the header is still a 403, and that check
// runs first. Order matters: telling a caller its value is malformed when the
// real problem is that it is trying to be evaluated in somebody else's scope
// would hide the more serious of the two.
func TestAuthorize_TenantMismatchStillBeatsScopeValidation(t *testing.T) {
	r := newTestRouter(grantingStore())

	body := `{"principal_id":"p-1","legal_entity_id":"` + goodEntity + `","action_type":"PAYMENT_APPROVE","tenant_id":"someone-else"}`
	req := httptest.NewRequest(http.MethodPost, handler.AuthorizePath, bytes.NewBufferString(body))
	req.Header.Set("X-Tenant-Id", goodTenant)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusForbidden {
		t.Fatalf("expected 403 tenant_scope_mismatch, got %d: %s", w.Code, w.Body.String())
	}
}

// THE OTHER HALF, and the one that would be wrong to get wrong: principal_id is
// TEXT in every table and compared as text, so a non-UUID principal is a valid
// comparison that matches nothing. Validating it would refuse the
// service-account ids this service has never required to be UUIDs — and it is
// exactly the kind of over-application a "validate the scope" change invites.
func TestAuthorize_NonUuidPrincipalIsAccepted(t *testing.T) {
	store := grantingStore()
	r := newTestRouter(store)

	body := `{"principal_id":"svc-accounts-payable","legal_entity_id":"` + goodEntity + `","action_type":"PAYMENT_APPROVE","tenant_id":"` + goodTenant + `"}`
	req := httptest.NewRequest(http.MethodPost, handler.AuthorizePath, bytes.NewBufferString(body))
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200 for a non-UUID principal, got %d: %s — principal_id is TEXT, and refusing one would break every service account",
			w.Code, w.Body.String())
	}
	if store.recordedParams.PrincipalID != "svc-accounts-payable" {
		t.Errorf("recorded principal = %q", store.recordedParams.PrincipalID)
	}
}

// The sentinel is not a UUID and must not be refused as one. It is the whole
// point of PlatformScopeSentinel that a platform-wide act has somewhere to put
// itself.
func TestAuthorize_PlatformSentinelIsNotRefusedAsMalformed(t *testing.T) {
	store := grantingStore()
	r := newTestRouter(store)

	body := `{"principal_id":"p-1","legal_entity_id":"PLATFORM","action_type":"PAYMENT_APPROVE","tenant_id":"` + goodTenant + `"}`
	req := httptest.NewRequest(http.MethodPost, handler.AuthorizePath, bytes.NewBufferString(body))
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	// Recorded against the RESOLVED entity, never the sentinel — the evidence
	// has to name the scope the decision was actually evaluated in.
	if store.recordedParams.LegalEntityID != "platform-scope-entity" {
		t.Errorf("recorded entity = %q, want the configured platform-scope entity",
			store.recordedParams.LegalEntityID)
	}
}

// ── the three §8.3 validation routes ────────────────────────────────────────

func TestValidationRoutes_MalformedEntityIsBadRequest(t *testing.T) {
	cases := []struct{ name, path, body string }{
		{
			"entity scope",
			handler.EntityScopeValidatePath,
			`{"principal_id":"p-1","legal_entity_ids":["not-a-uuid"]}`,
		},
		{
			"entity scope, one bad among good",
			handler.EntityScopeValidatePath,
			`{"principal_id":"p-1","legal_entity_ids":["` + goodEntity + `","nope"]}`,
		},
		{
			"sod validate",
			handler.SoDValidatePath,
			`{"principal_id":"p-1","legal_entity_id":"nope","candidate_actions":["PAYMENT_APPROVE"]}`,
		},
		{
			"delegated access",
			handler.DelegatedAccessEvaluatePath,
			`{"principal_id":"p-1","legal_entity_id":"nope"}`,
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			store := grantingStore()
			r := newTestRouter(store)
			w := postJSON(t, r, c.path, c.body, adminHeaders())

			if w.Code != http.StatusBadRequest {
				t.Fatalf("expected 400, got %d: %s", w.Code, w.Body.String())
			}
			if got := decodeError(t, w); got["error"] != "invalid_scope" {
				t.Errorf("error = %q, want invalid_scope", got["error"])
			}
		})
	}
}

// ── the admin reads ─────────────────────────────────────────────────────────

// requireTenant covers every /v1/admin/* read and the two decision reads, so a
// malformed header on any of them is a 400 rather than a 503 from inside row
// security.
func TestAdminReads_MalformedTenantHeaderIsBadRequest(t *testing.T) {
	paths := []string{
		"/v1/admin/roles",
		"/v1/admin/role-assignments",
		"/v1/admin/delegated-authorities",
		"/v1/admin/sod-rules",
		"/v1/admin/abac-rules",
		handler.AccessDecisionsPath,
		handler.AccessDecisionsPath + "/11111111-1111-4111-8111-999999999999",
	}

	for _, path := range paths {
		t.Run(path, func(t *testing.T) {
			r := newTestRouter(emptyPageStore())

			req := httptest.NewRequest(http.MethodGet, path, nil)
			req.Header.Set("X-Principal-Id", "p-caller")
			req.Header.Set("X-Tenant-Id", "acme-corp")
			w := httptest.NewRecorder()
			r.ServeHTTP(w, req)

			if w.Code != http.StatusBadRequest {
				t.Fatalf("expected 400, got %d: %s", w.Code, w.Body.String())
			}
			got := decodeError(t, w)
			if got["error"] != "invalid_scope" {
				t.Errorf("error = %q, want invalid_scope", got["error"])
			}
			if got["field"] != "X-Tenant-Id" {
				t.Errorf("field = %q, want X-Tenant-Id", got["field"])
			}
		})
	}
}

// A MISSING tenant is still 401, not 400. The two are different facts — "you
// did not identify your organisation" and "the organisation you named is not a
// reference" — and the first is an authentication problem.
func TestMissingTenantIsStillUnauthorizedNotBadRequest(t *testing.T) {
	r := newTestRouter(emptyPageStore())

	req := httptest.NewRequest(http.MethodGet, "/v1/admin/roles", nil)
	req.Header.Set("X-Principal-Id", "p-caller")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 for a missing tenant, got %d: %s", w.Code, w.Body.String())
	}
	if got := decodeError(t, w); got["error"] != "missing_tenant_scope" {
		t.Errorf("error = %q, want missing_tenant_scope", got["error"])
	}
}
