package handler_test

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"testing"

	"zoiko.io/accounts-payable-svc/internal/domain"
)

const popPath = "/v1/control-populations/open-invoices"
const recID = "44444444-4444-4444-4444-444444444444"

func popURL(extra string) string {
	return popPath + "?legal_entity_id=" + entityA + extra
}

func TestControlPopulation_MissingTenant_401(t *testing.T) {
	s := newStubStore()
	rec := doRequestAs(newRouter(s, &stubPublisher{}, &stubAuthZ{}), http.MethodGet, popURL(""), nil, "p1", "")
	if rec.Code != http.StatusUnauthorized || s.popCalls != 0 {
		t.Fatalf("got %d calls=%d", rec.Code, s.popCalls)
	}
}

func TestControlPopulation_MissingPrincipal_401(t *testing.T) {
	s := newStubStore()
	rec := doRequest(newRouter(s, &stubPublisher{}, &stubAuthZ{}), http.MethodGet, popURL(""), nil, "")
	if rec.Code != http.StatusUnauthorized || s.popCalls != 0 {
		t.Fatalf("got %d calls=%d", rec.Code, s.popCalls)
	}
}

func TestControlPopulation_BadRequests_400(t *testing.T) {
	cases := map[string]string{
		"missing legal entity": popPath,
		"invalid legal entity": popPath + "?legal_entity_id=nope",
		"unknown param":        popURL("&vendor=x"),
		"bad period":           popURL("&period_id=2026-13"),
		"bad period format":    popURL("&period_id=2026-9"),
		"bad limit":            popURL("&limit=0"),
		"bad cursor":           popURL("&cursor=!!!"),
		"non-uuid cursor":      popURL("&cursor=" + base64.RawURLEncoding.EncodeToString([]byte("abc"))),
	}
	for name, u := range cases {
		t.Run(name, func(t *testing.T) {
			s := newStubStore()
			rec := doRequest(newRouter(s, &stubPublisher{}, &stubAuthZ{}), http.MethodGet, u, nil, "p1")
			if rec.Code != http.StatusBadRequest || s.popCalls != 0 {
				t.Fatalf("got %d calls=%d body=%s", rec.Code, s.popCalls, rec.Body.String())
			}
		})
	}
}

func TestControlPopulation_UnknownPopulation_404(t *testing.T) {
	s := newStubStore()
	rec := doRequest(newRouter(s, &stubPublisher{}, &stubAuthZ{}), http.MethodGet,
		"/v1/control-populations/nope?legal_entity_id="+entityA, nil, "p1")
	if rec.Code != http.StatusNotFound || s.popCalls != 0 {
		t.Fatalf("got %d", rec.Code)
	}
}

func TestControlPopulation_AuthzDenied_403(t *testing.T) {
	s := newStubStore()
	rec := doRequest(newRouter(s, &stubPublisher{}, &stubAuthZ{err: domain.ErrAuthorizationDenied}), http.MethodGet, popURL(""), nil, "p1")
	if rec.Code != http.StatusForbidden || s.popCalls != 0 {
		t.Fatalf("got %d calls=%d", rec.Code, s.popCalls)
	}
}

func TestControlPopulation_AuthzUnavailable_503_FailClosed(t *testing.T) {
	s := newStubStore()
	rec := doRequest(newRouter(s, &stubPublisher{}, &stubAuthZ{err: domain.ErrAuthorizationServiceUnavailable}), http.MethodGet, popURL(""), nil, "p1")
	if rec.Code != http.StatusServiceUnavailable || s.popCalls != 0 {
		t.Fatalf("got %d calls=%d", rec.Code, s.popCalls)
	}
}

func TestControlPopulation_TooLarge_422(t *testing.T) {
	s := newStubStore()
	s.popErr = domain.ErrPopulationTooLarge
	rec := doRequest(newRouter(s, &stubPublisher{}, &stubAuthZ{}), http.MethodGet, popURL(""), nil, "p1")
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("got %d", rec.Code)
	}
}

func TestControlPopulation_StoreError_503(t *testing.T) {
	s := newStubStore()
	s.popErr = errors.New("db down")
	rec := doRequest(newRouter(s, &stubPublisher{}, &stubAuthZ{}), http.MethodGet, popURL(""), nil, "p1")
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("got %d", rec.Code)
	}
}

func TestControlPopulation_HappyPath_Shape(t *testing.T) {
	s := newStubStore()
	s.popPage = &domain.ControlPopulationPage{
		Records: []domain.ControlRecord{{
			RecordID: recID, Reference: "INV-1", Amount: "1250.50", Currency: "USD", Date: "2026-09-01",
			Attributes: map[string]string{"status": "RECEIVED", "vendor_id": "v1", "due_date": "2026-10-01"},
		}},
		NextRecordID:   recID,
		Watermark:      "1:abc",
		DeclaredTotals: domain.DeclaredTotals{RowCount: 3, Totals: map[string]string{"USD": "3751.50"}},
	}
	cursor := base64.RawURLEncoding.EncodeToString([]byte(recID))
	rec := doRequest(newRouter(s, &stubPublisher{}, &stubAuthZ{}), http.MethodGet,
		popURL("&period_id=2026-02&limit=1&cursor="+cursor), nil, "p1")
	if rec.Code != http.StatusOK {
		t.Fatalf("got %d body=%s", rec.Code, rec.Body.String())
	}
	if s.popQuery.TenantID != tenantA || s.popQuery.LegalEntityID != entityA ||
		s.popQuery.PeriodEnd != "2026-02-28" || s.popQuery.Limit != 1 || s.popQuery.AfterRecordID != recID {
		t.Fatalf("unexpected query %+v", s.popQuery)
	}
	var body struct {
		Records []struct {
			RecordID   string            `json:"record_id"`
			Reference  string            `json:"reference"`
			Amount     any               `json:"amount"`
			Currency   string            `json:"currency"`
			Date       string            `json:"date"`
			Attributes map[string]string `json:"attributes"`
		} `json:"records"`
		NextCursor     string `json:"next_cursor"`
		Watermark      string `json:"watermark"`
		DeclaredTotals struct {
			RowCount int               `json:"row_count"`
			Totals   map[string]string `json:"totals"`
		} `json:"declared_totals"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if len(body.Records) != 1 || body.Records[0].Amount != "1250.50" || body.Records[0].Attributes["vendor_id"] != "v1" {
		t.Fatalf("bad records %+v", body.Records)
	}
	if body.NextCursor != cursor || body.Watermark != "1:abc" ||
		body.DeclaredTotals.RowCount != 3 || body.DeclaredTotals.Totals["USD"] != "3751.50" {
		t.Fatalf("bad envelope %+v", body)
	}
}

func TestControlPopulation_LastPage_EmptyCursor(t *testing.T) {
	s := newStubStore()
	rec := doRequest(newRouter(s, &stubPublisher{}, &stubAuthZ{}), http.MethodGet, popURL(""), nil, "p1")
	var raw map[string]json.RawMessage
	_ = json.Unmarshal(rec.Body.Bytes(), &raw)
	if string(raw["next_cursor"]) != `""` || string(raw["records"]) != `[]` {
		t.Fatalf("body=%s", rec.Body.String())
	}
}
