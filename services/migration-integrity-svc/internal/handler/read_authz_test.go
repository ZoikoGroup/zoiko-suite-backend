package handler_test

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"go.uber.org/zap"
	"zoiko.io/migration-integrity-svc/internal/authz"
	"zoiko.io/migration-integrity-svc/internal/domain"
	"zoiko.io/migration-integrity-svc/internal/handler"
	"zoiko.io/migration-integrity-svc/internal/store"
)

// Read-authorization tests (SPEC_DEVIATIONS A9): GET /v1/migrations and
// GET /v1/migrations/{id} checked only the tenant (RLS), so any principal in
// the tenant could read every legal entity's migration jobs. Reads now run
// the same per-entity CheckAllowed the writes do, against MIGRATION_JOB_VIEW.

// authzSpy is a stand-in authorization-svc that records every request and
// answers with the decision its callback returns.
type authzSpy struct {
	mu       sync.Mutex
	requests []map[string]string
}

func (s *authzSpy) record(body map[string]string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.requests = append(s.requests, body)
}

func (s *authzSpy) find(actionType string) map[string]string {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, req := range s.requests {
		if req["action_type"] == actionType {
			return req
		}
	}
	return nil
}

func newRouterWithAuthz(t *testing.T, decide func(body map[string]string) string) (http.Handler, *authzSpy) {
	t.Helper()
	spy := &authzSpy{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]string
		_ = json.NewDecoder(r.Body).Decode(&body)
		spy.record(body)
		outcome := "GRANTED"
		if decide != nil {
			outcome = decide(body)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(map[string]string{"decision_outcome": outcome})
	}))
	t.Cleanup(srv.Close)

	logger := zap.NewNop()
	h := handler.NewHandler(store.NewMemoryStore(), authz.NewClient(srv.URL, logger), logger, 95)
	return handler.NewRouter(h), spy
}

// validateOneJob creates a read fixture through the real router.
func validateOneJob(t *testing.T, router http.Handler, tenantID, entityID, principalID string) domain.MigrationJob {
	t.Helper()
	validateReq := domain.ValidateMigrationRequest{
		LegalEntityID:  entityID,
		MigrationName:  "read authorization fixture",
		SourceSystem:   "LEGACY_ERP",
		TargetService:  "ledger-svc",
		RequiredFields: []string{"amount"},
		Records: []domain.MigrationRecord{
			{Ref: "REC-READ-1", Fields: map[string]string{"amount": "12.00"}},
		},
	}
	body, _ := json.Marshal(validateReq)
	req := httptest.NewRequest(http.MethodPost, "/v1/migrations/validate", bytes.NewBuffer(body))
	withEnvelope(req)
	req.Header.Set("X-Tenant-ID", tenantID)
	req.Header.Set("X-Principal-Id", principalID)
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("fixture: expected 201, got %d: %s", rec.Code, rec.Body.String())
	}
	var job domain.MigrationJob
	_ = json.Unmarshal(rec.Body.Bytes(), &job)
	return job
}

// issueRead sends one GET with the test tenant and an optional principal
// (empty means the header is absent, which must be refused).
func issueRead(t *testing.T, router http.Handler, path, tenantID, principalID string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	withEnvelope(req)
	req.Header.Set("X-Tenant-ID", tenantID)
	if principalID == "" {
		req.Header.Del("X-Principal-Id")
	} else {
		req.Header.Set("X-Principal-Id", principalID)
	}
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	return rec
}

// The read check is the write check: action MIGRATION_JOB_VIEW, evaluated at
// the job's own legal entity.
func TestReads_AuthorizePerLegalEntity(t *testing.T) {
	router, spy := newRouterWithAuthz(t, nil) // grants everything
	job := validateOneJob(t, router, "tenant-read-authz", "LE-READ-1", "reader-1")

	for _, tc := range []struct{ name, path string }{
		{"get job", "/v1/migrations/" + job.ID},
		{"list jobs", "/v1/migrations?legal_entity_id=LE-READ-1"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if rec := issueRead(t, router, tc.path, "tenant-read-authz", "reader-1"); rec.Code != http.StatusOK {
				t.Fatalf("expected 200 for an authorized reader, got %d: %s", rec.Code, rec.Body.String())
			}
		})
	}

	view := spy.find("MIGRATION_JOB_VIEW")
	if view == nil {
		t.Fatal("reads must call authorization-svc with MIGRATION_JOB_VIEW")
	}
	if view["legal_entity_id"] != "LE-READ-1" {
		t.Fatalf("read check must run at the job's legal entity, got %q", view["legal_entity_id"])
	}
	if view["principal_id"] != "reader-1" {
		t.Fatalf("read check must name the calling principal, got %q", view["principal_id"])
	}
}

// Denied at the entity: 403 for both reads. The author-time write stays
// granted, so the denial is the read's own.
func TestReads_DeniedPerEntity(t *testing.T) {
	router, _ := newRouterWithAuthz(t, func(body map[string]string) string {
		if body["action_type"] == "MIGRATION_JOB_VIEW" {
			return "DENIED"
		}
		return "GRANTED"
	})
	job := validateOneJob(t, router, "tenant-read-authz", "LE-READ-1", "reader-2")

	for _, tc := range []struct{ name, path string }{
		{"get job", "/v1/migrations/" + job.ID},
		{"list jobs", "/v1/migrations?legal_entity_id=LE-READ-1"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if rec := issueRead(t, router, tc.path, "tenant-read-authz", "reader-2"); rec.Code != http.StatusForbidden {
				t.Fatalf("expected 403 when the entity read is denied, got %d: %s", rec.Code, rec.Body.String())
			}
		})
	}
}

// A tenant-wide listing has no legal entity to authorize against; it must be
// refused rather than silently returning every entity's jobs.
func TestListJobs_RequiresLegalEntity(t *testing.T) {
	router, _ := newRouterWithAuthz(t, nil)
	validateOneJob(t, router, "tenant-read-authz", "LE-READ-1", "reader-1")

	if rec := issueRead(t, router, "/v1/migrations", "tenant-read-authz", "reader-1"); rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 without legal_entity_id, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestReads_RequirePrincipal(t *testing.T) {
	router, _ := newRouterWithAuthz(t, nil)
	job := validateOneJob(t, router, "tenant-read-authz", "LE-READ-1", "reader-1")

	for _, path := range []string{"/v1/migrations/" + job.ID, "/v1/migrations?legal_entity_id=LE-READ-1"} {
		if rec := issueRead(t, router, path, "tenant-read-authz", ""); rec.Code != http.StatusUnauthorized {
			t.Fatalf("expected 401 without a principal on %s, got %d", path, rec.Code)
		}
	}
}

// Raw source values are evidence, not exposure -- as long as where they can
// be read is pinned. This test pins the boundary decided for SPEC_DEVIATIONS
// A10: the value reaches only a principal authorized per legal entity for
// the job (a denied read echoes nothing), and the list read never carries
// audit entries at all.
func TestReads_SourceValuesOnlyForAuthorizedReaders(t *testing.T) {
	const rawSalary = "12,345.67" // fails the numeric format check; kept raw as evidence

	// newJobWithRawValue creates one job whose single violation carries the
	// raw offending value, through the real router.
	newJobWithRawValue := func(t *testing.T, router http.Handler, principalID string) domain.MigrationJob {
		t.Helper()
		validateReq := domain.ValidateMigrationRequest{
			LegalEntityID:  "LE-READ-VALUE",
			MigrationName:  "raw source value fixture",
			SourceSystem:   "LEGACY_ERP",
			TargetService:  "payroll-svc",
			RequiredFields: []string{"salary"},
			Records: []domain.MigrationRecord{
				{Ref: "REC-VALUE-1", Fields: map[string]string{"salary": rawSalary}},
			},
		}
		body, _ := json.Marshal(validateReq)
		req := httptest.NewRequest(http.MethodPost, "/v1/migrations/validate", bytes.NewBuffer(body))
		withEnvelope(req)
		req.Header.Set("X-Tenant-ID", "tenant-read-authz")
		req.Header.Set("X-Principal-Id", principalID)
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, req)
		if rec.Code != http.StatusCreated {
			t.Fatalf("fixture: expected 201, got %d: %s", rec.Code, rec.Body.String())
		}
		var job domain.MigrationJob
		_ = json.Unmarshal(rec.Body.Bytes(), &job)
		return job
	}

	t.Run("denied reader never sees the value", func(t *testing.T) {
		router, _ := newRouterWithAuthz(t, func(body map[string]string) string {
			if body["action_type"] == "MIGRATION_JOB_VIEW" {
				return "DENIED"
			}
			return "GRANTED"
		})
		job := newJobWithRawValue(t, router, "reader-value-denied")
		rec := issueRead(t, router, "/v1/migrations/"+job.ID, "tenant-read-authz", "reader-value-denied")
		if rec.Code != http.StatusForbidden {
			t.Fatalf("expected 403 for a denied reader, got %d", rec.Code)
		}
		if strings.Contains(rec.Body.String(), rawSalary) {
			t.Fatal("a denied read must not echo the raw source value")
		}
	})

	t.Run("authorized reader gets the evidence", func(t *testing.T) {
		router, _ := newRouterWithAuthz(t, nil) // grants everything
		job := newJobWithRawValue(t, router, "reader-value-allowed")
		rec := issueRead(t, router, "/v1/migrations/"+job.ID, "tenant-read-authz", "reader-value-allowed")
		if rec.Code != http.StatusOK {
			t.Fatalf("expected 200 for an authorized reader, got %d: %s", rec.Code, rec.Body.String())
		}
		if !strings.Contains(rec.Body.String(), rawSalary) {
			t.Fatal("an authorized read must return the raw source value: it is the evidence remediation acts on")
		}
	})

	t.Run("list read carries no source values", func(t *testing.T) {
		router, _ := newRouterWithAuthz(t, nil)
		newJobWithRawValue(t, router, "reader-value-list")
		rec := issueRead(t, router, "/v1/migrations?legal_entity_id=LE-READ-VALUE", "tenant-read-authz", "reader-value-list")
		if rec.Code != http.StatusOK {
			t.Fatalf("expected 200 for an authorized list read, got %d: %s", rec.Code, rec.Body.String())
		}
		if strings.Contains(rec.Body.String(), rawSalary) || strings.Contains(rec.Body.String(), "audit_entries") {
			t.Fatal("the list read returns job summaries and must not carry audit entries / source values")
		}
	})
}
