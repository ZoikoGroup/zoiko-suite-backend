package handler_test

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"go.uber.org/zap"

	"zoiko.io/data-ingestion-svc/internal/authz"
	"zoiko.io/data-ingestion-svc/internal/domain"
	"zoiko.io/data-ingestion-svc/internal/handler"
	"zoiko.io/data-ingestion-svc/internal/store"
)

const (
	tieTenant = "tenant-tie-1"
	tieEntity = "le-tie-1"
	tieBatch  = "imb_6f1c1c0e-0000-4000-8000-000000000001"
	tiePath   = "/v1/control-populations/import-batch-tieout"
)

// tieStore overrides only the control-population method; any other Store method panics (nil embedded interface),
// which would mean the handler wandered outside its contract.
type tieStore struct {
	store.Store
	tenant string
	query  domain.ImportBatchTieoutQuery
	page   *domain.ControlPopulationPage
	err    error
	calls  int
}

func (s *tieStore) QueryImportBatchTieout(_ context.Context, tenantID string, q domain.ImportBatchTieoutQuery) (*domain.ControlPopulationPage, error) {
	s.calls++
	s.tenant, s.query = tenantID, q
	if s.err != nil {
		return nil, s.err
	}
	if s.page != nil {
		return s.page, nil
	}
	return &domain.ControlPopulationPage{Watermark: "di1:n=1;md5=x"}, nil
}

// fakeAuthz answers GRANTED, DENIED or a 500 outage, and records the last question.
type fakeAuthz struct {
	srv          *httptest.Server
	deny, outage bool
	asked        map[string]string
}

func newFakeAuthz(t *testing.T) *fakeAuthz {
	f := &fakeAuthz{asked: map[string]string{}}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&f.asked)
		if f.outage {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		out := "GRANTED"
		if f.deny {
			out = "DENIED"
		}
		_ = json.NewEncoder(w).Encode(map[string]string{"decision_outcome": out})
	}))
	t.Cleanup(f.srv.Close)
	return f
}

func tieGet(t *testing.T, s *tieStore, az *fakeAuthz, path, tenant, principal string) *httptest.ResponseRecorder {
	t.Helper()
	h := handler.NewRouter(handler.NewHandler(s, authz.NewClient(az.srv.URL, zap.NewNop()), zap.NewNop()))
	req := httptest.NewRequest(http.MethodGet, path, nil)
	if tenant != "" {
		req.Header.Set("X-Tenant-Id", tenant)
	}
	if principal != "" {
		req.Header.Set("X-Principal-Id", principal)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func TestControlPopulation_ImportBatchTieout_HappyPath(t *testing.T) {
	s := &tieStore{page: &domain.ControlPopulationPage{
		Records:        []domain.ControlPopulationRecord{{RecordID: tieBatch, Reference: tieBatch, Amount: "1234.50", Currency: "XXX", Date: "2026-09-01", Attributes: map[string]string{"row_count": "10"}}},
		Watermark:      "di1:n=1;md5=abc",
		DeclaredTotals: domain.ControlDeclaredTotals{RowCount: 1, Totals: map[string]string{"XXX": "1234.50"}},
	}}
	az := newFakeAuthz(t)
	rec := tieGet(t, s, az, tiePath+"?legal_entity_id="+tieEntity+"&batch_id="+tieBatch+"&limit=3", tieTenant, "p1")
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}
	var body struct {
		Records    []domain.ControlPopulationRecord `json:"records"`
		NextCursor string                           `json:"next_cursor"`
		Watermark  string                           `json:"watermark"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body.Watermark != "di1:n=1;md5=abc" || len(body.Records) != 1 || body.NextCursor != "" {
		t.Fatalf("unexpected body: %s", rec.Body.String())
	}
	if s.tenant != tieTenant || s.query != (domain.ImportBatchTieoutQuery{LegalEntityID: tieEntity, BatchID: tieBatch}) {
		t.Fatalf("store got tenant=%q query=%+v", s.tenant, s.query)
	}
	if az.asked["action_type"] != "DATA_INGESTION_CONTROL_POPULATION_READ" || az.asked["legal_entity_id"] != tieEntity || az.asked["principal_id"] != "p1" {
		t.Fatalf("authz asked %+v", az.asked)
	}

	// a cursor round-trips as the decoded record id
	cur := base64.RawURLEncoding.EncodeToString([]byte(tieBatch))
	if got := tieGet(t, s, az, tiePath+"?legal_entity_id="+tieEntity+"&batch_id="+tieBatch+"&cursor="+cur, tieTenant, "p1").Code; got != http.StatusOK {
		t.Fatalf("cursor request: %d", got)
	}
	if s.query.AfterRecordID != tieBatch {
		t.Fatalf("cursor not decoded: %q", s.query.AfterRecordID)
	}
}

func TestControlPopulation_ImportBatchTieout_RejectsMalformedRequests(t *testing.T) {
	ok := "?legal_entity_id=" + tieEntity + "&batch_id=" + tieBatch
	cases := map[string]struct {
		path, tenant, principal string
		want                    int
	}{
		"no tenant":          {tiePath + ok, "", "p1", http.StatusUnauthorized},
		"no principal":       {tiePath + ok, tieTenant, "", http.StatusUnauthorized},
		"missing entity":     {tiePath + "?batch_id=" + tieBatch, tieTenant, "p1", http.StatusBadRequest},
		"bad entity":         {tiePath + "?legal_entity_id=-x&batch_id=" + tieBatch, tieTenant, "p1", http.StatusBadRequest},
		"missing batch":      {tiePath + "?legal_entity_id=" + tieEntity, tieTenant, "p1", http.StatusBadRequest},
		"batch not imb_":     {tiePath + "?legal_entity_id=" + tieEntity + "&batch_id=123", tieTenant, "p1", http.StatusBadRequest},
		"batch upper-case":   {tiePath + "?legal_entity_id=" + tieEntity + "&batch_id=imb_6F1C1C0E-0000-4000-8000-000000000001", tieTenant, "p1", http.StatusBadRequest},
		"period_id rejected": {tiePath + ok + "&period_id=2026-09", tieTenant, "p1", http.StatusBadRequest},
		"unknown param":      {tiePath + ok + "&bogus=1", tieTenant, "p1", http.StatusBadRequest},
		"repeated param":     {tiePath + ok + "&batch_id=" + tieBatch, tieTenant, "p1", http.StatusBadRequest},
		"limit zero":         {tiePath + ok + "&limit=0", tieTenant, "p1", http.StatusBadRequest},
		"limit over max":     {tiePath + ok + "&limit=5001", tieTenant, "p1", http.StatusBadRequest},
		"cursor not base64":  {tiePath + ok + "&cursor=***", tieTenant, "p1", http.StatusBadRequest},
		"unknown population": {"/v1/control-populations/nope" + ok, tieTenant, "p1", http.StatusNotFound},
	}
	for name, c := range cases {
		s := &tieStore{}
		if got := tieGet(t, s, newFakeAuthz(t), c.path, c.tenant, c.principal).Code; got != c.want {
			t.Errorf("%s: status %d, want %d", name, got, c.want)
		}
		if s.calls != 0 {
			t.Errorf("%s: the store must not be consulted", name)
		}
	}
}

func TestControlPopulation_ImportBatchTieout_AuthorizationNotFoundAndStoreFailuresFailClosed(t *testing.T) {
	path := tiePath + "?legal_entity_id=" + tieEntity + "&batch_id=" + tieBatch

	s := &tieStore{}
	az := newFakeAuthz(t)
	az.deny = true
	if got := tieGet(t, s, az, path, tieTenant, "p1").Code; got != http.StatusForbidden {
		t.Errorf("denied caller: status %d", got)
	}
	if s.calls != 0 {
		t.Errorf("the store is never consulted for a denied caller")
	}

	s = &tieStore{}
	az = newFakeAuthz(t)
	az.outage = true
	if got := tieGet(t, s, az, path, tieTenant, "p1").Code; got == http.StatusOK || s.calls != 0 {
		t.Errorf("an authorization outage never serves data: status %d, store calls %d", got, s.calls)
	}

	s = &tieStore{err: domain.ErrImportBatchNotFound}
	if got := tieGet(t, s, newFakeAuthz(t), path, tieTenant, "p1").Code; got != http.StatusNotFound {
		t.Errorf("unknown batch: status %d", got)
	}
	s = &tieStore{err: errors.New("db down")}
	if got := tieGet(t, s, newFakeAuthz(t), path, tieTenant, "p1").Code; got != http.StatusServiceUnavailable {
		t.Errorf("store failure: status %d", got)
	}
}
