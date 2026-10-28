package handler_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"zoiko.io/configuration-feature-flag-svc/internal/domain"
)

// The direct write routes (POST /v1/config, POST /v1/flags) reach the same
// AA-001 store gates the governed routes do: the published-definition lookup
// refuses an unregistered key on either path, and the value/scope gates refuse
// exactly the same values. Those refusals carry wire codes the contract pins
// to a 4xx, and the direct routes used to answer 503 store_unavailable for all
// of them — telling a caller "the database is down, retry" about a request
// that could never succeed until the caller fixed it.
//
// These tests pin the per-code status the governed routes already answer
// (handler.go governedCodeStatus) so the two paths cannot drift apart again.

// errorCode unmarshals the {"error": "<code>"} body shape every refusal uses.
func errorCode(t *testing.T, body []byte) string {
	t.Helper()
	var got map[string]any
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatalf("refusal body is not JSON: %v: %s", err, body)
	}
	if code, ok := got["code"].(string); ok {
		return code
	}
	return ""
}

func TestUpsertConfigEntry_CodedRefusalsAnswerTheirDocumented4xx(t *testing.T) {
	cases := []struct {
		name       string
		storeErr   error
		wantStatus int
		wantCode   string
	}{
		{"undeclared key", domain.ErrKeyNotRegistered, http.StatusBadRequest, "key_not_registered"},
		{"value type mismatch", domain.ErrTypeMismatch, http.StatusBadRequest, "type_mismatch"},
		{"value constraint", domain.ErrValueConstraintFailed, http.StatusBadRequest, "value_constraint_failed"},
		{"scope not allowed", domain.ErrScopeNotAllowed, http.StatusForbidden, "scope_not_allowed"},
		{"material key needs change", domain.ErrMaterialKeyRequiresChange, http.StatusConflict, "material_key_requires_change"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			store := &stubStore{configEntryErr: tc.storeErr}
			r := newTestRouter(store)

			body := `{"key":"k","value":100,"environment":"staging","created_by_principal_id":"admin-1"}`
			req := authed(httptest.NewRequest(http.MethodPost, "/v1/config", strings.NewReader(body)))
			w := httptest.NewRecorder()
			r.ServeHTTP(w, req)

			if w.Code != tc.wantStatus {
				t.Fatalf("expected %d %s, got %d: %s", tc.wantStatus, tc.wantCode, w.Code, w.Body.String())
			}
			if got := errorCode(t, w.Body.Bytes()); got != tc.wantCode {
				t.Fatalf("expected error %q, got %q", tc.wantCode, got)
			}
		})
	}
}

func TestUpsertFeatureFlag_CodedRefusalsAnswerTheirDocumented4xx(t *testing.T) {
	cases := []struct {
		name       string
		storeErr   error
		wantStatus int
		wantCode   string
	}{
		{"undeclared key", domain.ErrKeyNotRegistered, http.StatusBadRequest, "key_not_registered"},
		{"retired key", domain.ErrFlagKeyRetired, http.StatusConflict, "flag_key_retired"},
		{"targeting not permitted", domain.ErrTargetingNotPermitted, http.StatusForbidden, "targeting_not_permitted"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			store := &stubStore{flagErr: tc.storeErr}
			r := newTestRouter(store)

			body := `{"key":"k","enabled":true,"environment":"staging","created_by_principal_id":"admin-1"}`
			req := authed(httptest.NewRequest(http.MethodPost, "/v1/flags", strings.NewReader(body)))
			w := httptest.NewRecorder()
			r.ServeHTTP(w, req)

			if w.Code != tc.wantStatus {
				t.Fatalf("expected %d %s, got %d: %s", tc.wantStatus, tc.wantCode, w.Code, w.Body.String())
			}
			if got := errorCode(t, w.Body.Bytes()); got != tc.wantCode {
				t.Fatalf("expected error %q, got %q", tc.wantCode, got)
			}
		})
	}
}

// A refusal whose code the contract does not pin is still an outage: only
// codes the governed routes map may change the direct routes' behaviour.
func TestUpsertConfigEntry_UnpinnedStoreErrorStays503(t *testing.T) {
	store := &stubStore{configEntryErr: domain.ErrStoreUnavailable}
	r := newTestRouter(store)

	body := `{"key":"k","value":100,"environment":"staging","created_by_principal_id":"admin-1"}`
	req := authed(httptest.NewRequest(http.MethodPost, "/v1/config", strings.NewReader(body)))
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected 503, got %d: %s", w.Code, w.Body.String())
	}
	if got := errorCode(t, w.Body.Bytes()); got != "store_unavailable" {
		t.Fatalf("expected store_unavailable, got %q", got)
	}
}
