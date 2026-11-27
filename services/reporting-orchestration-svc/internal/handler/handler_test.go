package handler_test

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/google/uuid"

	"zoiko.io/reporting-orchestration-svc/internal/authz"
	"zoiko.io/reporting-orchestration-svc/internal/handler"
)

func TestHealthCheck(t *testing.T) {
	router := newRouterNoDB(t)
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/healthz", nil)) // no headers at all
	body := expectStatus(t, rec, http.StatusOK)
	if body["status"] != "ok" || body["service"] != "reporting-orchestration-svc" {
		t.Fatalf("unexpected body: %v", body)
	}
}

func TestReadyzReflectsDatabase(t *testing.T) {
	for _, tc := range []struct {
		name string
		ping fakePinger
		want int
	}{{"db up", fakePinger{}, http.StatusOK}, {"db down", fakePinger{err: errBoom}, http.StatusServiceUnavailable}} {
		t.Run(tc.name, func(t *testing.T) {
			router := handler.NewRouter(handler.NewHandler(handler.Dependencies{Authz: newPolicyAuthz(), Ready: tc.ping}))
			rec := httptest.NewRecorder()
			router.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/readyz", nil))
			expectStatus(t, rec, tc.want)
		})
	}
	t.Run("real pool", func(t *testing.T) {
		router := newRouter(t)
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/readyz", nil))
		if body := expectStatus(t, rec, http.StatusOK); body["status"] != "ready" {
			t.Fatalf("unexpected body %v", body)
		}
	})
}

// TestCreateDefinitionAndTriggerRun ports the pre-rewrite lifecycle test.
// Intentional change: the rewrite gives definitions the REP-05 lifecycle,
// so a new definition is DRAFT and must be VALIDATED then ACTIVE before a
// run may be requested (the old test expected ACTIVE on create).
func TestCreateDefinitionAndTriggerRun(t *testing.T) {
	c := newClient(t, newRouter(t))

	// 1. Create.
	def := expectStatus(t, c.do(http.MethodPost, "/v1/reports/definitions", c.definitionBody("FINANCIAL_SUMMARY")), http.StatusCreated)
	id, _ := def["id"].(string)
	if id == "" || def["status"] != "DRAFT" || def["version"].(float64) != 1 || def["created_by"] != "principal-01" {
		t.Fatalf("create: %v", def)
	}
	// A run on a DRAFT definition is refused.
	expectProblem(t, c.do(http.MethodPost, "/v1/reports/definitions/"+id+"/runs", nil), http.StatusConflict, "DEFINITION_NOT_ACTIVE")

	// 2. Get, 3. List.
	expectStatus(t, c.do(http.MethodGet, "/v1/reports/definitions/"+id, nil), http.StatusOK)
	list := expectStatus(t, c.do(http.MethodGet, "/v1/reports/definitions?legal_entity_id="+c.entity, nil), http.StatusOK)
	if list["count"].(float64) != 1 {
		t.Fatalf("list: %v", list)
	}

	// Lifecycle: DRAFT -> ACTIVE directly is illegal; stale If-Match is 412.
	status := "/v1/reports/definitions/" + id + "/status"
	expectProblem(t, c.do(http.MethodPatch, status, map[string]string{"status": "ACTIVE"}), http.StatusConflict, "ILLEGAL_TRANSITION")
	expectProblem(t, c.do(http.MethodPatch, status, map[string]string{"status": "VALIDATED"}, header("If-Match", `"7"`)),
		http.StatusPreconditionFailed, "VERSION_MISMATCH")
	expectStatus(t, c.do(http.MethodPatch, status, map[string]string{"status": "VALIDATED"}, header("If-Match", `"1"`)), http.StatusOK)
	expectStatus(t, c.do(http.MethodPatch, status, map[string]string{"status": "ACTIVE"}), http.StatusOK)

	// 4. Trigger run: recorded, not completed (202, NOT_IMPLEMENTED).
	run := expectStatus(t, c.do(http.MethodPost, "/v1/reports/definitions/"+id+"/runs",
		map[string]string{"triggered_by": "MANUAL", "period_start": "2026-07-01", "period_end": "2026-09-30"}), http.StatusAccepted)
	runID, _ := run["id"].(string)
	if runID == "" || run["status"] != "NOT_IMPLEMENTED" || run["row_count"].(float64) != 0 || run["output_location"] != nil {
		t.Fatalf("FABRICATION check failed: %v", run)
	}

	// 5. Get run, 6. List runs.
	got := expectStatus(t, c.do(http.MethodGet, "/v1/reports/runs/"+runID, nil), http.StatusOK)
	if got["definition_id"] != id {
		t.Fatalf("get run: %v", got)
	}
	runs := expectStatus(t, c.do(http.MethodGet, "/v1/reports/runs?definition_id="+id, nil), http.StatusOK)
	if runs["count"].(float64) != 1 {
		t.Fatalf("list runs: %v", runs)
	}

	// 7. Pause blocks runs; deprecate is terminal.
	expectStatus(t, c.do(http.MethodPatch, status, map[string]string{"status": "PAUSED"}), http.StatusOK)
	expectProblem(t, c.do(http.MethodPost, "/v1/reports/definitions/"+id+"/runs", nil), http.StatusConflict, "DEFINITION_NOT_ACTIVE")
	expectStatus(t, c.do(http.MethodPatch, status, map[string]string{"status": "DEPRECATED"}), http.StatusOK)
	expectProblem(t, c.do(http.MethodPatch, status, map[string]string{"status": "ACTIVE"}), http.StatusConflict, "ILLEGAL_TRANSITION")
}

func TestValidationErrors(t *testing.T) {
	c := newClient(t, newRouterNoDB(t))
	body := c.definitionBody("FINANCIAL_SUMMARY")
	body["report_name"] = ""
	expectProblem(t, c.do(http.MethodPost, "/v1/reports/definitions", body), http.StatusBadRequest, "VALIDATION_FAILED")
	body = c.definitionBody("CRYSTAL_BALL")
	expectProblem(t, c.do(http.MethodPost, "/v1/reports/definitions", body), http.StatusBadRequest, "VALIDATION_FAILED")
	expectProblem(t, c.do(http.MethodPost, "/v1/reports/definitions", []byte(`{oops`)), http.StatusBadRequest, "MALFORMED_JSON")
	expectProblem(t, c.do(http.MethodGet, "/v1/reports/definitions/not-a-uuid", nil), http.StatusBadRequest, "INVALID_ID")
	expectProblem(t, c.do(http.MethodGet, "/v1/reports/definitions?limit=500", nil), http.StatusBadRequest, "INVALID_QUERY")
	body = c.definitionBody("FINANCIAL_SUMMARY")
	body["tenant_id"] = "someone-else"
	expectProblem(t, c.do(http.MethodPost, "/v1/reports/definitions", body), http.StatusForbidden, "TENANT_MISMATCH")
	if rec := c.do(http.MethodPost, "/v1/reports/definitions", c.definitionBody("FINANCIAL_SUMMARY"), drop("X-Principal-Id")); rec.Code != http.StatusUnauthorized {
		t.Fatalf("missing principal: expected 401, got %d", rec.Code)
	}
	if rec := c.do(http.MethodPost, "/v1/reports/definitions", c.definitionBody("FINANCIAL_SUMMARY"), drop("Idempotency-Key")); rec.Code != http.StatusBadRequest {
		t.Fatalf("missing Idempotency-Key: expected 400, got %d", rec.Code)
	}
}

func TestTriggerRunOnMissingDefinition(t *testing.T) {
	c := newClient(t, newRouter(t))
	expectProblem(t, c.do(http.MethodPost, "/v1/reports/definitions/"+uuid.NewString()+"/runs", nil), http.StatusNotFound, "NOT_FOUND")
	expectProblem(t, c.do(http.MethodPost, "/v1/reports/definitions/non-existent-id/runs", nil), http.StatusBadRequest, "INVALID_ID")
}

// TestAuthorizationDeniedAndUnavailable: policy-driven stub, allowed path
// above; here a denied principal gets 403 and an unreachable
// authorization-svc 503 — nothing is written in either case.
func TestAuthorizationDeniedAndUnavailable(t *testing.T) {
	policy := newPolicyAuthz()
	router, _ := newRouterWith(t, policy)
	c := newClient(t, router)

	policy.deny["intruder|REPORT_DEFINITION_CREATE"] = true
	expectProblem(t, c.do(http.MethodPost, "/v1/reports/definitions", c.definitionBody("CASH_FLOW"), as("intruder")),
		http.StatusForbidden, "AUTHORIZATION_DENIED")
	policy.override["AUDIT_EXPORT_REQUEST"] = authz.ErrAuthzServiceUnavailable
	expectProblem(t, c.do(http.MethodPost, "/v1/audit-exports", map[string]string{
		"legal_entity_id": c.entity, "archive_id": "arch-1", "purpose": "audit"}), http.StatusServiceUnavailable, "AUTHZ_UNAVAILABLE")

	list := expectStatus(t, c.do(http.MethodGet, "/v1/reports/definitions", nil), http.StatusOK)
	if list["count"].(float64) != 0 {
		t.Fatalf("denied create must not persist: %v", list)
	}
	if want := c.tenant + "|intruder|" + c.entity + "|REPORT_DEFINITION_CREATE"; policy.calls[0] != want {
		t.Fatalf("authz must be asked with tenant, principal, entity, action: got %q want %q", policy.calls[0], want)
	}
}

func TestCrossTenantAndIdempotency(t *testing.T) {
	router := newRouter(t)
	owner := newClient(t, router)
	other := newClient(t, router)
	other.entity = owner.entity

	id := owner.activeDefinition("PAYROLL_SUMMARY")
	expectProblem(t, other.do(http.MethodGet, "/v1/reports/definitions/"+id, nil), http.StatusNotFound, "NOT_FOUND")
	expectProblem(t, other.do(http.MethodPatch, "/v1/reports/definitions/"+id+"/status", map[string]string{"status": "PAUSED"}),
		http.StatusNotFound, "NOT_FOUND")
	expectProblem(t, other.do(http.MethodPost, "/v1/reports/definitions/"+id+"/runs", nil), http.StatusNotFound, "NOT_FOUND")

	key := uuid.NewString()
	first := expectStatus(t, owner.do(http.MethodPost, "/v1/reports/definitions/"+id+"/runs", map[string]string{"triggered_by": "API"}, idem(key)), http.StatusAccepted)
	rec := owner.do(http.MethodPost, "/v1/reports/definitions/"+id+"/runs", map[string]string{"triggered_by": "API"}, idem(key))
	second := expectStatus(t, rec, http.StatusAccepted)
	if first["id"] != second["id"] || rec.Header().Get("Idempotent-Replayed") != "true" {
		t.Fatalf("replay must return the same run: %v vs %v", first["id"], second["id"])
	}
	expectProblem(t, owner.do(http.MethodPost, "/v1/reports/definitions/"+id+"/runs", map[string]string{"triggered_by": "MANUAL"}, idem(key)),
		http.StatusUnprocessableEntity, "IDEMPOTENCY_KEY_REUSED")
	runs := expectStatus(t, other.do(http.MethodGet, "/v1/reports/runs?definition_id="+id, nil), http.StatusOK)
	if runs["count"].(float64) != 0 {
		t.Fatalf("other tenant must see no runs: %v", runs)
	}
}

// TestAuditExportLifecycleAndFailures drives AUD-10 end to end, including
// the failure semantics: an unreachable archive or invalid digest FAILS the
// export (424, terminal, empty manifest) instead of sealing fabricated
// content; a legal hold blocks delivery.
func TestAuditExportLifecycleAndFailures(t *testing.T) {
	c := newClient(t, newRouter(t))
	create := func(archive string) string {
		body := expectStatus(t, c.do(http.MethodPost, "/v1/audit-exports", map[string]string{
			"legal_entity_id": c.entity, "archive_id": archive, "purpose": "External auditor inspection"}), http.StatusCreated)
		if body["status"] != "REQUESTED" {
			t.Fatalf("create export: %v", body)
		}
		return body["export_id"].(string)
	}
	base := func(id string) string { return "/v1/audit-exports/" + id }

	id := create("arch-" + uuid.NewString()[:6])
	expectProblem(t, c.do(http.MethodPost, base(id)+"/build", nil), http.StatusConflict, "EXPORT_INVALID_STATE")
	expectStatus(t, c.do(http.MethodPost, base(id)+"/redact", map[string]string{"field_or_scope": "employee.national_id", "reason": "PII"}), http.StatusCreated)
	expectProblem(t, c.do(http.MethodPost, base(id)+"/approve", nil), http.StatusForbidden, "SELF_APPROVAL_FORBIDDEN")
	expectStatus(t, c.do(http.MethodPost, base(id)+"/approve", nil, as("partner-02")), http.StatusOK)
	built := expectStatus(t, c.do(http.MethodPost, base(id)+"/build", nil), http.StatusOK)
	if built["status"] != "VALIDATING" || built["build_attempts"].(float64) != 1 {
		t.Fatalf("build: %v", built)
	}
	expectProblem(t, c.do(http.MethodPost, base(id)+"/build", nil), http.StatusConflict, "EXPORT_INVALID_STATE")
	sealed := expectStatus(t, c.do(http.MethodPost, base(id)+"/seal", nil), http.StatusOK)
	if sealed["status"] != "SEALED" || len(sealed["seal_digest"].(string)) != 64 {
		t.Fatalf("seal: %v", sealed)
	}
	manifest := expectStatus(t, c.do(http.MethodGet, base(id)+"/manifest", nil), http.StatusOK)
	if manifest["integrity_verified"] != true || len(manifest["entries"].([]any)) != 1 {
		t.Fatalf("manifest: %v", manifest)
	}
	expectProblem(t, c.do(http.MethodPost, base(id)+"/redact", map[string]string{"field_or_scope": "x", "reason": "late"}),
		http.StatusConflict, "EXPORT_SEALED_IMMUTABLE")
	recipient := uuid.NewString()
	expectStatus(t, c.do(http.MethodPost, base(id)+"/deliver", map[string]string{
		"delivered_to_principal_id": recipient, "delivery_channel": "SECURE_PORTAL"}), http.StatusOK)
	revoked := expectStatus(t, c.do(http.MethodPost, base(id)+"/revoke", map[string]string{"reason": "Engagement terminated"}), http.StatusOK)
	if revoked["status"] != "REVOKED" {
		t.Fatalf("revoke: %v", revoked)
	}
	other := newClient(t, c.router)
	expectProblem(t, other.do(http.MethodGet, base(id), nil), http.StatusNotFound, "NOT_FOUND")

	// Archive unreachable -> 424, export FAILED with reason, empty manifest.
	missing := create("arch-missing-" + uuid.NewString()[:6])
	expectStatus(t, c.do(http.MethodPost, base(missing)+"/approve", nil, as("partner-02")), http.StatusOK)
	expectProblem(t, c.do(http.MethodPost, base(missing)+"/build", nil), http.StatusFailedDependency, "ARCHIVE_UNAVAILABLE")
	failed := expectStatus(t, c.do(http.MethodGet, base(missing), nil), http.StatusOK)
	if failed["status"] != "FAILED" || failed["failure_reason"] == "" {
		t.Fatalf("failed build must be recorded: %v", failed)
	}
	if m := expectStatus(t, c.do(http.MethodGet, base(missing)+"/manifest", nil), http.StatusOK); len(m["entries"].([]any)) != 0 {
		t.Fatalf("FAILED export must have an empty manifest: %v", m)
	}
	expectProblem(t, c.do(http.MethodPost, base(missing)+"/build", nil), http.StatusConflict, "EXPORT_INVALID_STATE")
	expectProblem(t, c.do(http.MethodPost, base(missing)+"/seal", nil), http.StatusConflict, "EXPORT_INVALID_STATE")

	// Invalid digest -> 424.
	bad := create("arch-baddigest-" + uuid.NewString()[:6])
	expectStatus(t, c.do(http.MethodPost, base(bad)+"/approve", nil, as("partner-02")), http.StatusOK)
	expectProblem(t, c.do(http.MethodPost, base(bad)+"/build", nil), http.StatusFailedDependency, "ARCHIVE_DIGEST_INVALID")

	// Legal hold blocks delivery; export stays SEALED.
	held := create("arch-hold-" + uuid.NewString()[:6])
	expectStatus(t, c.do(http.MethodPost, base(held)+"/approve", nil, as("partner-02")), http.StatusOK)
	expectStatus(t, c.do(http.MethodPost, base(held)+"/build", nil), http.StatusOK)
	expectStatus(t, c.do(http.MethodPost, base(held)+"/seal", nil), http.StatusOK)
	expectProblem(t, c.do(http.MethodPost, base(held)+"/deliver", map[string]string{
		"delivered_to_principal_id": recipient, "delivery_channel": "SECURE_PORTAL"}), http.StatusConflict, "LEGAL_HOLD_ACTIVE")
	if h := expectStatus(t, c.do(http.MethodGet, base(held), nil), http.StatusOK); h["status"] != "SEALED" {
		t.Fatalf("held export must remain SEALED: %v", h)
	}
}

// TestBuildInterruptedIsRetriable: an export left BUILDING by a crashed
// worker is refused (409 BUILD_IN_PROGRESS) while its lease is live and
// completes on a retried build once the lease has expired.
func TestBuildInterruptedIsRetriable(t *testing.T) {
	router, pool := newRouterWith(t, newPolicyAuthz())
	c := newClient(t, router)
	body := expectStatus(t, c.do(http.MethodPost, "/v1/audit-exports", map[string]string{
		"legal_entity_id": c.entity, "archive_id": "arch-" + uuid.NewString()[:6], "purpose": "p"}), http.StatusCreated)
	id := body["export_id"].(string)
	expectStatus(t, c.do(http.MethodPost, "/v1/audit-exports/"+id+"/approve", nil, as("partner-02")), http.StatusOK)

	// Simulate a worker that claimed the build and died.
	setBuild := func(sql string) {
		tx, err := pool.Begin(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = tx.Rollback(t.Context()) }()
		if _, err := tx.Exec(t.Context(), "SELECT set_config('app.tenant_id', $1, true)", c.tenant); err != nil {
			t.Fatal(err)
		}
		if _, err := tx.Exec(t.Context(), sql, id); err != nil {
			t.Fatal(err)
		}
		if err := tx.Commit(t.Context()); err != nil {
			t.Fatal(err)
		}
	}
	setBuild(`UPDATE audit_export_requests SET status='BUILDING', build_started_at=now(), build_attempts=1, version=version+1 WHERE export_id=$1`)
	expectProblem(t, c.do(http.MethodPost, "/v1/audit-exports/"+id+"/build", nil), http.StatusConflict, "BUILD_IN_PROGRESS")

	setBuild(`UPDATE audit_export_requests SET build_started_at=now() - interval '1 hour' WHERE export_id=$1`)
	built := expectStatus(t, c.do(http.MethodPost, "/v1/audit-exports/"+id+"/build", nil), http.StatusOK)
	if built["status"] != "VALIDATING" || built["build_attempts"].(float64) != 2 {
		t.Fatalf("retried build: %v", built)
	}
}
