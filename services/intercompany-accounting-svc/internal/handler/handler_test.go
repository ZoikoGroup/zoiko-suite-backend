package handler_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"slices"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"go.uber.org/zap"

	"zoiko.io/intercompany-accounting-svc/internal/domain"
	"zoiko.io/intercompany-accounting-svc/internal/handler"
	"zoiko.io/intercompany-accounting-svc/internal/ledger"
	"zoiko.io/intercompany-accounting-svc/internal/middleware"
)

// ── stubs ─────────────────────────────────────────────────────────────────────

// eventRecord captures one call the stub store received in place of
// publishing an event — the handler no longer owns publishing (see
// internal/store's doc comment); these let tests assert the same intent the
// old stubPublisher counters did.
type eventRecord struct {
	entry         domain.IntercompanyEntry
	actorID       string
	correlationID string
}

type stubStoreReal struct {
	entries         map[string]*domain.IntercompanyEntry
	bySourceJournal map[string]string

	createdEvents []eventRecord
	matchEvents   []eventRecord

	// updateMatchErr, when set, is returned by UpdateMatch without writing.
	updateMatchErr error

	// control-population fake
	cpPage  *domain.ControlPopulationPage
	cpErr   error
	cpCalls int
	cpQuery domain.ControlPopulationQuery
}

func newStubStoreReal() *stubStoreReal {
	return &stubStoreReal{entries: make(map[string]*domain.IntercompanyEntry), bySourceJournal: make(map[string]string)}
}

func (s *stubStoreReal) CreateEntry(_ context.Context, entry *domain.IntercompanyEntry, actorID, correlationID string) (bool, error) {
	key := entry.TenantID + "|" + entry.SourceJournalID
	if existingID, ok := s.bySourceJournal[key]; ok {
		*entry = *s.entries[existingID]
		return false, nil
	}
	s.bySourceJournal[key] = entry.IntercompanyEntryID
	s.entries[entry.IntercompanyEntryID] = entry
	s.createdEvents = append(s.createdEvents, eventRecord{entry: *entry, actorID: actorID, correlationID: correlationID})
	return true, nil
}

func (s *stubStoreReal) GetEntry(_ context.Context, id string) (*domain.IntercompanyEntry, error) {
	entry, ok := s.entries[id]
	if !ok {
		return nil, domain.ErrEntryNotFound
	}
	// A copy, like a real read: the handler mutates what it gets back
	// before calling UpdateMatch, and that must not leak into the stored
	// row or the status guard below would see the new status, not the old.
	cp := *entry
	return &cp, nil
}

func (s *stubStoreReal) ListEntries(_ context.Context, sourceEntityID, targetEntityID string) ([]domain.IntercompanyEntry, error) {
	var out []domain.IntercompanyEntry
	for _, entry := range s.entries {
		if sourceEntityID != "" && entry.SourceLegalEntityID != sourceEntityID {
			continue
		}
		if targetEntityID != "" && entry.TargetLegalEntityID != targetEntityID {
			continue
		}
		out = append(out, *entry)
	}
	return out, nil
}

func (s *stubStoreReal) UpdateMatch(_ context.Context, entry *domain.IntercompanyEntry, actorID, correlationID string) error {
	if s.updateMatchErr != nil {
		return s.updateMatchErr
	}
	existing, ok := s.entries[entry.IntercompanyEntryID]
	if !ok {
		return domain.ErrEntryNotFound
	}
	if !slices.Contains(domain.MatchableStatuses, existing.MatchStatus) {
		return domain.ErrInvalidPairTransition
	}
	existing.TargetJournalID = entry.TargetJournalID
	existing.MatchStatus = entry.MatchStatus
	existing.MismatchReason = entry.MismatchReason
	existing.UpdatedAt = time.Now().UTC()
	s.matchEvents = append(s.matchEvents, eventRecord{entry: *existing, actorID: actorID, correlationID: correlationID})
	return nil
}

func (s *stubStoreReal) AcknowledgeCounterparty(_ context.Context, id, principalID string) error {
	entry, ok := s.entries[id]
	if !ok || entry.MatchStatus != domain.MatchStatusUnmatched {
		return domain.ErrInvalidPairTransition
	}
	now := time.Now().UTC()
	entry.MatchStatus, entry.AcknowledgedAt, entry.AcknowledgedByPrincipalID = domain.MatchStatusAwaitingCounterparty, &now, &principalID
	return nil
}

func (s *stubStoreReal) DisputeIntercompany(_ context.Context, id, principalID, reason string) error {
	entry, ok := s.entries[id]
	if !ok || entry.MatchStatus != domain.MatchStatusMismatch {
		return domain.ErrInvalidPairTransition
	}
	now := time.Now().UTC()
	entry.MatchStatus, entry.DisputedAt, entry.DisputedByPrincipalID, entry.DisputeReason = domain.MatchStatusDisputed, &now, &principalID, &reason
	return nil
}

func (s *stubStoreReal) ResolveMismatch(_ context.Context, id, principalID, resolutionNote string) error {
	entry, ok := s.entries[id]
	if !ok || entry.MatchStatus != domain.MatchStatusDisputed {
		return domain.ErrInvalidPairTransition
	}
	now := time.Now().UTC()
	entry.MatchStatus, entry.ResolvedAt, entry.ResolvedByPrincipalID, entry.ResolutionNote = domain.MatchStatusResolved, &now, &principalID, &resolutionNote
	return nil
}

// Compile-time proof the stub still satisfies the contract the handler
// depends on.
var _ handler.Store = (*stubStoreReal)(nil)

type stubAuthZ struct{ err error }

func (a *stubAuthZ) CheckAllowed(_ context.Context, _, _, _ string) error { return a.err }

type stubLedger struct {
	journal *ledger.JournalDetail
	err     error
}

func (l *stubLedger) GetJournal(_ context.Context, _, _ string) (*ledger.JournalDetail, error) {
	if l.err != nil {
		return nil, l.err
	}
	if l.journal == nil {
		return nil, domain.ErrEntryNotFound
	}
	return l.journal, nil
}

// ── router factory ─────────────────────────────────────────────────────────────

func newRouter(s *stubStoreReal, authz *stubAuthZ, leg *stubLedger) chi.Router {
	r := chi.NewRouter()
	r.Use(func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			req = req.WithContext(middleware.WithTenant(req.Context(), "tenant-abc"))
			next.ServeHTTP(w, req)
		})
	})
	h := handler.New(s, authz, leg, zap.NewNop())
	handler.RegisterRoutes(r, h)
	return r
}

func doReq(r chi.Router, method, path string, body any, principalID string) *httptest.ResponseRecorder {
	var buf bytes.Buffer
	if body != nil {
		_ = json.NewEncoder(&buf).Encode(body)
	}
	req := httptest.NewRequest(method, path, &buf)
	req.Header.Set("Content-Type", "application/json")
	if principalID != "" {
		req.Header.Set("X-Principal-Id", principalID)
	}
	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, req)
	return rr
}

// ── CreateEntry Tests ─────────────────────────────────────────────────────────

func TestCreateEntry_MissingPrincipal(t *testing.T) {
	r := newRouter(newStubStoreReal(), &stubAuthZ{}, &stubLedger{})
	rr := doReq(r, http.MethodPost, "/v1/intercompany/entries/", map[string]any{
		"source_legal_entity_id": "le-us",
		"target_legal_entity_id": "le-uk",
		"source_journal_id":      "j-001",
		"amount":                 5000.0,
		"currency_code":          "USD",
	}, "")
	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 got %d", rr.Code)
	}
}

func TestCreateEntry_SameEntityForbidden(t *testing.T) {
	r := newRouter(newStubStoreReal(), &stubAuthZ{}, &stubLedger{})
	rr := doReq(r, http.MethodPost, "/v1/intercompany/entries/", map[string]any{
		"source_legal_entity_id": "le-same",
		"target_legal_entity_id": "le-same",
		"source_journal_id":      "j-001",
		"amount":                 5000.0,
		"currency_code":          "USD",
	}, "principal-1")
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 got %d", rr.Code)
	}
}

func TestCreateEntry_NegativeAmount(t *testing.T) {
	r := newRouter(newStubStoreReal(), &stubAuthZ{}, &stubLedger{})
	rr := doReq(r, http.MethodPost, "/v1/intercompany/entries/", map[string]any{
		"source_legal_entity_id": "le-us",
		"target_legal_entity_id": "le-uk",
		"source_journal_id":      "j-001",
		"amount":                 -100.0,
		"currency_code":          "USD",
	}, "principal-1")
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 got %d", rr.Code)
	}
}

func TestCreateEntry_HappyPath(t *testing.T) {
	s := newStubStoreReal()
	r := newRouter(s, &stubAuthZ{}, &stubLedger{})
	rr := doReq(r, http.MethodPost, "/v1/intercompany/entries/", map[string]any{
		"source_legal_entity_id": "le-us",
		"target_legal_entity_id": "le-uk",
		"source_journal_id":      "j-001",
		"amount":                 5000.0,
		"currency_code":          "USD",
	}, "principal-1")
	if rr.Code != http.StatusCreated {
		t.Fatalf("expected 201 got %d: %s", rr.Code, rr.Body.String())
	}
	var entry domain.IntercompanyEntry
	if err := json.NewDecoder(rr.Body).Decode(&entry); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if entry.MatchStatus != "UNMATCHED" {
		t.Errorf("expected UNMATCHED got %q", entry.MatchStatus)
	}
	if len(s.createdEvents) != 1 {
		t.Errorf("expected 1 created event got %d", len(s.createdEvents))
	}
}

func TestCreateEntry_RetriedSourceJournalID_ReturnsOriginalNotDuplicate(t *testing.T) {
	s := newStubStoreReal()
	r := newRouter(s, &stubAuthZ{}, &stubLedger{})
	body := map[string]any{
		"source_legal_entity_id": "le-us",
		"target_legal_entity_id": "le-uk",
		"source_journal_id":      "j-001",
		"amount":                 5000.0,
		"currency_code":          "USD",
	}

	first := doReq(r, http.MethodPost, "/v1/intercompany/entries/", body, "principal-1")
	if first.Code != http.StatusCreated {
		t.Fatalf("expected 201 on first call, got %d: %s", first.Code, first.Body.String())
	}
	var firstEntry domain.IntercompanyEntry
	_ = json.NewDecoder(first.Body).Decode(&firstEntry)

	retry := doReq(r, http.MethodPost, "/v1/intercompany/entries/", body, "principal-1")
	if retry.Code != http.StatusOK {
		t.Fatalf("expected 200 on retried call for the same source_journal_id, got %d: %s", retry.Code, retry.Body.String())
	}
	var retryEntry domain.IntercompanyEntry
	_ = json.NewDecoder(retry.Body).Decode(&retryEntry)
	if retryEntry.IntercompanyEntryID != firstEntry.IntercompanyEntryID {
		t.Fatalf("retried call resolved to a different intercompany_entry_id (%s) than the original (%s)", retryEntry.IntercompanyEntryID, firstEntry.IntercompanyEntryID)
	}
	if len(s.createdEvents) != 1 {
		t.Fatalf("expected exactly 1 created event, got %d — replay must not re-publish", len(s.createdEvents))
	}
	if len(s.entries) != 1 {
		t.Fatalf("expected exactly 1 intercompany entry to exist, got %d — a retry must not create a duplicate", len(s.entries))
	}
}

// ── MatchEntry Tests ──────────────────────────────────────────────────────────

func TestMatchEntry_AlreadyMatched(t *testing.T) {
	s := newStubStoreReal()
	targetJ := "target-j-001"
	s.entries["entry-matched"] = &domain.IntercompanyEntry{
		IntercompanyEntryID: "entry-matched",
		TenantID:            "tenant-abc",
		SourceLegalEntityID: "le-us",
		TargetLegalEntityID: "le-uk",
		Amount:              5000.0,
		MatchStatus:         "MATCHED",
		TargetJournalID:     &targetJ,
	}
	r := newRouter(s, &stubAuthZ{}, &stubLedger{})
	rr := doReq(r, http.MethodPost, "/v1/intercompany/entries/entry-matched/match", map[string]any{
		"target_journal_id": "target-j-002",
	}, "principal-1")
	if rr.Code != http.StatusUnprocessableEntity {
		t.Fatalf("expected 422 got %d", rr.Code)
	}
}

func TestMatchEntry_TargetEntityMismatch(t *testing.T) {
	s := newStubStoreReal()
	s.entries["entry-1"] = &domain.IntercompanyEntry{
		IntercompanyEntryID: "entry-1",
		TenantID:            "tenant-abc",
		SourceLegalEntityID: "le-us",
		TargetLegalEntityID: "le-uk",
		Amount:              5000.0,
		MatchStatus:         "UNMATCHED",
	}
	leg := &stubLedger{
		journal: &ledger.JournalDetail{
			JournalID:     "target-j-wrong-entity",
			LegalEntityID: "le-de", // Doesn't match target le-uk
			Status:        "FINALIZED",
			Lines:         []ledger.JournalLine{{AccountCode: "1000", DebitAmount: 5000.0}},
		},
	}
	r := newRouter(s, &stubAuthZ{}, leg)
	rr := doReq(r, http.MethodPost, "/v1/intercompany/entries/entry-1/match", map[string]any{
		"target_journal_id": "target-j-wrong-entity",
	}, "principal-1")

	if rr.Code != http.StatusUnprocessableEntity {
		t.Fatalf("expected 422 got %d", rr.Code)
	}
	if len(s.matchEvents) != 1 || s.matchEvents[0].entry.MatchStatus != "MISMATCH" {
		t.Errorf("expected 1 mismatch event got %d", len(s.matchEvents))
	}
	if s.entries["entry-1"].MatchStatus != "MISMATCH" {
		t.Errorf("expected store match_status to be MISMATCH, got %q", s.entries["entry-1"].MatchStatus)
	}
}

func TestMatchEntry_HappyPath(t *testing.T) {
	s := newStubStoreReal()
	s.entries["entry-1"] = &domain.IntercompanyEntry{
		IntercompanyEntryID: "entry-1",
		TenantID:            "tenant-abc",
		SourceLegalEntityID: "le-us",
		TargetLegalEntityID: "le-uk",
		Amount:              5000.0,
		MatchStatus:         "UNMATCHED",
	}
	leg := &stubLedger{
		journal: &ledger.JournalDetail{
			JournalID:     "target-j-001",
			LegalEntityID: "le-uk",
			Status:        "FINALIZED",
			Lines:         []ledger.JournalLine{{AccountCode: "1000", DebitAmount: 5000.0}},
		},
	}
	r := newRouter(s, &stubAuthZ{}, leg)
	rr := doReq(r, http.MethodPost, "/v1/intercompany/entries/entry-1/match", map[string]any{
		"target_journal_id": "target-j-001",
	}, "principal-1")

	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200 got %d: %s", rr.Code, rr.Body.String())
	}
	if len(s.matchEvents) != 1 || s.matchEvents[0].entry.MatchStatus != "MATCHED" {
		t.Errorf("expected 1 posted event got %d", len(s.matchEvents))
	}
	if s.entries["entry-1"].MatchStatus != "MATCHED" {
		t.Errorf("expected store match_status to be MATCHED, got %q", s.entries["entry-1"].MatchStatus)
	}
}

// TestMatchEntry_MismatchNotRecorded_Is503 proves a MISMATCH outcome is only
// reported once the store has recorded it. Before, every mismatch branch
// discarded UpdateMatch's error and answered 422 MISMATCH regardless, so a
// failed write left the pair UNMATCHED with no mismatch event while the
// caller was told otherwise.
func TestMatchEntry_MismatchNotRecorded_Is503(t *testing.T) {
	s := newStubStoreReal()
	s.entries["entry-1"] = &domain.IntercompanyEntry{
		IntercompanyEntryID: "entry-1",
		TenantID:            "tenant-abc",
		SourceLegalEntityID: "le-us",
		TargetLegalEntityID: "le-uk",
		Amount:              5000.0,
		MatchStatus:         domain.MatchStatusUnmatched,
	}
	s.updateMatchErr = errors.New("connection reset")
	leg := &stubLedger{journal: &ledger.JournalDetail{
		JournalID: "target-j-001", LegalEntityID: "le-uk", Status: "FINALIZED",
		Lines: []ledger.JournalLine{{AccountCode: "1000", DebitAmount: 4000.0}}, // amount mismatch
	}}
	r := newRouter(s, &stubAuthZ{}, leg)
	rr := doReq(r, http.MethodPost, "/v1/intercompany/entries/entry-1/match", map[string]any{
		"target_journal_id": "target-j-001",
	}, "principal-1")

	if rr.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected 503 got %d: %s", rr.Code, rr.Body.String())
	}
	if s.entries["entry-1"].MatchStatus != domain.MatchStatusUnmatched {
		t.Errorf("expected the pair to stay UNMATCHED, got %q", s.entries["entry-1"].MatchStatus)
	}
}

// TestMatchEntry_StatusMovedSinceRead_Is422 proves a pair that left the
// matchable statuses between the handler's read and its write (e.g. disputed
// by a concurrent request) is refused, not overwritten.
func TestMatchEntry_StatusMovedSinceRead_Is422(t *testing.T) {
	s := newStubStoreReal()
	s.entries["entry-1"] = &domain.IntercompanyEntry{
		IntercompanyEntryID: "entry-1",
		TenantID:            "tenant-abc",
		SourceLegalEntityID: "le-us",
		TargetLegalEntityID: "le-uk",
		Amount:              5000.0,
		MatchStatus:         domain.MatchStatusMismatch,
	}
	s.updateMatchErr = domain.ErrInvalidPairTransition
	leg := &stubLedger{journal: &ledger.JournalDetail{
		JournalID: "target-j-001", LegalEntityID: "le-uk", Status: "FINALIZED",
		Lines: []ledger.JournalLine{{AccountCode: "1000", DebitAmount: 5000.0}},
	}}
	r := newRouter(s, &stubAuthZ{}, leg)
	rr := doReq(r, http.MethodPost, "/v1/intercompany/entries/entry-1/match", map[string]any{
		"target_journal_id": "target-j-001",
	}, "principal-1")

	if rr.Code != http.StatusUnprocessableEntity {
		t.Fatalf("expected 422 got %d: %s", rr.Code, rr.Body.String())
	}
	var body map[string]string
	_ = json.NewDecoder(rr.Body).Decode(&body)
	if body["error_code"] != "invalid_transition" {
		t.Errorf("expected error_code invalid_transition, got %q", body["error_code"])
	}
	if len(s.matchEvents) != 0 {
		t.Errorf("expected no match event, got %d", len(s.matchEvents))
	}
}
