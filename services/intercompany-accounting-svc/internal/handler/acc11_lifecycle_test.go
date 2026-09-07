package handler_test

import (
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"zoiko.io/intercompany-accounting-svc/internal/domain"
)

func seedEntry(s *stubStoreReal, id, matchStatus string) *domain.IntercompanyEntry {
	entry := &domain.IntercompanyEntry{
		IntercompanyEntryID: id, TenantID: "tenant-abc",
		SourceLegalEntityID: "entity-a", TargetLegalEntityID: "entity-b",
		SourceJournalID: "journal-1", Amount: 100, CurrencyCode: "GBP",
		MatchStatus: matchStatus, CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(),
	}
	s.entries[id] = entry
	return entry
}

// ── AcknowledgeCounterparty ──────────────────────────────────────────────────

func TestAcknowledgeCounterparty_FromOpen_MovesToAwaitingCounterparty(t *testing.T) {
	s := newStubStoreReal()
	seedEntry(s, "e1", domain.MatchStatusUnmatched)
	r := newRouter(s, &stubPublisher{}, &stubAuthZ{}, &stubLedger{})

	resp := doReq(r, http.MethodPost, "/v1/intercompany/entries/e1/acknowledge", nil, "user-1")
	if resp.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", resp.Code, resp.Body.String())
	}
	if s.entries["e1"].MatchStatus != domain.MatchStatusAwaitingCounterparty {
		t.Fatalf("expected AWAITING_COUNTERPARTY, got %q", s.entries["e1"].MatchStatus)
	}
}

func TestAcknowledgeCounterparty_AlreadyMatched_Refused(t *testing.T) {
	s := newStubStoreReal()
	seedEntry(s, "e1", domain.MatchStatusMatched)
	r := newRouter(s, &stubPublisher{}, &stubAuthZ{}, &stubLedger{})

	resp := doReq(r, http.MethodPost, "/v1/intercompany/entries/e1/acknowledge", nil, "user-1")
	if resp.Code != http.StatusUnprocessableEntity {
		t.Fatalf("expected 422, got %d: %s", resp.Code, resp.Body.String())
	}
}

// ── DisputeIntercompany ──────────────────────────────────────────────────────

func TestDisputeIntercompany_MissingReason_Returns400(t *testing.T) {
	s := newStubStoreReal()
	seedEntry(s, "e1", domain.MatchStatusMismatch)
	r := newRouter(s, &stubPublisher{}, &stubAuthZ{}, &stubLedger{})

	resp := doReq(r, http.MethodPost, "/v1/intercompany/entries/e1/dispute", map[string]string{}, "user-1")
	if resp.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d: %s", resp.Code, resp.Body.String())
	}
}

func TestDisputeIntercompany_FromMismatch_MovesToDisputed(t *testing.T) {
	s := newStubStoreReal()
	seedEntry(s, "e1", domain.MatchStatusMismatch)
	r := newRouter(s, &stubPublisher{}, &stubAuthZ{}, &stubLedger{})

	resp := doReq(r, http.MethodPost, "/v1/intercompany/entries/e1/dispute", map[string]string{"reason": "amount looks wrong"}, "user-1")
	if resp.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", resp.Code, resp.Body.String())
	}
	if s.entries["e1"].MatchStatus != domain.MatchStatusDisputed {
		t.Fatalf("expected DISPUTED, got %q", s.entries["e1"].MatchStatus)
	}
	if s.entries["e1"].DisputeReason == nil || *s.entries["e1"].DisputeReason != "amount looks wrong" {
		t.Fatalf("expected dispute reason recorded, got %+v", s.entries["e1"].DisputeReason)
	}
}

func TestDisputeIntercompany_FromOpen_Refused(t *testing.T) {
	s := newStubStoreReal()
	seedEntry(s, "e1", domain.MatchStatusUnmatched)
	r := newRouter(s, &stubPublisher{}, &stubAuthZ{}, &stubLedger{})

	resp := doReq(r, http.MethodPost, "/v1/intercompany/entries/e1/dispute", map[string]string{"reason": "x"}, "user-1")
	if resp.Code != http.StatusUnprocessableEntity {
		t.Fatalf("expected 422 disputing a pair that was never even checked, got %d: %s", resp.Code, resp.Body.String())
	}
}

// ── ResolveMismatch ──────────────────────────────────────────────────────────

func TestResolveMismatch_MissingNote_Returns400(t *testing.T) {
	s := newStubStoreReal()
	seedEntry(s, "e1", domain.MatchStatusDisputed)
	r := newRouter(s, &stubPublisher{}, &stubAuthZ{}, &stubLedger{})

	resp := doReq(r, http.MethodPost, "/v1/intercompany/entries/e1/resolve", map[string]string{}, "user-1")
	if resp.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d: %s", resp.Code, resp.Body.String())
	}
}

func TestResolveMismatch_FromDisputed_MovesToResolved(t *testing.T) {
	s := newStubStoreReal()
	seedEntry(s, "e1", domain.MatchStatusDisputed)
	r := newRouter(s, &stubPublisher{}, &stubAuthZ{}, &stubLedger{})

	resp := doReq(r, http.MethodPost, "/v1/intercompany/entries/e1/resolve", map[string]string{"resolution_note": "FX rounding, accepted as-is"}, "user-1")
	if resp.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", resp.Code, resp.Body.String())
	}
	if s.entries["e1"].MatchStatus != domain.MatchStatusResolved {
		t.Fatalf("expected RESOLVED, got %q", s.entries["e1"].MatchStatus)
	}
}

func TestResolveMismatch_FromMismatch_Refused(t *testing.T) {
	// ResolveMismatch is only reachable from DISPUTED — a straight MISMATCH
	// that was never disputed has no dispute to resolve.
	s := newStubStoreReal()
	seedEntry(s, "e1", domain.MatchStatusMismatch)
	r := newRouter(s, &stubPublisher{}, &stubAuthZ{}, &stubLedger{})

	resp := doReq(r, http.MethodPost, "/v1/intercompany/entries/e1/resolve", map[string]string{"resolution_note": "x"}, "user-1")
	if resp.Code != http.StatusUnprocessableEntity {
		t.Fatalf("expected 422, got %d: %s", resp.Code, resp.Body.String())
	}
}

// ── "One side missing" (MatchIntercompany against a nonexistent counterparty journal) ──

func TestMatchEntry_CounterpartyJournalMissing_RecordsMismatchNotOutage(t *testing.T) {
	s := newStubStoreReal()
	seedEntry(s, "e1", domain.MatchStatusUnmatched)
	// Default stubLedger{} (no journal, no err) reports the journal as not
	// found — exactly ACC-11's own negative path, "One side missing."
	r := newRouter(s, &stubPublisher{}, &stubAuthZ{}, &stubLedger{})

	resp := doReq(r, http.MethodPost, "/v1/intercompany/entries/e1/match", map[string]string{"target_journal_id": "nonexistent-journal"}, "user-1")
	if resp.Code != http.StatusUnprocessableEntity {
		t.Fatalf("expected 422 (a real accounting fact), not a 5xx outage, got %d: %s", resp.Code, resp.Body.String())
	}
	if s.entries["e1"].MatchStatus != domain.MatchStatusMismatch {
		t.Fatalf("expected MISMATCH recorded, got %q", s.entries["e1"].MatchStatus)
	}
	if s.entries["e1"].MismatchReason == nil || *s.entries["e1"].MismatchReason != domain.ErrCounterpartyJournalMissing.Error() {
		t.Fatalf("expected the counterparty-journal-missing reason recorded, got %+v", s.entries["e1"].MismatchReason)
	}
}

// ── Re-matching after a mismatch ─────────────────────────────────────────────

func TestMatchEntry_Rematch_AfterMismatch_Allowed(t *testing.T) {
	s := newStubStoreReal()
	seedEntry(s, "e1", domain.MatchStatusMismatch)
	r := newRouter(s, &stubPublisher{}, &stubAuthZ{}, &stubLedger{})

	// Still missing the counterparty journal, so it stays MISMATCH — the
	// point of this test is that the attempt itself is not refused as
	// "already matched", unlike a genuinely MATCHED or DISPUTED pair.
	resp := doReq(r, http.MethodPost, "/v1/intercompany/entries/e1/match", map[string]string{"target_journal_id": "still-missing"}, "user-1")
	if resp.Code == http.StatusUnprocessableEntity {
		var body map[string]string
		_ = json.Unmarshal(resp.Body.Bytes(), &body)
		if body["error_code"] == "entry_already_matched" {
			t.Fatalf("expected re-matching a MISMATCH pair to be allowed, got entry_already_matched")
		}
	}
}
