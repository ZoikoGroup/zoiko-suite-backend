package handler_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"go.uber.org/zap"

	"zoiko.io/secret-vault-integration-svc/internal/domain"
	"zoiko.io/secret-vault-integration-svc/internal/handler"
	"zoiko.io/secret-vault-integration-svc/internal/store"
)

// ── stub store ────────────────────────────────────────────────────────────────

type stubStore struct {
	policy        *domain.SecretPolicy
	policyCreated bool
	policyErr     error

	findPolicyResult *domain.SecretPolicy
	findPolicyErr    error

	version        *domain.SecretPolicyVersion
	versionCreated bool
	versionErr     error

	findVersionResult *domain.SecretPolicyVersion
	findVersionErr    error

	activated    *domain.SecretPolicyVersion
	activateErr  error

	history    []*domain.SecretPolicyVersion
	historyErr error

	applicable    []*domain.ApplicableSecretPolicyVersion
	applicableErr error

	applicableByPath    *domain.ApplicableSecretPolicyVersion
	applicableByPathErr error

	lease        *domain.SecretLease
	leaseCreated bool
	leaseErr     error

	findLeaseResult *domain.SecretLease
	findLeaseErr    error

	listLeasesResult []*domain.SecretLease
	listLeasesErr    error

	revokeLeaseResult      *domain.SecretLease
	revokeLeaseTransitioned bool
	revokeLeaseErr         error

	revokedByPath    []*domain.SecretLease
	revokedByPathErr error

	auditEntries []domain.RecordAuditEntryParams
	auditErr     error

	rotationEntry    *domain.SecretAccessAuditLog
	rotationEntryErr error

	listAuditResult []*domain.SecretAccessAuditLog
	listAuditErr    error
}

func (s *stubStore) CreateSecretPolicy(_ context.Context, _ domain.CreateSecretPolicyParams) (*domain.SecretPolicy, bool, error) {
	return s.policy, s.policyCreated, s.policyErr
}
func (s *stubStore) FindSecretPolicyByID(_ context.Context, _ string) (*domain.SecretPolicy, error) {
	return s.findPolicyResult, s.findPolicyErr
}
func (s *stubStore) CreateSecretPolicyVersion(_ context.Context, _ domain.CreateSecretPolicyVersionParams) (*domain.SecretPolicyVersion, bool, error) {
	return s.version, s.versionCreated, s.versionErr
}
func (s *stubStore) FindSecretPolicyVersionByID(_ context.Context, _ string) (*domain.SecretPolicyVersion, error) {
	return s.findVersionResult, s.findVersionErr
}
func (s *stubStore) ActivateVersion(_ context.Context, _, _ string) (*domain.SecretPolicyVersion, []*domain.SecretPolicyVersion, bool, error) {
	return s.activated, nil, s.activated != nil, s.activateErr
}
func (s *stubStore) ListVersionHistory(_ context.Context, _ string) ([]*domain.SecretPolicyVersion, error) {
	return s.history, s.historyErr
}
func (s *stubStore) FindApplicableVersions(_ context.Context, _ string, _, _ *string) ([]*domain.ApplicableSecretPolicyVersion, error) {
	return s.applicable, s.applicableErr
}
func (s *stubStore) FindApplicableVersionByPath(_ context.Context, _ string, _, _ *string) (*domain.ApplicableSecretPolicyVersion, error) {
	return s.applicableByPath, s.applicableByPathErr
}
func (s *stubStore) CreateLease(_ context.Context, _ domain.CreateLeaseParams) (*domain.SecretLease, bool, error) {
	return s.lease, s.leaseCreated, s.leaseErr
}
func (s *stubStore) FindLeaseByID(_ context.Context, _ string) (*domain.SecretLease, error) {
	return s.findLeaseResult, s.findLeaseErr
}
func (s *stubStore) ListLeases(_ context.Context, _ store.LeaseListFilter) ([]*domain.SecretLease, error) {
	return s.listLeasesResult, s.listLeasesErr
}
func (s *stubStore) RevokeLease(_ context.Context, _ string) (*domain.SecretLease, bool, error) {
	return s.revokeLeaseResult, s.revokeLeaseTransitioned, s.revokeLeaseErr
}
func (s *stubStore) RevokeLeasesBySecretPath(_ context.Context, _ string) ([]*domain.SecretLease, error) {
	return s.revokedByPath, s.revokedByPathErr
}
func (s *stubStore) RecordAuditEntry(_ context.Context, params domain.RecordAuditEntryParams) (*domain.SecretAccessAuditLog, error) {
	s.auditEntries = append(s.auditEntries, params)
	return &domain.SecretAccessAuditLog{AuditLogID: "audit-1", EventType: params.EventType, RecordedAt: time.Now().UTC()}, s.auditErr
}
func (s *stubStore) FindAuditEntryByRotationRequestID(_ context.Context, _ string) (*domain.SecretAccessAuditLog, error) {
	return s.rotationEntry, s.rotationEntryErr
}
func (s *stubStore) ListAuditLog(_ context.Context, _ store.AuditListFilter) ([]*domain.SecretAccessAuditLog, error) {
	return s.listAuditResult, s.listAuditErr
}

// ── stub vault backend ───────────────────────────────────────────────────────

type stubVault struct {
	getToken    string
	getErr      error
	putErr      error
	putCalls    int
	rotateErr   error
	rotateCalls int
}

func (v *stubVault) Get(_ context.Context, _ string) (string, error) { return v.getToken, v.getErr }
func (v *stubVault) Put(_ context.Context, _ string, _ []byte) error {
	v.putCalls++
	return v.putErr
}
func (v *stubVault) Rotate(_ context.Context, _ string) error {
	v.rotateCalls++
	return v.rotateErr
}

// ── stub publisher ───────────────────────────────────────────────────────────

type stubPublisher struct {
	requestedCalls int
	grantedCalls   int
	rotationCalls  int
}

func (p *stubPublisher) PublishAccessRequested(_ context.Context, _, _, _ string) error {
	p.requestedCalls++
	return nil
}
func (p *stubPublisher) PublishAccessGranted(_ context.Context, _ domain.SecretLease, _ string) error {
	p.grantedCalls++
	return nil
}
func (p *stubPublisher) PublishRotationCompleted(_ context.Context, _, _ string, _ int, _ string) error {
	p.rotationCalls++
	return nil
}

func newTestRouter(s *stubStore, v *stubVault, p *stubPublisher) chi.Router {
	r := chi.NewRouter()
	h := handler.New(s, v, p, zap.NewNop())
	handler.RegisterRoutes(r, h)
	return r
}

func defaultRouter(s *stubStore) chi.Router {
	return newTestRouter(s, &stubVault{getToken: "local-lease:stub"}, &stubPublisher{})
}

// ── CreateSecretPolicy ───────────────────────────────────────────────────────

func TestCreateSecretPolicy_Created(t *testing.T) {
	s := &stubStore{
		policy:        &domain.SecretPolicy{SecretPolicyID: "sp-1", SecretClass: "DATABASE_CREDENTIAL", SecretPath: "kv/db"},
		policyCreated: true,
	}
	r := defaultRouter(s)

	body := `{"secret_class":"DATABASE_CREDENTIAL","secret_path":"kv/db","created_by_principal_id":"admin-1"}`
	req := httptest.NewRequest(http.MethodPost, "/v1/secret-policies", strings.NewReader(body))
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d: %s", w.Code, w.Body.String())
	}
}

func TestCreateSecretPolicy_MissingField(t *testing.T) {
	r := defaultRouter(&stubStore{})
	req := httptest.NewRequest(http.MethodPost, "/v1/secret-policies", strings.NewReader(`{"secret_class":"X"}`))
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", w.Code)
	}
}

func TestCreateSecretPolicy_Conflict(t *testing.T) {
	s := &stubStore{policyErr: domain.ErrConflict}
	r := defaultRouter(s)
	body := `{"secret_class":"X","secret_path":"kv/x","created_by_principal_id":"admin-1"}`
	req := httptest.NewRequest(http.MethodPost, "/v1/secret-policies", strings.NewReader(body))
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusConflict {
		t.Fatalf("expected 409, got %d", w.Code)
	}
}

func TestCreateSecretPolicy_InvalidDataClassification(t *testing.T) {
	r := defaultRouter(&stubStore{})
	body := `{"secret_class":"DATABASE_CREDENTIAL","secret_path":"kv/db","created_by_principal_id":"admin-1","data_classification":"INVALID_CLASSIFICATION"}`
	req := httptest.NewRequest(http.MethodPost, "/v1/secret-policies", strings.NewReader(body))
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d: %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "invalid_classification") {
		t.Fatalf("expected invalid_classification error message, got %s", w.Body.String())
	}
}

// ── CreateSecretPolicyVersion ────────────────────────────────────────────────

func TestCreateSecretPolicyVersion_Created(t *testing.T) {
	s := &stubStore{
		version:        &domain.SecretPolicyVersion{SecretPolicyVersionID: "spv-1", VersionStatus: "DRAFT"},
		versionCreated: true,
	}
	r := defaultRouter(s)
	body := `{"allowed_workload_ids":["svc-a"],"max_lease_duration_seconds":300,"effective_from":"2026-01-01T00:00:00Z","created_by_principal_id":"admin-1"}`
	req := httptest.NewRequest(http.MethodPost, "/v1/secret-policies/sp-1/versions", strings.NewReader(body))
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d: %s", w.Code, w.Body.String())
	}
}

func TestCreateSecretPolicyVersion_InvalidMaxLeaseDuration(t *testing.T) {
	r := defaultRouter(&stubStore{})
	body := `{"max_lease_duration_seconds":0,"effective_from":"2026-01-01T00:00:00Z","created_by_principal_id":"admin-1"}`
	req := httptest.NewRequest(http.MethodPost, "/v1/secret-policies/sp-1/versions", strings.NewReader(body))
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d: %s", w.Code, w.Body.String())
	}
}

func TestCreateSecretPolicyVersion_PolicyNotFound(t *testing.T) {
	s := &stubStore{versionErr: domain.ErrSecretPolicyNotFound}
	r := defaultRouter(s)
	body := `{"max_lease_duration_seconds":300,"effective_from":"2026-01-01T00:00:00Z","created_by_principal_id":"admin-1"}`
	req := httptest.NewRequest(http.MethodPost, "/v1/secret-policies/missing/versions", strings.NewReader(body))
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d", w.Code)
	}
}

// ── ActivateVersion ──────────────────────────────────────────────────────────

func TestActivateVersion_Success(t *testing.T) {
	s := &stubStore{
		findVersionResult: &domain.SecretPolicyVersion{SecretPolicyVersionID: "spv-1", SecretPolicyID: "sp-1", VersionStatus: "DRAFT"},
		activated:         &domain.SecretPolicyVersion{SecretPolicyVersionID: "spv-1", SecretPolicyID: "sp-1", VersionStatus: "ACTIVE"},
	}
	r := defaultRouter(s)
	body := `{"activated_by_principal_id":"admin-1"}`
	req := httptest.NewRequest(http.MethodPost, "/v1/secret-policies/sp-1/versions/spv-1/activate", strings.NewReader(body))
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
}

func TestActivateVersion_MissingActor(t *testing.T) {
	r := defaultRouter(&stubStore{})
	req := httptest.NewRequest(http.MethodPost, "/v1/secret-policies/sp-1/versions/spv-1/activate", strings.NewReader(`{}`))
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", w.Code)
	}
}

func TestActivateVersion_PolicyMismatch(t *testing.T) {
	s := &stubStore{findVersionResult: &domain.SecretPolicyVersion{SecretPolicyVersionID: "spv-1", SecretPolicyID: "OTHER"}}
	r := defaultRouter(s)
	body := `{"activated_by_principal_id":"admin-1"}`
	req := httptest.NewRequest(http.MethodPost, "/v1/secret-policies/sp-1/versions/spv-1/activate", strings.NewReader(body))
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d", w.Code)
	}
}

// ── PutSecretMaterial ────────────────────────────────────────────────────────

func TestPutSecretMaterial_Success(t *testing.T) {
	s := &stubStore{findPolicyResult: &domain.SecretPolicy{SecretPolicyID: "sp-1", SecretPath: "kv/db"}}
	v := &stubVault{}
	r := newTestRouter(s, v, &stubPublisher{})

	body := `{"material_base64":"c2VjcmV0LXZhbHVl"}`
	req := httptest.NewRequest(http.MethodPost, "/v1/secret-policies/sp-1/material", strings.NewReader(body))
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	if v.putCalls != 1 {
		t.Errorf("expected vault Put called once, got %d", v.putCalls)
	}
}

func TestPutSecretMaterial_MissingField(t *testing.T) {
	r := defaultRouter(&stubStore{})
	req := httptest.NewRequest(http.MethodPost, "/v1/secret-policies/sp-1/material", strings.NewReader(`{}`))
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", w.Code)
	}
}

func TestPutSecretMaterial_PolicyNotFound(t *testing.T) {
	s := &stubStore{findPolicyErr: domain.ErrSecretPolicyNotFound}
	r := defaultRouter(s)
	body := `{"material_base64":"c2VjcmV0"}`
	req := httptest.NewRequest(http.MethodPost, "/v1/secret-policies/missing/material", strings.NewReader(body))
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d", w.Code)
	}
}

// ── ListVersionHistory / ListApplicableSecretPolicyVersions ─────────────────

func TestListVersionHistory_EmptyReturnsArray(t *testing.T) {
	r := defaultRouter(&stubStore{history: nil})
	req := httptest.NewRequest(http.MethodGet, "/v1/secret-policies/sp-1/versions", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusOK || strings.TrimSpace(w.Body.String()) != "[]" {
		t.Fatalf("expected 200 empty array, got %d: %s", w.Code, w.Body.String())
	}
}

func TestListApplicableSecretPolicyVersions_MissingSecretClass(t *testing.T) {
	r := defaultRouter(&stubStore{})
	req := httptest.NewRequest(http.MethodGet, "/v1/secret-policies", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", w.Code)
	}
}

// ── Broker ───────────────────────────────────────────────────────────────────

func brokerBody(secretPath, principal, requestID string) string {
	return `{"secret_path":"` + secretPath + `","requested_by_principal_id":"` + principal + `","request_id":"` + requestID + `"}`
}

func TestBroker_Granted(t *testing.T) {
	s := &stubStore{
		applicableByPath: &domain.ApplicableSecretPolicyVersion{
			SecretPolicyVersion: domain.SecretPolicyVersion{
				SecretPolicyVersionID:   "spv-1",
				AllowedWorkloadIDs:      json.RawMessage(`["svc-a"]`),
				MaxLeaseDurationSeconds: 300,
			},
			SecretClass: "DATABASE_CREDENTIAL",
			SecretPath:  "kv/db",
		},
		lease: &domain.SecretLease{
			LeaseID: "lease-1", SecretPath: "kv/db", ExpiresAt: time.Now().Add(5 * time.Minute),
		},
		leaseCreated: true,
	}
	v := &stubVault{getToken: "local-lease:abc"}
	pub := &stubPublisher{}
	r := newTestRouter(s, v, pub)

	req := httptest.NewRequest(http.MethodPost, "/v1/secrets/broker", strings.NewReader(brokerBody("kv/db", "svc-a", "req-1")))
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	if pub.grantedCalls != 1 {
		t.Errorf("expected secret.access.granted published once, got %d", pub.grantedCalls)
	}
	if pub.requestedCalls != 1 {
		t.Errorf("expected secret.access.requested published once, got %d", pub.requestedCalls)
	}
	var got map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("failed to unmarshal: %v", err)
	}
	if got["lease_token"] != "local-lease:abc" {
		t.Errorf("expected lease_token forwarded, got %v", got["lease_token"])
	}
}

func TestBroker_NoApplicablePolicy(t *testing.T) {
	s := &stubStore{applicableByPathErr: domain.ErrSecretPolicyNotFound}
	r := defaultRouter(s)

	req := httptest.NewRequest(http.MethodPost, "/v1/secrets/broker", strings.NewReader(brokerBody("kv/missing", "svc-a", "req-1")))
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d: %s", w.Code, w.Body.String())
	}
	if len(s.auditEntries) != 2 { // REQUESTED + DENIED
		t.Fatalf("expected 2 audit entries (REQUESTED, DENIED), got %d", len(s.auditEntries))
	}
	if s.auditEntries[1].EventType != "DENIED" {
		t.Errorf("expected second audit entry DENIED, got %s", s.auditEntries[1].EventType)
	}
}

func TestBroker_NotAuthorized(t *testing.T) {
	s := &stubStore{
		applicableByPath: &domain.ApplicableSecretPolicyVersion{
			SecretPolicyVersion: domain.SecretPolicyVersion{
				SecretPolicyVersionID: "spv-1",
				AllowedWorkloadIDs:    json.RawMessage(`["svc-a"]`),
			},
			SecretClass: "DATABASE_CREDENTIAL",
			SecretPath:  "kv/db",
		},
	}
	r := defaultRouter(s)

	req := httptest.NewRequest(http.MethodPost, "/v1/secrets/broker", strings.NewReader(brokerBody("kv/db", "svc-b", "req-1")))
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusForbidden {
		t.Fatalf("expected 403, got %d: %s", w.Code, w.Body.String())
	}
	if len(s.auditEntries) != 2 { // REQUESTED + DENIED
		t.Fatalf("expected 2 audit entries (REQUESTED, DENIED), got %d", len(s.auditEntries))
	}
	denied := s.auditEntries[1]
	if denied.EventType != "DENIED" {
		t.Errorf("expected second audit entry DENIED, got %s", denied.EventType)
	}
	// A policy WAS resolved here — the caller just wasn't authorized — so
	// secret_class must be preserved, unlike the no-applicable-policy case.
	if denied.SecretClass != "DATABASE_CREDENTIAL" {
		t.Errorf("expected DENIED audit entry to carry the resolved secret_class, got %q", denied.SecretClass)
	}
}

func TestBroker_CorrelationIDFromBody_UsedWhenHeaderAbsent(t *testing.T) {
	s := &stubStore{
		applicableByPath: &domain.ApplicableSecretPolicyVersion{
			SecretPolicyVersion: domain.SecretPolicyVersion{
				SecretPolicyVersionID:   "spv-1",
				AllowedWorkloadIDs:      json.RawMessage(`["svc-a"]`),
				MaxLeaseDurationSeconds: 300,
			},
			SecretClass: "DATABASE_CREDENTIAL",
			SecretPath:  "kv/db",
		},
		lease: &domain.SecretLease{
			LeaseID: "lease-1", SecretPath: "kv/db", ExpiresAt: time.Now().Add(5 * time.Minute),
		},
		leaseCreated: true,
	}
	r := defaultRouter(s)

	body := `{"secret_path":"kv/db","requested_by_principal_id":"svc-a","request_id":"req-1","correlation_id":"corr-from-body"}`
	req := httptest.NewRequest(http.MethodPost, "/v1/secrets/broker", strings.NewReader(body))
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	for _, e := range s.auditEntries {
		if e.CorrelationID != "corr-from-body" {
			t.Errorf("expected correlation_id %q from body to flow into audit entry, got %q", "corr-from-body", e.CorrelationID)
		}
	}
}

func TestBroker_MissingField(t *testing.T) {
	r := defaultRouter(&stubStore{})
	req := httptest.NewRequest(http.MethodPost, "/v1/secrets/broker", strings.NewReader(`{"secret_path":"kv/db"}`))
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", w.Code)
	}
}

func TestBroker_VaultUnavailable(t *testing.T) {
	s := &stubStore{
		applicableByPath: &domain.ApplicableSecretPolicyVersion{
			SecretPolicyVersion: domain.SecretPolicyVersion{
				SecretPolicyVersionID: "spv-1",
				AllowedWorkloadIDs:    json.RawMessage(`["svc-a"]`),
			},
			SecretPath: "kv/db",
		},
	}
	v := &stubVault{getErr: context.DeadlineExceeded}
	r := newTestRouter(s, v, &stubPublisher{})

	req := httptest.NewRequest(http.MethodPost, "/v1/secrets/broker", strings.NewReader(brokerBody("kv/db", "svc-a", "req-1")))
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected 503, got %d", w.Code)
	}
}

// ── Leases ───────────────────────────────────────────────────────────────────

func TestGetLease_Found(t *testing.T) {
	s := &stubStore{findLeaseResult: &domain.SecretLease{LeaseID: "lease-1"}}
	r := defaultRouter(s)
	req := httptest.NewRequest(http.MethodGet, "/v1/secrets/leases/lease-1", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
}

func TestGetLease_NotFound(t *testing.T) {
	s := &stubStore{findLeaseErr: domain.ErrLeaseNotFound}
	r := defaultRouter(s)
	req := httptest.NewRequest(http.MethodGet, "/v1/secrets/leases/missing", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d", w.Code)
	}
}

func TestListLeases_EmptyReturnsArray(t *testing.T) {
	r := defaultRouter(&stubStore{listLeasesResult: nil})
	req := httptest.NewRequest(http.MethodGet, "/v1/secrets/leases", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusOK || strings.TrimSpace(w.Body.String()) != "[]" {
		t.Fatalf("expected 200 empty array, got %d: %s", w.Code, w.Body.String())
	}
}

func TestRevokeLease_Success(t *testing.T) {
	s := &stubStore{
		revokeLeaseResult:       &domain.SecretLease{LeaseID: "lease-1", Status: "REVOKED"},
		revokeLeaseTransitioned: true,
	}
	r := defaultRouter(s)
	req := httptest.NewRequest(http.MethodPost, "/v1/secrets/leases/lease-1/revoke", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	if len(s.auditEntries) != 1 || s.auditEntries[0].EventType != "REVOKED" {
		t.Errorf("expected one REVOKED audit entry, got %+v", s.auditEntries)
	}
}

func TestRevokeLease_InvalidTransition(t *testing.T) {
	s := &stubStore{revokeLeaseErr: domain.ErrInvalidTransition}
	r := defaultRouter(s)
	req := httptest.NewRequest(http.MethodPost, "/v1/secrets/leases/lease-1/revoke", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusConflict {
		t.Fatalf("expected 409, got %d", w.Code)
	}
}

// ── Rotate ───────────────────────────────────────────────────────────────────

func TestRotate_Success(t *testing.T) {
	s := &stubStore{
		findPolicyResult: &domain.SecretPolicy{SecretPolicyID: "sp-1", SecretClass: "DATABASE_CREDENTIAL", SecretPath: "kv/db"},
		revokedByPath: []*domain.SecretLease{
			{LeaseID: "lease-1", SecretPath: "kv/db"},
		},
	}
	v := &stubVault{}
	pub := &stubPublisher{}
	r := newTestRouter(s, v, pub)

	body := `{"request_id":"rot-1","rotated_by_principal_id":"admin-1"}`
	req := httptest.NewRequest(http.MethodPost, "/v1/secret-policies/sp-1/rotate", strings.NewReader(body))
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	if v.rotateCalls != 1 {
		t.Errorf("expected vault Rotate called once, got %d", v.rotateCalls)
	}
	if pub.rotationCalls != 1 {
		t.Errorf("expected secret.rotation.completed published once, got %d", pub.rotationCalls)
	}
	// One REVOKED entry for the affected lease + one ROTATED entry.
	if len(s.auditEntries) != 2 {
		t.Fatalf("expected 2 audit entries (REVOKED, ROTATED), got %d: %+v", len(s.auditEntries), s.auditEntries)
	}
	var got map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("failed to unmarshal: %v", err)
	}
	if got["revoked_lease_count"].(float64) != 1 {
		t.Errorf("expected revoked_lease_count=1, got %v", got["revoked_lease_count"])
	}
}

func TestRotate_IdempotentReplay_DoesNotRotateAgain(t *testing.T) {
	s := &stubStore{
		rotationEntry: &domain.SecretAccessAuditLog{SecretPath: "kv/db", RecordedAt: time.Now()},
	}
	v := &stubVault{}
	r := newTestRouter(s, v, &stubPublisher{})

	body := `{"request_id":"rot-1","rotated_by_principal_id":"admin-1"}`
	req := httptest.NewRequest(http.MethodPost, "/v1/secret-policies/sp-1/rotate", strings.NewReader(body))
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	if v.rotateCalls != 0 {
		t.Errorf("expected vault Rotate NOT called on idempotent replay, got %d calls", v.rotateCalls)
	}
}

func TestRotate_MissingRequestID(t *testing.T) {
	r := defaultRouter(&stubStore{})
	body := `{"rotated_by_principal_id":"admin-1"}`
	req := httptest.NewRequest(http.MethodPost, "/v1/secret-policies/sp-1/rotate", strings.NewReader(body))
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", w.Code)
	}
}

func TestRotate_PolicyNotFound(t *testing.T) {
	s := &stubStore{findPolicyErr: domain.ErrSecretPolicyNotFound}
	r := defaultRouter(s)
	body := `{"request_id":"rot-1","rotated_by_principal_id":"admin-1"}`
	req := httptest.NewRequest(http.MethodPost, "/v1/secret-policies/missing/rotate", strings.NewReader(body))
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d", w.Code)
	}
}

// ── ListAuditLog ─────────────────────────────────────────────────────────────

func TestListAuditLog_EmptyReturnsArray(t *testing.T) {
	r := defaultRouter(&stubStore{listAuditResult: nil})
	req := httptest.NewRequest(http.MethodGet, "/v1/secrets/audit", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusOK || strings.TrimSpace(w.Body.String()) != "[]" {
		t.Fatalf("expected 200 empty array, got %d: %s", w.Code, w.Body.String())
	}
}
