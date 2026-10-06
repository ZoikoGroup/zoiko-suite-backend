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
	"zoiko.io/board-resolutions-svc/internal/evidencereq"
	"zoiko.io/board-resolutions-svc/internal/middleware"
)

type stubStore struct {
	meetings    map[string]*domain.BoardMeeting
	resolutions map[string]*domain.BoardResolution
	roster      map[string]map[string]bool
	votes       map[string][]domain.CastVoteRecord

	lastMeetingFilter    domain.MeetingFilter
	lastResolutionFilter domain.ResolutionFilter
}

func newStubStore() *stubStore {
	return &stubStore{
		meetings:    make(map[string]*domain.BoardMeeting),
		resolutions: make(map[string]*domain.BoardResolution),
		roster:      make(map[string]map[string]bool),
		votes:       make(map[string][]domain.CastVoteRecord),
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

func (s *stubStore) ListMeetings(_ context.Context, f domain.MeetingFilter) ([]domain.BoardMeeting, error) {
	s.lastMeetingFilter = f
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

func (s *stubStore) ListResolutions(_ context.Context, f domain.ResolutionFilter) ([]domain.BoardResolution, error) {
	s.lastResolutionFilter = f
	var out []domain.BoardResolution
	for _, r := range s.resolutions {
		out = append(out, *r)
	}
	return out, nil
}

func (s *stubStore) OpenVoting(_ context.Context, id string, voterPrincipalIDs []string, quorumThreshold int) (*domain.BoardResolution, error) {
	r, ok := s.resolutions[id]
	if !ok {
		return nil, domain.ErrResolutionNotFound
	}
	if len(voterPrincipalIDs) == 0 {
		return nil, domain.ErrEmptyRoster
	}
	if quorumThreshold < 1 || quorumThreshold > len(voterPrincipalIDs) {
		return nil, domain.ErrInvalidQuorumThreshold
	}
	if r.Status != domain.ResolutionStatusProposed {
		if r.Status.IsFinal() {
			return nil, domain.ErrResolutionAlreadyFinalized
		}
		return nil, domain.ErrResolutionNotOpen
	}
	roster := make(map[string]bool, len(voterPrincipalIDs))
	for _, v := range voterPrincipalIDs {
		roster[v] = true
	}
	s.roster[id] = roster
	q := quorumThreshold
	r.Status = domain.ResolutionStatusOpen
	r.QuorumThreshold = &q
	return r, nil
}

func (s *stubStore) CastVote(_ context.Context, id, voterPrincipalID string, vote domain.Vote, castBy string) (*domain.CastVoteRecord, error) {
	r, ok := s.resolutions[id]
	if !ok {
		return nil, domain.ErrResolutionNotFound
	}
	if r.Status != domain.ResolutionStatusOpen {
		return nil, domain.ErrResolutionNotOpen
	}
	if !s.roster[id][voterPrincipalID] {
		return nil, domain.ErrNotEligibleVoter
	}
	for _, v := range s.votes[id] {
		if v.VoterPrincipalID == voterPrincipalID {
			return nil, domain.ErrAlreadyVoted
		}
	}
	rec := domain.CastVoteRecord{ResolutionID: id, VoterPrincipalID: voterPrincipalID, Vote: vote, CastBy: castBy}
	s.votes[id] = append(s.votes[id], rec)
	switch vote {
	case domain.VoteFor:
		r.VotesFor++
	case domain.VoteAgainst:
		r.VotesAgainst++
	case domain.VoteAbstain:
		r.Abstentions++
	}
	return &rec, nil
}

func (s *stubStore) CloseVoting(_ context.Context, id, closedBy string, req *domain.CloseVotingRequest) (*domain.BoardResolution, error) {
	r, ok := s.resolutions[id]
	if !ok {
		return nil, domain.ErrResolutionNotFound
	}
	if r.Status != domain.ResolutionStatusOpen {
		return nil, domain.ErrResolutionNotOpen
	}
	if r.CreatedBy == closedBy {
		return nil, domain.ErrSelfApprovalNotAllowed
	}
	votesCast := len(s.votes[id])
	threshold := 0
	if r.QuorumThreshold != nil {
		threshold = *r.QuorumThreshold
	}
	if votesCast >= threshold && r.VotesFor > r.VotesAgainst {
		r.Status = domain.ResolutionStatusPassed
		r.PassedBy = &closedBy
		r.DocumentVaultID = req.DocumentVaultID
	} else {
		r.Status = domain.ResolutionStatusFailed
	}
	return r, nil
}

func (s *stubStore) SupersedeResolution(_ context.Context, id string, req *domain.SupersedeResolutionRequest) (*domain.BoardResolution, error) {
	r, ok := s.resolutions[id]
	if !ok {
		return nil, domain.ErrResolutionNotFound
	}
	if r.Status != domain.ResolutionStatusPassed {
		return nil, domain.ErrResolutionNotPassed
	}
	r.Status = domain.ResolutionStatusSuperseded
	r.SupersededBy = &req.SupersededBy
	return r, nil
}

func (s *stubStore) GetVoterRoster(_ context.Context, id string) ([]domain.VoterRosterEntry, error) {
	var out []domain.VoterRosterEntry
	for v := range s.roster[id] {
		out = append(out, domain.VoterRosterEntry{ResolutionID: id, VoterPrincipalID: v})
	}
	return out, nil
}

func (s *stubStore) GetVoteLedger(_ context.Context, id string) ([]domain.CastVoteRecord, error) {
	return s.votes[id], nil
}

func (s *stubStore) GetQuorumEvidence(_ context.Context, id string) (*domain.QuorumEvidence, error) {
	r, ok := s.resolutions[id]
	if !ok {
		return nil, domain.ErrResolutionNotFound
	}
	threshold := 0
	if r.QuorumThreshold != nil {
		threshold = *r.QuorumThreshold
	}
	votesCast := len(s.votes[id])
	return &domain.QuorumEvidence{
		ResolutionID: id, RosterSize: len(s.roster[id]), QuorumThreshold: threshold,
		VotesCast: votesCast, VotesFor: r.VotesFor, VotesAgainst: r.VotesAgainst, Abstentions: r.Abstentions,
		QuorumMet: votesCast >= threshold,
	}, nil
}

type stubPublisher struct{ published []string }

func (p *stubPublisher) Publish(_ context.Context, params events.PublishParams) error {
	p.published = append(p.published, params.EventType)
	return nil
}

var _ events.Publisher = (*stubPublisher)(nil)

type stubAuthz struct {
	err   error
	calls []string
}

func (s *stubAuthz) CheckAllowed(_ context.Context, _, _, actionType string) error {
	s.calls = append(s.calls, actionType)
	return s.err
}

var _ AuthZClient = (*stubAuthz)(nil)

// stubEvidenceReq grants sufficiency by default, matching a
// SATISFIED/NO_REQUIREMENTS_DEFINED outcome but skipping the network call.
// Tests can set err to exercise the fail-closed path.
type stubEvidenceReq struct {
	err            error
	lastDomainCode string
}

func (s *stubEvidenceReq) EvaluateSufficient(_ context.Context, _, _, domainCode, _, _, _ string, _ []evidencereq.Artifact) error {
	s.lastDomainCode = domainCode
	return s.err
}

var _ EvidenceReqClient = (*stubEvidenceReq)(nil)

// newTestRouter wires the SAME middleware chain the server does.
//
// The old harness called handler methods directly with a request that merely
// carried an X-Tenant-Id header. The handlers read the tenant from the request
// CONTEXT, which only TenantContextMiddleware populates — so every test ran
// with no tenant at all and passed regardless, because the middleware's
// missing-header fallback ("default") was also the fallback inside
// GetTenantID. A harness that skips the middleware cannot see a tenant-scoping
// bug, which is precisely the class of bug this service had.
func newTestRouter(s *stubStore, pub *stubPublisher, az *stubAuthz, er *stubEvidenceReq) chi.Router {
	logger := zap.NewNop()
	h := New(s, pub, az, er, logger)
	r := chi.NewRouter()
	r.Use(middleware.TenantContextMiddleware)
	RegisterRoutes(r, h)
	return r
}

func newDefaultRouter() (chi.Router, *stubStore) {
	s := newStubStore()
	return newTestRouter(s, &stubPublisher{}, &stubAuthz{}, &stubEvidenceReq{}), s
}

const (
	testTenant    = "tenant-test-01"
	testPrincipal = "principal-test-01"
)

func buildRequest(method, path string, body interface{}) *http.Request {
	return buildRequestAs(method, path, body, testPrincipal, testTenant)
}

func buildRequestAs(method, path string, body interface{}, principalID, tenantID string) *http.Request {
	var buf bytes.Buffer
	if body != nil {
		_ = json.NewEncoder(&buf).Encode(body)
	}
	r := httptest.NewRequest(method, path, &buf)
	r.Header.Set("Content-Type", "application/json")
	if tenantID != "" {
		r.Header.Set("X-Tenant-Id", tenantID)
	}
	if principalID != "" {
		r.Header.Set("X-Principal-Id", principalID)
	}
	return r
}

// createResolution files a PROPOSED resolution as principalID and returns it.
func createResolution(t *testing.T, r chi.Router, principalID string) domain.BoardResolution {
	t.Helper()
	body := domain.CreateResolutionRequest{
		MeetingID:        "",
		LegalEntityID:    "le-001",
		ResolutionNumber: "RES-2026-001",
		Title:            "Approve Annual Budget",
		Content:          "Resolved that the proposed 2026 operational budget be approved...",
		Category:         domain.ResolutionCategoryFinancial,
		EffectiveFrom:    "2026-01-01",
	}
	w := httptest.NewRecorder()
	r.ServeHTTP(w, buildRequestAs(http.MethodPost, "/v1/resolutions", body, principalID, testTenant))
	if w.Code != http.StatusCreated {
		t.Fatalf("setup: expected 201 creating a resolution, got %d — %s", w.Code, w.Body.String())
	}
	var created domain.BoardResolution
	if err := json.NewDecoder(w.Body).Decode(&created); err != nil {
		t.Fatalf("setup: decode created resolution: %v", err)
	}
	return created
}

func TestCreateMeeting(t *testing.T) {
	r, _ := newDefaultRouter()
	body := domain.CreateMeetingRequest{
		LegalEntityID: "le-001",
		Title:         "Q1 Board of Directors Meeting",
		ScheduledAt:   time.Now().Add(24 * time.Hour),
		Location:      "Executive Boardroom",
		EffectiveFrom: "2026-01-01",
	}
	w := httptest.NewRecorder()
	r.ServeHTTP(w, buildRequest(http.MethodPost, "/v1/meetings", body))
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
	if resp.TenantID != testTenant {
		t.Errorf("meeting recorded under tenant %q, want the caller's %q", resp.TenantID, testTenant)
	}
}

// openAndVote drives a resolution through OpenVoting and a single FOR vote so
// tests exercising CloseVoting don't each re-derive the setup.
func openAndVote(t *testing.T, r chi.Router, resolutionID, voterID string) {
	t.Helper()
	wOpen := httptest.NewRecorder()
	r.ServeHTTP(wOpen, buildRequest(http.MethodPost, "/v1/resolutions/"+resolutionID+"/open-voting",
		domain.OpenVotingRequest{VoterPrincipalIDs: []string{voterID}, QuorumThreshold: 1}))
	if wOpen.Code != http.StatusOK {
		t.Fatalf("setup: open-voting: expected 200, got %d — %s", wOpen.Code, wOpen.Body.String())
	}
	wVote := httptest.NewRecorder()
	r.ServeHTTP(wVote, buildRequestAs(http.MethodPost, "/v1/resolutions/"+resolutionID+"/vote",
		domain.CastVoteRequest{Vote: domain.VoteFor}, voterID, testTenant))
	if wVote.Code != http.StatusOK {
		t.Fatalf("setup: vote: expected 200, got %d — %s", wVote.Code, wVote.Body.String())
	}
}

func TestCloseVoting(t *testing.T) {
	r, _ := newDefaultRouter()
	created := createResolution(t, r, "drafter-001")
	openAndVote(t, r, created.ResolutionID, "director-001")

	// A different principal closes it — the drafter may not.
	wClose := httptest.NewRecorder()
	r.ServeHTTP(wClose, buildRequestAs(http.MethodPost, "/v1/resolutions/"+created.ResolutionID+"/close-voting",
		domain.CloseVotingRequest{}, "chairperson-001", testTenant))
	if wClose.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d — %s", wClose.Code, wClose.Body.String())
	}
	var passed domain.BoardResolution
	_ = json.NewDecoder(wClose.Body).Decode(&passed)
	if passed.Status != domain.ResolutionStatusPassed {
		t.Errorf("expected PASSED, got %s", passed.Status)
	}
	if passed.PassedBy == nil || *passed.PassedBy != "chairperson-001" {
		t.Errorf("expected the pass attributed to the authenticated principal, got %v", passed.PassedBy)
	}
}

func TestCloseVoting_EvidenceMissing_Returns422(t *testing.T) {
	r := newTestRouter(newStubStore(), &stubPublisher{}, &stubAuthz{},
		&stubEvidenceReq{err: evidencereq.ErrEvidenceMissing})
	created := createResolution(t, r, "drafter-001")
	openAndVote(t, r, created.ResolutionID, "director-001")

	w := httptest.NewRecorder()
	r.ServeHTTP(w, buildRequestAs(http.MethodPost, "/v1/resolutions/"+created.ResolutionID+"/close-voting",
		domain.CloseVotingRequest{}, "chairperson-001", testTenant))
	if w.Code != http.StatusUnprocessableEntity {
		t.Fatalf("expected 422 when required evidence is missing, got %d — %s", w.Code, w.Body.String())
	}
}

func TestCloseVoting_EvidenceServiceUnavailable_Returns503(t *testing.T) {
	r := newTestRouter(newStubStore(), &stubPublisher{}, &stubAuthz{},
		&stubEvidenceReq{err: evidencereq.ErrServiceUnavailable})
	created := createResolution(t, r, "drafter-001")
	openAndVote(t, r, created.ResolutionID, "director-001")

	w := httptest.NewRecorder()
	r.ServeHTTP(w, buildRequestAs(http.MethodPost, "/v1/resolutions/"+created.ResolutionID+"/close-voting",
		domain.CloseVotingRequest{}, "chairperson-001", testTenant))
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected 503 when evidence-requirements-svc is unavailable, got %d — %s", w.Code, w.Body.String())
	}
}

// Segregation of Duties (docs/original_doc/zoiko_suite_doc1.txt §12.3): the
// resolution's drafter may not be the one who closes voting on it.
func TestCloseVoting_BySameCreator_Returns403(t *testing.T) {
	r, _ := newDefaultRouter()
	created := createResolution(t, r, testPrincipal)
	if created.CreatedBy != testPrincipal {
		t.Fatalf("expected created_by to be the authenticated principal, got %s", created.CreatedBy)
	}
	openAndVote(t, r, created.ResolutionID, "director-001")

	w := httptest.NewRecorder()
	r.ServeHTTP(w, buildRequest(http.MethodPost, "/v1/resolutions/"+created.ResolutionID+"/close-voting",
		domain.CloseVotingRequest{}))
	if w.Code != http.StatusForbidden {
		t.Fatalf("expected 403 closing voting on a resolution created by the same principal, got %d — %s", w.Code, w.Body.String())
	}
}

// Quorum and voter eligibility are evaluated against a frozen as-of
// entitlement population (LEG-04 §6.1) — a non-roster principal must not be
// able to vote at all.
func TestCastVote_NonRosterVoter_Returns403(t *testing.T) {
	r, _ := newDefaultRouter()
	created := createResolution(t, r, "drafter-001")
	wOpen := httptest.NewRecorder()
	r.ServeHTTP(wOpen, buildRequest(http.MethodPost, "/v1/resolutions/"+created.ResolutionID+"/open-voting",
		domain.OpenVotingRequest{VoterPrincipalIDs: []string{"director-001"}, QuorumThreshold: 1}))
	if wOpen.Code != http.StatusOK {
		t.Fatalf("setup: open-voting: expected 200, got %d", wOpen.Code)
	}

	w := httptest.NewRecorder()
	r.ServeHTTP(w, buildRequestAs(http.MethodPost, "/v1/resolutions/"+created.ResolutionID+"/vote",
		domain.CastVoteRequest{Vote: domain.VoteFor}, "not-a-director", testTenant))
	if w.Code != http.StatusForbidden {
		t.Fatalf("expected 403 for a non-roster voter, got %d — %s", w.Code, w.Body.String())
	}
}

func TestCastVote_InvalidValue_Returns400(t *testing.T) {
	r, _ := newDefaultRouter()
	created := createResolution(t, r, "drafter-001")

	w := httptest.NewRecorder()
	r.ServeHTTP(w, buildRequest(http.MethodPost, "/v1/resolutions/"+created.ResolutionID+"/vote",
		map[string]string{"vote": "MAYBE"}))
	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for an invalid vote value, got %d — %s", w.Code, w.Body.String())
	}
}

func TestOpenVoting_EmptyRoster_Returns400(t *testing.T) {
	r, _ := newDefaultRouter()
	created := createResolution(t, r, "drafter-001")

	w := httptest.NewRecorder()
	r.ServeHTTP(w, buildRequest(http.MethodPost, "/v1/resolutions/"+created.ResolutionID+"/open-voting",
		domain.OpenVotingRequest{VoterPrincipalIDs: nil, QuorumThreshold: 1}))
	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for an empty roster, got %d — %s", w.Code, w.Body.String())
	}
}

func TestSupersedeResolution_NotPassed_Returns409(t *testing.T) {
	r, _ := newDefaultRouter()
	created := createResolution(t, r, "drafter-001")

	w := httptest.NewRecorder()
	r.ServeHTTP(w, buildRequest(http.MethodPost, "/v1/resolutions/"+created.ResolutionID+"/supersede",
		domain.SupersedeResolutionRequest{SupersededBy: "res-999"}))
	if w.Code != http.StatusConflict {
		t.Fatalf("expected 409 superseding a non-PASSED resolution, got %d — %s", w.Code, w.Body.String())
	}
}

// ── the gaps closed in this pass ──────────────────────────────────────────────

// created_by used to be taken verbatim from the request body, so a drafter
// could file their own resolution under someone else's name and then pass it:
// the SoD check compares created_by against the passing principal, and those
// two strings would no longer match. The one control the doctrine rests on was
// defeated by a field the same caller filled in.
func TestCreateResolution_CreatedByIsTheAuthenticatedPrincipal(t *testing.T) {
	r, store := newDefaultRouter()

	body := domain.CreateResolutionRequest{
		LegalEntityID: "le-001",
		Title:         "Approve Executive Compensation",
		Content:       "Resolved...",
		Category:      domain.ResolutionCategoryExecutive,
		EffectiveFrom: "2026-01-01",
		CreatedBy:     "somebody-else", // the forgery attempt
	}
	w := httptest.NewRecorder()
	r.ServeHTTP(w, buildRequest(http.MethodPost, "/v1/resolutions", body))
	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for a created_by naming another principal, got %d — %s", w.Code, w.Body.String())
	}
	if len(store.resolutions) != 0 {
		t.Fatalf("a refused create must not store a resolution, found %d", len(store.resolutions))
	}

	// Omitted entirely, the attribution comes from the header.
	body.CreatedBy = ""
	w = httptest.NewRecorder()
	r.ServeHTTP(w, buildRequest(http.MethodPost, "/v1/resolutions", body))
	if w.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d — %s", w.Code, w.Body.String())
	}
	var created domain.BoardResolution
	_ = json.NewDecoder(w.Body).Decode(&created)
	if created.CreatedBy != testPrincipal {
		t.Errorf("created_by is %q, want the authenticated principal %q", created.CreatedBy, testPrincipal)
	}
}

func TestCreateMeeting_CreatedByIsTheAuthenticatedPrincipal(t *testing.T) {
	r, _ := newDefaultRouter()
	body := domain.CreateMeetingRequest{
		LegalEntityID: "le-001",
		Title:         "Q2 Board Meeting",
		ScheduledAt:   time.Now().Add(24 * time.Hour),
		EffectiveFrom: "2026-01-01",
		CreatedBy:     "somebody-else",
	}
	w := httptest.NewRecorder()
	r.ServeHTTP(w, buildRequest(http.MethodPost, "/v1/meetings", body))
	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for a created_by naming another principal, got %d — %s", w.Code, w.Body.String())
	}
}

// Every request must carry a tenant. The middleware used to substitute the
// literal tenant "default", so unscoped callers shared one bucket.
func TestRequests_WithoutTenantScope_Are401(t *testing.T) {
	r, _ := newDefaultRouter()

	cases := []struct {
		name, method, path string
		body               interface{}
	}{
		{"list meetings", http.MethodGet, "/v1/meetings", nil},
		{"get meeting", http.MethodGet, "/v1/meetings/mtg-1", nil},
		{"list resolutions", http.MethodGet, "/v1/resolutions", nil},
		{"get resolution", http.MethodGet, "/v1/resolutions/res-1", nil},
		{"create meeting", http.MethodPost, "/v1/meetings", domain.CreateMeetingRequest{
			LegalEntityID: "le-001", Title: "T", ScheduledAt: time.Now(), EffectiveFrom: "2026-01-01"}},
		{"create resolution", http.MethodPost, "/v1/resolutions", domain.CreateResolutionRequest{
			LegalEntityID: "le-001", Title: "T", Content: "C",
			Category: domain.ResolutionCategoryGovernance, EffectiveFrom: "2026-01-01"}},
		{"open-voting", http.MethodPost, "/v1/resolutions/res-1/open-voting", domain.OpenVotingRequest{}},
		{"vote", http.MethodPost, "/v1/resolutions/res-1/vote", domain.CastVoteRequest{}},
		{"close-voting", http.MethodPost, "/v1/resolutions/res-1/close-voting", domain.CloseVotingRequest{}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			w := httptest.NewRecorder()
			r.ServeHTTP(w, buildRequestAs(tc.method, tc.path, tc.body, testPrincipal, ""))
			if w.Code != http.StatusUnauthorized {
				t.Fatalf("expected 401 without a tenant, got %d — %s", w.Code, w.Body.String())
			}
		})
	}
}

func TestRequests_WithoutPrincipal_Are401(t *testing.T) {
	r, _ := newDefaultRouter()
	for _, path := range []string{"/v1/meetings", "/v1/resolutions"} {
		w := httptest.NewRecorder()
		r.ServeHTTP(w, buildRequestAs(http.MethodGet, path, nil, "", testTenant))
		if w.Code != http.StatusUnauthorized {
			t.Fatalf("%s: expected 401 without a principal, got %d", path, w.Code)
		}
	}
}

// The category is the domain_code sent to evidence-requirements-svc, so an
// unrecognised one asks the catalog about a domain that does not exist and
// comes back with no requirements — an evidence gate bypassed by a typo.
func TestCreateResolution_UnknownCategory_IsRejected(t *testing.T) {
	r, _ := newDefaultRouter()
	body := domain.CreateResolutionRequest{
		LegalEntityID: "le-001", Title: "T", Content: "C",
		Category: "FINANCIAL_", EffectiveFrom: "2026-01-01",
	}
	w := httptest.NewRecorder()
	r.ServeHTTP(w, buildRequest(http.MethodPost, "/v1/resolutions", body))
	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for an unrecognised category, got %d — %s", w.Code, w.Body.String())
	}
}

// legal_entity_id is the authorization scope. Empty, it used to be sent to
// authorization-svc, which rejects an empty scope — so a missing required
// field surfaced as "authorization service unavailable".
func TestCreateResolution_MissingLegalEntity_Is400NotAnAuthzFailure(t *testing.T) {
	r, _ := newDefaultRouter()
	body := domain.CreateResolutionRequest{
		Title: "T", Content: "C", Category: domain.ResolutionCategoryGovernance, EffectiveFrom: "2026-01-01",
	}
	w := httptest.NewRecorder()
	r.ServeHTTP(w, buildRequest(http.MethodPost, "/v1/resolutions", body))
	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for a missing legal_entity_id, got %d — %s", w.Code, w.Body.String())
	}
}

// A misspelled field used to be discarded silently.
func TestCreateResolution_UnknownField_IsRejected(t *testing.T) {
	r, _ := newDefaultRouter()
	w := httptest.NewRecorder()
	r.ServeHTTP(w, buildRequest(http.MethodPost, "/v1/resolutions", map[string]any{
		"legal_entity_id": "le-001",
		"titel":           "typo",
		"title":           "Approve",
		"content":         "C",
		"category":        "GOVERNANCE",
		"effective_from":  "2026-01-01",
	}))
	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for an unknown field, got %d — %s", w.Code, w.Body.String())
	}
}

func TestLists_ArePagedAndValidated(t *testing.T) {
	r, store := newDefaultRouter()

	for _, path := range []string{"/v1/meetings", "/v1/resolutions"} {
		for _, q := range []string{"?limit=abc", "?limit=0", "?limit=99999", "?offset=-1"} {
			w := httptest.NewRecorder()
			r.ServeHTTP(w, buildRequest(http.MethodGet, path+q, nil))
			if w.Code != http.StatusBadRequest {
				t.Errorf("%s%s: expected 400, got %d", path, q, w.Code)
			}
		}
	}

	w := httptest.NewRecorder()
	r.ServeHTTP(w, buildRequest(http.MethodGet, "/v1/resolutions", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	if store.lastResolutionFilter.Limit != defaultLimit {
		t.Errorf("expected a bounded default limit of %d, got %d", defaultLimit, store.lastResolutionFilter.Limit)
	}

	w = httptest.NewRecorder()
	r.ServeHTTP(w, buildRequest(http.MethodGet, "/v1/meetings?limit=5&offset=10", nil))
	if store.lastMeetingFilter.Limit != 5 || store.lastMeetingFilter.Offset != 10 {
		t.Errorf("paging not passed through: %+v", store.lastMeetingFilter)
	}
}

// A FAILED resolution's voting must not be re-closed into force.
func TestCloseVoting_AlreadyFailed_Returns409(t *testing.T) {
	store := newStubStore()
	r := newTestRouter(store, &stubPublisher{}, &stubAuthz{}, &stubEvidenceReq{})
	created := createResolution(t, r, "drafter-001")
	store.resolutions[created.ResolutionID].Status = domain.ResolutionStatusFailed

	w := httptest.NewRecorder()
	r.ServeHTTP(w, buildRequestAs(http.MethodPost, "/v1/resolutions/"+created.ResolutionID+"/close-voting",
		domain.CloseVotingRequest{}, "chairperson-001", testTenant))
	if w.Code != http.StatusConflict {
		t.Fatalf("expected 409 closing voting on a FAILED resolution, got %d — %s", w.Code, w.Body.String())
	}
}

func TestWrites_AreAuthorized(t *testing.T) {
	store := newStubStore()
	authz := &stubAuthz{}
	r := newTestRouter(store, &stubPublisher{}, authz, &stubEvidenceReq{})
	created := createResolution(t, r, "drafter-001")
	store.roster[created.ResolutionID] = map[string]bool{"director-001": true}
	q := 1
	store.resolutions[created.ResolutionID].Status = domain.ResolutionStatusOpen
	store.resolutions[created.ResolutionID].QuorumThreshold = &q

	// Every mutating route, asserted as a table rather than one test each, so
	// a new route added without a check is visible here.
	cases := []struct {
		name, method, path, action string
		body                       interface{}
	}{
		{"create meeting", http.MethodPost, "/v1/meetings", "MEETING_CREATE", domain.CreateMeetingRequest{
			LegalEntityID: "le-001", Title: "T", ScheduledAt: time.Now(), EffectiveFrom: "2026-01-01"}},
		{"create resolution", http.MethodPost, "/v1/resolutions", "RESOLUTION_CREATE", domain.CreateResolutionRequest{
			LegalEntityID: "le-001", Title: "T", Content: "C",
			Category: domain.ResolutionCategoryGovernance, EffectiveFrom: "2026-01-01"}},
		{"open-voting", http.MethodPost, "/v1/resolutions/" + created.ResolutionID + "/open-voting", "RESOLUTION_OPEN_VOTING",
			domain.OpenVotingRequest{VoterPrincipalIDs: []string{"director-001"}, QuorumThreshold: 1}},
		{"vote", http.MethodPost, "/v1/resolutions/" + created.ResolutionID + "/vote", "RESOLUTION_VOTE",
			domain.CastVoteRequest{Vote: domain.VoteFor}},
		{"close-voting", http.MethodPost, "/v1/resolutions/" + created.ResolutionID + "/close-voting", "RESOLUTION_CLOSE_VOTING",
			domain.CloseVotingRequest{}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			authz.calls = nil
			authz.err = domain.ErrSelfApprovalNotAllowed // any non-nil, non-denial error → fail closed
			w := httptest.NewRecorder()
			r.ServeHTTP(w, buildRequestAs(tc.method, tc.path, tc.body, "chairperson-001", testTenant))
			if w.Code != http.StatusServiceUnavailable {
				t.Fatalf("expected the write refused (503, fail closed) when authz errors, got %d — %s",
					w.Code, w.Body.String())
			}
			if len(authz.calls) != 1 || authz.calls[0] != tc.action {
				t.Fatalf("expected one %s authorization check, got %v", tc.action, authz.calls)
			}
			authz.err = nil
		})
	}
}

// The category travels to evidence-requirements-svc as the domain_code — the
// gate is only meaningful if it asks about the right domain.
func TestCloseVoting_SendsTheResolutionsCategoryAsDomainCode(t *testing.T) {
	er := &stubEvidenceReq{}
	r := newTestRouter(newStubStore(), &stubPublisher{}, &stubAuthz{}, er)
	created := createResolution(t, r, "drafter-001")
	openAndVote(t, r, created.ResolutionID, "director-001")

	w := httptest.NewRecorder()
	r.ServeHTTP(w, buildRequestAs(http.MethodPost, "/v1/resolutions/"+created.ResolutionID+"/close-voting",
		domain.CloseVotingRequest{}, "chairperson-001", testTenant))
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d — %s", w.Code, w.Body.String())
	}
	if er.lastDomainCode != string(domain.ResolutionCategoryFinancial) {
		t.Errorf("evidence gate asked about domain %q, want the resolution's own category %q",
			er.lastDomainCode, domain.ResolutionCategoryFinancial)
	}
}

func TestCloseVoting_PublishesResolutionPassed(t *testing.T) {
	pub := &stubPublisher{}
	r := newTestRouter(newStubStore(), pub, &stubAuthz{}, &stubEvidenceReq{})
	created := createResolution(t, r, "drafter-001")
	openAndVote(t, r, created.ResolutionID, "director-001")

	w := httptest.NewRecorder()
	r.ServeHTTP(w, buildRequestAs(http.MethodPost, "/v1/resolutions/"+created.ResolutionID+"/close-voting",
		domain.CloseVotingRequest{}, "chairperson-001", testTenant))
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d — %s", w.Code, w.Body.String())
	}
	var sawPassed bool
	for _, e := range pub.published {
		if e == "resolution.passed" {
			sawPassed = true
		}
	}
	if !sawPassed {
		t.Errorf("expected a resolution.passed event, got %v", pub.published)
	}
}
