package handler_test

import (
	"context"
	"errors"
	"net/http"
	"testing"

	"zoiko.io/general-ledger-svc/internal/domain"
)

const fxURL = w5Base + "fx-rate-observations"

func fxQ(extra string) string {
	return fxURL + "?legal_entity_id=" + cpEntity + "&period_id=2026-09&rate_type=SPOT" + extra
}

func (s *stubStore) QueryFXRateObservations(_ context.Context, t string, q domain.FXRateObservationsQuery) (*domain.ControlPopulationPage, error) {
	s.wave5.fxRates = append(s.wave5.fxRates, q)
	s.wave5.tenantSeen = append(s.wave5.tenantSeen, t)
	if s.wave5.err != nil {
		return nil, s.wave5.err
	}
	if s.wave5.page != nil {
		return s.wave5.page, nil
	}
	return &domain.ControlPopulationPage{}, nil
}

func TestFXRateObservations_BadRequests(t *testing.T) {
	cases := map[string]string{
		"missing period":     fxURL + "?legal_entity_id=" + cpEntity + "&rate_type=SPOT",
		"bad period":         fxURL + "?legal_entity_id=" + cpEntity + "&period_id=2026-13&rate_type=SPOT",
		"date as period":     fxURL + "?legal_entity_id=" + cpEntity + "&period_id=2026-09-01&rate_type=SPOT",
		"missing rate type":  fxURL + "?legal_entity_id=" + cpEntity + "&period_id=2026-09",
		"unknown rate type":  fxURL + "?legal_entity_id=" + cpEntity + "&period_id=2026-09&rate_type=MID",
		"lower-case type":    fxURL + "?legal_entity_id=" + cpEntity + "&period_id=2026-09&rate_type=spot",
		"unknown param":      fxQ("&bogus=1"),
		"fiscal_period":      fxQ("&fiscal_period=2026-09"),
		"repeated rate_type": fxQ("&rate_type=CLOSING"),
		"missing entity":     fxURL + "?period_id=2026-09&rate_type=SPOT",
		"bad entity":         fxURL + "?legal_entity_id=x&period_id=2026-09&rate_type=SPOT",
		"bad limit":          fxQ("&limit=5001"),
		"bad cursor":         fxQ("&cursor=***"),
	}
	for name, url := range cases {
		t.Run(name, func(t *testing.T) {
			s := newStubStore()
			resp := doRequest(newRouter(s, &stubPublisher{}, &stubAuthZ{}), http.MethodGet, url, nil, "svc-1")
			if resp.Code != http.StatusBadRequest {
				t.Fatalf("expected 400, got %d: %s", resp.Code, resp.Body.String())
			}
			if len(s.wave5.fxRates) != 0 {
				t.Fatal("store must not be queried for an invalid request")
			}
		})
	}
}

func TestFXRateObservations_ScopeAuthorizationAndFailClosed(t *testing.T) {
	s := newStubStore()
	az := &cpRecordingAuthZ{}
	if resp := doRequest(newRouterWithAuthz(s, az), http.MethodGet, fxQ("&limit=7"), nil, "svc-1"); resp.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", resp.Code, resp.Body.String())
	}
	if q := s.wave5.fxRates[0]; q.PeriodID != "2026-09" || q.RateType != "SPOT" || q.Limit != 7 || q.AfterRecordID != "" {
		t.Fatalf("unexpected query %+v", q)
	}
	if az.action != "GL_CONTROL_POPULATION_READ" || az.entity != cpEntity || az.principal != "svc-1" {
		t.Fatalf("unexpected authz call %+v", az)
	}
	if len(s.wave5.tenantSeen) != 1 || s.wave5.tenantSeen[0] != testTenantID {
		t.Fatalf("expected one tenant-scoped store call, got %v", s.wave5.tenantSeen)
	}

	r := newRouter(newStubStore(), &stubPublisher{}, &stubAuthZ{})
	if got := doRequestAs(r, http.MethodGet, fxQ(""), nil, "svc-1", "").Code; got != http.StatusUnauthorized {
		t.Fatalf("missing tenant: %d", got)
	}
	if got := doRequest(r, http.MethodGet, fxQ(""), nil, "").Code; got != http.StatusUnauthorized {
		t.Fatalf("missing principal: %d", got)
	}
	s2 := newStubStore()
	if got := doRequest(newRouter(s2, &stubPublisher{}, &stubAuthZ{err: domain.ErrAuthorizationDenied}), http.MethodGet, fxQ(""), nil, "svc-1").Code; got != http.StatusForbidden {
		t.Fatalf("denied: %d", got)
	}
	if got := doRequest(newRouter(s2, &stubPublisher{}, &stubAuthZ{err: errors.New("authz down")}), http.MethodGet, fxQ(""), nil, "svc-1").Code; got != http.StatusServiceUnavailable {
		t.Fatalf("authz outage: %d", got)
	}
	if len(s2.wave5.fxRates) != 0 {
		t.Fatal("store must not be called when authorization fails")
	}
	s3 := newStubStore()
	s3.wave5.err = domain.ErrControlPopulationTooLarge
	if got := doRequest(newRouter(s3, &stubPublisher{}, &stubAuthZ{}), http.MethodGet, fxQ(""), nil, "svc-1").Code; got != http.StatusUnprocessableEntity {
		t.Fatalf("over the cap: %d", got)
	}
	s3.wave5.err = errors.New("db down")
	if got := doRequest(newRouter(s3, &stubPublisher{}, &stubAuthZ{}), http.MethodGet, fxQ(""), nil, "svc-1").Code; got != http.StatusServiceUnavailable {
		t.Fatalf("store down: %d", got)
	}
}
