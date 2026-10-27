package handler_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"zoiko.io/reconciliation-intelligence-svc/internal/domain"
	"zoiko.io/reconciliation-intelligence-svc/internal/handler"
	"zoiko.io/reconciliation-intelligence-svc/internal/store"
)

// A6: reads are authorized per legal entity, like writes. Before this, RLS
// scoped them to the tenant only, so any principal of the tenant could read
// every legal entity's jobs and suggestions.

func get(router http.Handler, path, principal string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodGet, path, nil)
	if principal != "" {
		req.Header.Set("X-Principal-Id", principal)
	}
	withEnvelope(req)
	if principal == "" {
		req.Header.Del("X-Principal-Id")
	}
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	return rec
}

// createJob analyzes runLE3003 (legal entity LE-3003) and returns the job id.
func createJob(t *testing.T, router http.Handler) string {
	t.Helper()
	rec := postAnalyze(router, "LE-3003", runLE3003)
	if rec.Code != http.StatusCreated {
		t.Fatalf("analyze: %d %s", rec.Code, rec.Body.String())
	}
	var job domain.ReconciliationJob
	_ = json.Unmarshal(rec.Body.Bytes(), &job)
	return job.ID
}

func TestGetJob_AuthorizedAgainstTheJobsLegalEntity(t *testing.T) {
	az := &recordingAuthz{deny: map[[2]string]bool{}}
	router := newTestRouter(t, deps{authz: az})
	id := createJob(t, router)

	if rec := get(router, "/v1/reconciliations/"+id, "reader-1"); rec.Code != http.StatusOK {
		t.Fatalf("granted read: expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	reads := az.asked(handler.RECONCILIATION_READ)
	if len(reads) != 1 || reads[0].LegalEntityID != "LE-3003" || reads[0].PrincipalID != "reader-1" {
		t.Fatalf("expected one RECONCILIATION_READ decision for reader-1 on LE-3003, got %+v", reads)
	}

	// The request's own X-Legal-Entity-Id header ("entity-test") is not what
	// is checked: the job's entity is. Deny that one and the read is refused.
	az.deny[[2]string{handler.RECONCILIATION_READ, "LE-3003"}] = true
	if rec := get(router, "/v1/reconciliations/"+id, "reader-2"); rec.Code != http.StatusForbidden {
		t.Fatalf("denied read: expected 403, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestListJobs_AuthorizedPerLegalEntity(t *testing.T) {
	az := &recordingAuthz{deny: map[[2]string]bool{{handler.RECONCILIATION_READ, "LE-OTHER"}: true}}
	router := newTestRouter(t, deps{authz: az})
	createJob(t, router)

	rec := get(router, "/v1/reconciliations?legal_entity_id=LE-3003", "reader-1")
	if rec.Code != http.StatusOK {
		t.Fatalf("granted list: expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	var page struct {
		Count int `json:"count"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &page)
	if page.Count != 1 {
		t.Fatalf("expected the LE-3003 job listed, got count %d", page.Count)
	}

	if rec := get(router, "/v1/reconciliations?legal_entity_id=LE-OTHER", "reader-1"); rec.Code != http.StatusForbidden {
		t.Fatalf("denied list: expected 403, got %d: %s", rec.Code, rec.Body.String())
	}
}

// "Every entity in the tenant" is not something one decision can grant, so
// listing without an entity is refused rather than authorized against
// nothing.
func TestListJobs_RequiresLegalEntity(t *testing.T) {
	az := &recordingAuthz{}
	router := newTestRouter(t, deps{authz: az})
	createJob(t, router)

	if rec := get(router, "/v1/reconciliations", "reader-1"); rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 without legal_entity_id, got %d: %s", rec.Code, rec.Body.String())
	}
	if n := len(az.asked(handler.RECONCILIATION_READ)); n != 0 {
		t.Fatalf("no read decision should be asked for, got %d", n)
	}
}

func TestReads_RequirePrincipal(t *testing.T) {
	router := setupTestRouter(t)
	id := createJob(t, router)
	for _, path := range []string{"/v1/reconciliations/" + id, "/v1/reconciliations?legal_entity_id=LE-3003"} {
		if rec := get(router, path, ""); rec.Code != http.StatusUnauthorized {
			t.Errorf("%s with no principal: expected 401, got %d: %s", path, rec.Code, rec.Body.String())
		}
	}
}

// authorization-svc unreachable: reads fail closed, never open.
func TestReads_AuthzUnavailable_FailClosed(t *testing.T) {
	memStore := store.NewMemoryStore()
	id := createJob(t, newTestRouter(t, deps{store: memStore}))

	// Same store, authorization-svc unreachable.
	router := newTestRouter(t, deps{store: memStore, authzURL: "http://127.0.0.1:1"})
	for _, path := range []string{"/v1/reconciliations/" + id, "/v1/reconciliations?legal_entity_id=LE-3003"} {
		if rec := get(router, path, "reader-1"); rec.Code != http.StatusServiceUnavailable {
			t.Errorf("%s with authorization-svc down: expected 503, got %d: %s", path, rec.Code, rec.Body.String())
		}
	}
}
