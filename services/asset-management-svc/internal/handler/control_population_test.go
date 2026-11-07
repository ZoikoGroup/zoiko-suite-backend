package handler_test

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/go-chi/chi/v5"
	"go.uber.org/zap"

	"zoiko.io/asset-management-svc/internal/domain"
	"zoiko.io/asset-management-svc/internal/handler"
	"zoiko.io/asset-management-svc/internal/middleware"
)

const (
	popPath   = "/v1/control-populations/depreciation-run-postings"
	popEntity = "le-pop-1"
	popRecord = "5e7e0000-0000-4000-8000-000000000001:IT"
)

// KernelCurrency is the currency the stub ledger posts depreciation in on the kernel path.
func (l *stubLedger) KernelCurrency() string { return "GBP" }

// popRecorder records what the endpoint hands the store.
type popRecorder struct {
	tenant string
	query  domain.DepreciationRunPostingsQuery
	page   *domain.ControlPopulationPage
	err    error
	calls  int
}

var popStubs = map[*stubStore]*popRecorder{}

func (s *stubStore) rec() *popRecorder {
	if popStubs[s] == nil {
		popStubs[s] = &popRecorder{}
	}
	return popStubs[s]
}

func (s *stubStore) QueryDepreciationRunPostings(_ context.Context, tenantID string, q domain.DepreciationRunPostingsQuery) (*domain.ControlPopulationPage, error) {
	p := s.rec()
	p.calls++
	p.tenant, p.query = tenantID, q
	if p.err != nil {
		return nil, p.err
	}
	if p.page != nil {
		return p.page, nil
	}
	return &domain.ControlPopulationPage{Watermark: "am1:n=0;md5=x"}, nil
}

func popGet(s *stubStore, az *stubAuthZ, ledger *stubLedger, path, tenant, principal string) *httptest.ResponseRecorder {
	r := chi.NewRouter()
	r.Use(func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			if tenant != "" {
				req = req.WithContext(middleware.WithTenant(req.Context(), tenant))
			}
			next.ServeHTTP(w, req)
		})
	})
	h := handler.New(s, &stubPublisher{}, az, zap.NewNop()).WithLedgerClient(ledger)
	handler.RegisterRoutes(r, h)
	req := httptest.NewRequest(http.MethodGet, path, nil)
	if principal != "" {
		req.Header.Set("X-Principal-Id", principal)
	}
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	return rec
}

func TestControlPopulation_DepreciationRunPostings_HappyPath(t *testing.T) {
	s := newStubStore()
	s.rec().page = &domain.ControlPopulationPage{
		Records: []domain.ControlPopulationRecord{{RecordID: popRecord, Reference: "ast-dep:5e7e0000-0000-4000-8000-000000000001:IT",
			Amount: "1000.00", Currency: "GBP", Date: "2026-01-31", Attributes: map[string]string{"asset_category": "IT"}}},
		NextRecordID: popRecord, Watermark: "am1:n=1;md5=x",
		DeclaredTotals: domain.ControlDeclaredTotals{RowCount: 1, Totals: map[string]string{"GBP": "1000.00"}},
	}
	rec := popGet(s, &stubAuthZ{}, &stubLedger{kernel: true}, popPath+"?legal_entity_id="+popEntity+"&period_id=2026-01&limit=7", "tenant-abc", "p1")
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
	if body.Watermark != "am1:n=1;md5=x" || len(body.Records) != 1 || body.Records[0].Currency != "GBP" {
		t.Fatalf("unexpected body: %s", rec.Body.String())
	}
	if want := base64.RawURLEncoding.EncodeToString([]byte(popRecord)); body.NextCursor != want {
		t.Fatalf("next_cursor %q, want %q", body.NextCursor, want)
	}
	p := s.rec()
	if p.tenant != "tenant-abc" || p.query.LegalEntityID != popEntity || p.query.Currency != "GBP" ||
		p.query.PeriodID != "2026-01" || p.query.Limit != 7 {
		t.Fatalf("store got tenant=%q query=%+v", p.tenant, p.query)
	}

	// no period means no cut-off; the cursor round-trips decoded
	if got := popGet(s, &stubAuthZ{}, &stubLedger{kernel: true}, popPath+"?legal_entity_id="+popEntity+"&cursor="+body.NextCursor, "tenant-abc", "p1").Code; got != http.StatusOK {
		t.Fatalf("cursor request: %d", got)
	}
	if p.query.PeriodID != "" || p.query.AfterRecordID != popRecord {
		t.Fatalf("query %+v", p.query)
	}
}

func TestControlPopulation_DepreciationRunPostings_RejectsMalformedRequests(t *testing.T) {
	ok := "?legal_entity_id=" + popEntity
	cases := map[string]struct {
		path, tenant, principal string
		want                    int
	}{
		"no tenant":          {popPath + ok, "", "p1", http.StatusUnauthorized},
		"no principal":       {popPath + ok, "tenant-abc", "", http.StatusUnauthorized},
		"missing entity":     {popPath, "tenant-abc", "p1", http.StatusBadRequest},
		"bad entity":         {popPath + "?legal_entity_id=-x", "tenant-abc", "p1", http.StatusBadRequest},
		"bad period":         {popPath + ok + "&period_id=2026-13", "tenant-abc", "p1", http.StatusBadRequest},
		"date as period":     {popPath + ok + "&period_id=2026-01-31", "tenant-abc", "p1", http.StatusBadRequest},
		"unknown param":      {popPath + ok + "&bogus=1", "tenant-abc", "p1", http.StatusBadRequest},
		"repeated param":     {popPath + ok + "&limit=1&limit=2", "tenant-abc", "p1", http.StatusBadRequest},
		"limit zero":         {popPath + ok + "&limit=0", "tenant-abc", "p1", http.StatusBadRequest},
		"limit over max":     {popPath + ok + "&limit=5001", "tenant-abc", "p1", http.StatusBadRequest},
		"cursor not base64":  {popPath + ok + "&cursor=***", "tenant-abc", "p1", http.StatusBadRequest},
		"unknown population": {"/v1/control-populations/nope" + ok, "tenant-abc", "p1", http.StatusNotFound},
	}
	for name, c := range cases {
		s := newStubStore()
		if got := popGet(s, &stubAuthZ{}, &stubLedger{kernel: true}, c.path, c.tenant, c.principal).Code; got != c.want {
			t.Errorf("%s: status %d, want %d", name, got, c.want)
		}
		if s.rec().calls != 0 {
			t.Errorf("%s: the store must not be consulted", name)
		}
	}
}

// Without the kernel path the register has no currency of its own and posts one journal per run, so the population
// would be untrue: it answers 422, never a guessed currency.
func TestControlPopulation_DepreciationRunPostings_UnavailableWithoutTheKernelPath(t *testing.T) {
	path := popPath + "?legal_entity_id=" + popEntity
	s := newStubStore()
	if got := popGet(s, &stubAuthZ{}, &stubLedger{kernel: false}, path, "tenant-abc", "p1").Code; got != http.StatusUnprocessableEntity {
		t.Errorf("kernel off: status %d", got)
	}
	if s.rec().calls != 0 {
		t.Error("the store must not be consulted when the population is unavailable")
	}
}

func TestControlPopulation_DepreciationRunPostings_AuthorizationAndStoreFailuresFailClosed(t *testing.T) {
	path := popPath + "?legal_entity_id=" + popEntity

	s := newStubStore()
	if got := popGet(s, &stubAuthZ{err: domain.ErrAuthorizationDenied}, &stubLedger{kernel: true}, path, "tenant-abc", "p1").Code; got != http.StatusForbidden {
		t.Errorf("denied caller: status %d", got)
	}
	if s.rec().calls != 0 {
		t.Error("the store is never consulted for a denied caller")
	}

	s = newStubStore()
	s.rec().err = domain.ErrControlPopulationTooLarge
	if got := popGet(s, &stubAuthZ{}, &stubLedger{kernel: true}, path, "tenant-abc", "p1").Code; got != http.StatusUnprocessableEntity {
		t.Errorf("over the cap: status %d", got)
	}

	s = newStubStore()
	s.rec().err = errors.New("db down")
	if got := popGet(s, &stubAuthZ{}, &stubLedger{kernel: true}, path, "tenant-abc", "p1").Code; got != http.StatusServiceUnavailable {
		t.Errorf("store failure: status %d", got)
	}
}
