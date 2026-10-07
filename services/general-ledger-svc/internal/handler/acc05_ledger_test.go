package handler_test

import (
	"encoding/json"
	"net/http"
	"testing"

	"zoiko.io/general-ledger-svc/internal/domain"
)

// ── QueryLedger ──────────────────────────────────────────────────────────────

func TestQueryLedger_MissingLegalEntityID_Returns400(t *testing.T) {
	r := newRouter(newStubStore(), &stubPublisher{}, &stubAuthZ{})
	resp := doRequest(r, http.MethodGet, "/v1/ledger/entries", nil, "reader-1")
	if resp.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 without legal_entity_id, got %d: %s", resp.Code, resp.Body.String())
	}
}

func TestQueryLedger_ScopedToRequestedEntity_OtherEntityLeaksNothing(t *testing.T) {
	s := newStubStore()
	entityA, entityB := "entity-a", "entity-b"
	s.ledgerEntries = []domain.LedgerEntry{
		{LedgerEntryID: "e1", LegalEntityID: entityA, AccountCode: "1000", EntrySeq: 1},
		{LedgerEntryID: "e2", LegalEntityID: entityB, AccountCode: "1000", EntrySeq: 2},
	}
	r := newRouter(s, &stubPublisher{}, &stubAuthZ{})
	resp := doRequest(r, http.MethodGet, "/v1/ledger/entries?legal_entity_id="+entityA, nil, "reader-1")
	if resp.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", resp.Code, resp.Body.String())
	}
	var got []domain.LedgerEntry
	if err := json.Unmarshal(resp.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if len(got) != 1 || got[0].LedgerEntryID != "e1" {
		t.Fatalf("expected only entity-a's entry, got %+v — cross-book query leakage", got)
	}
}

func TestQueryLedger_AuthorizationDenied_Returns403(t *testing.T) {
	r := newRouter(newStubStore(), &stubPublisher{}, &stubAuthZ{err: domain.ErrAuthorizationDenied})
	resp := doRequest(r, http.MethodGet, "/v1/ledger/entries?legal_entity_id=entity-a", nil, "reader-1")
	if resp.Code != http.StatusForbidden {
		t.Fatalf("expected 403, got %d: %s", resp.Code, resp.Body.String())
	}
}

// ── QueryLedgerAsOf ──────────────────────────────────────────────────────────

func TestQueryLedgerAsOf_MissingWatermark_Returns400(t *testing.T) {
	r := newRouter(newStubStore(), &stubPublisher{}, &stubAuthZ{})
	resp := doRequest(r, http.MethodGet, "/v1/ledger/as-of?legal_entity_id=entity-a", nil, "reader-1")
	if resp.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 without as_of_entry_seq, got %d: %s", resp.Code, resp.Body.String())
	}
}

func TestQueryLedgerAsOf_ExcludesEntriesPastWatermark(t *testing.T) {
	s := newStubStore()
	entityA := "entity-a"
	s.ledgerEntries = []domain.LedgerEntry{
		{LedgerEntryID: "e1", LegalEntityID: entityA, EntrySeq: 1},
		{LedgerEntryID: "e2", LegalEntityID: entityA, EntrySeq: 2},
		{LedgerEntryID: "e3", LegalEntityID: entityA, EntrySeq: 3},
	}
	r := newRouter(s, &stubPublisher{}, &stubAuthZ{})
	resp := doRequest(r, http.MethodGet, "/v1/ledger/as-of?legal_entity_id="+entityA+"&as_of_entry_seq=2", nil, "reader-1")
	if resp.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", resp.Code, resp.Body.String())
	}
	var got []domain.LedgerEntry
	if err := json.Unmarshal(resp.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("expected 2 entries at or before watermark 2, got %d: %+v", len(got), got)
	}
}

// ── QuerySourceEntries ───────────────────────────────────────────────────────

func TestQuerySourceEntries_MissingSourceEventID_Returns400(t *testing.T) {
	r := newRouter(newStubStore(), &stubPublisher{}, &stubAuthZ{})
	resp := doRequest(r, http.MethodGet, "/v1/ledger/source-entries", nil, "reader-1")
	if resp.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 without source_event_id, got %d: %s", resp.Code, resp.Body.String())
	}
}

// ── QueryAccountBalance ──────────────────────────────────────────────────────

func TestQueryAccountBalance_MissingFields_Returns400(t *testing.T) {
	r := newRouter(newStubStore(), &stubPublisher{}, &stubAuthZ{})
	resp := doRequest(r, http.MethodGet, "/v1/ledger/balance?legal_entity_id=entity-a", nil, "reader-1")
	if resp.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 without account_code/fiscal_period, got %d: %s", resp.Code, resp.Body.String())
	}
}

func TestQueryAccountBalance_ReturnsProjectionRow(t *testing.T) {
	s := newStubStore()
	s.ledgerBalance = &domain.LedgerBalance{AccountCode: "1000", DebitTotal: 500, CreditTotal: 200, NetBalance: 300}
	r := newRouter(s, &stubPublisher{}, &stubAuthZ{})
	resp := doRequest(r, http.MethodGet, "/v1/ledger/balance?legal_entity_id=entity-a&account_code=1000&fiscal_period=2026-07", nil, "reader-1")
	if resp.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", resp.Code, resp.Body.String())
	}
	var got domain.LedgerBalance
	if err := json.Unmarshal(resp.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if got.NetBalance != 300 {
		t.Fatalf("expected net_balance 300, got %v", got.NetBalance)
	}
}

// ── RebuildDerivedBalanceProjection ──────────────────────────────────────────

func TestRebuildDerivedBalanceProjection_MissingFields_Returns400(t *testing.T) {
	r := newRouter(newStubStore(), &stubPublisher{}, &stubAuthZ{})
	resp := doRequest(r, http.MethodPost, "/v1/ledger/rebuild-balances", map[string]string{}, "admin-1")
	if resp.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 without legal_entity_id/fiscal_period, got %d: %s", resp.Code, resp.Body.String())
	}
}

func TestRebuildDerivedBalanceProjection_AuthorizationDenied_Returns403_NeverCallsStore(t *testing.T) {
	s := newStubStore()
	r := newRouter(s, &stubPublisher{}, &stubAuthZ{err: domain.ErrAuthorizationDenied})
	resp := doRequest(r, http.MethodPost, "/v1/ledger/rebuild-balances", map[string]string{
		"legal_entity_id": "entity-a", "fiscal_period": "2026-07",
	}, "operator-1")
	if resp.Code != http.StatusForbidden {
		t.Fatalf("expected 403, got %d: %s", resp.Code, resp.Body.String())
	}
	if s.rebuildCalled {
		t.Fatal("expected the store's rebuild to never be reached when authorization is denied")
	}
}

func TestRebuildDerivedBalanceProjection_Authorized_CallsStoreWithScope(t *testing.T) {
	s := newStubStore()
	r := newRouter(s, &stubPublisher{}, &stubAuthZ{})
	resp := doRequest(r, http.MethodPost, "/v1/ledger/rebuild-balances", map[string]string{
		"legal_entity_id": "entity-a", "fiscal_period": "2026-07",
	}, "admin-1")
	if resp.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", resp.Code, resp.Body.String())
	}
	if !s.rebuildCalled {
		t.Fatal("expected the store's rebuild to be called")
	}
	if s.lastRebuildRequest.LegalEntityID != "entity-a" || s.lastRebuildRequest.FiscalPeriod != "2026-07" {
		t.Fatalf("expected the rebuild scope to be passed through, got %+v", s.lastRebuildRequest)
	}
}
