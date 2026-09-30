package handler_test

import (
	"bytes"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"go.uber.org/zap"

	"zoiko.io/secret-vault-integration-svc/internal/domain"
	"zoiko.io/secret-vault-integration-svc/internal/handler"
	svcmiddleware "zoiko.io/secret-vault-integration-svc/internal/middleware"
)

func routerWithSharedRule(s *stubStore, v *stubVault, on bool) chi.Router {
	r := chi.NewRouter()
	r.Use(svcmiddleware.TenantContext())
	h := handler.New(s, v, &stubPublisher{}, testAuthz(), testAuthzScopeID, 0, zap.NewNop()).RequireSharedSecretException(on)
	handler.RegisterRoutes(r, h)
	return r
}

const erPolicy = "11111111-0000-4000-8000-000000000002"

func erStore(exceptions ...*domain.SharedSecretException) *stubStore {
	return &stubStore{
		findPolicyResult:     &domain.SecretPolicy{SecretPolicyID: erPolicy, SecretPath: "kv/db", SecretClass: "DATABASE_CREDENTIAL"},
		listExceptionsResult: exceptions,
	}
}

func postER(r http.Handler, body string) *httptest.ResponseRecorder {
	req := authed(httptest.NewRequest(http.MethodPost, "/v1/secret-policies/"+erPolicy+"/emergency-retrieval", bytes.NewBufferString(body)))
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w
}

func activeEx(path, approver string) *domain.SharedSecretException {
	return &domain.SharedSecretException{ExceptionID: "44444444-0000-4000-8000-000000000001", SecretPath: path, Status: "ACTIVE", ApprovedByPrincipalID: approver, ExpiresAt: time.Now().Add(time.Hour)}
}

// The store once returned every global exception whatever its status or path;
// the handler must not accept a revoked one, or one for another secret.
func TestEmergencyRetrieval_RevokedOrOtherPathExceptionDoesNotUnlock(t *testing.T) {
	revoked := activeEx("kv/db", "approver-2")
	revoked.Status = "REVOKED"
	s := erStore(revoked, activeEx("kv/other", "approver-2"))
	v := &stubVault{getMaterial: []byte("top-secret")}
	w := postER(newTestRouter(s, v, &stubPublisher{}), `{"request_id":"er-1","reason":"incident"}`)
	if w.Code != http.StatusForbidden || v.getMaterialCalls != 0 {
		t.Fatalf("revoked/other-path exceptions must not unlock break-glass: %d %s (vault calls %d)", w.Code, w.Body.String(), v.getMaterialCalls)
	}
}

func TestEmergencyRetrieval_ApproverCannotRetrieve(t *testing.T) {
	s := erStore(activeEx("kv/db", testPrincipal))
	v := &stubVault{getMaterial: []byte("top-secret")}
	w := postER(newTestRouter(s, v, &stubPublisher{}), `{"request_id":"er-1","reason":"incident"}`)
	if w.Code != http.StatusForbidden || !strings.Contains(w.Body.String(), "exception_self_approval") || v.getMaterialCalls != 0 {
		t.Fatalf("self-approved retrieval must be 403 exception_self_approval: %d %s", w.Code, w.Body.String())
	}
}

func TestEmergencyRetrieval_ReasonRequired(t *testing.T) {
	s := erStore(activeEx("kv/db", "approver-2"))
	v := &stubVault{getMaterial: []byte("top-secret")}
	w := postER(newTestRouter(s, v, &stubPublisher{}), `{"request_id":"er-1"}`)
	if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), `"reason"`) {
		t.Fatalf("missing reason must be 400: %d %s", w.Code, w.Body.String())
	}
}

// No evidence, no material: the audit write used to follow the release and
// its failure was only logged.
func TestEmergencyRetrieval_EvidenceFailureWithholdsMaterial(t *testing.T) {
	s := erStore(activeEx("kv/db", "approver-2"))
	s.auditErr = errors.New("db down")
	v := &stubVault{getMaterial: []byte("top-secret")}
	w := postER(newTestRouter(s, v, &stubPublisher{}), `{"request_id":"er-1","reason":"incident"}`)
	if w.Code != http.StatusServiceUnavailable || strings.Contains(w.Body.String(), "material_base64") || v.getMaterialCalls != 0 {
		t.Fatalf("an unrecorded retrieval must release nothing: %d %s (vault calls %d)", w.Code, w.Body.String(), v.getMaterialCalls)
	}
}

func sharedGrantingStore() *stubStore {
	s := grantingStore()
	s.applicableByPath.AllowedWorkloadIDs = json.RawMessage(`["` + testWorkload + `","svc-b"]`)
	return s
}

func brokerAs(r http.Handler) *httptest.ResponseRecorder {
	req := asWorkload(authed(httptest.NewRequest(http.MethodPost, "/v1/secrets/broker", strings.NewReader(brokerBody("kv/db", testWorkload, "req-1")))), testWorkload)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w
}

func TestBroker_SharedSecretWithoutException_Refused(t *testing.T) {
	s := sharedGrantingStore()
	v := &stubVault{getToken: "ltk:v3:x"}
	w := brokerAs(routerWithSharedRule(s, v, true))
	if w.Code != http.StatusForbidden || !strings.Contains(w.Body.String(), "shared_secret_exception_required") {
		t.Fatalf("a shared secret with no exception must be 403: %d %s", w.Code, w.Body.String())
	}
	if v.getCalls != 0 {
		t.Fatal("no lease token may be minted for a refused shared secret")
	}
	var denied bool
	for _, e := range s.auditEntries {
		if e.EventType == "DENIED" && strings.Contains(e.OutcomeDetail, "shared-secret exception") {
			denied = true
		}
	}
	if !denied {
		t.Fatalf("the refusal must be recorded as DENIED evidence, got %+v", s.auditEntries)
	}
}

func TestBroker_SharedSecretWithException_Granted(t *testing.T) {
	s := sharedGrantingStore()
	s.listExceptionsResult = []*domain.SharedSecretException{activeEx("kv/db", "approver-2")}
	w := brokerAs(routerWithSharedRule(s, &stubVault{getToken: "ltk:v3:x"}, true))
	if w.Code != http.StatusOK {
		t.Fatalf("a documented shared secret must be granted: %d %s", w.Code, w.Body.String())
	}
}

func TestBroker_SingleWorkloadSecret_NoExceptionNeeded(t *testing.T) {
	w := brokerAs(routerWithSharedRule(grantingStore(), &stubVault{getToken: "ltk:v3:x"}, true))
	if w.Code != http.StatusOK {
		t.Fatalf("an unshared secret needs no exception: %d %s", w.Code, w.Body.String())
	}
}

// A version stored before the ceiling was lowered must not issue beyond it.
func TestBroker_LeaseClampedToCurrentCeiling(t *testing.T) {
	s := grantingStore()
	s.applicableByPath.MaxLeaseDurationSeconds = 7 * 24 * 3600 // stored a week
	v := &stubVault{getToken: "ltk:v3:x"}
	r := chi.NewRouter()
	r.Use(svcmiddleware.TenantContext())
	handler.RegisterRoutes(r, handler.New(s, v, &stubPublisher{}, testAuthz(), testAuthzScopeID, 300, zap.NewNop()))
	before := time.Now()
	if w := brokerAs(r); w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	if len(v.getExpiries) == 0 || v.getExpiries[0].After(before.Add(301*time.Second)) {
		t.Fatalf("lease must be clamped to the 300s ceiling, token expiry %v", v.getExpiries)
	}
}
