package handler_test

import (
	"encoding/json"
	"net/http"
	"testing"

	"zoiko.io/bank-reconciliation-svc/internal/domain"
)

const periodEntity = "22222222-2222-4222-8222-222222222222"

func TestPeriodStatus_ReturnsPerAccountStatusForTheCallersTenant(t *testing.T) {
	s := newStubStore()
	s.periodStatus = []domain.AccountReconciliationStatus{{
		BankAccountID:   "acct-1",
		LatestCertified: &domain.RunStatusDigest{RunID: "r-1", StatementDate: "2026-10-30", Status: "CERTIFIED"},
	}}
	r := newRouter(s, &stubPublisher{}, &stubAuthZ{}, &stubLedger{})
	rec := doRequest(r, http.MethodGet,
		"/v1/reconciliation-runs/period-status?legal_entity_id="+periodEntity+"&period_start=2026-10-01&period_end=2026-10-31", nil, "close-svc")
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	var got domain.PeriodReconciliationStatus
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if len(got.Accounts) != 1 || got.Accounts[0].LatestCertified.RunID != "r-1" || got.PeriodEnd != "2026-10-31" {
		t.Fatalf("response %+v", got)
	}
	want := []string{testTenant, periodEntity, "2026-10-01", "2026-10-31"}
	for i := range want {
		if s.periodStatusArgs[i] != want[i] {
			t.Fatalf("store asked %v, want %v (tenant must come from the verified header)", s.periodStatusArgs, want)
		}
	}
}

func TestPeriodStatus_NoRunsIsAnEmptyListNotNull(t *testing.T) {
	r := newRouter(newStubStore(), &stubPublisher{}, &stubAuthZ{}, &stubLedger{})
	rec := doRequest(r, http.MethodGet,
		"/v1/reconciliation-runs/period-status?legal_entity_id="+periodEntity+"&period_start=2026-10-01&period_end=2026-10-31", nil, "close-svc")
	if rec.Code != http.StatusOK || !json.Valid(rec.Body.Bytes()) {
		t.Fatalf("got %d %s", rec.Code, rec.Body.String())
	}
	var raw map[string]json.RawMessage
	_ = json.Unmarshal(rec.Body.Bytes(), &raw)
	if string(raw["accounts"]) != "[]" {
		t.Fatalf("accounts = %s, want []", raw["accounts"])
	}
}

func TestPeriodStatus_RejectsBadParameters(t *testing.T) {
	r := newRouter(newStubStore(), &stubPublisher{}, &stubAuthZ{}, &stubLedger{})
	for _, q := range []string{
		"legal_entity_id=not-a-uuid&period_start=2026-10-01&period_end=2026-10-31",
		"legal_entity_id=" + periodEntity + "&period_end=2026-10-31",
		"legal_entity_id=" + periodEntity + "&period_start=2026-10-31&period_end=2026-10-01",
		"legal_entity_id=" + periodEntity + "&period_start=01/10/2026&period_end=2026-10-31",
	} {
		if rec := doRequest(r, http.MethodGet, "/v1/reconciliation-runs/period-status?"+q, nil, "close-svc"); rec.Code != http.StatusBadRequest {
			t.Errorf("%s: got %d, want 400", q, rec.Code)
		}
	}
}

func TestPeriodStatus_RequiresReadPermission(t *testing.T) {
	r := newRouter(newStubStore(), &stubPublisher{}, &stubAuthZ{err: domain.ErrAuthorizationDenied}, &stubLedger{})
	rec := doRequest(r, http.MethodGet,
		"/v1/reconciliation-runs/period-status?legal_entity_id="+periodEntity+"&period_start=2026-10-01&period_end=2026-10-31", nil, "nobody")
	if rec.Code != http.StatusForbidden {
		t.Fatalf("got %d, want 403", rec.Code)
	}
}
