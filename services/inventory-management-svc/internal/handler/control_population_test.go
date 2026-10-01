package handler_test

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
	"go.uber.org/zap"

	"zoiko.io/inventory-management-svc/internal/domain"
	"zoiko.io/inventory-management-svc/internal/handler"
	"zoiko.io/inventory-management-svc/internal/middleware"
)

// ctrlPopStub records ControlPopulationStockCountLines calls and returns a
// canned page or error.
type ctrlPopStub struct {
	calls int
	last  domain.StockCountLinesQuery
	page  *domain.ControlPopulationPage
	err   error
}

func (s *stubStore) ControlPopulationStockCountLines(_ context.Context, q domain.StockCountLinesQuery) (*domain.ControlPopulationPage, error) {
	s.ctrlPop.calls++
	s.ctrlPop.last = q
	if s.ctrlPop.err != nil {
		return nil, s.ctrlPop.err
	}
	return s.ctrlPop.page, nil
}

const (
	cpEntity = "11111111-1111-4111-8111-111111111111"
	cpCount  = "22222222-2222-4222-8222-222222222222"
	cpLine   = "33333333-3333-4333-8333-333333333333"
)

func cpRouter(s *stubStore, authz *stubAuthZ, tenant string) chi.Router {
	r := chi.NewRouter()
	r.Use(func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			if tenant != "" {
				req = req.WithContext(middleware.WithTenant(req.Context(), tenant))
			}
			next.ServeHTTP(w, req)
		})
	})
	handler.RegisterRoutes(r, handler.New(s, &stubPublisher{}, authz, zap.NewNop()))
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

func cpPath(extra string) string {
	p := "/v1/control-populations/stock-count-lines?legal_entity_id=" + cpEntity + "&count_id=" + cpCount + "&side=book"
	return p + extra
}

func TestControlPopulation_MissingIdentity_Returns401(t *testing.T) {
	s := newStubStore()
	if rr := cpGet(cpRouter(s, &stubAuthZ{}, ""), cpPath(""), "p1"); rr.Code != http.StatusUnauthorized {
		t.Fatalf("missing tenant: got %d", rr.Code)
	}
	if rr := cpGet(cpRouter(s, &stubAuthZ{}, "tenant-abc"), cpPath(""), ""); rr.Code != http.StatusUnauthorized {
		t.Fatalf("missing principal: got %d", rr.Code)
	}
	if s.ctrlPop.calls != 0 {
		t.Fatal("store must not be called")
	}
}

func TestControlPopulation_BadRequests_Return400(t *testing.T) {
	base := "/v1/control-populations/stock-count-lines"
	cases := map[string]string{
		"missing legal_entity_id": base + "?count_id=" + cpCount + "&side=book",
		"bad legal_entity_id":     base + "?legal_entity_id=nope&count_id=" + cpCount + "&side=book",
		"missing count_id":        base + "?legal_entity_id=" + cpEntity + "&side=book",
		"bad count_id":            base + "?legal_entity_id=" + cpEntity + "&count_id=x&side=book",
		"missing side":            base + "?legal_entity_id=" + cpEntity + "&count_id=" + cpCount,
		"bad side":                base + "?legal_entity_id=" + cpEntity + "&count_id=" + cpCount + "&side=frozen",
		"unknown param":           cpPath("&colour=red"),
		"period_id is unknown":    cpPath("&period_id=2026-09"),
		"repeated side":           cpPath("&side=physical"),
		"repeated limit":          cpPath("&limit=1&limit=2"),
		"limit not a number":      cpPath("&limit=abc"),
		"limit zero":              cpPath("&limit=0"),
		"limit above max":         cpPath("&limit=5001"),
		"cursor not base64":       cpPath("&cursor=!!!"),
		"cursor not a uuid":       cpPath("&cursor=" + base64.RawURLEncoding.EncodeToString([]byte("hello"))),
	}
	for name, path := range cases {
		t.Run(name, func(t *testing.T) {
			s := newStubStore()
			rr := cpGet(cpRouter(s, &stubAuthZ{}, "tenant-abc"), path, "p1")
			if rr.Code != http.StatusBadRequest {
				t.Fatalf("got %d body %s", rr.Code, rr.Body.String())
			}
			if s.ctrlPop.calls != 0 {
				t.Fatal("store must not be called")
			}
		})
	}
}

func TestControlPopulation_UnknownPopulation_Returns404(t *testing.T) {
	s := newStubStore()
	rr := cpGet(cpRouter(s, &stubAuthZ{}, "tenant-abc"), "/v1/control-populations/nope?legal_entity_id="+cpEntity, "p1")
	if rr.Code != http.StatusNotFound || s.ctrlPop.calls != 0 {
		t.Fatalf("got %d calls %d", rr.Code, s.ctrlPop.calls)
	}
}

func TestControlPopulation_UnknownCount_Returns404(t *testing.T) {
	s := newStubStore()
	s.ctrlPop.err = domain.ErrStockCountNotFound
	rr := cpGet(cpRouter(s, &stubAuthZ{}, "tenant-abc"), cpPath(""), "p1")
	if rr.Code != http.StatusNotFound {
		t.Fatalf("got %d", rr.Code)
	}
}

func TestControlPopulation_AuthzDeniedAndUnavailable_StoreNotCalled(t *testing.T) {
	for name, tc := range map[string]struct {
		err  error
		want int
	}{
		"denied":      {domain.ErrAuthorizationDenied, http.StatusForbidden},
		"unavailable": {errors.New("authz down"), http.StatusServiceUnavailable},
	} {
		t.Run(name, func(t *testing.T) {
			s := newStubStore()
			rr := cpGet(cpRouter(s, &stubAuthZ{err: tc.err}, "tenant-abc"), cpPath(""), "p1")
			if rr.Code != tc.want {
				t.Fatalf("got %d", rr.Code)
			}
			if s.ctrlPop.calls != 0 {
				t.Fatal("store must not be called when authorization does not allow")
			}
		})
	}
}

func TestControlPopulation_TooLarge_Returns422(t *testing.T) {
	s := newStubStore()
	s.ctrlPop.err = domain.ErrPopulationTooLarge
	if rr := cpGet(cpRouter(s, &stubAuthZ{}, "tenant-abc"), cpPath(""), "p1"); rr.Code != http.StatusUnprocessableEntity {
		t.Fatalf("got %d", rr.Code)
	}
}

func TestControlPopulation_StoreUnavailable_Returns503(t *testing.T) {
	s := newStubStore()
	s.ctrlPop.err = errors.New("db down")
	if rr := cpGet(cpRouter(s, &stubAuthZ{}, "tenant-abc"), cpPath(""), "p1"); rr.Code != http.StatusServiceUnavailable {
		t.Fatalf("got %d", rr.Code)
	}
}

func TestControlPopulation_HappyPath_Shape(t *testing.T) {
	s := newStubStore()
	s.ctrlPop.page = &domain.ControlPopulationPage{
		Records: []domain.ControlRecord{{
			RecordID: cpLine, Reference: cpCount + "|i|l", Amount: "10.0000", Currency: "XXX", Date: "2026-09-01",
			Attributes: map[string]string{"uom": "EACH", "frozen_system_quantity": "12.0000"},
		}},
		NextCursor:     cpLine,
		Watermark:      "1:abc",
		DeclaredTotals: domain.DeclaredTotals{RowCount: 1, Totals: map[string]string{"XXX": "10.0000"}},
	}
	path := cpPath("&limit=7&cursor=" + url.QueryEscape(base64.RawURLEncoding.EncodeToString([]byte(cpLine))))
	rr := cpGet(cpRouter(s, &stubAuthZ{}, "tenant-abc"), path, "p1")
	if rr.Code != http.StatusOK {
		t.Fatalf("got %d %s", rr.Code, rr.Body.String())
	}
	q := s.ctrlPop.last
	if q.TenantID != "tenant-abc" || q.LegalEntityID != cpEntity || q.CountID != cpCount || q.Side != "book" || q.Limit != 7 || q.AfterRecordID != cpLine {
		t.Fatalf("unexpected query %+v", q)
	}
	var body map[string]any
	if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body["watermark"] != "1:abc" {
		t.Fatalf("watermark %v", body["watermark"])
	}
	if got, _ := base64.RawURLEncoding.DecodeString(body["next_cursor"].(string)); string(got) != cpLine {
		t.Fatalf("next_cursor must be base64url of the last record_id, got %v", body["next_cursor"])
	}
	rec := body["records"].([]any)[0].(map[string]any)
	if rec["amount"] != "10.0000" || rec["currency"] != "XXX" {
		t.Fatalf("amount must be a decimal string: %v", rec)
	}
	if !strings.Contains(rr.Body.String(), `"row_count":1`) || !strings.Contains(rr.Body.String(), `"XXX":"10.0000"`) {
		t.Fatalf("declared_totals missing: %s", rr.Body.String())
	}
}

func TestControlPopulation_DefaultLimit(t *testing.T) {
	s := newStubStore()
	s.ctrlPop.page = &domain.ControlPopulationPage{}
	rr := cpGet(cpRouter(s, &stubAuthZ{}, "tenant-abc"), cpPath(""), "p1")
	if rr.Code != http.StatusOK || s.ctrlPop.last.Limit != 1000 {
		t.Fatalf("got %d limit %d", rr.Code, s.ctrlPop.last.Limit)
	}
	if !strings.Contains(rr.Body.String(), `"records":[]`) {
		t.Fatalf("records must be an empty array: %s", rr.Body.String())
	}
}
