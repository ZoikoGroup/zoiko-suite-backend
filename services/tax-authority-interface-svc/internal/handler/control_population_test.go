package handler

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
	"time"

	"github.com/go-chi/chi/v5"
	"go.uber.org/zap"

	"zoiko.io/tax-authority-interface-svc/internal/authz"
	"zoiko.io/tax-authority-interface-svc/internal/domain"
	"zoiko.io/tax-authority-interface-svc/internal/events"
	"zoiko.io/tax-authority-interface-svc/internal/middleware"
	"zoiko.io/tax-authority-interface-svc/internal/store"
)

type fakePopStore struct {
	store.Store
	gotTenant string
	gotQuery  domain.UnacknowledgedFilingsQuery
	calls     int
	page      *domain.ControlPopulationPage
	err       error
}

func (f *fakePopStore) QueryUnacknowledgedFilings(_ context.Context, tenantID string, q domain.UnacknowledgedFilingsQuery) (*domain.ControlPopulationPage, error) {
	f.calls++
	f.gotTenant, f.gotQuery = tenantID, q
	return f.page, f.err
}

// plainStore does not implement ControlPopulationReader.
type plainStore struct{ store.Store }

func newPopRouter(t *testing.T, st store.Store, authzStatus int, decision string) chi.Router {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]string
		_ = json.NewDecoder(r.Body).Decode(&body)
		if body["action_type"] != "TAX_AUTHORITY_CONTROL_POPULATION_READ" {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(authzStatus)
		_ = json.NewEncoder(w).Encode(map[string]string{"decision_outcome": decision})
	}))
	t.Cleanup(srv.Close)
	h := New(st, events.NewMockPublisher(), authz.NewClient(srv.URL), zap.NewNop())
	r := chi.NewRouter()
	r.Use(middleware.TenantContext)
	RegisterRoutes(r, h)
	return r
}

func popGet(r chi.Router, query string, tenant, principal string) *httptest.ResponseRecorder {
	req := httptest.NewRequest("GET", "/v1/control-populations/unacknowledged-filings?"+query, nil)
	if tenant != "" {
		req.Header.Set("X-Tenant-Id", tenant)
	}
	if principal != "" {
		req.Header.Set("X-Principal-Id", principal)
	}
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w
}

func okPage() *domain.ControlPopulationPage {
	return &domain.ControlPopulationPage{
		Records: []domain.ControlPopulationRecord{{
			RecordID: "sub-1", Reference: "sub-1", Amount: "1250.5000", Currency: "XXX", Date: "2026-01-05",
			Attributes: map[string]string{"interface_id": "if-1", "tax_period": "2025-Q4", "filing_type": "VAT", "status": "PENDING", "age_days": "3"},
		}},
		NextRecordID: "sub-1",
		Watermark:    "tx1:n=2;md5=abc",
		DeclaredTotals: domain.ControlDeclaredTotals{
			RowCount: 2, Totals: map[string]string{"XXX": "2500.0000"},
		},
	}
}

func TestControlPopulation_Validation(t *testing.T) {
	cases := []struct{ name, q string }{
		{"missing entity", "submitted_before=2026-02-01"},
		{"bad entity", "legal_entity_id=%20x&submitted_before=2026-02-01"},
		{"missing submitted_before", "legal_entity_id=le-1"},
		{"empty submitted_before", "legal_entity_id=le-1&submitted_before="},
		{"bad date", "legal_entity_id=le-1&submitted_before=2026-13-01"},
		{"bad rfc3339", "legal_entity_id=le-1&submitted_before=2026-02-01T00:00:00"},
		{"garbage", "legal_entity_id=le-1&submitted_before=yesterday"},
		{"period_id rejected", "legal_entity_id=le-1&submitted_before=2026-02-01&period_id=2026-01"},
		{"unknown param", "legal_entity_id=le-1&submitted_before=2026-02-01&foo=bar"},
		{"repeated param", "legal_entity_id=le-1&submitted_before=2026-02-01&submitted_before=2026-03-01"},
		{"repeated entity", "legal_entity_id=le-1&legal_entity_id=le-2&submitted_before=2026-02-01"},
		{"limit zero", "legal_entity_id=le-1&submitted_before=2026-02-01&limit=0"},
		{"limit too big", "legal_entity_id=le-1&submitted_before=2026-02-01&limit=5001"},
		{"limit nan", "legal_entity_id=le-1&submitted_before=2026-02-01&limit=x"},
		{"bad cursor", "legal_entity_id=le-1&submitted_before=2026-02-01&cursor=***"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fs := &fakePopStore{Store: store.NewMemoryStore(), page: okPage()}
			r := newPopRouter(t, fs, 200, "GRANTED")
			w := popGet(r, tc.q, "t1", "p1")
			if w.Code != http.StatusBadRequest {
				t.Fatalf("want 400, got %d: %s", w.Code, w.Body.String())
			}
			if fs.calls != 0 {
				t.Fatalf("store must not be called on a rejected request")
			}
		})
	}
}

func TestControlPopulation_SubmittedBeforeForms(t *testing.T) {
	cases := []struct {
		raw  string
		want time.Time
	}{
		{"2026-02-01", time.Date(2026, 2, 1, 0, 0, 0, 0, time.UTC)},
		{url.QueryEscape("2026-02-01T10:30:00Z"), time.Date(2026, 2, 1, 10, 30, 0, 0, time.UTC)},
		{url.QueryEscape("2026-02-01T10:30:00+02:00"), time.Date(2026, 2, 1, 8, 30, 0, 0, time.UTC)},
	}
	for _, tc := range cases {
		fs := &fakePopStore{Store: store.NewMemoryStore(), page: okPage()}
		r := newPopRouter(t, fs, 200, "GRANTED")
		w := popGet(r, "legal_entity_id=le-1&submitted_before="+tc.raw, "t1", "p1")
		if w.Code != 200 {
			t.Fatalf("%s: want 200, got %d: %s", tc.raw, w.Code, w.Body.String())
		}
		if !fs.gotQuery.SubmittedBefore.Equal(tc.want) || fs.gotQuery.SubmittedBefore.Location() != time.UTC {
			t.Fatalf("%s: got %v want %v (UTC)", tc.raw, fs.gotQuery.SubmittedBefore, tc.want)
		}
	}
}

func TestControlPopulation_AuthAndErrors(t *testing.T) {
	const q = "legal_entity_id=le-1&submitted_before=2026-02-01"
	t.Run("no tenant 401", func(t *testing.T) {
		r := newPopRouter(t, &fakePopStore{Store: store.NewMemoryStore(), page: okPage()}, 200, "GRANTED")
		if w := popGet(r, q, "", "p1"); w.Code != 401 {
			t.Fatalf("got %d", w.Code)
		}
	})
	t.Run("no principal 401", func(t *testing.T) {
		r := newPopRouter(t, &fakePopStore{Store: store.NewMemoryStore(), page: okPage()}, 200, "GRANTED")
		if w := popGet(r, q, "t1", ""); w.Code != 401 {
			t.Fatalf("got %d", w.Code)
		}
	})
	t.Run("deny 403", func(t *testing.T) {
		fs := &fakePopStore{Store: store.NewMemoryStore(), page: okPage()}
		r := newPopRouter(t, fs, 200, "DENIED")
		if w := popGet(r, q, "t1", "p1"); w.Code != 403 {
			t.Fatalf("got %d", w.Code)
		}
		if fs.calls != 0 {
			t.Fatal("store called despite deny")
		}
	})
	t.Run("authz outage 503 fail closed", func(t *testing.T) {
		fs := &fakePopStore{Store: store.NewMemoryStore(), page: okPage()}
		r := newPopRouter(t, fs, 500, "GRANTED")
		if w := popGet(r, q, "t1", "p1"); w.Code != 503 {
			t.Fatalf("got %d", w.Code)
		}
		if fs.calls != 0 {
			t.Fatal("store called during authz outage")
		}
	})
	t.Run("unknown population 404", func(t *testing.T) {
		r := newPopRouter(t, &fakePopStore{Store: store.NewMemoryStore(), page: okPage()}, 200, "GRANTED")
		req := httptest.NewRequest("GET", "/v1/control-populations/nope?"+q, nil)
		req.Header.Set("X-Tenant-Id", "t1")
		req.Header.Set("X-Principal-Id", "p1")
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		if w.Code != 404 {
			t.Fatalf("got %d", w.Code)
		}
	})
	t.Run("store error 503", func(t *testing.T) {
		r := newPopRouter(t, &fakePopStore{Store: store.NewMemoryStore(), err: errors.New("boom")}, 200, "GRANTED")
		if w := popGet(r, q, "t1", "p1"); w.Code != 503 {
			t.Fatalf("got %d", w.Code)
		}
	})
	t.Run("too large 422", func(t *testing.T) {
		r := newPopRouter(t, &fakePopStore{Store: store.NewMemoryStore(), err: domain.ErrControlPopulationTooLarge}, 200, "GRANTED")
		if w := popGet(r, q, "t1", "p1"); w.Code != 422 {
			t.Fatalf("got %d", w.Code)
		}
	})
	t.Run("store without reader 503", func(t *testing.T) {
		r := newPopRouter(t, plainStore{store.NewMemoryStore()}, 200, "GRANTED")
		if w := popGet(r, q, "t1", "p1"); w.Code != 503 {
			t.Fatalf("got %d", w.Code)
		}
	})
}

func TestControlPopulation_HappyPath(t *testing.T) {
	fs := &fakePopStore{Store: store.NewMemoryStore(), page: okPage()}
	r := newPopRouter(t, fs, 200, "GRANTED")
	cur := base64.RawURLEncoding.EncodeToString([]byte("sub-0"))
	w := popGet(r, "legal_entity_id=le-1&submitted_before=2026-02-01&limit=10&cursor="+cur, "tenant-a", "p1")
	if w.Code != 200 {
		t.Fatalf("got %d: %s", w.Code, w.Body.String())
	}
	if fs.gotTenant != "tenant-a" || fs.gotQuery.LegalEntityID != "le-1" || fs.gotQuery.Limit != 10 || fs.gotQuery.AfterRecordID != "sub-0" {
		t.Fatalf("query not passed through: %+v tenant=%s", fs.gotQuery, fs.gotTenant)
	}
	var resp map[string]json.RawMessage
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"records", "next_cursor", "watermark", "declared_totals"} {
		if _, ok := resp[k]; !ok {
			t.Fatalf("missing %s", k)
		}
	}
	if !strings.Contains(w.Body.String(), `"amount":"1250.5000"`) {
		t.Fatalf("amount must be a decimal string: %s", w.Body.String())
	}
	var nc string
	_ = json.Unmarshal(resp["next_cursor"], &nc)
	if b, _ := base64.RawURLEncoding.DecodeString(nc); string(b) != "sub-1" {
		t.Fatalf("next_cursor should encode last record id, got %q", nc)
	}

	fs2 := &fakePopStore{Store: store.NewMemoryStore(), page: &domain.ControlPopulationPage{}}
	r2 := newPopRouter(t, fs2, 200, "GRANTED")
	w2 := popGet(r2, "legal_entity_id=le-1&submitted_before=2026-02-01", "t1", "p1")
	if w2.Code != 200 || fs2.gotQuery.Limit != 1000 {
		t.Fatalf("default limit: code=%d limit=%d", w2.Code, fs2.gotQuery.Limit)
	}
	if !strings.Contains(w2.Body.String(), `"records":[]`) || !strings.Contains(w2.Body.String(), `"totals":{}`) {
		t.Fatalf("empty page must render [] and {}: %s", w2.Body.String())
	}
}
