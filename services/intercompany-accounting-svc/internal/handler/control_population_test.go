package handler_test

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
	"go.uber.org/zap"

	"zoiko.io/intercompany-accounting-svc/internal/domain"
	"zoiko.io/intercompany-accounting-svc/internal/handler"
	"zoiko.io/intercompany-accounting-svc/internal/middleware"
)

func (s *stubStoreReal) ControlPopulation(_ context.Context, q domain.ControlPopulationQuery) (*domain.ControlPopulationPage, error) {
	s.cpCalls++
	s.cpQuery = q
	if s.cpErr != nil {
		return nil, s.cpErr
	}
	if s.cpPage != nil {
		p := *s.cpPage
		return &p, nil
	}
	return &domain.ControlPopulationPage{Records: []domain.ControlRecord{}, Watermark: "0:",
		DeclaredTotals: domain.ControlDeclaredTotals{Totals: map[string]string{}}}, nil
}

const cpBase = "/v1/control-populations/entry-legs"

// cpRouter builds a router whose tenant is optional (tenant == "" = none).
func cpRouter(s *stubStoreReal, authz *stubAuthZ, tenant string) chi.Router {
	r := chi.NewRouter()
	r.Use(func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			if tenant != "" {
				req = req.WithContext(middleware.WithTenant(req.Context(), tenant))
			}
			next.ServeHTTP(w, req)
		})
	})
	handler.RegisterRoutes(r, handler.New(s, &stubPublisher{}, authz, &stubLedger{}, zap.NewNop()))
	return r
}

func cpGet(r chi.Router, path, principal string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodGet, path, nil)
	if principal != "" {
		req.Header.Set("X-Principal-Id", principal)
	}
	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, req)
	return rr
}

func TestControlPopulation_Unauthenticated(t *testing.T) {
	s := newStubStoreReal()
	if rr := cpGet(cpRouter(s, &stubAuthZ{}, ""), cpBase+"?legal_entity_id=le-1&leg=source", "p1"); rr.Code != http.StatusUnauthorized {
		t.Fatalf("missing tenant: expected 401 got %d", rr.Code)
	}
	if rr := cpGet(cpRouter(s, &stubAuthZ{}, "tenant-abc"), cpBase+"?legal_entity_id=le-1&leg=source", ""); rr.Code != http.StatusUnauthorized {
		t.Fatalf("missing principal: expected 401 got %d", rr.Code)
	}
	if s.cpCalls != 0 {
		t.Fatal("store must not be called")
	}
}

func TestControlPopulation_BadRequests(t *testing.T) {
	cases := map[string]string{
		"missing legal_entity_id": "?leg=source",
		"empty legal_entity_id":   "?legal_entity_id=&leg=source",
		"bad legal_entity_id":     "?legal_entity_id=le%201;drop&leg=source",
		"leading dash entity":     "?legal_entity_id=-le&leg=source",
		"too long entity":         "?legal_entity_id=" + strings.Repeat("a", 65) + "&leg=source",
		"missing leg":             "?legal_entity_id=le-1",
		"bad leg":                 "?legal_entity_id=le-1&leg=both",
		"upper-case leg":          "?legal_entity_id=le-1&leg=SOURCE",
		"period_id rejected":      "?legal_entity_id=le-1&leg=source&period_id=2026-09",
		"unknown param":           "?legal_entity_id=le-1&leg=source&status=MATCHED",
		"repeated leg":            "?legal_entity_id=le-1&leg=source&leg=target",
		"repeated entity":         "?legal_entity_id=le-1&legal_entity_id=le-2&leg=source",
		"limit zero":              "?legal_entity_id=le-1&leg=source&limit=0",
		"limit negative":          "?legal_entity_id=le-1&leg=source&limit=-5",
		"limit nan":               "?legal_entity_id=le-1&leg=source&limit=abc",
		"cursor not base64":       "?legal_entity_id=le-1&leg=source&cursor=%21%21%21",
		"cursor empty payload":    "?legal_entity_id=le-1&leg=source&cursor=",
	}
	for name, qs := range cases {
		t.Run(name, func(t *testing.T) {
			s := newStubStoreReal()
			rr := cpGet(cpRouter(s, &stubAuthZ{}, "tenant-abc"), cpBase+qs, "p1")
			if rr.Code != http.StatusBadRequest {
				t.Fatalf("expected 400 got %d: %s", rr.Code, rr.Body.String())
			}
			if s.cpCalls != 0 {
				t.Fatal("store must not be called")
			}
		})
	}
}

func TestControlPopulation_UnknownPopulation404(t *testing.T) {
	s := newStubStoreReal()
	rr := cpGet(cpRouter(s, &stubAuthZ{}, "tenant-abc"), "/v1/control-populations/nope?legal_entity_id=le-1&leg=source", "p1")
	if rr.Code != http.StatusNotFound {
		t.Fatalf("expected 404 got %d", rr.Code)
	}
	if s.cpCalls != 0 {
		t.Fatal("store must not be called")
	}
}

func TestControlPopulation_AuthzFailClosed(t *testing.T) {
	for name, tc := range map[string]struct {
		err  error
		code int
	}{
		"denied":      {domain.ErrAuthorizationDenied, http.StatusForbidden},
		"unavailable": {context.DeadlineExceeded, http.StatusServiceUnavailable},
	} {
		t.Run(name, func(t *testing.T) {
			s := newStubStoreReal()
			rr := cpGet(cpRouter(s, &stubAuthZ{err: tc.err}, "tenant-abc"), cpBase+"?legal_entity_id=le-1&leg=target", "p1")
			if rr.Code != tc.code {
				t.Fatalf("expected %d got %d", tc.code, rr.Code)
			}
			if s.cpCalls != 0 {
				t.Fatal("store must not be called when authz fails")
			}
		})
	}
}

func TestControlPopulation_TooLarge422(t *testing.T) {
	s := newStubStoreReal()
	s.cpErr = domain.ErrPopulationTooLarge
	rr := cpGet(cpRouter(s, &stubAuthZ{}, "tenant-abc"), cpBase+"?legal_entity_id=le-1&leg=source", "p1")
	if rr.Code != http.StatusUnprocessableEntity {
		t.Fatalf("expected 422 got %d", rr.Code)
	}
}

func TestControlPopulation_StoreErrorIs503(t *testing.T) {
	s := newStubStoreReal()
	s.cpErr = context.Canceled
	rr := cpGet(cpRouter(s, &stubAuthZ{}, "tenant-abc"), cpBase+"?legal_entity_id=le-1&leg=source", "p1")
	if rr.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected 503 got %d", rr.Code)
	}
}

func TestControlPopulation_HappyPathShape(t *testing.T) {
	s := newStubStoreReal()
	s.cpPage = &domain.ControlPopulationPage{
		Records: []domain.ControlRecord{{
			RecordID: "id-1", Reference: "ref-1", Amount: "1234.5678", Currency: "USD", Date: "2026-09-01",
			Attributes: map[string]string{"leg": "source"},
		}},
		NextCursor:     "id-1",
		Watermark:      "1:abc",
		DeclaredTotals: domain.ControlDeclaredTotals{RowCount: 1, Totals: map[string]string{"USD": "1234.5678"}},
	}
	after := base64.RawURLEncoding.EncodeToString([]byte("id-0"))
	rr := cpGet(cpRouter(s, &stubAuthZ{}, "tenant-abc"), cpBase+"?legal_entity_id=le-1&leg=source&limit=9999&cursor="+after, "p1")
	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200 got %d: %s", rr.Code, rr.Body.String())
	}
	if s.cpQuery.TenantID != "tenant-abc" || s.cpQuery.LegalEntityID != "le-1" || s.cpQuery.Leg != "source" ||
		s.cpQuery.AfterRecordID != "id-0" || s.cpQuery.Limit != 5000 {
		t.Fatalf("unexpected query passed to store: %+v", s.cpQuery)
	}

	var raw map[string]any
	if err := json.Unmarshal(rr.Body.Bytes(), &raw); err != nil {
		t.Fatal(err)
	}
	rec := raw["records"].([]any)[0].(map[string]any)
	if amt, ok := rec["amount"].(string); !ok || amt != "1234.5678" {
		t.Fatalf("amount must be a decimal string, got %#v", rec["amount"])
	}
	if raw["watermark"] != "1:abc" {
		t.Fatalf("watermark: %v", raw["watermark"])
	}
	if raw["next_cursor"] != base64.RawURLEncoding.EncodeToString([]byte("id-1")) {
		t.Fatalf("next_cursor must be opaque base64url of last record_id, got %v", raw["next_cursor"])
	}
	dt := raw["declared_totals"].(map[string]any)
	if dt["row_count"].(float64) != 1 || dt["totals"].(map[string]any)["USD"] != "1234.5678" {
		t.Fatalf("declared_totals: %v", dt)
	}
}

func TestControlPopulation_DefaultLimit(t *testing.T) {
	s := newStubStoreReal()
	cpGet(cpRouter(s, &stubAuthZ{}, "tenant-abc"), cpBase+"?legal_entity_id=le-1&leg=target", "p1")
	if s.cpQuery.Limit != 1000 || s.cpQuery.Leg != "target" {
		t.Fatalf("unexpected query: %+v", s.cpQuery)
	}
}
