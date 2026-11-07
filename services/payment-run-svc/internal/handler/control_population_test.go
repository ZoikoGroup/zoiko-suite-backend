package handler_test

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"zoiko.io/payment-run-svc/internal/domain"
)

const (
	popTenant = "6f1c1c0e-0000-4000-8000-000000000001"
	popEntity = "6f1c1c0e-0000-4000-8000-000000000002"
	popPath   = "/v1/control-populations/unapplied-supplier-payments"
)

func (e *env) popGet(query, tenant, principal string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodGet, popPath+query, nil)
	if tenant != "" {
		req.Header.Set("X-Tenant-Id", tenant)
	}
	if principal != "" {
		req.Header.Set("X-Principal-Id", principal)
	}
	rec := httptest.NewRecorder()
	e.router.ServeHTTP(rec, req)
	return rec
}

func TestUnappliedSupplierPayments_HappyPathPassesScopeToTheStore(t *testing.T) {
	e := newEnv()
	e.store.popPage = &domain.ControlPopulationPage{
		Records: []domain.ControlPopulationRecord{{RecordID: "a:AP_INVOICE:INV-1", Reference: "INV-1", Amount: "90.00",
			Currency: "USD", Date: "2026-08-01", Attributes: map[string]string{"age_days": "30"}}},
		NextRecordID: "a:AP_INVOICE:INV-1", Watermark: "pr1:n=1;md5=x",
		DeclaredTotals: domain.ControlDeclaredTotals{RowCount: 1, Totals: map[string]string{"USD": "90.00"}},
	}
	rec := e.popGet("?legal_entity_id="+popEntity+"&unapplied_before=2026-09-01&limit=7", popTenant, "p1")
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}
	var got struct {
		Records        []domain.ControlPopulationRecord `json:"records"`
		NextCursor     string                           `json:"next_cursor"`
		Watermark      string                           `json:"watermark"`
		DeclaredTotals domain.ControlDeclaredTotals     `json:"declared_totals"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.Watermark != "pr1:n=1;md5=x" || got.DeclaredTotals.Totals["USD"] != "90.00" || len(got.Records) != 1 {
		t.Fatalf("unexpected body: %s", rec.Body.String())
	}
	if want := base64.RawURLEncoding.EncodeToString([]byte("a:AP_INVOICE:INV-1")); got.NextCursor != want {
		t.Fatalf("next_cursor %q, want %q", got.NextCursor, want)
	}
	if e.store.popTenant != popTenant || e.store.popQuery.LegalEntityID != popEntity ||
		e.store.popQuery.UnappliedBefore != "2026-09-01" || e.store.popQuery.Limit != 7 {
		t.Fatalf("store got tenant=%q query=%+v", e.store.popTenant, e.store.popQuery)
	}
}

func TestUnappliedSupplierPayments_RejectsMalformedRequests(t *testing.T) {
	e := newEnv()
	ok := "?legal_entity_id=" + popEntity + "&unapplied_before=2026-09-01"
	cases := map[string]struct {
		query, tenant, principal string
		want                     int
	}{
		"no tenant":            {ok, "", "p1", http.StatusUnauthorized},
		"tenant not a uuid":    {ok, "tenant-1", "p1", http.StatusUnauthorized},
		"no principal":         {ok, popTenant, "", http.StatusUnauthorized},
		"missing entity":       {"?unapplied_before=2026-09-01", popTenant, "p1", http.StatusBadRequest},
		"entity not a uuid":    {"?legal_entity_id=le-1&unapplied_before=2026-09-01", popTenant, "p1", http.StatusBadRequest},
		"missing cut-off":      {"?legal_entity_id=" + popEntity, popTenant, "p1", http.StatusBadRequest},
		"bad cut-off":          {"?legal_entity_id=" + popEntity + "&unapplied_before=2026-13-40", popTenant, "p1", http.StatusBadRequest},
		"datetime cut-off":     {"?legal_entity_id=" + popEntity + "&unapplied_before=2026-09-01T00:00:00Z", popTenant, "p1", http.StatusBadRequest},
		"period_id rejected":   {ok + "&period_id=2026-09", popTenant, "p1", http.StatusBadRequest},
		"unknown param":        {ok + "&bogus=1", popTenant, "p1", http.StatusBadRequest},
		"repeated param":       {ok + "&limit=1&limit=2", popTenant, "p1", http.StatusBadRequest},
		"limit zero":           {ok + "&limit=0", popTenant, "p1", http.StatusBadRequest},
		"limit over max":       {ok + "&limit=5001", popTenant, "p1", http.StatusBadRequest},
		"cursor not base64":    {ok + "&cursor=***", popTenant, "p1", http.StatusBadRequest},
		"cursor empty payload": {ok + "&cursor=", popTenant, "p1", http.StatusBadRequest},
	}
	for name, c := range cases {
		if got := e.popGet(c.query, c.tenant, c.principal).Code; got != c.want {
			t.Errorf("%s: status %d, want %d", name, got, c.want)
		}
	}
	if e.store.popTenant != "" {
		t.Errorf("the store must not be consulted for a malformed request")
	}
}

func TestUnappliedSupplierPayments_AuthorizationAndStoreFailuresFailClosed(t *testing.T) {
	ok := "?legal_entity_id=" + popEntity + "&unapplied_before=2026-09-01"

	e := newEnv()
	e.authz.deny = true
	if got := e.popGet(ok, popTenant, "p1").Code; got != http.StatusForbidden {
		t.Errorf("denied caller: status %d", got)
	}
	if e.store.popTenant != "" {
		t.Errorf("the store is never consulted for a denied caller")
	}

	e = newEnv()
	e.store.popErr = domain.ErrControlPopulationTooLarge
	if got := e.popGet(ok, popTenant, "p1").Code; got != http.StatusUnprocessableEntity {
		t.Errorf("over the cap: status %d", got)
	}

	e = newEnv()
	e.store.popErr = errors.New("db down")
	if got := e.popGet(ok, popTenant, "p1").Code; got != http.StatusServiceUnavailable {
		t.Errorf("store failure: status %d", got)
	}
}
