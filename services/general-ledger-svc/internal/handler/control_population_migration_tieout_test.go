package handler_test

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"

	"zoiko.io/general-ledger-svc/internal/domain"
)

const mbtURL = w5Base + "migration-batch-tieout"

const mbtBatchID = "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee"

func mbtQ(extra string) string {
	return mbtURL + "?legal_entity_id=" + cpEntity + "&batch_id=" + mbtBatchID + extra
}

func (s *stubStore) QueryMigrationBatchTieout(_ context.Context, t string, q domain.MigrationBatchTieoutQuery) (*domain.ControlPopulationPage, error) {
	s.wave5.tieout = append(s.wave5.tieout, q)
	s.wave5.tenantSeen = append(s.wave5.tenantSeen, t)
	if s.wave5.err != nil {
		return nil, s.wave5.err
	}
	if s.wave5.page != nil {
		return s.wave5.page, nil
	}
	return &domain.ControlPopulationPage{}, nil
}

func TestMigrationBatchTieout_BadRequests(t *testing.T) {
	upperBatchID := strings.ToUpper(mbtBatchID)
	cases := map[string]string{
		"period_id rejected":  mbtQ("&period_id=2026-09"),
		"unknown param":       mbtQ("&bogus=1"),
		"repeated limit":      mbtQ("&limit=1&limit=2"),
		"missing entity":      w5Base + "migration-batch-tieout?batch_id=" + mbtBatchID,
		"bad entity":          w5Base + "migration-batch-tieout?legal_entity_id=x&batch_id=" + mbtBatchID,
		"missing batch_id":    w5Base + "migration-batch-tieout?legal_entity_id=" + cpEntity,
		"upper-case batch_id": w5Base + "migration-batch-tieout?legal_entity_id=" + cpEntity + "&batch_id=" + upperBatchID,
		"not-a-uuid batch_id": w5Base + "migration-batch-tieout?legal_entity_id=" + cpEntity + "&batch_id=not-a-uuid",
		"bad limit":           mbtQ("&limit=5001"),
		"bad cursor":          mbtQ("&cursor=***"),
	}
	for name, url := range cases {
		t.Run(name, func(t *testing.T) {
			s := newStubStore()
			resp := doRequest(newRouter(s, &stubPublisher{}, &stubAuthZ{}), http.MethodGet, url, nil, "svc-1")
			if resp.Code != http.StatusBadRequest {
				t.Fatalf("expected 400, got %d: %s", resp.Code, resp.Body.String())
			}
			if len(s.wave5.tieout) != 0 {
				t.Fatal("store must not be queried for an invalid request")
			}
		})
	}
}

func TestMigrationBatchTieout_ScopeAuthorizationAndFailClosed(t *testing.T) {
	s := newStubStore()
	az := &cpRecordingAuthZ{}
	if resp := doRequest(newRouterWithAuthz(s, az), http.MethodGet, mbtQ("&limit=7"), nil, "svc-1"); resp.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", resp.Code, resp.Body.String())
	}
	if q := s.wave5.tieout[0]; q.LegalEntityID != cpEntity || q.BatchID != mbtBatchID || q.Limit != 7 || q.AfterRecordID != "" {
		t.Fatalf("unexpected query %+v", q)
	}
	if az.action != "GL_CONTROL_POPULATION_READ" || az.entity != cpEntity || az.principal != "svc-1" {
		t.Fatalf("unexpected authz call %+v", az)
	}
	if len(s.wave5.tenantSeen) != 1 || s.wave5.tenantSeen[0] != testTenantID {
		t.Fatalf("expected one tenant-scoped store call, got %v", s.wave5.tenantSeen)
	}

	r := newRouter(newStubStore(), &stubPublisher{}, &stubAuthZ{})
	if got := doRequestAs(r, http.MethodGet, mbtQ(""), nil, "svc-1", "").Code; got != http.StatusUnauthorized {
		t.Fatalf("missing tenant: %d", got)
	}
	if got := doRequest(r, http.MethodGet, mbtQ(""), nil, "").Code; got != http.StatusUnauthorized {
		t.Fatalf("missing principal: %d", got)
	}
	s2 := newStubStore()
	if got := doRequest(newRouter(s2, &stubPublisher{}, &stubAuthZ{err: domain.ErrAuthorizationDenied}), http.MethodGet, mbtQ(""), nil, "svc-1").Code; got != http.StatusForbidden {
		t.Fatalf("denied: %d", got)
	}
	if got := doRequest(newRouter(s2, &stubPublisher{}, &stubAuthZ{err: errors.New("authz down")}), http.MethodGet, mbtQ(""), nil, "svc-1").Code; got != http.StatusServiceUnavailable {
		t.Fatalf("authz outage: %d", got)
	}
	if len(s2.wave5.tieout) != 0 {
		t.Fatal("store must not be called when authorization fails")
	}
	s3 := newStubStore()
	s3.wave5.err = domain.ErrMigrationBatchNotFound
	if got := doRequest(newRouter(s3, &stubPublisher{}, &stubAuthZ{}), http.MethodGet, mbtQ(""), nil, "svc-1").Code; got != http.StatusNotFound {
		t.Fatalf("unknown batch: %d", got)
	}
	s3.wave5.err = errors.New("db down")
	if got := doRequest(newRouter(s3, &stubPublisher{}, &stubAuthZ{}), http.MethodGet, mbtQ(""), nil, "svc-1").Code; got != http.StatusServiceUnavailable {
		t.Fatalf("store down: %d", got)
	}
}
