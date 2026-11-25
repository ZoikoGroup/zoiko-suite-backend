package handler_test

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"zoiko.io/compliance-risk-scoring-svc/internal/authz"
	"zoiko.io/compliance-risk-scoring-svc/internal/service"
)

// ---------------------------------------------------------------------------
// Probes
// ---------------------------------------------------------------------------

func TestHealthCheck(t *testing.T) {
	router := setupTestRouter()

	req := httptest.NewRequest(http.MethodGet, "/healthz", nil) // no headers at all
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)

	body := expectStatus(t, rec, http.StatusOK)
	if body["status"] != "ok" || body["service"] != "compliance-risk-scoring-svc" {
		t.Fatalf("unexpected response body: %v", body)
	}
}

func TestReadyzReflectsDatabase(t *testing.T) {
	for _, tc := range []struct {
		name   string
		pinger fakePinger
		want   int
	}{
		{"db reachable", fakePinger{}, http.StatusOK},
		{"db down", fakePinger{err: errBoom}, http.StatusServiceUnavailable},
	} {
		t.Run(tc.name, func(t *testing.T) {
			router := routerWith(nil, allowAll(), tc.pinger)
			rec := httptest.NewRecorder()
			router.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/readyz", nil)) // no tenant/principal
			expectStatus(t, rec, tc.want)
		})
	}

	t.Run("real pool", func(t *testing.T) {
		s, _ := testStore(t)
		router := routerWith(s, allowAll(), s)
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/readyz", nil))
		body := expectStatus(t, rec, http.StatusOK)
		if body["status"] != "ready" {
			t.Fatalf("unexpected body %v", body)
		}
	})
}

// ---------------------------------------------------------------------------
// Refusals that happen before storage (no DB needed)
// ---------------------------------------------------------------------------

func TestValidationErrors(t *testing.T) {
	c := newClient(t, setupTestRouter())

	expectProblem(t, c.do(http.MethodPost, "/v1/risk-scores/calculate", map[string]any{"legal_entity_id": ""}),
		http.StatusBadRequest, "validation_failed")
	bad := calcBody(c.entity, "neg", counts(-1, 0, 0, 0, 0))
	expectProblem(t, c.do(http.MethodPost, "/v1/risk-scores/calculate", bad), http.StatusBadRequest, "validation_failed")
	unknown := calcBody(c.entity, "x", counts(0, 0, 0, 0, 0))
	unknown["risk_score_override"] = 1
	expectProblem(t, c.do(http.MethodPost, "/v1/risk-scores/calculate", unknown), http.StatusBadRequest, "validation_failed")
	expectProblem(t, c.do(http.MethodPost, "/v1/risk-scores/calculate", []byte(`{not json`)), http.StatusBadRequest, "malformed_json")
	rule := map[string]any{"rule_name": "r", "risk_category": "COMPOSITE", "moderate_threshold": "50",
		"high_threshold": "40", "critical_threshold": "90", "effective_from": time.Now().UTC().Format("2006-01-02")}
	expectProblem(t, c.do(http.MethodPost, "/v1/risk-scores/thresholds", rule), http.StatusBadRequest, "validation_failed")
	expectProblem(t, c.do(http.MethodGet, "/v1/risk-scores/not-a-uuid", nil), http.StatusBadRequest, "invalid_id")
	expectProblem(t, c.do(http.MethodGet, "/v1/risk-scores?risk_tier=SEVERE", nil), http.StatusBadRequest, "invalid_filter")
}

func TestMissingPrincipalAndIdempotencyKey(t *testing.T) {
	c := newClient(t, setupTestRouter())
	body := calcBody(c.entity, "x", counts(0, 0, 0, 0, 0))

	if rec := c.do(http.MethodPost, "/v1/risk-scores/calculate", body, drop("X-Principal-Id")); rec.Code != http.StatusUnauthorized {
		t.Fatalf("missing principal on write: expected 401, got %d: %s", rec.Code, rec.Body.String())
	}
	if rec := c.do(http.MethodGet, "/v1/risk-scores/"+uuid.NewString(), nil, drop("X-Principal-Id")); rec.Code != http.StatusUnauthorized {
		t.Fatalf("missing principal on read: expected 401, got %d: %s", rec.Code, rec.Body.String())
	}
	if rec := c.do(http.MethodPost, "/v1/risk-scores/calculate", body, drop("Idempotency-Key")); rec.Code != http.StatusBadRequest {
		t.Fatalf("missing Idempotency-Key: expected 400, got %d: %s", rec.Code, rec.Body.String())
	}
}

// TestAuthorizationDenied proves the deny path with a policy-driven stub:
// the score is never computed and the caller gets 403; an unreachable
// authorization-svc is 503 (fail closed), never an implicit allow.
func TestAuthorizationDenied(t *testing.T) {
	policy := allowAll()
	policy.outcomes[service.ActionRiskScoreCreate] = authz.ErrAuthorizationDenied
	policy.outcomes[service.ActionRiskThresholdRuleCreate] = authz.ErrAuthzServiceUnavailable
	c := newClient(t, routerWith(nil, policy, fakePinger{}))

	expectProblem(t, c.do(http.MethodPost, "/v1/risk-scores/calculate", calcBody(c.entity, "denied", counts(1, 1, 1, 1, 1))),
		http.StatusForbidden, "authorization_denied")

	rule := map[string]any{"rule_name": "r", "risk_category": "COMPOSITE", "moderate_threshold": "20",
		"high_threshold": "40", "critical_threshold": "90", "effective_from": time.Now().UTC().Format("2006-01-02")}
	expectProblem(t, c.do(http.MethodPost, "/v1/risk-scores/thresholds", rule), http.StatusServiceUnavailable, "authorization_unavailable")

	if len(policy.calls) != 2 || policy.calls[0] != c.tenant+"|principal-1|"+c.entity+"|"+service.ActionRiskScoreCreate {
		t.Fatalf("authz must be asked with tenant, principal, legal entity and action; calls=%v", policy.calls)
	}
}

func TestLegalEntityScopeMismatch(t *testing.T) {
	c := newClient(t, setupTestRouter())
	expectProblem(t, c.do(http.MethodPost, "/v1/risk-scores/calculate", calcBody("LE-other", "x", counts(0, 0, 0, 0, 0))),
		http.StatusForbidden, "legal_entity_scope_mismatch")
	body := calcBody(c.entity, "x", counts(0, 0, 0, 0, 0))
	body["tenant_id"] = "someone-else"
	expectProblem(t, c.do(http.MethodPost, "/v1/risk-scores/calculate", body), http.StatusForbidden, "tenant_mismatch")
}

func TestScoringModelIsPublished(t *testing.T) {
	c := newClient(t, setupTestRouter())
	model := expectStatus(t, c.do(http.MethodGet, "/v1/risk-scores/model", nil), http.StatusOK)
	if model["model_version"] != "CRS-WEIGHTED-1.0" || len(model["factors"].([]any)) != 5 {
		t.Fatalf("model: %v", model)
	}
}

// ---------------------------------------------------------------------------
// Postgres-backed behaviour
// ---------------------------------------------------------------------------

// TestCalculateAndLifecycleRiskScore ports the 000001-era lifecycle test
// (calculate -> get -> list -> create threshold -> list thresholds ->
// archive) onto the rewritten API. One intentional difference: the inputs
// score HIGH, and the rewrite forbids archiving an unreviewed HIGH/CRITICAL
// score, so a second principal reviews it before archive.
func TestCalculateAndLifecycleRiskScore(t *testing.T) {
	s, _ := testStore(t)
	c := newClient(t, routerWith(s, allowAll(), s))

	rec := c.do(http.MethodPost, "/v1/risk-scores/calculate",
		calcBody(c.entity, "Q3 Organization Risk Review", counts(5, 2, 3, 1, 1)))
	created := expectStatus(t, rec, http.StatusCreated)
	id, _ := created["id"].(string)
	if id == "" {
		t.Fatalf("expected non-empty assessment ID: %v", created)
	}
	if rec.Header().Get("ETag") != `"1"` {
		t.Fatalf("expected ETag \"1\", got %q", rec.Header().Get("ETag"))
	}

	// Explainability: exact per-factor contributions under CRS-WEIGHTED-1.0.
	//   obligations 5/8 -> raw 62.50 x 30% = 18.75
	//   policy      2/5 -> raw 40.00 x 25% = 10.00
	//   audit       3/4 -> raw 75.00 x 20% = 15.00
	//   privacy     1/3 -> raw 33.33 x 15% =  5.00 (4.9995 half-up)
	//   tax         1/2 -> raw 50.00 x 10% =  5.00
	//   composite 53.75 >= HIGH 51.00 (platform default) -> HIGH, review required
	if created["composite_risk_score"] != "53.75" || created["risk_tier"] != "HIGH" {
		t.Fatalf("composite/tier: got %v / %v", created["composite_risk_score"], created["risk_tier"])
	}
	if created["requires_human_review"] != true || created["review_status"] != "PENDING_REVIEW" ||
		created["assurance_state"] != "ADVISORY" || created["model_version"] != "CRS-WEIGHTED-1.0" {
		t.Fatalf("advisory/review fields wrong: %v", created)
	}
	if hash, _ := created["input_hash"].(string); !strings.HasPrefix(hash, "sha256:") || len(hash) != 71 {
		t.Fatalf("input_hash: %v", created["input_hash"])
	}
	if applied := created["thresholds_applied"].(map[string]any); applied["source"] != "PLATFORM_DEFAULT" {
		t.Fatalf("thresholds_applied: %v", applied)
	}
	if !strings.Contains(created["band_rationale"].(string), "53.75 >= HIGH cut-off 51.00") {
		t.Fatalf("band_rationale: %v", created["band_rationale"])
	}
	factors := created["factor_breakdowns"].([]any)
	if len(factors) != 5 {
		t.Fatalf("expected 5 factor breakdowns, got %d", len(factors))
	}
	wantWeighted := map[string]string{
		"REGULATORY_OBLIGATIONS": "18.75", "POLICY_VIOLATIONS": "10.00", "AUDIT_EXCEPTIONS": "15.00",
		"DATA_PRIVACY": "5.00", "TAX_COMPLIANCE": "5.00",
	}
	for _, f := range factors {
		factor := f.(map[string]any)
		category := factor["risk_category"].(string)
		if factor["weighted_score"] != wantWeighted[category] {
			t.Fatalf("%s weighted_score: got %v want %s", category, factor["weighted_score"], wantWeighted[category])
		}
		if factor["risk_driver_summary"] == "" {
			t.Fatalf("%s has no driver summary", category)
		}
	}

	got := expectStatus(t, c.do(http.MethodGet, "/v1/risk-scores/"+id, nil), http.StatusOK)
	if got["composite_risk_score"] != "53.75" || len(got["factor_breakdowns"].([]any)) != 5 {
		t.Fatalf("stored score differs from response: %v", got)
	}

	list := expectStatus(t, c.do(http.MethodGet, "/v1/risk-scores?legal_entity_id="+c.entity, nil), http.StatusOK)
	if list["count"].(float64) != 1 || list["data"].([]any)[0].(map[string]any)["id"] != id {
		t.Fatalf("list: %v", list)
	}

	today := time.Now().UTC().Format("2006-01-02")
	rule := expectStatus(t, c.do(http.MethodPost, "/v1/risk-scores/thresholds", map[string]any{
		"rule_name": "High Policy Breach Threshold", "risk_category": "POLICY_VIOLATIONS",
		"moderate_threshold": "25", "high_threshold": "50.00", "critical_threshold": 75,
		"notification_channel": "COMPLIANCE_OPS_SLACK", "effective_from": today,
	}), http.StatusCreated)
	if rule["status"] != "DRAFT" || rule["high_threshold"] != "50.00" {
		t.Fatalf("threshold rule: %v", rule)
	}

	rules := expectStatus(t, c.do(http.MethodGet, "/v1/risk-scores/thresholds", nil), http.StatusOK)
	if rules["count"].(float64) != 1 {
		t.Fatalf("list thresholds: %v", rules)
	}

	expectProblem(t, c.do(http.MethodDelete, "/v1/risk-scores/"+id, nil), http.StatusConflict, "review_outstanding")
	expectProblem(t, c.do(http.MethodPost, "/v1/risk-scores/"+id+"/review",
		map[string]any{"disposition": "CONFIRMED", "note": "self"}), http.StatusForbidden, "segregation_of_duties")
	reviewed := expectStatus(t, c.do(http.MethodPost, "/v1/risk-scores/"+id+"/review",
		map[string]any{"disposition": "CONFIRMED", "note": "Matches open obligation register"}, as("reviewer-2")), http.StatusOK)
	if reviewed["review_status"] != "CONFIRMED" || reviewed["reviewed_by"] != "reviewer-2" {
		t.Fatalf("review: %v", reviewed)
	}
	archived := expectStatus(t, c.do(http.MethodDelete, "/v1/risk-scores/"+id, nil), http.StatusOK)
	if archived["status"] != "ARCHIVED" || archived["archived_by"] != "principal-1" {
		t.Fatalf("archive: %v", archived)
	}
	expectProblem(t, c.do(http.MethodPost, "/v1/risk-scores/"+id+"/archive", map[string]any{"reason": "again"}),
		http.StatusConflict, "invalid_transition")

	// Each state change enqueued its outbox event in the same transaction:
	// calculated, threshold exceeded, rule created, reviewed, archived.
	pending, err := s.CountPendingOutboxEvents(t.Context(), c.tenant)
	if err != nil {
		t.Fatal(err)
	}
	if pending != 5 {
		t.Fatalf("expected 5 pending outbox events, got %d", pending)
	}
}

func TestIdempotentReplay(t *testing.T) {
	s, _ := testStore(t)
	c := newClient(t, routerWith(s, allowAll(), s))
	key := uuid.NewString()
	body := calcBody(c.entity, "replay", counts(1, 0, 0, 0, 0))

	first := expectStatus(t, c.do(http.MethodPost, "/v1/risk-scores/calculate", body, idem(key)), http.StatusCreated)
	rec := c.do(http.MethodPost, "/v1/risk-scores/calculate", body, idem(key))
	second := expectStatus(t, rec, http.StatusCreated)
	if first["id"] != second["id"] || rec.Header().Get("Idempotent-Replayed") != "true" {
		t.Fatalf("replay must return the stored response: %v vs %v", first["id"], second["id"])
	}
	list := expectStatus(t, c.do(http.MethodGet, "/v1/risk-scores", nil), http.StatusOK)
	if list["count"].(float64) != 1 {
		t.Fatalf("replay must not create a second score: count=%v", list["count"])
	}
	expectProblem(t, c.do(http.MethodPost, "/v1/risk-scores/calculate", calcBody(c.entity, "replay", counts(2, 0, 0, 0, 0)), idem(key)),
		http.StatusUnprocessableEntity, "idempotency_key_reused")
}

func TestCrossTenantIsolation(t *testing.T) {
	s, _ := testStore(t)
	router := routerWith(s, allowAll(), s)
	owner := newClient(t, router)
	other := newClient(t, router)
	other.entity = owner.entity // same legal entity id, different tenant

	created := expectStatus(t, owner.do(http.MethodPost, "/v1/risk-scores/calculate",
		calcBody(owner.entity, "isolation", counts(8, 5, 4, 3, 2))), http.StatusCreated)
	id := created["id"].(string)
	if created["risk_tier"] != "CRITICAL" || created["composite_risk_score"] != "100.00" {
		t.Fatalf("saturated inputs must score 100.00 CRITICAL: %v", created)
	}

	expectProblem(t, other.do(http.MethodGet, "/v1/risk-scores/"+id, nil), http.StatusNotFound, "not_found")
	expectProblem(t, other.do(http.MethodPost, "/v1/risk-scores/"+id+"/review",
		map[string]any{"disposition": "DISMISSED", "note": "x"}, as("reviewer-2")), http.StatusNotFound, "not_found")
	expectProblem(t, other.do(http.MethodPost, "/v1/risk-scores/"+id+"/archive", nil), http.StatusNotFound, "not_found")
	list := expectStatus(t, other.do(http.MethodGet, "/v1/risk-scores?legal_entity_id="+owner.entity, nil), http.StatusOK)
	if list["count"].(float64) != 0 {
		t.Fatalf("other tenant must see none of the owner's scores: %v", list)
	}
	expectProblem(t, other.do(http.MethodGet, "/v1/risk-scores/subjects/LEGAL_ENTITY/"+owner.entity+"/history", nil),
		http.StatusNotFound, "not_found")
}

func TestRescoringSupersedesAndTracksHistory(t *testing.T) {
	s, _ := testStore(t)
	c := newClient(t, routerWith(s, allowAll(), s))

	first := expectStatus(t, c.do(http.MethodPost, "/v1/risk-scores/calculate",
		calcBody(c.entity, "first", counts(1, 0, 0, 0, 0))), http.StatusCreated)
	second := expectStatus(t, c.do(http.MethodPost, "/v1/risk-scores/calculate",
		calcBody(c.entity, "second", counts(2, 0, 0, 0, 0))), http.StatusCreated)

	// 1/8 -> 12.50 x 30% = 3.75 ; 2/8 -> 25.00 x 30% = 7.50 ; delta 3.75
	if second["previous_score_id"] != first["id"] || second["score_delta"] != "3.75" {
		t.Fatalf("history link: %v", second)
	}
	old := expectStatus(t, c.do(http.MethodGet, "/v1/risk-scores/"+first["id"].(string), nil), http.StatusOK)
	if old["status"] != "SUPERSEDED" || old["superseded_by_id"] != second["id"] {
		t.Fatalf("previous score must be SUPERSEDED: %v", old)
	}
	history := expectStatus(t, c.do(http.MethodGet, "/v1/risk-scores/subjects/legal_entity/"+c.entity+"/history", nil), http.StatusOK)
	if history["count"].(float64) != 2 || history["trend"] != "WORSENING" ||
		history["current_score"].(map[string]any)["id"] != second["id"] {
		t.Fatalf("history: %v", history)
	}
}

func TestThresholdMakerCheckerAndScoring(t *testing.T) {
	s, _ := testStore(t)
	policy := allowAll()
	c := newClient(t, routerWith(s, policy, s))
	today := time.Now().UTC().Format("2006-01-02")
	input := map[string]any{"rule_name": "Tight composite", "risk_category": "COMPOSITE",
		"moderate_threshold": "1.00", "high_threshold": "2.00", "critical_threshold": "3.00", "effective_from": today}

	rule := expectStatus(t, c.do(http.MethodPost, "/v1/risk-scores/thresholds", input), http.StatusCreated)
	id := rule["id"].(string)
	base := "/v1/risk-scores/thresholds/" + id

	input["rule_name"] = "Tight composite v2"
	expectProblem(t, c.do(http.MethodPut, base, input), http.StatusPreconditionRequired, "if_match_required")
	expectProblem(t, c.do(http.MethodPut, base, input, header("If-Match", `"9"`)), http.StatusPreconditionFailed, "version_mismatch")
	updated := expectStatus(t, c.do(http.MethodPut, base, input, header("If-Match", `"1"`)), http.StatusOK)
	if updated["rule_name"] != "Tight composite v2" || updated["version"].(float64) != 2 {
		t.Fatalf("update: %v", updated)
	}

	expectProblem(t, c.do(http.MethodPost, base+"/approve", nil), http.StatusForbidden, "segregation_of_duties")
	policy.deny["intruder|"+service.ActionRiskThresholdRuleApprove] = true
	expectProblem(t, c.do(http.MethodPost, base+"/approve", nil, as("intruder")), http.StatusForbidden, "authorization_denied")
	approved := expectStatus(t, c.do(http.MethodPost, base+"/approve", nil, as("checker-2")), http.StatusOK)
	if approved["status"] != "ACTIVE" || approved["approved_by"] != "checker-2" {
		t.Fatalf("approve: %v", approved)
	}
	expectProblem(t, c.do(http.MethodPut, base, input, header("If-Match", `"3"`)), http.StatusConflict, "invalid_transition")

	score := expectStatus(t, c.do(http.MethodPost, "/v1/risk-scores/calculate",
		calcBody(c.entity, "with rule", counts(1, 0, 0, 0, 0))), http.StatusCreated)
	applied := score["thresholds_applied"].(map[string]any)
	if score["risk_tier"] != "CRITICAL" || applied["source"] != "TENANT_RULE" || applied["rule_id"] != id {
		t.Fatalf("tenant rule must band the score: tier=%v applied=%v", score["risk_tier"], applied)
	}

	effective := expectStatus(t, c.do(http.MethodGet, "/v1/risk-scores/thresholds/effective?as_of="+today, nil), http.StatusOK)
	first := effective["thresholds"].([]any)[0].(map[string]any)
	if first["risk_category"] != "COMPOSITE" || first["source"] != "TENANT_RULE" {
		t.Fatalf("effective thresholds: %v", effective)
	}

	retired := expectStatus(t, c.do(http.MethodPost, base+"/retire", nil), http.StatusOK)
	if retired["status"] != "RETIRED" {
		t.Fatalf("retire: %v", retired)
	}
	expectProblem(t, c.do(http.MethodPost, base+"/retire", nil), http.StatusConflict, "invalid_transition")
	expectProblem(t, c.do(http.MethodGet, "/v1/risk-scores/thresholds/"+uuid.NewString(), nil), http.StatusNotFound, "not_found")
}
