package handler_test

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/go-chi/chi/v5"
	"go.uber.org/zap"

	"zoiko.io/authorization-svc/internal/handler"
	"zoiko.io/authorization-svc/internal/siem"
)

// A tenantless /v1/authorize about an ordinary entity is evaluated against every
// tenant's roles. Enforce mode refuses it; but a question about the
// platform-scope entity is tenantless by design and must still be answered, or
// enforcement could never be switched on.

func enforcingRouter(s *stubStore) chi.Router {
	r := chi.NewRouter()
	handler.RegisterRoutes(r, handler.New(s, &stubPublisher{}, &stubValidator{},
		siem.New("", "authorization-svc", zap.NewNop()), platformID, true, zap.NewNop()))
	return r
}

func tenantlessAuthorize(r chi.Router, entity string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, "/v1/authorize",
		bytes.NewBufferString(`{"principal_id":"p-1","legal_entity_id":"`+entity+`","action_type":"GOVERNANCE_DECISION_RECORD"}`))
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w
}

func TestTenantlessAuthorize_EnforcedForOrdinaryEntity(t *testing.T) {
	s := &stubStore{rbacActions: []string{"GOVERNANCE_DECISION_RECORD"}}
	w := tenantlessAuthorize(enforcingRouter(s), "22222222-2222-4222-8222-222222222222")
	if w.Code != http.StatusBadRequest {
		t.Fatalf("tenantless question about an ordinary entity under enforce: want 400, got %d: %s", w.Code, w.Body.String())
	}
	if s.recordedParams.ActionType != "" {
		t.Error("a decision was evaluated and recorded despite the refusal")
	}
}

func TestTenantlessAuthorize_PlatformScopeStillAnswered(t *testing.T) {
	for _, entity := range []string{"PLATFORM", platformID, "00000000-0000-0000-0000-00000000F001"} {
		s := &stubStore{rbacActions: []string{"GOVERNANCE_DECISION_RECORD"}, rbacBasis: "rbac:role=PLATFORM_OPERATOR"}
		w := tenantlessAuthorize(enforcingRouter(s), entity)
		if w.Code != http.StatusOK {
			t.Errorf("tenantless platform-scope question %q under enforce: want 200, got %d: %s", entity, w.Code, w.Body.String())
			continue
		}
		if s.grantedTenantArg != "" {
			t.Errorf("%q: evaluated with tenant %q, want none (platform grants are held by roles of any tenant)", entity, s.grantedTenantArg)
		}
	}
}

// Default (non-enforcing) behaviour is unchanged: tenantless is admitted.
func TestTenantlessAuthorize_DefaultStillAdmits(t *testing.T) {
	s := &stubStore{rbacActions: []string{"GOVERNANCE_DECISION_RECORD"}}
	if w := tenantlessAuthorize(newTestRouter(s), "22222222-2222-4222-8222-222222222222"); w.Code != http.StatusOK {
		t.Fatalf("default mode: want 200, got %d", w.Code)
	}
}
