package handler_test

import (
	"context"
	"errors"
	"net/http"
	"testing"

	"zoiko.io/general-ledger-svc/internal/domain"
)

const psURL = w5Base + "post-seal-entries"

func psQ(extra string) string { return psURL + "?legal_entity_id=" + cpEntity + extra }

func (s *stubStore) QueryPostSealEntries(_ context.Context, t string, q domain.PostSealEntriesQuery) (*domain.ControlPopulationPage, error) {
	s.wave5.postSeal = append(s.wave5.postSeal, q)
	s.wave5.tenantSeen = append(s.wave5.tenantSeen, t)
	if s.wave5.err != nil {
		return nil, s.wave5.err
	}
	if s.wave5.page != nil {
		return s.wave5.page, nil
	}
	return &domain.ControlPopulationPage{}, nil
}

func TestPostSealEntries_BadRequests(t *testing.T) {
	cases := map[string]string{
		"period_id rejected": psQ("&period_id=2026-09"),
		"fiscal_period":      psQ("&fiscal_period=2026-09"),
		"unknown param":      psQ("&bogus=1"),
		"repeated limit":     psQ("&limit=1&limit=2"),
		"missing entity":     psURL,
		"bad entity":         psURL + "?legal_entity_id=x",
		"bad limit":          psQ("&limit=5001"),
		"bad cursor":         psQ("&cursor=***"),
	}
	for name, url := range cases {
		t.Run(name, func(t *testing.T) {
			s := newStubStore()
			resp := doRequest(newRouter(s, &stubPublisher{}, &stubAuthZ{}), http.MethodGet, url, nil, "svc-1")
			if resp.Code != http.StatusBadRequest {
				t.Fatalf("expected 400, got %d: %s", resp.Code, resp.Body.String())
			}
			if len(s.wave5.postSeal) != 0 {
				t.Fatal("store must not be queried for an invalid request")
			}
		})
	}
}

func TestPostSealEntries_ScopeAuthorizationAndFailClosed(t *testing.T) {
	s := newStubStore()
	az := &cpRecordingAuthZ{}
	if resp := doRequest(newRouterWithAuthz(s, az), http.MethodGet, psQ("&limit=7"), nil, "svc-1"); resp.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", resp.Code, resp.Body.String())
	}
	if q := s.wave5.postSeal[0]; q.LegalEntityID != cpEntity || q.Limit != 7 || q.AfterRecordID != "" {
		t.Fatalf("unexpected query %+v", q)
	}
	if az.action != "GL_CONTROL_POPULATION_READ" || az.entity != cpEntity || az.principal != "svc-1" {
		t.Fatalf("unexpected authz call %+v", az)
	}
	if len(s.wave5.tenantSeen) != 1 || s.wave5.tenantSeen[0] != testTenantID {
		t.Fatalf("expected one tenant-scoped store call, got %v", s.wave5.tenantSeen)
	}

	r := newRouter(newStubStore(), &stubPublisher{}, &stubAuthZ{})
	if got := doRequestAs(r, http.MethodGet, psQ(""), nil, "svc-1", "").Code; got != http.StatusUnauthorized {
		t.Fatalf("missing tenant: %d", got)
	}
	if got := doRequest(r, http.MethodGet, psQ(""), nil, "").Code; got != http.StatusUnauthorized {
		t.Fatalf("missing principal: %d", got)
	}
	s2 := newStubStore()
	if got := doRequest(newRouter(s2, &stubPublisher{}, &stubAuthZ{err: domain.ErrAuthorizationDenied}), http.MethodGet, psQ(""), nil, "svc-1").Code; got != http.StatusForbidden {
		t.Fatalf("denied: %d", got)
	}
	if got := doRequest(newRouter(s2, &stubPublisher{}, &stubAuthZ{err: errors.New("authz down")}), http.MethodGet, psQ(""), nil, "svc-1").Code; got != http.StatusServiceUnavailable {
		t.Fatalf("authz outage: %d", got)
	}
	if len(s2.wave5.postSeal) != 0 {
		t.Fatal("store must not be called when authorization fails")
	}
	s3 := newStubStore()
	s3.wave5.err = domain.ErrControlPopulationTooLarge
	if got := doRequest(newRouter(s3, &stubPublisher{}, &stubAuthZ{}), http.MethodGet, psQ(""), nil, "svc-1").Code; got != http.StatusUnprocessableEntity {
		t.Fatalf("over the cap: %d", got)
	}
	s3.wave5.err = errors.New("db down")
	if got := doRequest(newRouter(s3, &stubPublisher{}, &stubAuthZ{}), http.MethodGet, psQ(""), nil, "svc-1").Code; got != http.StatusServiceUnavailable {
		t.Fatalf("store down: %d", got)
	}
}
