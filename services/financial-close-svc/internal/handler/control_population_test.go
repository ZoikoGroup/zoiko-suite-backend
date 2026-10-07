package handler_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/go-chi/chi/v5"
	"go.uber.org/zap"

	"zoiko.io/financial-close-svc/internal/domain"
	"zoiko.io/financial-close-svc/internal/handler"
	"zoiko.io/financial-close-svc/internal/middleware"
)

const cpBatchID = "0b1c2d3e-0000-4000-8000-000000000001"

type cpStore struct {
	*stubStore
	page  *domain.ControlPopulationPage
	err   error
	calls int
	last  domain.MigrationBatchTieoutQuery
}

func (s *cpStore) MigrationBatchTieout(_ context.Context, q domain.MigrationBatchTieoutQuery) (*domain.ControlPopulationPage, error) {
	s.calls++
	s.last = q
	return s.page, s.err
}

func cpServe(t *testing.T, st handler.Store, authzErr error, rawQuery string, tenant, principal string) *httptest.ResponseRecorder {
	t.Helper()
	h := handler.New(st, &stubPublisher{}, &stubAuthZ{err: authzErr}, &stubClients{}, []byte("k"), zap.NewNop())
	r := chi.NewRouter()
	r.Use(middleware.TenantContext())
	handler.RegisterRoutes(r, h)
	req := httptest.NewRequest(http.MethodGet, "/v1/control-populations/migration-batch-tieout?"+rawQuery, nil)
	if tenant != "" {
		req.Header.Set("X-Tenant-Id", tenant)
	}
	if principal != "" {
		req.Header.Set("X-Principal-Id", principal)
	}
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	return rec
}

func cpOKPage() *domain.ControlPopulationPage {
	return &domain.ControlPopulationPage{
		Records: []domain.ControlRecord{{
			RecordID: cpBatchID, Reference: cpBatchID, Amount: "0.30", Currency: "XXX", Date: "2026-09-01",
			Attributes: map[string]string{"expected_total_debits": "0.30", "crosswalk_debits": "0.30"},
		}},
		Watermark:      "fc1:n=1;md5=abc",
		DeclaredTotals: domain.DeclaredTotals{RowCount: 1, Totals: map[string]string{"XXX": "0.30"}},
	}
}

func TestMigrationBatchTieout_Validation(t *testing.T) {
	base := "legal_entity_id=le-1&batch_id=" + cpBatchID
	cases := []struct {
		name, q string
		want    int
	}{
		{"ok", base, 200},
		{"period_id rejected", base + "&period_id=2026-09", 400},
		{"unknown param", base + "&foo=bar", 400},
		{"repeated batch_id", base + "&batch_id=" + cpBatchID, 400},
		{"repeated limit", base + "&limit=1&limit=2", 400},
		{"missing batch_id", "legal_entity_id=le-1", 400},
		{"bad batch_id", "legal_entity_id=le-1&batch_id=nope", 400},
		{"upper-case batch_id", "legal_entity_id=le-1&batch_id=0B1C2D3E-0000-4000-8000-000000000001", 400},
		{"missing entity", "batch_id=" + cpBatchID, 400},
		{"bad entity", "legal_entity_id=%20x&batch_id=" + cpBatchID, 400},
		{"limit zero", base + "&limit=0", 400},
		{"limit too big", base + "&limit=5001", 400},
		{"limit max", base + "&limit=5000", 200},
		{"bad cursor", base + "&cursor=!!!", 400},
		{"cursor not uuid", base + "&cursor=YWJj", 400},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			st := &cpStore{stubStore: &stubStore{}, page: cpOKPage()}
			rec := cpServe(t, st, nil, c.q, "t1", "p1")
			if rec.Code != c.want {
				t.Fatalf("status = %d, want %d: %s", rec.Code, c.want, rec.Body.String())
			}
			if c.want == 400 && st.calls != 0 {
				t.Fatal("store must not be reached on a rejected request")
			}
		})
	}
}

func TestMigrationBatchTieout_AuthAndErrors(t *testing.T) {
	q := "legal_entity_id=le-1&batch_id=" + cpBatchID
	t.Run("no tenant 401", func(t *testing.T) {
		if rec := cpServe(t, &cpStore{stubStore: &stubStore{}}, nil, q, "", "p1"); rec.Code != 401 {
			t.Fatalf("got %d", rec.Code)
		}
	})
	t.Run("no principal 401", func(t *testing.T) {
		if rec := cpServe(t, &cpStore{stubStore: &stubStore{}}, nil, q, "t1", ""); rec.Code != 401 {
			t.Fatalf("got %d", rec.Code)
		}
	})
	t.Run("deny 403, store untouched", func(t *testing.T) {
		st := &cpStore{stubStore: &stubStore{}, page: cpOKPage()}
		if rec := cpServe(t, st, domain.ErrAuthorizationDenied, q, "t1", "p1"); rec.Code != 403 || st.calls != 0 {
			t.Fatalf("got %d calls=%d", rec.Code, st.calls)
		}
	})
	t.Run("authz outage 503 fail closed", func(t *testing.T) {
		st := &cpStore{stubStore: &stubStore{}, page: cpOKPage()}
		if rec := cpServe(t, st, errors.New("down"), q, "t1", "p1"); rec.Code != 503 || st.calls != 0 {
			t.Fatalf("got %d calls=%d", rec.Code, st.calls)
		}
	})
	t.Run("unknown batch 404", func(t *testing.T) {
		st := &cpStore{stubStore: &stubStore{}, err: domain.ErrMigrationBatchNotFound}
		if rec := cpServe(t, st, nil, q, "t1", "p1"); rec.Code != 404 {
			t.Fatalf("got %d", rec.Code)
		}
	})
	t.Run("store error 503", func(t *testing.T) {
		st := &cpStore{stubStore: &stubStore{}, err: errors.New("boom")}
		if rec := cpServe(t, st, nil, q, "t1", "p1"); rec.Code != 503 {
			t.Fatalf("got %d", rec.Code)
		}
	})
	t.Run("store without population support 503", func(t *testing.T) {
		if rec := cpServe(t, &stubStore{}, nil, q, "t1", "p1"); rec.Code != 503 {
			t.Fatalf("got %d", rec.Code)
		}
	})
}

func TestMigrationBatchTieout_HappyPath(t *testing.T) {
	st := &cpStore{stubStore: &stubStore{}, page: cpOKPage()}
	rec := cpServe(t, st, nil, "legal_entity_id=le-1&batch_id="+cpBatchID, "t1", "p1")
	if rec.Code != 200 {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}
	if st.last.TenantID != "t1" || st.last.LegalEntityID != "le-1" || st.last.BatchID != cpBatchID || st.last.Limit != 1000 {
		t.Fatalf("query = %+v", st.last)
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(rec.Body.Bytes(), &raw); err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"records", "next_cursor", "watermark", "declared_totals"} {
		if _, ok := raw[k]; !ok {
			t.Fatalf("missing %q in %s", k, rec.Body.String())
		}
	}
	var got domain.ControlPopulationPage
	_ = json.Unmarshal(rec.Body.Bytes(), &got)
	if len(got.Records) != 1 || got.Records[0].Amount != "0.30" || got.DeclaredTotals.Totals["XXX"] != "0.30" || got.Watermark == "" {
		t.Fatalf("body = %s", rec.Body.String())
	}
}
