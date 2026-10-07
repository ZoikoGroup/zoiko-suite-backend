package handler_test

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"testing"

	"github.com/go-chi/chi/v5"
	"go.uber.org/zap"

	"zoiko.io/general-ledger-svc/internal/handler"
	svcmiddleware "zoiko.io/general-ledger-svc/internal/middleware"

	"zoiko.io/general-ledger-svc/internal/domain"
)

const cpEntity = "11111111-1111-4111-8111-111111111111"

// cpRecordingAuthZ captures what the handler asked authorization-svc.
type cpRecordingAuthZ struct {
	principal, entity, action string
	err                       error
}

func (a *cpRecordingAuthZ) CheckAllowed(_ context.Context, principalID, legalEntityID, actionType string) error {
	a.principal, a.entity, a.action = principalID, legalEntityID, actionType
	return a.err
}

func cpURL(extra string) string {
	return "/v1/control-populations/account-postings?legal_entity_id=" + cpEntity +
		"&account_codes=1200,1210&normal_balance=DEBIT" + extra
}

func TestControlPopulation_MissingTenant_Returns401(t *testing.T) {
	r := newRouter(newStubStore(), &stubPublisher{}, &stubAuthZ{})
	resp := doRequestAs(r, http.MethodGet, cpURL(""), nil, "svc-1", "")
	if resp.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d: %s", resp.Code, resp.Body.String())
	}
}

func TestControlPopulation_MissingPrincipal_Returns401(t *testing.T) {
	r := newRouter(newStubStore(), &stubPublisher{}, &stubAuthZ{})
	resp := doRequest(r, http.MethodGet, cpURL(""), nil, "")
	if resp.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d: %s", resp.Code, resp.Body.String())
	}
}

func TestControlPopulation_BadRequests(t *testing.T) {
	base := "/v1/control-populations/account-postings"
	cases := map[string]string{
		"missing legal_entity_id": base + "?account_codes=1200&normal_balance=DEBIT",
		"invalid legal_entity_id": base + "?legal_entity_id=nope&account_codes=1200&normal_balance=DEBIT",
		"unknown param":           cpURL("&bogus=1"),
		"missing account_codes":   base + "?legal_entity_id=" + cpEntity + "&normal_balance=DEBIT",
		"empty code in list":      base + "?legal_entity_id=" + cpEntity + "&account_codes=1200,,1210&normal_balance=DEBIT",
		"code too long":           base + "?legal_entity_id=" + cpEntity + "&account_codes=" + repeat("x", 65) + "&normal_balance=DEBIT",
		"too many codes":          base + "?legal_entity_id=" + cpEntity + "&account_codes=" + codes(21) + "&normal_balance=DEBIT",
		"missing normal_balance":  base + "?legal_entity_id=" + cpEntity + "&account_codes=1200",
		"bad normal_balance":      cpURL("") + "x",
		"bad period_id":           cpURL("&period_id=2026-13"),
		"bad period_id format":    cpURL("&period_id=202609"),
		"bad limit":               cpURL("&limit=0"),
		"bad cursor":              cpURL("&cursor=not-a-cursor"),
		"duplicated param":        cpURL("&limit=1&limit=2"),
	}
	for name, url := range cases {
		t.Run(name, func(t *testing.T) {
			s := newStubStore()
			r := newRouter(s, &stubPublisher{}, &stubAuthZ{})
			resp := doRequest(r, http.MethodGet, url, nil, "svc-1")
			if resp.Code != http.StatusBadRequest {
				t.Fatalf("expected 400, got %d: %s", resp.Code, resp.Body.String())
			}
			if len(s.postingsCalls) != 0 {
				t.Fatal("store must not be queried for an invalid request")
			}
		})
	}
}

func repeat(s string, n int) string {
	out := ""
	for i := 0; i < n; i++ {
		out += s
	}
	return out
}

func codes(n int) string {
	out := ""
	for i := 0; i < n; i++ {
		if i > 0 {
			out += ","
		}
		out += "A" + string(rune('a'+i))
	}
	return out
}

func TestControlPopulation_UnknownPopulation_Returns404(t *testing.T) {
	r := newRouter(newStubStore(), &stubPublisher{}, &stubAuthZ{})
	resp := doRequest(r, http.MethodGet, "/v1/control-populations/nope?legal_entity_id="+cpEntity, nil, "svc-1")
	if resp.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d: %s", resp.Code, resp.Body.String())
	}
}

func TestControlPopulation_AuthzDenied_Returns403(t *testing.T) {
	s := newStubStore()
	r := newRouter(s, &stubPublisher{}, &stubAuthZ{err: domain.ErrAuthorizationDenied})
	resp := doRequest(r, http.MethodGet, cpURL(""), nil, "svc-1")
	if resp.Code != http.StatusForbidden {
		t.Fatalf("expected 403, got %d: %s", resp.Code, resp.Body.String())
	}
	if len(s.postingsCalls) != 0 {
		t.Fatal("store must not be queried when authorization is denied")
	}
}

func TestControlPopulation_AuthzUnavailable_Returns503(t *testing.T) {
	s := newStubStore()
	r := newRouter(s, &stubPublisher{}, &stubAuthZ{err: errors.New("authz down")})
	resp := doRequest(r, http.MethodGet, cpURL(""), nil, "svc-1")
	if resp.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected 503, got %d: %s", resp.Code, resp.Body.String())
	}
	if len(s.postingsCalls) != 0 {
		t.Fatal("fail closed: store must not be queried when authorization is unavailable")
	}
}

func TestControlPopulation_TooLarge_Returns422(t *testing.T) {
	s := newStubStore()
	s.postingsErr = domain.ErrControlPopulationTooLarge
	r := newRouter(s, &stubPublisher{}, &stubAuthZ{})
	resp := doRequest(r, http.MethodGet, cpURL(""), nil, "svc-1")
	if resp.Code != http.StatusUnprocessableEntity {
		t.Fatalf("expected 422, got %d: %s", resp.Code, resp.Body.String())
	}
}

func TestControlPopulation_HappyPath_JSONShapeAndScoping(t *testing.T) {
	s := newStubStore()
	lastID := "22222222-2222-4222-8222-222222222222"
	s.postingsPage = &domain.AccountPostingsPage{
		Records: []domain.ControlPopulationRecord{{
			RecordID: lastID, Reference: "INV-1", Amount: "1250.50", Currency: "USD", Date: "2026-09-01",
			Attributes: map[string]string{"account_code": "1200", "journal_id": "j1", "fiscal_period": "2026-09", "book_id": ""},
		}},
		NextRecordID:   lastID,
		Watermark:      "wm-1",
		DeclaredTotals: domain.ControlDeclaredTotals{RowCount: 2, Totals: map[string]string{"USD": "1300.50"}},
	}
	az := &cpRecordingAuthZ{}
	r := newRouterWithAuthz(s, az)
	resp := doRequest(r, http.MethodGet, cpURL("&period_id=2026-02&limit=1"), nil, "svc-1")
	if resp.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", resp.Code, resp.Body.String())
	}

	var raw map[string]json.RawMessage
	if err := json.Unmarshal(resp.Body.Bytes(), &raw); err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"records", "next_cursor", "watermark", "declared_totals"} {
		if _, ok := raw[k]; !ok {
			t.Fatalf("response missing %q: %s", k, resp.Body.String())
		}
	}
	var body struct {
		Records []struct {
			RecordID   string            `json:"record_id"`
			Reference  string            `json:"reference"`
			Amount     string            `json:"amount"`
			Currency   string            `json:"currency"`
			Date       string            `json:"date"`
			Attributes map[string]string `json:"attributes"`
		} `json:"records"`
		NextCursor     string `json:"next_cursor"`
		Watermark      string `json:"watermark"`
		DeclaredTotals struct {
			RowCount int64             `json:"row_count"`
			Totals   map[string]string `json:"totals"`
		} `json:"declared_totals"`
	}
	if err := json.Unmarshal(resp.Body.Bytes(), &body); err != nil {
		t.Fatalf("amounts must be JSON strings: %v", err)
	}
	if len(body.Records) != 1 || body.Records[0].Amount != "1250.50" || body.Records[0].Reference != "INV-1" ||
		body.Records[0].Date != "2026-09-01" || body.Records[0].Attributes["account_code"] != "1200" {
		t.Fatalf("unexpected records: %+v", body.Records)
	}
	if body.Watermark != "wm-1" || body.DeclaredTotals.RowCount != 2 || body.DeclaredTotals.Totals["USD"] != "1300.50" {
		t.Fatalf("unexpected watermark/totals: %+v", body)
	}
	dec, err := base64.RawURLEncoding.DecodeString(body.NextCursor)
	if err != nil || string(dec) != lastID {
		t.Fatalf("next_cursor should opaquely encode the last record_id, got %q", body.NextCursor)
	}

	if az.action != "GL_CONTROL_POPULATION_READ" || az.entity != cpEntity || az.principal != "svc-1" {
		t.Fatalf("unexpected authz call: %+v", az)
	}
	if len(s.postingsCalls) != 1 {
		t.Fatalf("expected one store call, got %d", len(s.postingsCalls))
	}
	c := s.postingsCalls[0]
	if c.tenantID != testTenantID || c.query.LegalEntityID != cpEntity || c.query.PeriodEnd != "2026-02-28" ||
		c.query.NormalBalance != "DEBIT" || c.query.Limit != 1 ||
		len(c.query.AccountCodes) != 2 || c.query.AccountCodes[0] != "1200" {
		t.Fatalf("unexpected store query: %+v", c)
	}

	// Follow the cursor: it must reach the store decoded.
	resp = doRequest(r, http.MethodGet, cpURL("&cursor="+body.NextCursor), nil, "svc-1")
	if resp.Code != http.StatusOK {
		t.Fatalf("expected 200 on cursor page, got %d: %s", resp.Code, resp.Body.String())
	}
	if got := s.postingsCalls[1].query.AfterRecordID; got != lastID {
		t.Fatalf("cursor not decoded to record id, got %q", got)
	}
}

func TestControlPopulation_EmptyPopulation_ReturnsEmptyArraysNotNull(t *testing.T) {
	r := newRouter(newStubStore(), &stubPublisher{}, &stubAuthZ{})
	resp := doRequest(r, http.MethodGet, cpURL(""), nil, "svc-1")
	if resp.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", resp.Code, resp.Body.String())
	}
	var raw map[string]json.RawMessage
	_ = json.Unmarshal(resp.Body.Bytes(), &raw)
	if string(raw["records"]) != "[]" {
		t.Fatalf("records should be [], got %s", raw["records"])
	}
	if string(raw["next_cursor"]) != `""` {
		t.Fatalf("next_cursor should be empty string, got %s", raw["next_cursor"])
	}
}

func newRouterWithAuthz(s *stubStore, a handler.AuthZClient) chi.Router {
	r := chi.NewRouter()
	r.Use(svcmiddleware.TenantContext())
	handler.RegisterRoutes(r, handler.New(s, &stubPublisher{}, a, &stubClose{}, zap.NewNop()))
	return r
}
