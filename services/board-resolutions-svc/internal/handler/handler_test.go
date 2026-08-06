package handler

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

	"zoiko.io/board-resolutions-svc/internal/domain"
	"zoiko.io/board-resolutions-svc/internal/events"
)

type stubStore struct {
	meetings    map[string]*domain.BoardMeeting
	resolutions map[string]*domain.BoardResolution
}

func newStubStore() *stubStore {
	return &stubStore{
		meetings:    make(map[string]*domain.BoardMeeting),
		resolutions: make(map[string]*domain.BoardResolution),
	}
}

func (s *stubStore) CreateMeeting(_ context.Context, m *domain.BoardMeeting) error {
	if m.MeetingID == "" {
		m.MeetingID = "mtg-test-001"
	}
	if m.Status == "" {
		m.Status = domain.MeetingStatusScheduled
	}
	s.meetings[m.MeetingID] = m
	return nil
}

func (s *stubStore) GetMeeting(_ context.Context, id string) (*domain.BoardMeeting, error) {
	if m, ok := s.meetings[id]; ok {
		return m, nil
	}
	return nil, domain.ErrMeetingNotFound
}

func (s *stubStore) ListMeetings(_ context.Context, _ string) ([]domain.BoardMeeting, error) {
	var out []domain.BoardMeeting
	for _, m := range s.meetings {
		out = append(out, *m)
	}
	return out, nil
}

func (s *stubStore) CreateResolution(_ context.Context, r *domain.BoardResolution) error {
	if r.ResolutionID == "" {
		r.ResolutionID = "res-test-001"
	}
	if r.Status == "" {
		r.Status = domain.ResolutionStatusProposed
	}
	s.resolutions[r.ResolutionID] = r
	return nil
}

func (s *stubStore) GetResolution(_ context.Context, id string) (*domain.BoardResolution, error) {
	if r, ok := s.resolutions[id]; ok {
		return r, nil
	}
	return nil, domain.ErrResolutionNotFound
}

func (s *stubStore) ListResolutions(_ context.Context, _, _, _ string) ([]domain.BoardResolution, error) {
	var out []domain.BoardResolution
	for _, r := range s.resolutions {
		out = append(out, *r)
	}
	return out, nil
}

func (s *stubStore) RecordVotes(_ context.Context, id string, req *domain.RecordVotesRequest) (*domain.BoardResolution, error) {
	r, ok := s.resolutions[id]
	if !ok {
		return nil, domain.ErrResolutionNotFound
	}
	r.VotesFor = req.VotesFor
	r.VotesAgainst = req.VotesAgainst
	r.Abstentions = req.Abstentions
	return r, nil
}

func (s *stubStore) PassResolution(_ context.Context, id string, req *domain.PassResolutionRequest) (*domain.BoardResolution, error) {
	r, ok := s.resolutions[id]
	if !ok {
		return nil, domain.ErrResolutionNotFound
	}
	if r.Status == domain.ResolutionStatusPassed {
		return nil, domain.ErrResolutionAlreadyFinalized
	}
	r.Status = domain.ResolutionStatusPassed
	r.PassedBy = &req.PassedBy
	return r, nil
}

type stubPublisher struct{}

func (p *stubPublisher) Publish(_ context.Context, _, _, _ string, _ interface{}) error {
	return nil
}

var _ events.Publisher = (*stubPublisher)(nil)

type stubAuthz struct{}

func (s *stubAuthz) CheckAllowed(_ context.Context, _, _, _ string) error {
	return nil
}

var _ AuthZClient = (*stubAuthz)(nil)

func newTestHandler() *Handler {
	logger, _ := zap.NewDevelopment()
	return New(newStubStore(), &stubPublisher{}, &stubAuthz{}, logger)
}

func buildRequest(method, path string, body interface{}) *http.Request {
	var buf bytes.Buffer
	if body != nil {
		_ = json.NewEncoder(&buf).Encode(body)
	}
	r := httptest.NewRequest(method, path, &buf)
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("X-Tenant-Id", "tenant-test-01")
	r.Header.Set("X-Principal-Id", "principal-test-01")
	return r
}

func TestCreateMeeting(t *testing.T) {
	h := newTestHandler()
	body := domain.CreateMeetingRequest{
		LegalEntityID: "le-001",
		Title:         "Q1 Board of Directors Meeting",
		ScheduledAt:   time.Now().Add(24 * time.Hour),
		Location:      "Executive Boardroom",
		EffectiveFrom: "2026-01-01",
		CreatedBy:     "secretary-001",
	}
	w := httptest.NewRecorder()
	h.CreateMeeting(w, buildRequest(http.MethodPost, "/v1/meetings", body))
	if w.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d — %s", w.Code, w.Body.String())
	}
	var resp domain.BoardMeeting
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}
	if resp.Title != "Q1 Board of Directors Meeting" {
		t.Errorf("unexpected title: %s", resp.Title)
	}
	if resp.Status != domain.MeetingStatusScheduled {
		t.Errorf("expected SCHEDULED, got %s", resp.Status)
	}
}

func TestPassResolution(t *testing.T) {
	h := newTestHandler()
	r := chi.NewRouter()
	RegisterRoutes(r, h)

	// First create a resolution
	body := domain.CreateResolutionRequest{
		MeetingID:        "mtg-001",
		LegalEntityID:    "le-001",
		ResolutionNumber: "RES-2026-001",
		Title:            "Approve Annual Budget",
		Content:          "Resolved that the proposed 2026 operational budget be approved...",
		Category:         domain.ResolutionCategoryFinancial,
		EffectiveFrom:    "2026-01-01",
		CreatedBy:        "chairperson-001",
	}
	wCreate := httptest.NewRecorder()
	r.ServeHTTP(wCreate, buildRequest(http.MethodPost, "/v1/resolutions", body))
	var created domain.BoardResolution
	_ = json.NewDecoder(wCreate.Body).Decode(&created)

	// Pass the resolution
	passBody := domain.PassResolutionRequest{
		PassedBy: "chairperson-001",
	}
	wPass := httptest.NewRecorder()
	r.ServeHTTP(wPass, buildRequest(http.MethodPost, "/v1/resolutions/"+created.ResolutionID+"/pass", passBody))
	if wPass.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d — %s", wPass.Code, wPass.Body.String())
	}
	var passed domain.BoardResolution
	_ = json.NewDecoder(wPass.Body).Decode(&passed)
	if passed.Status != domain.ResolutionStatusPassed {
		t.Errorf("expected PASSED, got %s", passed.Status)
	}
}

func TestPassResolution_BySameCreator_Returns403(t *testing.T) {
	// Segregation of Duties (docs/original_doc/zoiko_suite_doc1.txt §12.3):
	// the resolution's drafter/creator (identified by the deciding
	// principal, X-Principal-Id) may not be the one who passes it.
	h := newTestHandler()
	r := chi.NewRouter()
	RegisterRoutes(r, h)

	// Create a resolution as the same principal that will later attempt
	// to pass it. CreatedBy is set from X-Principal-Id server-side via
	// buildRequest's fixed "principal-test-01" header, not from the
	// request body's CreatedBy field, so the store's CreatedBy must match
	// that header for this test to exercise the self-approval path.
	body := domain.CreateResolutionRequest{
		MeetingID:        "mtg-002",
		LegalEntityID:    "le-001",
		ResolutionNumber: "RES-2026-002",
		Title:            "Approve Executive Compensation",
		Content:          "Resolved that executive compensation package be approved...",
		Category:         domain.ResolutionCategoryExecutive,
		EffectiveFrom:    "2026-01-01",
		CreatedBy:        "principal-test-01",
	}
	wCreate := httptest.NewRecorder()
	r.ServeHTTP(wCreate, buildRequest(http.MethodPost, "/v1/resolutions", body))
	var created domain.BoardResolution
	_ = json.NewDecoder(wCreate.Body).Decode(&created)
	if created.CreatedBy != "principal-test-01" {
		t.Fatalf("expected created resolution's CreatedBy to be principal-test-01, got %s", created.CreatedBy)
	}

	passBody := domain.PassResolutionRequest{
		PassedBy: "principal-test-01",
	}
	wPass := httptest.NewRecorder()
	r.ServeHTTP(wPass, buildRequest(http.MethodPost, "/v1/resolutions/"+created.ResolutionID+"/pass", passBody))
	if wPass.Code != http.StatusForbidden {
		t.Fatalf("expected 403 passing a resolution created by the same principal, got %d — %s", wPass.Code, wPass.Body.String())
	}
}
