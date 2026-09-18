package handler_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"go.uber.org/zap"

	"zoiko.io/secret-vault-integration-svc/internal/authz"
	"zoiko.io/secret-vault-integration-svc/internal/domain"
	"zoiko.io/secret-vault-integration-svc/internal/handler"
	svcmiddleware "zoiko.io/secret-vault-integration-svc/internal/middleware"
)

// ── malformed UUID path parameters ───────────────────────────────────────────

// TestPathParams_MalformedUUID_Are400NotServiceUnavailable pins the fix for a
// real defect found by probing the running service, not by reading the code.
//
// Every id in this service's routes maps to a Postgres UUID column. A value
// that is not a UUID was passed straight through to the driver, which rejected
// it with a generic query error — and every handler translates an unrecognised
// store error into 503 store_unavailable. So `GET /v1/secrets/leases/not-a-uuid`
// answered "this service is down" to what is purely a caller mistake. A client
// reading 503 retries with backoff and eventually pages someone, so one
// malformed id looked exactly like an outage.
//
// All six id-addressed routes are asserted together because the bug was
// uniform across them: fixing one and missing the rest would leave the same
// false outage reachable by a different URL.
func TestPathParams_MalformedUUID_Are400NotServiceUnavailable(t *testing.T) {
	const badID = "not-a-uuid"

	cases := []struct {
		name   string
		method string
		path   string
		body   string
		field  string
	}{
		{"CreateSecretPolicyVersion", http.MethodPost, "/v1/secret-policies/" + badID + "/versions",
			`{"effective_from":"2026-01-01T00:00:00Z","created_by_principal_id":"p","max_lease_duration_seconds":300}`, "secret_policy_id"},
		{"ActivateVersion", http.MethodPost, "/v1/secret-policies/" + badID + "/versions/" + validVersionID + "/activate",
			`{"activated_by_principal_id":"p"}`, "secret_policy_id"},
		{"ActivateVersion_versionID", http.MethodPost, "/v1/secret-policies/" + validPolicyID + "/versions/" + badID + "/activate",
			`{"activated_by_principal_id":"p"}`, "version_id"},
		{"ListVersionHistory", http.MethodGet, "/v1/secret-policies/" + badID + "/versions", "", "secret_policy_id"},
		{"PutSecretMaterial", http.MethodPost, "/v1/secret-policies/" + badID + "/material",
			`{"material_base64":"YWJj","written_by_principal_id":"p"}`, "secret_policy_id"},
		{"Rotate", http.MethodPost, "/v1/secret-policies/" + badID + "/rotate",
			`{"request_id":"r1","rotated_by_principal_id":"p"}`, "secret_policy_id"},
		{"GetLease", http.MethodGet, "/v1/secrets/leases/" + badID, "", "lease_id"},
		{"RevokeLease", http.MethodPost, "/v1/secrets/leases/" + badID + "/revoke",
			`{"revoked_by_principal_id":"p"}`, "lease_id"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := defaultRouter(&stubStore{})
			req := authed(httptest.NewRequest(tc.method, tc.path, strings.NewReader(tc.body)))
			w := httptest.NewRecorder()
			r.ServeHTTP(w, req)

			if w.Code != http.StatusBadRequest {
				t.Fatalf("expected 400 for a malformed id, got %d: %s", w.Code, w.Body.String())
			}
			var got map[string]string
			if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
				t.Fatalf("failed to unmarshal refusal: %v", err)
			}
			if got["error"] != "invalid_path_parameter" {
				t.Errorf("expected error=invalid_path_parameter, got %q", got["error"])
			}
			// The caller has to be told WHICH id it got wrong: two of these
			// routes carry two ids, and "invalid" without a field name is not
			// actionable on the one that has both.
			if got["field"] != tc.field {
				t.Errorf("expected field=%q, got %q", tc.field, got["field"])
			}
		})
	}
}

// TestPathParams_WellFormedButUnknownUUID_Still404 guards the other side of
// the fix: the guard must reject only syntactically invalid ids. A well-formed
// id that matches no row is a different answer — the row could have existed —
// and collapsing the two would tell a caller to stop looking for something it
// addressed perfectly correctly.
func TestPathParams_WellFormedButUnknownUUID_Still404(t *testing.T) {
	s := &stubStore{findLeaseErr: domain.ErrLeaseNotFound}
	r := defaultRouter(s)
	req := authed(httptest.NewRequest(http.MethodGet, "/v1/secrets/leases/"+unknownButValidID, nil))
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusNotFound {
		t.Fatalf("expected 404 for a well-formed but unknown id, got %d: %s", w.Code, w.Body.String())
	}
}

// ── rotation evidence ────────────────────────────────────────────────────────

// TestRotate_RotatedAuditEntryCarriesCallerTenant pins the fix for a defect
// that no unit test could have caught, because it only shows up when the
// write and the read are put together against a real database.
//
// The ROTATED audit entry was recorded with no tenant_id at all. ListAuditLog
// always filters on the caller's verified tenant scope, so a row with
// tenant_id = NULL matched nobody: rotation — the one event that invalidates
// every lease on a path — never appeared in any tenant's audit log, including
// the tenant that performed it. Verified live before the fix: five audit rows
// came back from the API for a path whose table held six.
func TestRotate_RotatedAuditEntryCarriesCallerTenant(t *testing.T) {
	s := &stubStore{
		findPolicyResult: &domain.SecretPolicy{
			SecretPolicyID: validPolicyID, SecretClass: "DATABASE_CREDENTIAL", SecretPath: "kv/db",
		},
		revokedByPath: []*domain.SecretLease{{LeaseID: validLeaseID, SecretPath: "kv/db"}},
	}
	r := newTestRouter(s, &stubVault{}, &stubPublisher{})

	body := `{"request_id":"rot-tenant-1","rotated_by_principal_id":"admin-1"}`
	req := authed(httptest.NewRequest(http.MethodPost, "/v1/secret-policies/"+validPolicyID+"/rotate", strings.NewReader(body)))
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}

	var rotated *domain.RecordAuditEntryParams
	for i := range s.auditEntries {
		if s.auditEntries[i].EventType == "ROTATED" {
			rotated = &s.auditEntries[i]
		}
	}
	if rotated == nil {
		t.Fatalf("no ROTATED audit entry recorded: %+v", s.auditEntries)
	}
	if rotated.TenantID == nil {
		t.Fatal("ROTATED audit entry has no tenant_id: it will be invisible to every tenant's audit query")
	}
	if *rotated.TenantID != testTenant {
		t.Errorf("ROTATED audit entry bound to tenant %q, want the caller's scope %q", *rotated.TenantID, testTenant)
	}
	// The count is recorded on the entry itself because it cannot be
	// reconstructed later: the REVOKED rows it produced are scattered across
	// whichever tenants held those leases.
	if rotated.OutcomeDetail != "revoked_lease_count=1" {
		t.Errorf("ROTATED outcome_detail = %q, want revoked_lease_count=1", rotated.OutcomeDetail)
	}
}

// TestRotate_IdempotentReplay_ReportsOriginalRevokedCount closes a gap the
// service's own progress notes flagged and left open: a replayed rotate
// answered revoked_lease_count: 0, which reads as "this rotation revoked
// nothing" — the opposite of what the first call did. The durable state was
// right either way; the response was not, and in an evidence trail the count
// is the substance of the record.
func TestRotate_IdempotentReplay_ReportsOriginalRevokedCount(t *testing.T) {
	s := &stubStore{
		rotationEntry: &domain.SecretAccessAuditLog{
			SecretPath:    "kv/db",
			OutcomeDetail: "revoked_lease_count=3",
			RecordedAt:    time.Now(),
		},
	}
	v := &stubVault{}
	r := newTestRouter(s, v, &stubPublisher{})

	body := `{"request_id":"rot-replay-1","rotated_by_principal_id":"admin-1"}`
	req := authed(httptest.NewRequest(http.MethodPost, "/v1/secret-policies/"+validPolicyID+"/rotate", strings.NewReader(body)))
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	if v.rotateCalls != 0 {
		t.Errorf("replay must not rotate again, got %d vault calls", v.rotateCalls)
	}
	var got map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("failed to unmarshal: %v", err)
	}
	if got["revoked_lease_count"].(float64) != 3 {
		t.Errorf("replay reported revoked_lease_count=%v, want 3 (what the original rotation actually revoked)", got["revoked_lease_count"])
	}
}

// TestRotate_ReplayOfPreFixEntry_ReportsZero documents the one case that
// stays imprecise on purpose. Entries written before outcome_detail carried a
// count have nothing to recover, and 0 is the honest answer there — inventing
// a number for a rotation whose count was never recorded would be worse than
// admitting it is unknown.
func TestRotate_ReplayOfPreFixEntry_ReportsZero(t *testing.T) {
	s := &stubStore{
		rotationEntry: &domain.SecretAccessAuditLog{SecretPath: "kv/db", OutcomeDetail: "", RecordedAt: time.Now()},
	}
	r := newTestRouter(s, &stubVault{}, &stubPublisher{})

	body := `{"request_id":"rot-legacy-1","rotated_by_principal_id":"admin-1"}`
	req := authed(httptest.NewRequest(http.MethodPost, "/v1/secret-policies/"+validPolicyID+"/rotate", strings.NewReader(body)))
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	var got map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("failed to unmarshal: %v", err)
	}
	if got["revoked_lease_count"].(float64) != 0 {
		t.Errorf("expected 0 for an entry with no recorded count, got %v", got["revoked_lease_count"])
	}
}

// ── domain metrics ───────────────────────────────────────────────────────────

// recordingMetrics captures what the handler reports, so the assertions below
// are about the handler's own decision points rather than about Prometheus.
type recordingMetrics struct {
	brokerDecisions []string
	revocations     []string
	rotations       []int
	authzDecisions  [][2]string
	vaultErrors     []string
}

func (m *recordingMetrics) BrokerDecision(outcome string) {
	m.brokerDecisions = append(m.brokerDecisions, outcome)
}
func (m *recordingMetrics) LeaseRevoked(cause string) { m.revocations = append(m.revocations, cause) }
func (m *recordingMetrics) SecretRotated(revokedLeases int) {
	m.rotations = append(m.rotations, revokedLeases)
}
func (m *recordingMetrics) AuthzDecision(action, outcome string) {
	m.authzDecisions = append(m.authzDecisions, [2]string{action, outcome})
}
func (m *recordingMetrics) VaultBackendError(operation string) {
	m.vaultErrors = append(m.vaultErrors, operation)
}

// TestBrokerDecisions_AreCounted covers the observability gap that made this
// service unalertable: grant, denial and deny-by-absence all left the same
// trace in http_requests_total — some non-2xx responses on one route — and two
// of the three are security events. An operator could not ask "what fraction
// of secret access is being refused" at all.
func TestBrokerDecisions_AreCounted(t *testing.T) {
	cases := []struct {
		name  string
		store *stubStore
		want  string
	}{
		{
			name: "granted",
			store: &stubStore{
				applicableByPath: &domain.ApplicableSecretPolicyVersion{
					SecretPolicyVersion: domain.SecretPolicyVersion{
						SecretPolicyVersionID:   validVersionID,
						AllowedWorkloadIDs:      []byte(`["` + testPrincipal + `"]`),
						MaxLeaseDurationSeconds: 300,
					},
					SecretClass: "DATABASE_CREDENTIAL",
					SecretPath:  "kv/db",
				},
				lease: &domain.SecretLease{
					LeaseID: validLeaseID, SecretPath: "kv/db", ExpiresAt: time.Now().Add(time.Minute),
				},
				leaseCreated: true,
			},
			want: "granted",
		},
		{
			name: "denied",
			store: &stubStore{
				applicableByPath: &domain.ApplicableSecretPolicyVersion{
					SecretPolicyVersion: domain.SecretPolicyVersion{
						SecretPolicyVersionID:   validVersionID,
						AllowedWorkloadIDs:      []byte(`["someone-else"]`),
						MaxLeaseDurationSeconds: 300,
					},
					SecretClass: "DATABASE_CREDENTIAL",
					SecretPath:  "kv/db",
				},
			},
			want: "denied",
		},
		{
			name:  "no_policy",
			store: &stubStore{applicableByPathErr: domain.ErrSecretPolicyNotFound},
			want:  "no_policy",
		},
		{
			name:  "store error",
			store: &stubStore{applicableByPathErr: domain.ErrStoreUnavailable},
			want:  "error",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m := &recordingMetrics{}
			r := meteredRouter(tc.store, &stubVault{getToken: "local-lease:stub"}, &stubPublisher{}, m)

			body := `{"secret_path":"kv/db","requested_by_principal_id":"` + testPrincipal + `","request_id":"brk-1"}`
			req := authed(httptest.NewRequest(http.MethodPost, "/v1/secrets/broker", strings.NewReader(body)))
			w := httptest.NewRecorder()
			r.ServeHTTP(w, req)

			if len(m.brokerDecisions) != 1 {
				t.Fatalf("expected exactly one broker decision recorded, got %v (status %d: %s)",
					m.brokerDecisions, w.Code, w.Body.String())
			}
			if m.brokerDecisions[0] != tc.want {
				t.Errorf("recorded broker decision %q, want %q", m.brokerDecisions[0], tc.want)
			}
		})
	}
}

// TestAuthzUnavailable_IsCountedSeparatelyFromDenied is the distinction that
// matters most in this set. This service fails closed, so an authorization-svc
// outage refuses every mutation — and without separate labels that total
// write outage is indistinguishable from a wave of legitimate denials, which
// is exactly the wrong conclusion to reach at 3am.
func TestAuthzUnavailable_IsCountedSeparatelyFromDenied(t *testing.T) {
	m := &recordingMetrics{}
	r := meteredRouterWithAuthz(&stubStore{}, &stubAuthz{err: errAuthzUnavailable}, m)

	body := `{"secret_class":"DATABASE_CREDENTIAL","secret_path":"kv/db","created_by_principal_id":"p"}`
	req := authed(httptest.NewRequest(http.MethodPost, "/v1/secret-policies", strings.NewReader(body)))
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected 503 fail-closed, got %d: %s", w.Code, w.Body.String())
	}
	if len(m.authzDecisions) != 1 {
		t.Fatalf("expected one authz decision recorded, got %v", m.authzDecisions)
	}
	if got := m.authzDecisions[0]; got[1] != "unavailable" {
		t.Errorf("recorded authz outcome %q for an unreachable authorization-svc, want \"unavailable\"", got[1])
	}
}

// TestRotation_IsCountedWithItsRevokedLeases proves the rotation counters move
// together: a rotation that revokes leases must report both, so an alert can
// distinguish a routine rotation from one that cut off a large fleet.
func TestRotation_IsCountedWithItsRevokedLeases(t *testing.T) {
	s := &stubStore{
		findPolicyResult: &domain.SecretPolicy{
			SecretPolicyID: validPolicyID, SecretClass: "DATABASE_CREDENTIAL", SecretPath: "kv/db",
		},
		revokedByPath: []*domain.SecretLease{
			{LeaseID: validLeaseID, SecretPath: "kv/db"},
			{LeaseID: unknownButValidID, SecretPath: "kv/db"},
		},
	}
	m := &recordingMetrics{}
	r := meteredRouter(s, &stubVault{}, &stubPublisher{}, m)

	body := `{"request_id":"rot-metric-1","rotated_by_principal_id":"admin-1"}`
	req := authed(httptest.NewRequest(http.MethodPost, "/v1/secret-policies/"+validPolicyID+"/rotate", strings.NewReader(body)))
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	if len(m.rotations) != 1 || m.rotations[0] != 2 {
		t.Errorf("recorded rotations %v, want exactly one rotation revoking 2 leases", m.rotations)
	}
}

// ── shared scaffolding for the regression cases above ────────────────────────

// The ids below are the UUID-shaped stand-ins these tests address routes with.
// They have to be well-formed UUIDs now that the routes reject anything else
// before the store is reached — which is the point of the guard being tested.
const (
	validPolicyID     = "11111111-0000-4000-8000-000000000001"
	validVersionID    = "22222222-0000-4000-8000-000000000001"
	validLeaseID      = "33333333-0000-4000-8000-000000000001"
	unknownButValidID = "44444444-0000-4000-8000-00000000dead"
)

// errAuthzUnavailable is what a client returns when it could not obtain a
// decision at all, as distinct from obtaining a DENIED one.
var errAuthzUnavailable = authz.ErrUnavailable

// meteredRouter is newTestRouter with a metrics recorder attached.
func meteredRouter(s *stubStore, v *stubVault, p *stubPublisher, m handler.DomainMetrics) chi.Router {
	r := chi.NewRouter()
	r.Use(svcmiddleware.TenantContext())
	h := handler.New(s, v, p, testAuthz(), testAuthzScopeID, zap.NewNop()).UseMetrics(m)
	handler.RegisterRoutes(r, h)
	return r
}

// meteredRouterWithAuthz is meteredRouter with a caller-supplied authz client,
// for the fail-closed case.
func meteredRouterWithAuthz(s *stubStore, az *stubAuthz, m handler.DomainMetrics) chi.Router {
	r := chi.NewRouter()
	r.Use(svcmiddleware.TenantContext())
	h := handler.New(s, &stubVault{}, &stubPublisher{}, az, testAuthzScopeID, zap.NewNop()).UseMetrics(m)
	handler.RegisterRoutes(r, h)
	return r
}
