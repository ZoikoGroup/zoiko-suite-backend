package handler_test

import (
	"encoding/json"
	"net/http"
	"testing"

	"zoiko.io/project-accounting-svc/internal/domain"
)

// ── RefreshProfitabilityProjection / GetProjectProfitability ────────────────

func TestGetProjectProfitability_NoProjectionYet_Returns404(t *testing.T) {
	s := newStubStore()
	r := newRouter(s, &stubPublisher{}, &stubAuthZ{})
	id := createActiveProject(t, r, "le-1", "PROF-1")

	rr := doReq(r, http.MethodGet, "/v1/profitability/projections?project_id="+id, nil, "reader-1")
	if rr.Code != http.StatusNotFound {
		t.Fatalf("expected 404 projection_not_built, got %d: %s", rr.Code, rr.Body.String())
	}
}

func TestRefreshProfitabilityProjection_Succeeds(t *testing.T) {
	s := newStubStore()
	r := newRouter(s, &stubPublisher{}, &stubAuthZ{})
	id := createActiveProject(t, r, "le-1", "PROF-2")

	rr := doReq(r, http.MethodPost, "/v1/profitability/projections/refresh", map[string]string{"project_id": id}, "preparer-1")
	if rr.Code != http.StatusOK {
		t.Fatalf("refresh failed: %d %s", rr.Code, rr.Body.String())
	}
	var proj domain.ProfitabilityProjection
	_ = json.NewDecoder(rr.Body).Decode(&proj)
	if proj.Status != domain.ProfitabilityProjectionStatusCurrent {
		t.Fatalf("expected CURRENT, got %q", proj.Status)
	}

	getRR := doReq(r, http.MethodGet, "/v1/profitability/projections?project_id="+id, nil, "reader-1")
	if getRR.Code != http.StatusOK {
		t.Fatalf("get failed: %d %s", getRR.Code, getRR.Body.String())
	}
}

// ── BuildProfitabilitySnapshot ("Stale project margin shown as
// certified") ────────────────────────────────────────────────────────────────

func TestBuildProfitabilitySnapshot_NoProjection_Returns404(t *testing.T) {
	s := newStubStore()
	r := newRouter(s, &stubPublisher{}, &stubAuthZ{})
	id := createActiveProject(t, r, "le-1", "PROF-3")

	rr := doReq(r, http.MethodPost, "/v1/profitability/snapshots/", map[string]string{"project_id": id}, "preparer-1")
	if rr.Code != http.StatusNotFound {
		t.Fatalf("expected 404 projection_not_built, got %d: %s", rr.Code, rr.Body.String())
	}
}

func TestBuildProfitabilitySnapshot_StaleProjection_Refused(t *testing.T) {
	s := newStubStore()
	r := newRouter(s, &stubPublisher{}, &stubAuthZ{})
	id := createActiveProject(t, r, "le-1", "PROF-4")

	rr := doReq(r, http.MethodPost, "/v1/profitability/projections/refresh", map[string]string{"project_id": id}, "preparer-1")
	if rr.Code != http.StatusOK {
		t.Fatalf("refresh failed: %d %s", rr.Code, rr.Body.String())
	}
	// Force the projection stale directly, simulating new source facts
	// having landed since the last refresh — the real scenario
	// CertifyProfitabilitySnapshot's own live re-check protects against.
	s.projections[id].Status = domain.ProfitabilityProjectionStatusStale

	buildRR := doReq(r, http.MethodPost, "/v1/profitability/snapshots/", map[string]string{"project_id": id}, "preparer-1")
	if buildRR.Code != http.StatusUnprocessableEntity {
		t.Fatalf("expected 422 projection_stale, got %d: %s", buildRR.Code, buildRR.Body.String())
	}
}

func TestBuildProfitabilitySnapshot_Succeeds(t *testing.T) {
	s := newStubStore()
	r := newRouter(s, &stubPublisher{}, &stubAuthZ{})
	id := createActiveProject(t, r, "le-1", "PROF-5")
	doReq(r, http.MethodPost, "/v1/profitability/projections/refresh", map[string]string{"project_id": id}, "preparer-1")

	rr := doReq(r, http.MethodPost, "/v1/profitability/snapshots/", map[string]string{"project_id": id}, "preparer-1")
	if rr.Code != http.StatusCreated {
		t.Fatalf("build snapshot failed: %d %s", rr.Code, rr.Body.String())
	}
	var snap domain.ProfitabilitySnapshot
	_ = json.NewDecoder(rr.Body).Decode(&snap)
	if snap.Status != domain.ProfitabilitySnapshotStatusReconciled {
		t.Fatalf("expected RECONCILED, got %q", snap.Status)
	}
}

// ── CertifyProfitabilitySnapshot ("Manual edit changes margin without
// source fact" — structurally impossible: no update endpoint exists for
// a snapshot's own economic fields) ──────────────────────────────────────────

func TestCertifyProfitabilitySnapshot_NotFound_Returns404(t *testing.T) {
	s := newStubStore()
	r := newRouter(s, &stubPublisher{}, &stubAuthZ{})

	rr := doReq(r, http.MethodPost, "/v1/profitability/snapshots/does-not-exist/certify", nil, "certifier-1")
	if rr.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d: %s", rr.Code, rr.Body.String())
	}
}

func TestCertifyProfitabilitySnapshot_SameCreator_Succeeds(t *testing.T) {
	// No self-certification SoD is enforced in this v1 — see migration
	// 000004's own doc comment: no metric-model-authorship registration
	// exists anywhere on this platform to gate against.
	s := newStubStore()
	r := newRouter(s, &stubPublisher{}, &stubAuthZ{})
	id := createActiveProject(t, r, "le-1", "PROF-6")
	doReq(r, http.MethodPost, "/v1/profitability/projections/refresh", map[string]string{"project_id": id}, "preparer-1")
	buildRR := doReq(r, http.MethodPost, "/v1/profitability/snapshots/", map[string]string{"project_id": id}, "preparer-1")
	var snap domain.ProfitabilitySnapshot
	_ = json.NewDecoder(buildRR.Body).Decode(&snap)

	rr := doReq(r, http.MethodPost, "/v1/profitability/snapshots/"+snap.SnapshotID+"/certify", nil, "preparer-1")
	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200 (self-certify permitted in this v1), got %d: %s", rr.Code, rr.Body.String())
	}
}

func TestCertifyProfitabilitySnapshot_AlreadyCertified_Refused(t *testing.T) {
	s := newStubStore()
	r := newRouter(s, &stubPublisher{}, &stubAuthZ{})
	id := createActiveProject(t, r, "le-1", "PROF-7")
	doReq(r, http.MethodPost, "/v1/profitability/projections/refresh", map[string]string{"project_id": id}, "preparer-1")
	buildRR := doReq(r, http.MethodPost, "/v1/profitability/snapshots/", map[string]string{"project_id": id}, "preparer-1")
	var snap domain.ProfitabilitySnapshot
	_ = json.NewDecoder(buildRR.Body).Decode(&snap)

	first := doReq(r, http.MethodPost, "/v1/profitability/snapshots/"+snap.SnapshotID+"/certify", nil, "certifier-1")
	if first.Code != http.StatusOK {
		t.Fatalf("first certify failed: %d %s", first.Code, first.Body.String())
	}
	second := doReq(r, http.MethodPost, "/v1/profitability/snapshots/"+snap.SnapshotID+"/certify", nil, "certifier-1")
	if second.Code != http.StatusUnprocessableEntity {
		t.Fatalf("expected 422 invalid_transition on re-certify, got %d: %s", second.Code, second.Body.String())
	}
}

// ── GetMarginBridge / GetCostRevenueDrilldown / GetProfitabilityAsOf ────────

func TestGetMarginBridge_NoCertifiedBaseline_ReturnsNilBaseline(t *testing.T) {
	s := newStubStore()
	r := newRouter(s, &stubPublisher{}, &stubAuthZ{})
	id := createActiveProject(t, r, "le-1", "PROF-8")
	doReq(r, http.MethodPost, "/v1/profitability/projections/refresh", map[string]string{"project_id": id}, "preparer-1")

	rr := doReq(r, http.MethodGet, "/v1/profitability/margin-bridge?project_id="+id, nil, "reader-1")
	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rr.Code, rr.Body.String())
	}
	var resp map[string]json.RawMessage
	_ = json.NewDecoder(rr.Body).Decode(&resp)
	if string(resp["certified_baseline"]) != "null" {
		t.Fatalf("expected certified_baseline=null before any snapshot is certified, got %s", resp["certified_baseline"])
	}
	if _, ok := resp["current"]; !ok {
		t.Fatal("expected the current projection to be present")
	}
}

func TestGetMarginBridge_AfterCertification_ComputesDeltas(t *testing.T) {
	s := newStubStore()
	r := newRouter(s, &stubPublisher{}, &stubAuthZ{})
	id := createActiveProject(t, r, "le-1", "PROF-9")
	doReq(r, http.MethodPost, "/v1/profitability/projections/refresh", map[string]string{"project_id": id}, "preparer-1")
	buildRR := doReq(r, http.MethodPost, "/v1/profitability/snapshots/", map[string]string{"project_id": id}, "preparer-1")
	var snap domain.ProfitabilitySnapshot
	_ = json.NewDecoder(buildRR.Body).Decode(&snap)
	certRR := doReq(r, http.MethodPost, "/v1/profitability/snapshots/"+snap.SnapshotID+"/certify", nil, "certifier-1")
	if certRR.Code != http.StatusOK {
		t.Fatalf("certify failed: %d %s", certRR.Code, certRR.Body.String())
	}

	// Simulate new facts landing after certification (a real cost entry
	// would do this; directly mutating the live projection is the
	// established stub-level shortcut this test file already uses for
	// "the live figures moved since baseline").
	s.projections[id].Revenue = 500
	s.projections[id].Cost = 100
	s.projections[id].Margin = 400

	rr := doReq(r, http.MethodGet, "/v1/profitability/margin-bridge?project_id="+id, nil, "reader-1")
	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rr.Code, rr.Body.String())
	}
	var resp struct {
		RevenueDelta float64 `json:"revenue_delta"`
		CostDelta    float64 `json:"cost_delta"`
		MarginDelta  float64 `json:"margin_delta"`
	}
	_ = json.NewDecoder(rr.Body).Decode(&resp)
	if resp.RevenueDelta != 500 || resp.CostDelta != 100 || resp.MarginDelta != 400 {
		t.Fatalf("expected deltas (500, 100, 400) against a zero-value certified baseline, got %+v", resp)
	}
}

func TestGetCostRevenueDrilldown_ComposesProjectionCostsAndRevenue(t *testing.T) {
	s := newStubStore()
	r := newRouter(s, &stubPublisher{}, &stubAuthZ{})
	id := createActiveProject(t, r, "le-1", "PROF-10")
	captureAPCost(t, r, id, "AP-PROF-10", 250)
	doReq(r, http.MethodPost, "/v1/profitability/projections/refresh", map[string]string{"project_id": id}, "preparer-1")

	rr := doReq(r, http.MethodGet, "/v1/profitability/cost-revenue-drilldown?project_id="+id, nil, "reader-1")
	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rr.Code, rr.Body.String())
	}
	var resp struct {
		Projection  domain.ProfitabilityProjection `json:"projection"`
		CostEntries []domain.CostEntry             `json:"cost_entries"`
		RevenueRun  *domain.RecognitionRun         `json:"revenue_run"`
	}
	_ = json.NewDecoder(rr.Body).Decode(&resp)
	if resp.Projection.ProjectID != id {
		t.Fatalf("expected the projection for %s, got %+v", id, resp.Projection)
	}
	if len(resp.CostEntries) != 1 || resp.CostEntries[0].SourceReference != "AP-PROF-10" {
		t.Fatalf("expected the captured cost entry to drill down, got %+v", resp.CostEntries)
	}
	if resp.RevenueRun != nil {
		t.Fatalf("expected no revenue run (none created), got %+v", resp.RevenueRun)
	}
}

func TestGetProfitabilityAsOf_BeforeCertification_Returns404(t *testing.T) {
	s := newStubStore()
	r := newRouter(s, &stubPublisher{}, &stubAuthZ{})
	id := createActiveProject(t, r, "le-1", "PROF-11")
	doReq(r, http.MethodPost, "/v1/profitability/projections/refresh", map[string]string{"project_id": id}, "preparer-1")

	rr := doReq(r, http.MethodGet, "/v1/profitability/as-of?project_id="+id, nil, "reader-1")
	if rr.Code != http.StatusNotFound {
		t.Fatalf("expected 404 (no certified snapshot yet), got %d: %s", rr.Code, rr.Body.String())
	}
}

func TestGetProfitabilityAsOf_AfterCertification_ReturnsSnapshot(t *testing.T) {
	s := newStubStore()
	r := newRouter(s, &stubPublisher{}, &stubAuthZ{})
	id := createActiveProject(t, r, "le-1", "PROF-12")
	doReq(r, http.MethodPost, "/v1/profitability/projections/refresh", map[string]string{"project_id": id}, "preparer-1")
	buildRR := doReq(r, http.MethodPost, "/v1/profitability/snapshots/", map[string]string{"project_id": id}, "preparer-1")
	var snap domain.ProfitabilitySnapshot
	_ = json.NewDecoder(buildRR.Body).Decode(&snap)
	doReq(r, http.MethodPost, "/v1/profitability/snapshots/"+snap.SnapshotID+"/certify", nil, "certifier-1")

	rr := doReq(r, http.MethodGet, "/v1/profitability/as-of?project_id="+id, nil, "reader-1")
	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rr.Code, rr.Body.String())
	}
	var asOf domain.ProfitabilitySnapshot
	_ = json.NewDecoder(rr.Body).Decode(&asOf)
	if asOf.SnapshotID != snap.SnapshotID {
		t.Fatalf("expected the certified snapshot, got %+v", asOf)
	}
}
