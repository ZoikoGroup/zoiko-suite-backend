package handler_test

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"zoiko.io/evidence-manifest-svc/internal/aggregator"
	"zoiko.io/evidence-manifest-svc/internal/domain"
)

// Tenant-scope tests for evidence-manifest-svc (tracker row 14).
//
// Before this change the service read no X-Tenant-Id at all. The tenant
// existed only as a request-body field on POST, validated as non-empty and
// otherwise trusted, and the two GET routes had no tenant input of any kind
// — a manifest id from the URL was the entire argument.
//
// What that exposed is the point. GET /{id}/records returns
// manifest_records.record_snapshot, which is a verbatim JSON copy of each
// source record: governance decisions, access decisions, workflow
// instances, snapshotted in full so a manifest stays reconstructable if the
// source service is unavailable. Doc 03 §14.4 makes these the bundles handed
// to an auditor, regulator or legal-discovery request. So an unguessed
// manifest id was one tenant's assembled evidence, in full.

// scopeTestSources returns sources with a real governance record
// configured, so seeding a manifest actually produces a ManifestRecord to
// isolate. defaultSources() leaves getResult nil, and the handler
// dereferences the returned record — harmless in production, where the real
// aggregator returns either a record or ErrSourceNotFound and never (nil,
// nil), but it makes an unconfigured stub panic rather than fail.
func scopeTestSources() (*stubGovernance, *stubAccess, *stubWorkflow, *stubPublisher) {
	gov := &stubGovernance{getResult: &aggregator.SourceRecord{
		SourceType:     domain.SourceGovernanceDecision,
		SourceRecordID: "gd-1",
		RawJSON:        []byte(`{"decision_id":"gd-1","outcome":"APPROVED"}`),
	}}
	return gov, &stubAccess{}, &stubWorkflow{}, &stubPublisher{}
}

func seedManifest(t *testing.T, r http.Handler, tenantID string) string {
	t.Helper()
	body, _ := json.Marshal(domain.GenerateManifestRequest{
		TenantID:              tenantID,
		LegalEntityID:         "e1",
		ScenarioType:          domain.ScenarioAudit,
		GovernanceDecisionIDs: []string{"gd-1"},
	})
	req := httptest.NewRequest(http.MethodPost, "/v1/evidence-manifests", bytes.NewReader(body))
	req.Header.Set("X-Tenant-Id", tenantID)
	req.Header.Set("X-Principal-Id", "principal-seed")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusCreated {
		t.Fatalf("seed manifest for %s: expected 201, got %d — %s", tenantID, w.Code, w.Body.String())
	}
	var m domain.EvidenceManifest
	if err := json.NewDecoder(w.Body).Decode(&m); err != nil {
		t.Fatalf("decode seeded manifest: %v", err)
	}
	return m.ManifestID
}

// TestMissingTenantHeader_Refused covers the blanket middleware. All three
// routes operate on one tenant's evidence — there are no platform-scope
// endpoints here — so a request with no verified tenant is refused outright
// rather than reaching a handler.
func TestMissingTenantHeader_Refused(t *testing.T) {
	gov, acc, wf, pub := scopeTestSources()
	r := newRouter(newStubStore(), gov, acc, wf, pub)

	body, _ := json.Marshal(domain.GenerateManifestRequest{
		TenantID: "t1", LegalEntityID: "e1", ScenarioType: domain.ScenarioAudit,
		GovernanceDecisionIDs: []string{"gd-1"},
	})

	for _, tc := range []struct {
		name, method, path string
		body               []byte
	}{
		{"generate manifest", http.MethodPost, "/v1/evidence-manifests", body},
		{"get manifest", http.MethodGet, "/v1/evidence-manifests/manifest-1", nil},
		{"list records", http.MethodGet, "/v1/evidence-manifests/manifest-1/records", nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var req *http.Request
			if tc.body != nil {
				req = httptest.NewRequest(tc.method, tc.path, bytes.NewReader(tc.body))
			} else {
				req = httptest.NewRequest(tc.method, tc.path, nil)
			}
			// Deliberately NO X-Tenant-Id.
			w := httptest.NewRecorder()
			r.ServeHTTP(w, req)

			if w.Code != http.StatusUnauthorized {
				t.Fatalf("expected 401 with no X-Tenant-Id, got %d: %s", w.Code, w.Body.String())
			}
			if bytes.Contains(w.Body.Bytes(), []byte("default-tenant")) {
				t.Fatalf("response must never mention a fabricated default tenant: %s", w.Body.String())
			}
		})
	}
}

// TestGenerateManifest_ForeignTenantInBody_Refused covers the
// caller-declared tenant. tenant_id stays in the request contract but may
// now only agree with the verified header; a mismatch is refused rather than
// silently resolved in either direction.
func TestGenerateManifest_ForeignTenantInBody_Refused(t *testing.T) {
	gov, acc, wf, pub := scopeTestSources()
	r := newRouter(newStubStore(), gov, acc, wf, pub)

	body, _ := json.Marshal(domain.GenerateManifestRequest{
		TenantID: "t2", LegalEntityID: "e1", ScenarioType: domain.ScenarioAudit,
		GovernanceDecisionIDs: []string{"gd-1"},
	})
	req := httptest.NewRequest(http.MethodPost, "/v1/evidence-manifests", bytes.NewReader(body))
	req.Header.Set("X-Tenant-Id", "t1")
	req.Header.Set("X-Principal-Id", "principal-test-01")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusForbidden {
		t.Fatalf("expected 403 when body tenant_id disagrees with the verified header, got %d: %s",
			w.Code, w.Body.String())
	}
}

// TestGetManifest_ForeignTenant_NotFound covers the by-id read. 404 rather
// than 403, so the endpoint cannot be used to confirm that another tenant's
// manifest id exists.
func TestGetManifest_ForeignTenant_NotFound(t *testing.T) {
	gov, acc, wf, pub := scopeTestSources()
	store := newStubStore()
	r := newRouter(store, gov, acc, wf, pub)

	manifestID := seedManifest(t, r, "t1")

	req := httptest.NewRequest(http.MethodGet, "/v1/evidence-manifests/"+manifestID, nil)
	req.Header.Set("X-Tenant-Id", "t2")
	req.Header.Set("X-Principal-Id", "principal-test-01")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusNotFound {
		t.Fatalf("expected 404 for another tenant's manifest, got %d: %s", w.Code, w.Body.String())
	}

	// Sanity: t1 still reads its own, so the above is isolation and not a
	// broken lookup.
	ownReq := httptest.NewRequest(http.MethodGet, "/v1/evidence-manifests/"+manifestID, nil)
	ownReq.Header.Set("X-Tenant-Id", "t1")
	ownReq.Header.Set("X-Principal-Id", "principal-test-01")
	ownW := httptest.NewRecorder()
	r.ServeHTTP(ownW, ownReq)
	if ownW.Code != http.StatusOK {
		t.Fatalf("t1 must still read its own manifest, got %d: %s", ownW.Code, ownW.Body.String())
	}
}

// TestListRecords_ForeignTenant_LeaksNothing is the one that matters most:
// record_snapshot is the evidence itself, not metadata about it.
func TestListRecords_ForeignTenant_LeaksNothing(t *testing.T) {
	gov, acc, wf, pub := scopeTestSources()
	store := newStubStore()
	r := newRouter(store, gov, acc, wf, pub)

	manifestID := seedManifest(t, r, "t1")

	req := httptest.NewRequest(http.MethodGet, "/v1/evidence-manifests/"+manifestID+"/records", nil)
	req.Header.Set("X-Tenant-Id", "t2")
	req.Header.Set("X-Principal-Id", "principal-test-01")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusNotFound {
		t.Fatalf("expected 404 listing another tenant's manifest records, got %d: %s", w.Code, w.Body.String())
	}
	// The snapshot payload must not appear at all — not the record ids, not
	// the source ids, nothing that came out of t1's sources.
	for _, needle := range []string{"gd-1", "record-", "record_snapshot"} {
		if bytes.Contains(w.Body.Bytes(), []byte(needle)) {
			t.Fatalf("ISOLATION FAILURE: t2's response contained %q from t1's evidence: %s",
				needle, w.Body.String())
		}
	}
}
