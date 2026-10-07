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
	"sync"
	"testing"

	"github.com/go-chi/chi/v5"
	"go.uber.org/zap"

	"zoiko.io/consolidation-svc/internal/domain"
	"zoiko.io/consolidation-svc/internal/handler"
	"zoiko.io/consolidation-svc/internal/middleware"
)

const (
	popRunID    = "11111111-1111-4111-8111-111111111111"
	popRecordID = "22222222-2222-4222-8222-222222222222"
)

// popFake records what the handler asked the store for and returns a canned answer.
type popFake struct {
	calls int
	last  domain.BalanceContributionsQuery
	page  *domain.ControlPopulationPage
	err   error
}

var popFakes sync.Map // *stubStore -> *popFake

// BalanceContributionsPopulation makes the shared stubStore satisfy handler.Store.
func (s *stubStore) BalanceContributionsPopulation(_ context.Context, q domain.BalanceContributionsQuery) (*domain.ControlPopulationPage, error) {
	v, ok := popFakes.Load(s)
	if !ok {
		return nil, errors.New("population fake not configured")
	}
	f := v.(*popFake)
	f.calls++
	f.last = q
	return f.page, f.err
}

// countingAuthZ records the action and entity authorised.
type countingAuthZ struct {
	err       error
	calls     int
	entity    string
	action    string
	principal string
}

func (a *countingAuthZ) CheckAllowed(_ context.Context, principalID, legalEntityID, actionType string) error {
	a.calls++
	a.principal, a.entity, a.action = principalID, legalEntityID, actionType
	return a.err
}

func popRouter(tenant string, f *popFake, az *countingAuthZ) chi.Router {
	s := newStubStore()
	popFakes.Store(s, f)
	r := chi.NewRouter()
	r.Use(func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			if tenant != "" {
				req = req.WithContext(middleware.WithTenant(req.Context(), tenant))
			}
			next.ServeHTTP(w, req)
		})
	})
	handler.RegisterRoutes(r, handler.New(s, &stubPublisher{}, az, &stubClients{}, zap.NewNop()))
	return r
}

func popGet(r chi.Router, path, principal string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodGet, path, nil)
	if principal != "" {
		req.Header.Set("X-Principal-Id", principal)
	}
	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, req)
	return rr
}

func okPage() *domain.ControlPopulationPage {
	return &domain.ControlPopulationPage{
		Records: []domain.ControlRecord{{
			RecordID: popRecordID, Reference: "1000-Cash", Amount: "1234.5678", Currency: "USD", Date: "2026-09-01",
			Attributes: map[string]string{"currency_basis": "target_currency_label"},
		}},
		NextCursor:     popRecordID,
		Watermark:      "1:abc",
		DeclaredTotals: domain.DeclaredTotals{RowCount: 1, Totals: map[string]string{"USD": "1234.5678"}},
	}
}

const (
	validQuery = "?legal_entity_id=child-1&run_id=" + popRunID
	basePath   = "/v1/control-populations/balance-contributions"
)

func TestControlPopulation_Identity401(t *testing.T) {
	f := &popFake{page: okPage()}
	if rr := popGet(popRouter("", f, &countingAuthZ{}), basePath+validQuery, "p1"); rr.Code != http.StatusUnauthorized {
		t.Fatalf("missing tenant: got %d", rr.Code)
	}
	if rr := popGet(popRouter("t1", f, &countingAuthZ{}), basePath+validQuery, ""); rr.Code != http.StatusUnauthorized {
		t.Fatalf("missing principal: got %d", rr.Code)
	}
	if f.calls != 0 {
		t.Fatal("store must not be called")
	}
}

func TestControlPopulation_BadRequests(t *testing.T) {
	cursorOK := base64.RawURLEncoding.EncodeToString([]byte(popRecordID))
	cases := map[string]string{
		"missing legal_entity_id": "?run_id=" + popRunID,
		"bad legal_entity_id":     "?legal_entity_id=" + url.QueryEscape("a b/../") + "&run_id=" + popRunID,
		"too long entity":         "?legal_entity_id=" + strings.Repeat("a", 65) + "&run_id=" + popRunID,
		"missing run_id":          "?legal_entity_id=child-1",
		"bad run_id":              "?legal_entity_id=child-1&run_id=not-a-uuid",
		"upper-case run_id":       "?legal_entity_id=child-1&run_id=11111111-1111-4111-8111-11111111111A",
		"period_id rejected":      validQuery + "&period_id=2026-09",
		"unknown param":           validQuery + "&foo=bar",
		"repeated entity":         validQuery + "&legal_entity_id=child-2",
		"repeated run_id":         validQuery + "&run_id=" + popRunID,
		"repeated limit":          validQuery + "&limit=1&limit=2",
		"limit zero":              validQuery + "&limit=0",
		"limit too large":         validQuery + "&limit=5001",
		"limit not int":           validQuery + "&limit=abc",
		"cursor not base64":       validQuery + "&cursor=***",
		"cursor not uuid":         validQuery + "&cursor=" + base64.RawURLEncoding.EncodeToString([]byte("nope")),
		"repeated cursor":         validQuery + "&cursor=" + cursorOK + "&cursor=" + cursorOK,
	}
	for name, qs := range cases {
		t.Run(name, func(t *testing.T) {
			f := &popFake{page: okPage()}
			az := &countingAuthZ{}
			rr := popGet(popRouter("t1", f, az), basePath+qs, "p1")
			if rr.Code != http.StatusBadRequest {
				t.Fatalf("got %d, want 400: %s", rr.Code, rr.Body.String())
			}
			if f.calls != 0 || az.calls != 0 {
				t.Fatal("neither store nor authz may be reached on a 400")
			}
		})
	}
}

func TestControlPopulation_NotFound(t *testing.T) {
	f := &popFake{page: okPage()}
	rr := popGet(popRouter("t1", f, &countingAuthZ{}), "/v1/control-populations/nope"+validQuery, "p1")
	if rr.Code != http.StatusNotFound || f.calls != 0 {
		t.Fatalf("unknown population: got %d calls=%d", rr.Code, f.calls)
	}
	f = &popFake{err: domain.ErrRunNotFound}
	rr = popGet(popRouter("t1", f, &countingAuthZ{}), basePath+validQuery, "p1")
	if rr.Code != http.StatusNotFound || f.calls != 1 {
		t.Fatalf("unknown run: got %d calls=%d", rr.Code, f.calls)
	}
}

func TestControlPopulation_AuthzFailClosed(t *testing.T) {
	for name, tc := range map[string]struct {
		err  error
		want int
	}{
		"denied":      {domain.ErrAuthorizationDenied, http.StatusForbidden},
		"unavailable": {domain.ErrAuthzServiceUnavailable, http.StatusServiceUnavailable},
	} {
		t.Run(name, func(t *testing.T) {
			f := &popFake{page: okPage()}
			az := &countingAuthZ{err: tc.err}
			rr := popGet(popRouter("t1", f, az), basePath+validQuery, "p1")
			if rr.Code != tc.want {
				t.Fatalf("got %d, want %d", rr.Code, tc.want)
			}
			if f.calls != 0 {
				t.Fatal("store must not be called when authz fails")
			}
		})
	}
}

func TestControlPopulation_TooLargeAndStoreDown(t *testing.T) {
	f := &popFake{err: domain.ErrPopulationTooLarge}
	if rr := popGet(popRouter("t1", f, &countingAuthZ{}), basePath+validQuery, "p1"); rr.Code != http.StatusUnprocessableEntity {
		t.Fatalf("too large: got %d", rr.Code)
	}
	f = &popFake{err: errors.New("db down")}
	if rr := popGet(popRouter("t1", f, &countingAuthZ{}), basePath+validQuery, "p1"); rr.Code != http.StatusServiceUnavailable {
		t.Fatalf("store down: got %d", rr.Code)
	}
}

func TestControlPopulation_HappyPath(t *testing.T) {
	f := &popFake{page: okPage()}
	az := &countingAuthZ{}
	cursor := base64.RawURLEncoding.EncodeToString([]byte(popRecordID))
	rr := popGet(popRouter("t1", f, az), basePath+validQuery+"&limit=2&cursor="+cursor, "p1")
	if rr.Code != http.StatusOK {
		t.Fatalf("got %d: %s", rr.Code, rr.Body.String())
	}
	if az.action != "CONSOLIDATION_CONTROL_POPULATION_READ" || az.entity != "child-1" || az.principal != "p1" {
		t.Fatalf("authz called with %+v", az)
	}
	want := domain.BalanceContributionsQuery{TenantID: "t1", RunID: popRunID, LegalEntityID: "child-1", Limit: 2, AfterRecordID: popRecordID}
	if f.last != want {
		t.Fatalf("store query %+v, want %+v", f.last, want)
	}

	var raw struct {
		Records []map[string]any `json:"records"`
		Next    string           `json:"next_cursor"`
		WM      string           `json:"watermark"`
		Totals  struct {
			RowCount int64             `json:"row_count"`
			Totals   map[string]string `json:"totals"`
		} `json:"declared_totals"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &raw); err != nil {
		t.Fatal(err)
	}
	if amt, isString := raw.Records[0]["amount"].(string); !isString || amt != "1234.5678" {
		t.Fatalf("amount must be an exact decimal string, got %#v", raw.Records[0]["amount"])
	}
	if raw.Records[0]["currency"] != "USD" || raw.WM != "1:abc" || raw.Totals.Totals["USD"] != "1234.5678" || raw.Totals.RowCount != 1 {
		t.Fatalf("unexpected body: %s", rr.Body.String())
	}
	dec, err := base64.RawURLEncoding.DecodeString(raw.Next)
	if err != nil || string(dec) != popRecordID {
		t.Fatalf("next_cursor must be base64url of the last record_id, got %q", raw.Next)
	}
}

func TestControlPopulation_DefaultLimitAndEmptyPage(t *testing.T) {
	f := &popFake{page: &domain.ControlPopulationPage{Watermark: "0:d41d8cd98f00b204e9800998ecf8427e"}}
	rr := popGet(popRouter("t1", f, &countingAuthZ{}), basePath+validQuery, "p1")
	if rr.Code != http.StatusOK || f.last.Limit != 1000 {
		t.Fatalf("got %d limit=%d", rr.Code, f.last.Limit)
	}
	var body map[string]json.RawMessage
	_ = json.Unmarshal(rr.Body.Bytes(), &body)
	if string(body["records"]) != "[]" || string(body["next_cursor"]) != `""` {
		t.Fatalf("empty page must serialise records=[] next_cursor=\"\": %s", rr.Body.String())
	}
}
