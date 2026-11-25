package handler_test

import (
	"encoding/json"
	"net/http"
	"testing"

	"zoiko.io/anomaly-detection-svc/internal/domain"
	"zoiko.io/anomaly-detection-svc/internal/handler"
	"zoiko.io/anomaly-detection-svc/internal/store"
	"zoiko.io/anomaly-detection-svc/internal/testdb"
)

// End-to-end HTTP flow against real Postgres (NOSUPERUSER NOBYPASSRLS app
// role, own schema). Skipped when TEST_DATABASE_URL is unset.
func TestHTTP_GovernedFlow_Postgres(t *testing.T) {
	_, app := testdb.Open(t, "anomaly_handler_it")
	az := allow(handler.ActionModelManage, handler.ActionAnomalyDetect, handler.ActionAnomalyAck,
		handler.ActionAnomalyDisposition, handler.ActionGovernedRead, handler.ActionRuleCreate,
		handler.ActionAnomalyRead)
	router := newRouter(store.NewPgStore(app), az, true)

	if w := do(t, router, http.MethodGet, "/readyz", ""); w.Code != http.StatusOK {
		t.Fatalf("readyz with DB: %d", w.Code)
	}

	w := do(t, router, http.MethodPost, "/v1/anomaly-governance/models",
		`{"domain_name":"ap","model_provider":"internal","model_version":"v1","review_threshold_bp":5000,"max_drift_bp":500}`)
	if w.Code != http.StatusCreated {
		t.Fatalf("register model: %d %s", w.Code, w.Body.String())
	}

	runBody := `{"domain_name":"ap","model_provider":"internal","model_version":"v1","feature_set_version":"fs1","observed_drift_bp":10,
		"signals":[{"source_entity_ref":"inv-1","severity":"HIGH","anomaly_score_bp":9000,"features":{"k":"v"}}]}`
	w = do(t, router, http.MethodPost, "/v1/anomaly-governance/runs", runBody, header("Idempotency-Key", "run-1"))
	if w.Code != http.StatusCreated {
		t.Fatalf("run detection: %d %s", w.Code, w.Body.String())
	}
	var run domain.DetectionRun
	_ = json.Unmarshal(w.Body.Bytes(), &run)
	if len(run.Signals) != 1 || run.Signals[0].Status != domain.SignalReviewPending {
		t.Fatalf("run: %+v", run)
	}
	replay := do(t, router, http.MethodPost, "/v1/anomaly-governance/runs", runBody, header("Idempotency-Key", "run-1"))
	var replayed domain.DetectionRun
	_ = json.Unmarshal(replay.Body.Bytes(), &replayed)
	if replay.Code != http.StatusCreated || replay.Header().Get("Idempotent-Replayed") != "true" || replayed.RunID != run.RunID {
		t.Fatalf("replay: %d %v %s", replay.Code, replay.Header(), replay.Body.String())
	}

	sig := run.Signals[0]
	closePath := "/v1/anomaly-governance/signals/" + sig.SignalID + ":close"
	closeBody := `{"outcome":"Dismissed","reason_code":"FALSE_POSITIVE","notes":"seasonal"}`
	// The submitter may not close (SoD) → 403.
	if w := do(t, router, http.MethodPost, closePath, closeBody, header("If-Match", `"1"`)); w.Code != http.StatusForbidden {
		t.Fatalf("SoD close: %d %s", w.Code, w.Body.String())
	}
	// Stale If-Match → 412.
	if w := do(t, router, http.MethodPost, closePath, closeBody, header("If-Match", `"7"`), header("X-Principal-Id", "reviewer")); w.Code != http.StatusPreconditionFailed {
		t.Fatalf("stale close: %d %s", w.Code, w.Body.String())
	}
	w = do(t, router, http.MethodPost, closePath, closeBody, header("If-Match", `"1"`), header("X-Principal-Id", "reviewer"))
	if w.Code != http.StatusOK || w.Header().Get("ETag") != `"2"` {
		t.Fatalf("close: %d %s", w.Code, w.Body.String())
	}
	// Terminal → 409.
	if w := do(t, router, http.MethodPost, closePath, closeBody, header("If-Match", `"2"`), header("X-Principal-Id", "reviewer")); w.Code != http.StatusConflict {
		t.Fatalf("close twice: %d %s", w.Code, w.Body.String())
	}

	if w := do(t, router, http.MethodGet, "/v1/anomaly-governance/signals/"+sig.SignalID+"/disposition", ""); w.Code != http.StatusOK {
		t.Fatalf("disposition: %d", w.Code)
	}
	if w := do(t, router, http.MethodGet, "/v1/anomaly-governance/signals/"+sig.SignalID, "", header("X-Tenant-Id", tenantB)); w.Code != http.StatusNotFound {
		t.Fatalf("cross-tenant signal: %d", w.Code)
	}
	if w := do(t, router, http.MethodGet, "/v1/anomaly-governance/runs/"+run.RunID, "", header("X-Tenant-Id", tenantB)); w.Code != http.StatusNotFound {
		t.Fatalf("cross-tenant run: %d", w.Code)
	}

	// Rule-engine surface, ported from the old handler_test.go flow.
	w = do(t, router, http.MethodPost, "/v1/anomalies/detect",
		`{"domain_name":"FINANCE","source_entity_id":"tx-9901","metric_type":"transaction_amount","observed_value":500000,"expected_value":15000,"std_deviation":5000}`)
	if w.Code != http.StatusCreated {
		t.Fatalf("detect: %d %s", w.Code, w.Body.String())
	}
	var rec domain.AnomalyRecord
	_ = json.Unmarshal(w.Body.Bytes(), &rec)
	if rec.Severity != domain.SeverityCritical || rec.Status != domain.StatusOpen {
		t.Fatalf("detected: %+v", rec)
	}
	w = do(t, router, http.MethodPost, "/v1/anomalies/"+rec.AnomalyID+"/status",
		`{"status":"UNDER_INVESTIGATION","resolution_notes":"reviewing"}`, header("If-Match", `"1"`))
	if w.Code != http.StatusOK {
		t.Fatalf("status update: %d %s", w.Code, w.Body.String())
	}
	if w := do(t, router, http.MethodGet, "/v1/anomalies?legal_entity_id=entity-001", ""); w.Code != http.StatusOK {
		t.Fatalf("list anomalies: %d", w.Code)
	}
}
