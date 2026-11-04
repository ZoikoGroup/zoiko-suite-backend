package handler_test

import (
	"net/http"
	"testing"

	"github.com/google/uuid"
	"go.uber.org/zap"

	"zoiko.io/reporting-orchestration-svc/internal/authz"
	"zoiko.io/reporting-orchestration-svc/internal/envelope"
	"zoiko.io/reporting-orchestration-svc/internal/handler"
)

// routerNoDBWith is newRouterNoDB with a caller-supplied authorizer. Store
// dependencies are nil: only paths refused before storage may be driven.
func routerNoDBWith(a handler.Authorizer) http.Handler {
	return handler.NewRouter(handler.NewHandler(handler.Dependencies{
		Archives: fakeArchives{}, Retentions: fakeRetention{}, Authz: a,
		Ready: fakePinger{}, Logger: zap.NewNop(),
	}))
}

// TestReadsRequirePrincipal: all nine tenant-data reads are 401 without
// X-Principal-Id, and authorization-svc is never asked.
func TestReadsRequirePrincipal(t *testing.T) {
	policy := newPolicyAuthz()
	c := newClient(t, routerNoDBWith(policy))
	id := uuid.NewString()
	for _, path := range []string{
		"/v1/reports/definitions", "/v1/reports/definitions/" + id,
		"/v1/reports/runs", "/v1/reports/runs/" + id,
		"/v1/audit-exports", "/v1/audit-exports/" + id,
		"/v1/audit-exports/" + id + "/manifest", "/v1/audit-exports/" + id + "/redactions",
		"/v1/audit-exports/" + id + "/deliveries",
	} {
		expectProblem(t, c.do(http.MethodGet, path, nil, drop("X-Principal-Id")), http.StatusUnauthorized, "PRINCIPAL_REQUIRED")
	}
	if len(policy.calls) != 0 {
		t.Fatalf("authz must not be asked without a principal: %v", policy.calls)
	}
}

// TestListReadAuthorization: lists are authorized before storage — deny is
// 403, unreachable authorization-svc is 503 — and the scope is the
// legal_entity_id filter, else X-Legal-Entity-Id, else the tenant.
func TestListReadAuthorization(t *testing.T) {
	policy := newPolicyAuthz()
	policy.override["REPORT_DEFINITION_READ"] = authz.ErrAuthorizationDenied
	policy.override["REPORT_RUN_READ"] = authz.ErrAuthorizationDenied
	policy.override["AUDIT_EXPORT_READ"] = authz.ErrAuthzServiceUnavailable
	c := newClient(t, routerNoDBWith(policy))

	expectProblem(t, c.do(http.MethodGet, "/v1/reports/definitions?legal_entity_id=LE-filter", nil), http.StatusForbidden, "AUTHORIZATION_DENIED")
	expectProblem(t, c.do(http.MethodGet, "/v1/reports/runs", nil), http.StatusForbidden, "AUTHORIZATION_DENIED")
	expectProblem(t, c.do(http.MethodGet, "/v1/audit-exports", nil, drop("X-Legal-Entity-Id")), http.StatusServiceUnavailable, "AUTHZ_UNAVAILABLE")

	want := []string{
		c.tenant + "|principal-01|LE-filter|REPORT_DEFINITION_READ",
		c.tenant + "|principal-01|" + c.entity + "|REPORT_RUN_READ",
		c.tenant + "|principal-01|" + c.tenant + "|AUDIT_EXPORT_READ",
	}
	if len(policy.calls) != len(want) {
		t.Fatalf("calls=%v", policy.calls)
	}
	for i := range want {
		if policy.calls[i] != want[i] {
			t.Fatalf("call %d: got %q want %q", i, policy.calls[i], want[i])
		}
	}
}

// TestWriteWithoutIdempotencyKeyRefusedInObserveMode: with envelope
// enforcement in observe mode the envelope admits a key-less write; the
// service itself must still refuse it with 400 before any handler runs.
func TestWriteWithoutIdempotencyKeyRefusedInObserveMode(t *testing.T) {
	for _, mode := range []string{"observe", "write_strict", "strict"} {
		t.Run(mode, func(t *testing.T) {
			t.Setenv(envelope.EnvVarMode, mode)
			policy := newPolicyAuthz()
			c := newClient(t, routerNoDBWith(policy))
			rec := c.do(http.MethodPost, "/v1/reports/definitions", c.definitionBody("CASH_FLOW"), drop("Idempotency-Key"))
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("mode %s: expected 400, got %d: %s", mode, rec.Code, rec.Body.String())
			}
			if mode == "observe" {
				expectProblem(t, rec, http.StatusBadRequest, "IDEMPOTENCY_KEY_REQUIRED")
			}
			if len(policy.calls) != 0 {
				t.Fatalf("a key-less write must not reach authorization: %v", policy.calls)
			}
		})
	}
}

// TestObjectReadAuthorization (Postgres): reads of one definition, run or
// export are authorized against that object's legal entity; a denied
// principal gets 403 on every one of them.
func TestObjectReadAuthorization(t *testing.T) {
	policy := newPolicyAuthz()
	router, _ := newRouterWith(t, policy)
	c := newClient(t, router)

	definitionID := c.activeDefinition("CASH_FLOW")
	run := expectStatus(t, c.do(http.MethodPost, "/v1/reports/definitions/"+definitionID+"/runs", nil), http.StatusAccepted)
	runID := run["id"].(string)
	export := expectStatus(t, c.do(http.MethodPost, "/v1/audit-exports", map[string]string{
		"legal_entity_id": c.entity, "archive_id": "arch-" + uuid.NewString()[:6], "purpose": "read authz"}), http.StatusCreated)
	exportID := export["export_id"].(string)

	for _, action := range []string{"REPORT_DEFINITION_READ", "REPORT_RUN_READ", "AUDIT_EXPORT_READ"} {
		policy.deny["reader-denied|"+action] = true
	}
	cases := []struct{ path, action string }{
		{"/v1/reports/definitions/" + definitionID, "REPORT_DEFINITION_READ"},
		{"/v1/reports/runs/" + runID, "REPORT_RUN_READ"},
		{"/v1/reports/runs?definition_id=" + definitionID, "REPORT_RUN_READ"},
		{"/v1/audit-exports/" + exportID, "AUDIT_EXPORT_READ"},
		{"/v1/audit-exports/" + exportID + "/manifest", "AUDIT_EXPORT_READ"},
		{"/v1/audit-exports/" + exportID + "/redactions", "AUDIT_EXPORT_READ"},
		{"/v1/audit-exports/" + exportID + "/deliveries", "AUDIT_EXPORT_READ"},
	}
	for _, tc := range cases {
		// No X-Legal-Entity-Id: the scope must come from the stored object.
		expectProblem(t, c.do(http.MethodGet, tc.path, nil, as("reader-denied"), drop("X-Legal-Entity-Id")),
			http.StatusForbidden, "AUTHORIZATION_DENIED")
		last := policy.calls[len(policy.calls)-1]
		if want := c.tenant + "|reader-denied|" + c.entity + "|" + tc.action; last != want {
			t.Fatalf("%s: authz call %q, want %q", tc.path, last, want)
		}
		expectStatus(t, c.do(http.MethodGet, tc.path, nil, as("reader-2")), http.StatusOK)
	}
}
