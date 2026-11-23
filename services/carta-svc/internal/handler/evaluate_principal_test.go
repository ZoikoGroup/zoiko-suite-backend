package handler_test

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"go.uber.org/zap"

	"zoiko.io/carta-svc/internal/authz"
	"zoiko.io/carta-svc/internal/handler"
	"zoiko.io/carta-svc/internal/store"
)

// recordingAuthz records every authorization question and answers with err.
type recordingAuthz struct {
	err   error
	calls [][3]string
}

func (a *recordingAuthz) CheckAllowed(_ context.Context, principal, le, action string) error {
	a.calls = append(a.calls, [3]string{principal, le, action})
	return a.err
}

func evaluateWithPrincipal(r http.Handler, principal string) *httptest.ResponseRecorder {
	body, _ := json.Marshal(map[string]interface{}{
		"legal_entity_id": "le-1",
		"context": map[string]interface{}{
			"subject_id": "subject-1", "subject_type": "USER",
			"device_trust_level": 90, "is_known_location": true,
			"resource_sensitivity": "LOW", "action_requested": "READ",
			"time_of_day_hour": 12,
		},
	})
	req := httptest.NewRequest(http.MethodPost, "/v1/carta/evaluate", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Tenant-ID", "t1")
	if principal != "" {
		req.Header.Set("X-Principal-Id", principal)
	}
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w
}

// A caller that presents a principal is authorized CARTA_EVALUATE on the
// body's legal entity and recorded as evaluated_by.
func TestEvaluate_PresentedPrincipalIsAuthorizedAndRecorded(t *testing.T) {
	az := &recordingAuthz{}
	r := handler.NewRouter(handler.New(store.NewMemoryStore(), az, zap.NewNop()))
	w := evaluateWithPrincipal(r, "alice")
	if w.Code != http.StatusCreated {
		t.Fatalf("evaluate = %d: %s", w.Code, w.Body.String())
	}
	if len(az.calls) != 1 || az.calls[0] != [3]string{"alice", "le-1", handler.CartaEvaluate} {
		t.Fatalf("authorization calls = %v, want one (alice, le-1, %s)", az.calls, handler.CartaEvaluate)
	}
	var got struct {
		EvaluatedBy string `json:"evaluated_by"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &got)
	if got.EvaluatedBy != "alice" {
		t.Fatalf("evaluated_by = %q, want alice", got.EvaluatedBy)
	}
}

func TestEvaluate_PresentedPrincipalDenied403(t *testing.T) {
	r := handler.NewRouter(handler.New(store.NewMemoryStore(), &recordingAuthz{err: authz.ErrAuthorizationDenied}, zap.NewNop()))
	if w := evaluateWithPrincipal(r, "mallory"); w.Code != http.StatusForbidden {
		t.Fatalf("denied principal: got %d, want 403: %s", w.Code, w.Body.String())
	}
}

func TestEvaluate_PresentedPrincipalAuthzUnavailable503(t *testing.T) {
	r := handler.NewRouter(handler.New(store.NewMemoryStore(), &recordingAuthz{err: authz.ErrAuthzServiceUnavailable}, zap.NewNop()))
	if w := evaluateWithPrincipal(r, "alice"); w.Code != http.StatusServiceUnavailable {
		t.Fatalf("authz unreachable: got %d, want 503: %s", w.Code, w.Body.String())
	}
}

// The principal-less gateway path is unchanged: no authorization call, no
// evaluated_by in the answer (it is not guessed).
func TestEvaluate_NoPrincipal_NoAuthzCallAndNoActor(t *testing.T) {
	az := &recordingAuthz{err: authz.ErrAuthorizationDenied}
	r := handler.NewRouter(handler.New(store.NewMemoryStore(), az, zap.NewNop()))
	w := evaluateWithPrincipal(r, "")
	if w.Code != http.StatusCreated {
		t.Fatalf("gateway path: got %d, want 201: %s", w.Code, w.Body.String())
	}
	if len(az.calls) != 0 {
		t.Fatalf("no principal must not consult authorization-svc, calls = %v", az.calls)
	}
	if bytes.Contains(w.Body.Bytes(), []byte("evaluated_by")) {
		t.Fatalf("evaluated_by must be absent when no principal was presented: %s", w.Body.String())
	}
}
