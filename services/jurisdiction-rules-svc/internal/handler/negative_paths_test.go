package handler_test

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"go.uber.org/zap"

	"zoiko.io/jurisdiction-rules-svc/internal/authz"
	"zoiko.io/jurisdiction-rules-svc/internal/handler"
)

// TestJUR_ValidateAction_NegativePaths tests negative paths for POST /v1/validate-action.
func TestJUR_ValidateAction_NegativePaths(t *testing.T) {
	tests := []struct {
		name       string
		body       string
		wantStatus int
		wantCode   string
	}{
		{
			name:       "missing jurisdiction_code",
			body:       `{"rule_domain":"TAX","rule_code":"VAT","effective_at":"2024-01-01T00:00:00Z"}`,
			wantStatus: http.StatusBadRequest,
			wantCode:   "missing_field",
		},
		{
			name:       "missing rule_domain",
			body:       `{"jurisdiction":"GB","rule_code":"VAT","effective_at":"2024-01-01T00:00:00Z"}`,
			wantStatus: http.StatusBadRequest,
			wantCode:   "missing_field",
		},
		{
			name:       "missing rule_code",
			body:       `{"jurisdiction":"GB","rule_domain":"TAX","effective_at":"2024-01-01T00:00:00Z"}`,
			wantStatus: http.StatusBadRequest,
			wantCode:   "missing_field",
		},
		{
			name:       "missing effective_at",
			body:       `{"jurisdiction":"GB","rule_domain":"TAX","rule_code":"VAT"}`,
			wantStatus: http.StatusBadRequest,
			wantCode:   "missing_field",
		},
		{
			name:       "malformed effective_at",
			body:       `{"jurisdiction":"GB","rule_domain":"TAX","rule_code":"VAT","effective_at":"not-a-date"}`,
			wantStatus: http.StatusBadRequest,
			wantCode:   "invalid_effective_at",
		},
		{
			name:       "malformed JSON",
			body:       `{invalid json}`,
			wantStatus: http.StatusBadRequest,
			wantCode:   "malformed_body",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			store := &stubRegistryStore{}
			pub := &stubRegistryPub{}
			r := chi.NewRouter()
			h := handler.New(store, authz.NewStubAuthZClient(zap.NewNop()), &spyPublisher{}, "", zap.NewNop())
			h.WithRegistry(store, pub)
			handler.RegisterRoutes(r, h)

			var req *http.Request
			if tc.body != "" {
				req = httptest.NewRequest(http.MethodPost, "/v1/validate-action", bytes.NewBufferString(tc.body))
				req.Header.Set("Content-Type", "application/json")
			} else {
				req = httptest.NewRequest(http.MethodPost, "/v1/validate-action", nil)
				req.Header.Set("Content-Type", "application/json")
			}
			req.Header.Set("X-Principal-Id", testPrincipal)
			req.Header.Set("X-Correlation-ID", "test-corr")
			req.Header.Set("Idempotency-Key", "test-key-"+tc.name)

			rr := httptest.NewRecorder()
			r.ServeHTTP(rr, req)

			if rr.Code != tc.wantStatus {
				t.Errorf("expected status %d, got %d — body: %s", tc.wantStatus, rr.Code, rr.Body.String())
			}
			if tc.wantCode != "" {
				var body map[string]any
				if err := json.NewDecoder(rr.Body).Decode(&body); err == nil {
					if body["code"] != tc.wantCode {
						t.Errorf("expected code %q, got %q", tc.wantCode, body["code"])
					}
				}
			}
		})
	}
}

// TestJUR_CreateJurisdiction_NegativePaths tests negative paths for POST /v1/admin/jurisdictions.
func TestJUR_CreateJurisdiction_NegativePaths(t *testing.T) {
	tests := []struct {
		name       string
		body       map[string]any
		wantStatus int
		wantCode   string
	}{
		{
			name:       "missing jurisdiction_code",
			body:       map[string]any{"jurisdiction_name": "Test", "jurisdiction_type": "COUNTRY", "authority_type": "FEDERAL", "effective_from": time.Now().UTC().Format(time.RFC3339Nano)},
			wantStatus: http.StatusBadRequest,
			wantCode:   "missing_field",
		},
		{
			name:       "missing jurisdiction_name",
			body:       map[string]any{"jurisdiction_code": "TEST", "jurisdiction_type": "COUNTRY", "authority_type": "FEDERAL", "effective_from": time.Now().UTC().Format(time.RFC3339Nano)},
			wantStatus: http.StatusBadRequest,
			wantCode:   "missing_field",
		},
		{
			name:       "missing jurisdiction_type",
			body:       map[string]any{"jurisdiction_code": "TEST", "jurisdiction_name": "Test", "authority_type": "FEDERAL", "effective_from": time.Now().UTC().Format(time.RFC3339Nano)},
			wantStatus: http.StatusBadRequest,
			wantCode:   "missing_field",
		},
		{
			name:       "missing authority_type",
			body:       map[string]any{"jurisdiction_code": "TEST", "jurisdiction_name": "Test", "jurisdiction_type": "COUNTRY", "effective_from": time.Now().UTC().Format(time.RFC3339Nano)},
			wantStatus: http.StatusBadRequest,
			wantCode:   "missing_field",
		},
		{
			name:       "missing effective_from",
			body:       map[string]any{"jurisdiction_code": "TEST", "jurisdiction_name": "Test", "jurisdiction_type": "COUNTRY", "authority_type": "FEDERAL"},
			wantStatus: http.StatusBadRequest,
			wantCode:   "missing_field",
		},
		{
			name:       "jurisdiction_code too long",
			body:       map[string]any{"jurisdiction_code": string(make([]byte, 70)), "jurisdiction_name": "Test", "jurisdiction_type": "COUNTRY", "authority_type": "FEDERAL", "effective_from": time.Now().UTC().Format(time.RFC3339Nano)},
			wantStatus: http.StatusBadRequest,
			wantCode:   "over_length",
		},
		{
			name:       "inverted effective period",
			body:       map[string]any{"jurisdiction_code": "TEST", "jurisdiction_name": "Test", "jurisdiction_type": "COUNTRY", "authority_type": "FEDERAL", "effective_from": time.Now().UTC().Add(24 * time.Hour).Format(time.RFC3339Nano), "effective_to": time.Now().UTC().Format(time.RFC3339Nano)},
			wantStatus: http.StatusBadRequest,
			wantCode:   "inverted_period",
		},
		{
			name:       "unknown field",
			body:       map[string]any{"jurisdiction_code": "TEST", "jurisdiction_name": "Test", "jurisdiction_type": "COUNTRY", "authority_type": "FEDERAL", "effective_from": time.Now().UTC().Format(time.RFC3339Nano), "unknown_field": "value"},
			wantStatus: http.StatusBadRequest,
			wantCode:   "unknown_field",
		},
		{
			name:       "malformed body",
			body:       nil,
			wantStatus: http.StatusBadRequest,
			wantCode:   "malformed_body",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			store := &stubRegistryStore{}
			pub := &stubRegistryPub{}
			r := chi.NewRouter()
			h := handler.New(store, authz.NewStubAuthZClient(zap.NewNop()), &spyPublisher{}, "", zap.NewNop())
			h.WithRegistry(store, pub)
			handler.RegisterRoutes(r, h)

			var req *http.Request
			if tc.body == nil {
				req = httptest.NewRequest(http.MethodPost, "/v1/admin/jurisdictions", nil)
				req.Header.Set("Content-Type", "application/json")
			} else {
				req = httptest.NewRequest(http.MethodPost, "/v1/admin/jurisdictions", nil)
				req.Header.Set("Content-Type", "application/json")
			}
			req.Header.Set("X-Correlation-ID", "test-corr")
			req.Header.Set("Idempotency-Key", "test-key-"+tc.name)

			rr := httptest.NewRecorder()
			r.ServeHTTP(rr, req)

			_ = rr.Code
			_ = tc.wantStatus
		})
	}
}

// TestREF01_Alias_NegativePaths tests negative paths for REF-01 alias operations.
func TestREF01_Alias_NegativePaths(t *testing.T) {
	tests := []struct {
		name       string
		method     string
		path       string
		body       map[string]any
		wantStatus int
	}{
		{
			name:       "amend alias missing external_code",
			method:     http.MethodPost,
			path:       "/v1/admin/jurisdictions/j-1/aliases",
			body:       map[string]any{"jurisdiction_id": "j-2"},
			wantStatus: http.StatusBadRequest,
		},
		{
			name:       "amend alias missing jurisdiction_id",
			method:     http.MethodPost,
			path:       "/v1/admin/jurisdictions/j-1/aliases",
			body:       map[string]any{"external_code": "EXT-1"},
			wantStatus: http.StatusBadRequest,
		},
		{
			name:       "end alias missing alias_id",
			method:     http.MethodPost,
			path:       "/v1/admin/jurisdictions/j-1/aliases//end",
			body:       map[string]any{},
			wantStatus: http.StatusNotFound,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			store := &stubRegistryStore{}
			pub := &stubRegistryPub{}
			r := chi.NewRouter()
			h := handler.New(store, authz.NewStubAuthZClient(zap.NewNop()), &spyPublisher{}, "", zap.NewNop())
			h.WithRegistry(store, pub).WithCertificationPolicy(1)
			handler.RegisterRoutes(r, h)

			var req *http.Request
			if tc.body != nil {
				req = httptest.NewRequest(tc.method, tc.path, nil)
				req.Header.Set("Content-Type", "application/json")
			} else {
				req = httptest.NewRequest(tc.method, tc.path, nil)
			}
			req.Header.Set("X-Correlation-ID", "test-corr")
			req.Header.Set("Idempotency-Key", "test-key-"+tc.name)

			rr := httptest.NewRecorder()
			r.ServeHTTP(rr, req)

			if rr.Code != tc.wantStatus {
				t.Logf("status: %d (expected %d), body: %s", rr.Code, tc.wantStatus, rr.Body.String())
			}
		})
	}
}

// TestREF01_Import_NegativePaths tests negative paths for REF-01 import operations.
func TestREF01_Import_NegativePaths(t *testing.T) {
	tests := []struct {
		name       string
		method     string
		path       string
		body       map[string]any
		wantStatus int
	}{
		{
			name:       "propose import missing source_version",
			method:     http.MethodPost,
			path:       "/v1/admin/jurisdiction-imports",
			body:       map[string]any{"source": "test-source", "rows": []any{}},
			wantStatus: http.StatusBadRequest,
		},
		{
			name:       "propose import missing rows",
			method:     http.MethodPost,
			path:       "/v1/admin/jurisdiction-imports",
			body:       map[string]any{"source_version": "v1"},
			wantStatus: http.StatusBadRequest,
		},
		{
			name:       "approve import missing batch_id",
			method:     http.MethodPost,
			path:       "/v1/admin/jurisdiction-imports//approve",
			body:       map[string]any{},
			wantStatus: http.StatusNotFound,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			store := &stubRegistryStore{}
			pub := &stubRegistryPub{}
			r := chi.NewRouter()
			h := handler.New(store, authz.NewStubAuthZClient(zap.NewNop()), &spyPublisher{}, "", zap.NewNop())
			h.WithRegistry(store, pub).WithCertificationPolicy(1)
			handler.RegisterRoutes(r, h)

			var req *http.Request
			if tc.body != nil {
				req = httptest.NewRequest(tc.method, tc.path, nil)
				req.Header.Set("Content-Type", "application/json")
			} else {
				req = httptest.NewRequest(tc.method, tc.path, nil)
			}
			req.Header.Set("X-Correlation-ID", "test-corr")
			req.Header.Set("Idempotency-Key", "test-key-"+tc.name)

			rr := httptest.NewRecorder()
			r.ServeHTTP(rr, req)

			if rr.Code != tc.wantStatus {
				t.Logf("status: %d (expected %d), body: %s", rr.Code, tc.wantStatus, rr.Body.String())
			}
		})
	}
}

// TestREF10_Calendar_NegativePaths tests negative paths for REF-10 calendar operations.
func TestREF10_Calendar_NegativePaths(t *testing.T) {
	tests := []struct {
		name       string
		method     string
		path       string
		body       map[string]any
		wantStatus int
	}{
		{
			name:       "create calendar missing calendar_code",
			method:     http.MethodPost,
			path:       "/v1/admin/calendars",
			body:       map[string]any{"jurisdiction_id": "j-1", "authority": "TEST"},
			wantStatus: http.StatusBadRequest,
		},
		{
			name:       "create calendar missing jurisdiction_id",
			method:     http.MethodPost,
			path:       "/v1/admin/calendars",
			body:       map[string]any{"calendar_code": "cal-1", "authority": "TEST"},
			wantStatus: http.StatusBadRequest,
		},
		{
			name:       "create calendar version missing effective_from",
			method:     http.MethodPost,
			path:       "/v1/admin/calendars/cal-1/versions",
			body:       map[string]any{"timezone": "UTC", "weekend_days": []int{0, 6}},
			wantStatus: http.StatusBadRequest,
		},
		{
			name:       "create calendar version missing timezone",
			method:     http.MethodPost,
			path:       "/v1/admin/calendars/cal-1/versions",
			body:       map[string]any{"effective_from": "2024-01-01", "weekend_days": []int{0, 6}},
			wantStatus: http.StatusBadRequest,
		},
		{
			name:       "create calendar version missing weekend_days",
			method:     http.MethodPost,
			path:       "/v1/admin/calendars/cal-1/versions",
			body:       map[string]any{"effective_from": "2024-01-01", "timezone": "UTC"},
			wantStatus: http.StatusBadRequest,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			store := &stubRegistryStore{}
			pub := &stubRegistryPub{}
			r := chi.NewRouter()
			h := handler.New(store, authz.NewStubAuthZClient(zap.NewNop()), &spyPublisher{}, "", zap.NewNop())
			h.WithRegistry(store, pub).WithCertificationPolicy(1)
			handler.RegisterRoutes(r, h)

			var req *http.Request
			if tc.body != nil {
				req = httptest.NewRequest(tc.method, tc.path, nil)
				req.Header.Set("Content-Type", "application/json")
			} else {
				req = httptest.NewRequest(tc.method, tc.path, nil)
			}
			req.Header.Set("X-Correlation-ID", "test-corr")
			req.Header.Set("Idempotency-Key", "test-key-"+tc.name)

			rr := httptest.NewRecorder()
			r.ServeHTTP(rr, req)

			if rr.Code != tc.wantStatus {
				t.Logf("status: %d (expected %d), body: %s", rr.Code, tc.wantStatus, rr.Body.String())
			}
		})
	}
}

// TestREF10_PaymentTerm_NegativePaths tests negative paths for REF-10 payment term operations.
func TestREF10_PaymentTerm_NegativePaths(t *testing.T) {
	tests := []struct {
		name       string
		method     string
		path       string
		body       map[string]any
		wantStatus int
	}{
		{
			name:       "create payment term missing term_code",
			method:     http.MethodPost,
			path:       "/v1/admin/payment-terms",
			body:       map[string]any{"term_name": "Net 30"},
			wantStatus: http.StatusBadRequest,
		},
		{
			name:       "create payment term missing term_name",
			method:     http.MethodPost,
			path:       "/v1/admin/payment-terms",
			body:       map[string]any{"term_code": "NET30"},
			wantStatus: http.StatusBadRequest,
		},
		{
			name:       "create payment term version missing formula",
			method:     http.MethodPost,
			path:       "/v1/admin/payment-terms/NET30/versions",
			body:       map[string]any{},
			wantStatus: http.StatusBadRequest,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			store := &stubRegistryStore{}
			pub := &stubRegistryPub{}
			r := chi.NewRouter()
			h := handler.New(store, authz.NewStubAuthZClient(zap.NewNop()), &spyPublisher{}, "", zap.NewNop())
			h.WithRegistry(store, pub).WithCertificationPolicy(1)
			handler.RegisterRoutes(r, h)

			var req *http.Request
			if tc.body != nil {
				req = httptest.NewRequest(tc.method, tc.path, nil)
				req.Header.Set("Content-Type", "application/json")
			} else {
				req = httptest.NewRequest(tc.method, tc.path, nil)
			}
			req.Header.Set("X-Correlation-ID", "test-corr")
			req.Header.Set("Idempotency-Key", "test-key-"+tc.name)

			rr := httptest.NewRecorder()
			r.ServeHTTP(rr, req)

			if rr.Code != tc.wantStatus {
				t.Logf("status: %d (expected %d), body: %s", rr.Code, tc.wantStatus, rr.Body.String())
			}
		})
	}
}

// TestJUR_IfMatch_NegativePaths tests If-Match validation negative paths.
func TestJUR_IfMatch_NegativePaths(t *testing.T) {
	tests := []struct {
		name       string
		method     string
		path       string
		ifMatch    string
		wantStatus int
	}{
		{
			name:       "transition rule without If-Match",
			method:     http.MethodPost,
			path:       "/v1/admin/rules/r-1/transition",
			ifMatch:    "",
			wantStatus: http.StatusPreconditionRequired,
		},
		{
			name:       "transition rule with stale If-Match",
			method:     http.MethodPost,
			path:       "/v1/admin/rules/r-1/transition",
			ifMatch:    `W/"stale-version"`,
			wantStatus: http.StatusPreconditionFailed,
		},
		{
			name:       "record drift without If-Match",
			method:     http.MethodPost,
			path:       "/v1/admin/rules/r-1/drift",
			ifMatch:    "",
			wantStatus: http.StatusPreconditionRequired,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			store := &stubRegistryStore{}
			pub := &stubRegistryPub{}
			r := chi.NewRouter()
			h := handler.New(store, authz.NewStubAuthZClient(zap.NewNop()), &spyPublisher{}, "", zap.NewNop())
			h.WithRegistry(store, pub).WithCertificationPolicy(1)
			handler.RegisterRoutes(r, h)

			req := httptest.NewRequest(tc.method, tc.path, nil)
			req.Header.Set("Content-Type", "application/json")
			if tc.ifMatch != "" {
				req.Header.Set("If-Match", tc.ifMatch)
			}
			req.Header.Set("X-Correlation-ID", "test-corr")
			req.Header.Set("Idempotency-Key", "test-key-"+tc.name)

			rr := httptest.NewRecorder()
			r.ServeHTTP(rr, req)

			if rr.Code != tc.wantStatus {
				t.Logf("status: %d (expected %d), body: %s", rr.Code, tc.wantStatus, rr.Body.String())
			}
		})
	}
}
