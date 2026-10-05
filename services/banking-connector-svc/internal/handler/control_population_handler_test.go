package handler

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/go-chi/chi/v5"
	"go.uber.org/zap"

	"zoiko.io/banking-connector-svc/internal/authz"
	"zoiko.io/banking-connector-svc/internal/domain"
	"zoiko.io/banking-connector-svc/internal/events"
	"zoiko.io/banking-connector-svc/internal/middleware"
	"zoiko.io/banking-connector-svc/internal/store"
)

type fakeCPStore struct {
	last  domain.ControlPopulationQuery
	calls int
	page  *domain.ControlPopulationPage
	err   error
}

func (f *fakeCPStore) ControlPopulation(_ context.Context, q domain.ControlPopulationQuery) (*domain.ControlPopulationPage, error) {
	f.calls++
	f.last = q
	return f.page, f.err
}

// authzServer answers /v1/authorize with the given outcome, or 500 when
// outcome is "" (authorization-svc unavailable). It records the last request.
func authzServer(t *testing.T, outcome string, seen *map[string]string) *httptest.Server {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if outcome == "" {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		if seen != nil {
			var body map[string]string
			_ = json.NewDecoder(r.Body).Decode(&body)
			*seen = body
		}
		_ = json.NewEncoder(w).Encode(map[string]string{"decision_outcome": outcome})
	}))
	t.Cleanup(srv.Close)
	return srv
}

func cpRouter(t *testing.T, outcome string, f *fakeCPStore, seen *map[string]string) http.Handler {
	h := New(store.NewMemoryStore(), events.NewMockPublisher(), authz.NewClient(authzServer(t, outcome, seen).URL), zap.NewNop())
	r := chi.NewRouter()
	r.Use(middleware.TenantContext)
	RegisterControlPopulationRoutes(r, h, f)
	return r
}

func cpGet(r http.Handler, path string, tenant, principal string) *httptest.ResponseRecorder {
	req := httptest.NewRequest("GET", path, nil)
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

func samplePage() *domain.ControlPopulationPage {
	return &domain.ControlPopulationPage{
		Records: []domain.ControlRecord{
			{RecordID: "t-1", Reference: "REF1", Amount: "-20.0000", Currency: "USD", Date: "2026-09-01",
				Attributes: map[string]string{"bank_account_id": "acct-1", "statement_id": "s-1", "status": "NORMALIZED"}},
		},
		NextCursor: "t-1",
		Watermark:  "1:abc",
		DeclaredTotals: domain.ControlDeclaredTotals{RowCount: 2,
			Totals: map[string]string{"USD": "80.5000"}},
	}
}

const cpBase = "/v1/control-populations/bank-transactions?legal_entity_id=le-1"

func TestControlPopulation_MissingTenant401(t *testing.T) {
	f := &fakeCPStore{page: samplePage()}
	if w := cpGet(cpRouter(t, "GRANTED", f, nil), cpBase, "", "p1"); w.Code != 401 {
		t.Fatalf("got %d", w.Code)
	}
	if f.calls != 0 {
		t.Fatal("store must not be called")
	}
}

func TestControlPopulation_MissingPrincipal401(t *testing.T) {
	f := &fakeCPStore{page: samplePage()}
	if w := cpGet(cpRouter(t, "GRANTED", f, nil), cpBase, "t1", ""); w.Code != 401 {
		t.Fatalf("got %d", w.Code)
	}
}

func TestControlPopulation_BadRequests(t *testing.T) {
	f := &fakeCPStore{page: samplePage()}
	r := cpRouter(t, "GRANTED", f, nil)
	for name, path := range map[string]string{
		"missing legal_entity_id": "/v1/control-populations/bank-transactions",
		"invalid legal_entity_id": "/v1/control-populations/bank-transactions?legal_entity_id=%27%3B--",
		"unknown param":           cpBase + "&colour=red",
		"bad period":              cpBase + "&period_id=2026-13",
		"bad period format":       cpBase + "&period_id=2026-9",
		"bad bank_account_id":     cpBase + "&bank_account_id=a%20b",
		"empty bank_account_id":   cpBase + "&bank_account_id=",
		"bad limit":               cpBase + "&limit=0",
		"bad cursor":              cpBase + "&cursor=!!!",
		"repeated param":          cpBase + "&limit=1&limit=2",
		"statements unknown":      "/v1/control-populations/bank-statements?legal_entity_id=le-1&account_codes=1",
	} {
		if w := cpGet(r, path, "t1", "p1"); w.Code != 400 {
			t.Errorf("%s: got %d %s", name, w.Code, w.Body.String())
		}
	}
	if f.calls != 0 {
		t.Fatal("store must not be called for rejected requests")
	}
}

func TestControlPopulation_UnknownPopulation404(t *testing.T) {
	f := &fakeCPStore{page: samplePage()}
	if w := cpGet(cpRouter(t, "GRANTED", f, nil), "/v1/control-populations/nope?legal_entity_id=le-1", "t1", "p1"); w.Code != 404 {
		t.Fatalf("got %d", w.Code)
	}
}

func TestControlPopulation_AuthzDenied403(t *testing.T) {
	f := &fakeCPStore{page: samplePage()}
	if w := cpGet(cpRouter(t, "DENIED", f, nil), cpBase, "t1", "p1"); w.Code != 403 {
		t.Fatalf("got %d", w.Code)
	}
	if f.calls != 0 {
		t.Fatal("store must not be called when denied")
	}
}

func TestControlPopulation_AuthzUnavailable503(t *testing.T) {
	f := &fakeCPStore{page: samplePage()}
	if w := cpGet(cpRouter(t, "", f, nil), cpBase, "t1", "p1"); w.Code != 503 {
		t.Fatalf("got %d", w.Code)
	}
	if f.calls != 0 {
		t.Fatal("store must not be called when authz unavailable")
	}
}

func TestControlPopulation_TooLarge422(t *testing.T) {
	f := &fakeCPStore{err: domain.ErrPopulationTooLarge}
	if w := cpGet(cpRouter(t, "GRANTED", f, nil), cpBase, "t1", "p1"); w.Code != 422 {
		t.Fatalf("got %d", w.Code)
	}
}

func TestControlPopulation_HappyPath_Transactions(t *testing.T) {
	f := &fakeCPStore{page: samplePage()}
	var seen map[string]string
	r := cpRouter(t, "GRANTED", f, &seen)
	w := cpGet(r, cpBase+"&period_id=2026-02&bank_account_id=acct-1&limit=10", "t1", "p1")
	if w.Code != 200 {
		t.Fatalf("got %d %s", w.Code, w.Body.String())
	}
	if seen["action_type"] != BANKING_CONTROL_POPULATION_READ || seen["legal_entity_id"] != "le-1" || seen["principal_id"] != "p1" {
		t.Fatalf("authz request: %v", seen)
	}
	q := f.last
	if q.TenantID != "t1" || q.LegalEntityID != "le-1" || q.PeriodEnd != "2026-02-28" || q.BankAccountID != "acct-1" || q.Limit != 10 || q.Population != "bank-transactions" {
		t.Fatalf("query passed to store: %+v", q)
	}

	var raw map[string]json.RawMessage
	if err := json.Unmarshal(w.Body.Bytes(), &raw); err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"records", "next_cursor", "watermark", "declared_totals"} {
		if _, ok := raw[k]; !ok {
			t.Fatalf("missing %s in %s", k, w.Body.String())
		}
	}
	var body struct {
		Records []map[string]any `json:"records"`
		Next    string           `json:"next_cursor"`
		WM      string           `json:"watermark"`
		DT      struct {
			RowCount int               `json:"row_count"`
			Totals   map[string]string `json:"totals"`
		} `json:"declared_totals"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &body)
	if s, ok := body.Records[0]["amount"].(string); !ok || s != "-20.0000" {
		t.Fatalf("amount must be a string: %v", body.Records[0]["amount"])
	}
	dec, _ := base64.RawURLEncoding.DecodeString(body.Next)
	if string(dec) != "t-1" {
		t.Fatalf("cursor not round-trippable: %q", body.Next)
	}
	if body.WM != "1:abc" || body.DT.RowCount != 2 || body.DT.Totals["USD"] != "80.5000" {
		t.Fatalf("body: %+v", body)
	}

	// The cursor is fed back as the keyset position.
	if w := cpGet(r, cpBase+"&cursor="+body.Next, "t1", "p1"); w.Code != 200 || f.last.AfterRecordID != "t-1" {
		t.Fatalf("cursor round trip: %d %+v", w.Code, f.last)
	}
}

func TestControlPopulation_HappyPath_Statements(t *testing.T) {
	f := &fakeCPStore{page: &domain.ControlPopulationPage{
		Records: []domain.ControlRecord{{RecordID: "s-1", Reference: "acct-1|2026-09-01", Amount: "1000.0000", Currency: "USD", Date: "2026-09-01",
			Attributes: map[string]string{"bank_account_id": "acct-1", "closing_balance_available": "true"}}},
		Watermark:      "1:x",
		DeclaredTotals: domain.ControlDeclaredTotals{RowCount: 1, Totals: map[string]string{"USD": "1000.0000"}},
	}}
	w := cpGet(cpRouter(t, "GRANTED", f, nil), "/v1/control-populations/bank-statements?legal_entity_id=le-1", "t1", "p1")
	if w.Code != 200 {
		t.Fatalf("got %d %s", w.Code, w.Body.String())
	}
	if f.last.Population != "bank-statements" || f.last.Limit != controlDefaultLimit {
		t.Fatalf("query: %+v", f.last)
	}
	var body domain.ControlPopulationPage
	_ = json.Unmarshal(w.Body.Bytes(), &body)
	if body.NextCursor != "" || body.Records[0].Reference != "acct-1|2026-09-01" {
		t.Fatalf("body: %s", w.Body.String())
	}
}
