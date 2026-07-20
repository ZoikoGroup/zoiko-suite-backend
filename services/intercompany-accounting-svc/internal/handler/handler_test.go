package handler_test

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
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

type stubStoreReal struct {
	entries map[string]*domain.IntercompanyEntry
}

func newStubStoreReal() *stubStoreReal {
	return &stubStoreReal{entries: make(map[string]*domain.IntercompanyEntry)}
}

func (s *stubStoreReal) CreateEntry(_ context.Context, entry *domain.IntercompanyEntry) error {
	s.entries[entry.IntercompanyEntryID] = entry
	return nil
}

func (s *stubStoreReal) GetEntry(_ context.Context, id string) (*domain.IntercompanyEntry, error) {
	entry, ok := s.entries[id]
	if !ok {
		return nil, domain.ErrEntryNotFound
	}
	return entry, nil
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

func (s *stubStoreReal) UpdateMatch(_ context.Context, id, targetJournalID, matchStatus string, mismatchReason *string) error {
	entry, ok := s.entries[id]
	if !ok {
		return domain.ErrEntryNotFound
	}
	entry.TargetJournalID = &targetJournalID
	entry.MatchStatus = matchStatus
	entry.MismatchReason = mismatchReason
	entry.UpdatedAt = time.Now().UTC()
	return nil
}

type stubPublisher struct {
	created, posted, mismatched int
}

func (p *stubPublisher) PublishEntryCreated(_ context.Context, _ string, _ domain.IntercompanyEntry) {
	p.created++
}
func (p *stubPublisher) PublishEntryPosted(_ context.Context, _ string, _ domain.IntercompanyEntry) {
	p.posted++
}
func (p *stubPublisher) PublishMismatchDetected(_ context.Context, _ string, _ domain.IntercompanyEntry, _ string) {
	p.mismatched++
}

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

func newRouter(s *stubStoreReal, pub *stubPublisher, authz *stubAuthZ, leg *stubLedger) chi.Router {
	r := chi.NewRouter()
	r.Use(func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			req = req.WithContext(middleware.WithTenant(req.Context(), "tenant-abc"))
			next.ServeHTTP(w, req)
		})
	})
	h := handler.New(s, pub, authz, leg, zap.NewNop())
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
	r := newRouter(newStubStoreReal(), &stubPublisher{}, &stubAuthZ{}, &stubLedger{})
	rr := doReq(r, http.MethodPost, "/v1/intercompany/entries/", map[string]any{
		"source_legal_entity_id": "le-us",
		"target_legal_entity_id": "le-uk",
		"source_journal_id":     "j-001",
		"amount":                5000.0,
		"currency_code":         "USD",
	}, "")
	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 got %d", rr.Code)
	}
}

func TestCreateEntry_SameEntityForbidden(t *testing.T) {
	r := newRouter(newStubStoreReal(), &stubPublisher{}, &stubAuthZ{}, &stubLedger{})
	rr := doReq(r, http.MethodPost, "/v1/intercompany/entries/", map[string]any{
		"source_legal_entity_id": "le-same",
		"target_legal_entity_id": "le-same",
		"source_journal_id":     "j-001",
		"amount":                5000.0,
		"currency_code":         "USD",
	}, "principal-1")
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 got %d", rr.Code)
	}
}

func TestCreateEntry_NegativeAmount(t *testing.T) {
	r := newRouter(newStubStoreReal(), &stubPublisher{}, &stubAuthZ{}, &stubLedger{})
	rr := doReq(r, http.MethodPost, "/v1/intercompany/entries/", map[string]any{
		"source_legal_entity_id": "le-us",
		"target_legal_entity_id": "le-uk",
		"source_journal_id":     "j-001",
		"amount":                -100.0,
		"currency_code":         "USD",
	}, "principal-1")
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 got %d", rr.Code)
	}
}

func TestCreateEntry_HappyPath(t *testing.T) {
	pub := &stubPublisher{}
	r := newRouter(newStubStoreReal(), pub, &stubAuthZ{}, &stubLedger{})
	rr := doReq(r, http.MethodPost, "/v1/intercompany/entries/", map[string]any{
		"source_legal_entity_id": "le-us",
		"target_legal_entity_id": "le-uk",
		"source_journal_id":     "j-001",
		"amount":                5000.0,
		"currency_code":         "USD",
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
	if pub.created != 1 {
		t.Errorf("expected 1 created event got %d", pub.created)
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
	r := newRouter(s, &stubPublisher{}, &stubAuthZ{}, &stubLedger{})
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
	pub := &stubPublisher{}
	leg := &stubLedger{
		journal: &ledger.JournalDetail{
			JournalID:     "target-j-wrong-entity",
			LegalEntityID: "le-de", // Doesn't match target le-uk
			Status:        "FINALIZED",
			Lines:         []ledger.JournalLine{{AccountCode: "1000", DebitAmount: 5000.0}},
		},
	}
	r := newRouter(s, pub, &stubAuthZ{}, leg)
	rr := doReq(r, http.MethodPost, "/v1/intercompany/entries/entry-1/match", map[string]any{
		"target_journal_id": "target-j-wrong-entity",
	}, "principal-1")

	if rr.Code != http.StatusUnprocessableEntity {
		t.Fatalf("expected 422 got %d", rr.Code)
	}
	if pub.mismatched != 1 {
		t.Errorf("expected 1 mismatch event got %d", pub.mismatched)
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
	pub := &stubPublisher{}
	leg := &stubLedger{
		journal: &ledger.JournalDetail{
			JournalID:     "target-j-001",
			LegalEntityID: "le-uk",
			Status:        "FINALIZED",
			Lines:         []ledger.JournalLine{{AccountCode: "1000", DebitAmount: 5000.0}},
		},
	}
	r := newRouter(s, pub, &stubAuthZ{}, leg)
	rr := doReq(r, http.MethodPost, "/v1/intercompany/entries/entry-1/match", map[string]any{
		"target_journal_id": "target-j-001",
	}, "principal-1")

	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200 got %d: %s", rr.Code, rr.Body.String())
	}
	if pub.posted != 1 {
		t.Errorf("expected 1 posted event got %d", pub.posted)
	}
	if s.entries["entry-1"].MatchStatus != "MATCHED" {
		t.Errorf("expected store match_status to be MATCHED, got %q", s.entries["entry-1"].MatchStatus)
	}
}
