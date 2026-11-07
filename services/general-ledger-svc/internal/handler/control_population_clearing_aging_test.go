package handler_test

import (
	"context"
	"errors"
	"net/http"
	"testing"

	"zoiko.io/general-ledger-svc/internal/domain"
)

const caURL = w5Base + "clearing-balance-aging"

func caQ(extra string) string {
	return caURL + "?legal_entity_id=" + cpEntity + "&aged_before=2026-09-01" + extra
}

func (s *stubStore) QueryClearingBalanceAging(_ context.Context, t string, q domain.ClearingBalanceAgingQuery) (*domain.ControlPopulationPage, error) {
	s.wave5.aging = append(s.wave5.aging, q)
	s.wave5.tenantSeen = append(s.wave5.tenantSeen, t)
	if s.wave5.err != nil {
		return nil, s.wave5.err
	}
	if s.wave5.page != nil {
		return s.wave5.page, nil
	}
	return &domain.ControlPopulationPage{}, nil
}

func TestClearingBalanceAging_BadRequests(t *testing.T) {
	cases := map[string]string{
		"period_id rejected":   caQ("&period_id=2026-09"),
		"unknown param":        caQ("&bogus=1"),
		"created_before":       caQ("&created_before=2026-09-01"),
		"repeated aged_before": caQ("&aged_before=2026-09-02"),
		"missing entity":       caURL + "?aged_before=2026-09-01",
		"bad entity":           caURL + "?legal_entity_id=x&aged_before=2026-09-01",
		"missing aged_before":  caURL + "?legal_entity_id=" + cpEntity,
		"empty aged_before":    caURL + "?legal_entity_id=" + cpEntity + "&aged_before=",
		"garbage":              caURL + "?legal_entity_id=" + cpEntity + "&aged_before=yesterday",
		"bad month":            caURL + "?legal_entity_id=" + cpEntity + "&aged_before=2026-13-01",
		"timestamp not a date": caURL + "?legal_entity_id=" + cpEntity + "&aged_before=2026-09-01T00:00:00Z",
		"slash date":           caURL + "?legal_entity_id=" + cpEntity + "&aged_before=2026/09/01",
		"bad limit":            caQ("&limit=5001"),
		"bad cursor":           caQ("&cursor=***"),
	}
	for name, url := range cases {
		t.Run(name, func(t *testing.T) {
			s := newStubStore()
			r := newRouter(s, &stubPublisher{}, &stubAuthZ{})
			resp := doRequest(r, http.MethodGet, url, nil, "svc-1")
			if resp.Code != http.StatusBadRequest {
				t.Fatalf("expected 400, got %d: %s", resp.Code, resp.Body.String())
			}
			if len(s.wave5.aging) != 0 {
				t.Fatal("store must not be queried for an invalid request")
			}
		})
	}
}

func TestClearingBalanceAging_HappyPathPassesScopeAndFailsClosed(t *testing.T) {
	s := newStubStore()
	s.wave5.page = &domain.ControlPopulationPage{
		Records: []domain.ControlPopulationRecord{{RecordID: "CLR-1:USD:", Reference: "CLR-1", Amount: "40.00", Currency: "USD",
			Date: "2026-08-15", Attributes: map[string]string{"aged_amount": "40.00"}}},
		Watermark:      "gl9:n=1;entry_seq=3;md5=x",
		DeclaredTotals: domain.ControlDeclaredTotals{RowCount: 1, Totals: map[string]string{"USD": "40.00"}},
	}
	az := &cpRecordingAuthZ{}
	resp := doRequest(newRouterWithAuthz(s, az), http.MethodGet, caQ("&limit=7"), nil, "svc-1")
	if resp.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", resp.Code, resp.Body.String())
	}
	if q := s.wave5.aging[0]; q.LegalEntityID != cpEntity || q.AgedBefore != "2026-09-01" || q.Limit != 7 {
		t.Fatalf("unexpected query %+v", q)
	}
	if az.action != "GL_CONTROL_POPULATION_READ" || az.entity != cpEntity || az.principal != "svc-1" {
		t.Fatalf("unexpected authz call %+v", az)
	}
	if len(s.wave5.tenantSeen) != 1 || s.wave5.tenantSeen[0] != testTenantID {
		t.Fatalf("expected one tenant-scoped store call, got %v", s.wave5.tenantSeen)
	}

	// 401 without tenant or principal, 403 / 503 on authorization, 422 over the cap, 503 on store failure.
	r := newRouter(newStubStore(), &stubPublisher{}, &stubAuthZ{})
	if got := doRequestAs(r, http.MethodGet, caQ(""), nil, "svc-1", "").Code; got != http.StatusUnauthorized {
		t.Fatalf("missing tenant: %d", got)
	}
	if got := doRequest(r, http.MethodGet, caQ(""), nil, "").Code; got != http.StatusUnauthorized {
		t.Fatalf("missing principal: %d", got)
	}
	s2 := newStubStore()
	if got := doRequest(newRouter(s2, &stubPublisher{}, &stubAuthZ{err: domain.ErrAuthorizationDenied}), http.MethodGet, caQ(""), nil, "svc-1").Code; got != http.StatusForbidden {
		t.Fatalf("denied: %d", got)
	}
	if got := doRequest(newRouter(s2, &stubPublisher{}, &stubAuthZ{err: errors.New("authz down")}), http.MethodGet, caQ(""), nil, "svc-1").Code; got != http.StatusServiceUnavailable {
		t.Fatalf("authz outage: %d", got)
	}
	if len(s2.wave5.aging) != 0 {
		t.Fatal("store must not be called when authorization fails")
	}
	s3 := newStubStore()
	s3.wave5.err = domain.ErrControlPopulationTooLarge
	if got := doRequest(newRouter(s3, &stubPublisher{}, &stubAuthZ{}), http.MethodGet, caQ(""), nil, "svc-1").Code; got != http.StatusUnprocessableEntity {
		t.Fatalf("over the cap: %d", got)
	}
	s3.wave5.err = errors.New("db down")
	if got := doRequest(newRouter(s3, &stubPublisher{}, &stubAuthZ{}), http.MethodGet, caQ(""), nil, "svc-1").Code; got != http.StatusServiceUnavailable {
		t.Fatalf("store down: %d", got)
	}
}
