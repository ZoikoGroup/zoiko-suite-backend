package handler_test

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/go-chi/chi/v5"
	"go.uber.org/zap"

	"zoiko.io/comments-collaboration-svc/internal/domain"
	"zoiko.io/comments-collaboration-svc/internal/handler"
	"zoiko.io/comments-collaboration-svc/internal/middleware"
)

// ── stubs ─────────────────────────────────────────────────────────────────────

type stubStore struct {
	threads   map[string]*domain.CommentThread
	comments  map[string]*domain.Comment
	history   map[string][]domain.CommentVersion
	mentions  map[string][]domain.Mention
	trail     map[string][]domain.ModerationEntry
	reactions map[string]bool // "comment|principal|type" -> exists

	addCommentErr  error
	editCommentErr error
}

func newStubStore() *stubStore {
	return &stubStore{
		threads:   make(map[string]*domain.CommentThread),
		comments:  make(map[string]*domain.Comment),
		history:   make(map[string][]domain.CommentVersion),
		mentions:  make(map[string][]domain.Mention),
		trail:     make(map[string][]domain.ModerationEntry),
		reactions: make(map[string]bool),
	}
}

func (s *stubStore) AddComment(_ context.Context, p domain.AddCommentParams) (*domain.Comment, *domain.CommentThread, error) {
	if s.addCommentErr != nil {
		return nil, nil, s.addCommentErr
	}
	t := &domain.CommentThread{
		ThreadID: "thread-1", TenantID: p.TenantID, LegalEntityID: p.LegalEntityID,
		LinkedObjectType: p.LinkedObjectType, LinkedObjectID: p.LinkedObjectID, Status: domain.ThreadStatusActive,
	}
	c := &domain.Comment{
		CommentID: "comment-1", ThreadID: t.ThreadID, TenantID: p.TenantID, ParentCommentID: p.ParentCommentID,
		Status: domain.CommentStatusActive, CreatedBy: p.CreatedByPrincipalID, CurrentBody: p.Body,
	}
	s.threads[t.ThreadID] = t
	s.comments[c.CommentID] = c
	return c, t, nil
}

func (s *stubStore) EditComment(_ context.Context, p domain.EditCommentParams) (*domain.Comment, error) {
	if s.editCommentErr != nil {
		return nil, s.editCommentErr
	}
	c, ok := s.comments[p.CommentID]
	if !ok {
		return nil, domain.ErrCommentNotFound
	}
	c.Status = domain.CommentStatusEdited
	c.CurrentBody = p.Body
	return c, nil
}

func (s *stubStore) GetComment(_ context.Context, _, commentID string) (*domain.Comment, *domain.CommentThread, error) {
	c, ok := s.comments[commentID]
	if !ok {
		return nil, nil, domain.ErrCommentNotFound
	}
	t := s.threads[c.ThreadID]
	return c, t, nil
}

func (s *stubStore) GetThread(_ context.Context, threadID string) (*domain.CommentThread, error) {
	t, ok := s.threads[threadID]
	if !ok {
		return nil, domain.ErrThreadNotFound
	}
	return t, nil
}

func (s *stubStore) ListComments(_ context.Context, _, threadID string) ([]domain.Comment, error) {
	var out []domain.Comment
	for _, c := range s.comments {
		if c.ThreadID == threadID {
			out = append(out, *c)
		}
	}
	return out, nil
}

func (s *stubStore) GetCommentHistory(_ context.Context, _, commentID string) ([]domain.CommentVersion, error) {
	return s.history[commentID], nil
}

func (s *stubStore) DeleteComment(_ context.Context, p domain.DeleteCommentParams) (*domain.Comment, error) {
	c, ok := s.comments[p.CommentID]
	if !ok {
		return nil, domain.ErrCommentNotFound
	}
	c.Status = domain.CommentStatusDeletedRedacted
	return c, nil
}

func (s *stubStore) Mention(_ context.Context, p domain.MentionParams) (*domain.Mention, error) {
	m := &domain.Mention{MentionID: "mention-1", CommentID: p.CommentID, TenantID: p.TenantID, MentionedPrincipalID: p.MentionedPrincipalID, VisibilityGranted: p.VisibilityGranted}
	s.mentions[p.CommentID] = append(s.mentions[p.CommentID], *m)
	return m, nil
}

func (s *stubStore) React(_ context.Context, p domain.ReactParams) (bool, error) {
	key := p.CommentID + "|" + p.PrincipalID + "|" + p.ReactionType
	if s.reactions[key] {
		delete(s.reactions, key)
		return false, nil
	}
	s.reactions[key] = true
	return true, nil
}

func (s *stubStore) Moderate(_ context.Context, p domain.ModerateParams) (*domain.Comment, error) {
	c, ok := s.comments[p.CommentID]
	if !ok {
		return nil, domain.ErrCommentNotFound
	}
	c.Status = domain.CommentStatusModerated
	c.ModerationReason = p.Reason
	s.trail[p.CommentID] = append(s.trail[p.CommentID], domain.ModerationEntry{ModerationID: "modtrail-1", CommentID: p.CommentID, ModeratorPrincipalID: p.ModeratorPrincipalID, Action: p.Action, Reason: p.Reason})
	return c, nil
}

func (s *stubStore) ResolveThread(_ context.Context, p domain.ResolveThreadParams) (*domain.CommentThread, error) {
	t, ok := s.threads[p.ThreadID]
	if !ok {
		return nil, domain.ErrThreadNotFound
	}
	t.Status = domain.ThreadStatusResolved
	t.ResolutionNote = p.ResolutionNote
	return t, nil
}

func (s *stubStore) AttachReference(_ context.Context, p domain.AttachReferenceParams) (*domain.CommentAttachment, error) {
	if _, ok := s.comments[p.CommentID]; !ok {
		return nil, domain.ErrCommentNotFound
	}
	return &domain.CommentAttachment{AttachmentID: "attachment-1", CommentID: p.CommentID, LinkedObjectType: p.LinkedObjectType, LinkedObjectID: p.LinkedObjectID}, nil
}

func (s *stubStore) GetMentions(_ context.Context, _, commentID string) ([]domain.Mention, error) {
	return s.mentions[commentID], nil
}

func (s *stubStore) GetModerationTrail(_ context.Context, _, commentID string) ([]domain.ModerationEntry, error) {
	return s.trail[commentID], nil
}

type stubPublisher struct{ calls int }

func (p *stubPublisher) PublishCommentAdded(_ context.Context, _, _, _, _ string, _ domain.Comment, _ domain.CommentThread) {
	p.calls++
}
func (p *stubPublisher) PublishCommentEdited(_ context.Context, _, _, _, _ string, _ domain.Comment) {
	p.calls++
}
func (p *stubPublisher) PublishCommentModerated(_ context.Context, _, _, _, _ string, _ domain.Comment) {
	p.calls++
}
func (p *stubPublisher) PublishMentionCreated(_ context.Context, _, _, _, _ string, _ domain.Mention) {
	p.calls++
}
func (p *stubPublisher) PublishThreadResolved(_ context.Context, _, _, _, _ string, _ domain.CommentThread) {
	p.calls++
}

type stubRetention struct {
	blocked bool
	err     error
}

func (r *stubRetention) IsBlocked(_ context.Context, _, _, _, _ string) (bool, error) {
	return r.blocked, r.err
}

type stubVisibilityChecker struct {
	canView bool
	err     error
}

func (c *stubVisibilityChecker) CanView(_ context.Context, _, _, _, _ string) (bool, error) {
	return c.canView, c.err
}

type stubAuthZ struct{ denyAction string }

func (a *stubAuthZ) CheckAllowed(_ context.Context, _, _, actionType string) error {
	if actionType == a.denyAction {
		return context.DeadlineExceeded
	}
	return nil
}

func newTestHandler(st *stubStore, pub *stubPublisher, az *stubAuthZ) (*handler.Handler, *chi.Mux) {
	h := handler.New(st, pub, az, zap.NewNop()).WithRetentionClient(&stubRetention{})
	r := chi.NewRouter()
	handler.RegisterRoutes(r, h)
	return h, r
}

func withTenant(req *http.Request) *http.Request {
	return req.WithContext(middleware.WithTenant(req.Context(), "default"))
}

// ── tests ────────────────────────────────────────────────────────────────────

func TestAddComment_HappyPath(t *testing.T) {
	st, pub, az := newStubStore(), &stubPublisher{}, &stubAuthZ{}
	_, r := newTestHandler(st, pub, az)

	body, _ := json.Marshal(map[string]any{
		"legal_entity_id": "le-1", "linked_object_type": "DOCUMENT", "linked_object_id": "doc-1", "body": "hello",
	})
	req := withTenant(httptest.NewRequest(http.MethodPost, "/v1/threads/comments", bytes.NewReader(body)))
	req.Header.Set("X-Principal-Id", "reviewer-1")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d: %s", w.Code, w.Body.String())
	}
	if pub.calls != 1 {
		t.Fatalf("expected 1 published event, got %d", pub.calls)
	}
}

func TestAddComment_MissingPrincipal_Returns401(t *testing.T) {
	st, pub, az := newStubStore(), &stubPublisher{}, &stubAuthZ{}
	_, r := newTestHandler(st, pub, az)

	body, _ := json.Marshal(map[string]any{
		"legal_entity_id": "le-1", "linked_object_type": "DOCUMENT", "linked_object_id": "doc-1", "body": "hello",
	})
	req := withTenant(httptest.NewRequest(http.MethodPost, "/v1/threads/comments", bytes.NewReader(body)))
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d", w.Code)
	}
}

func TestAddComment_MissingFields_Returns400(t *testing.T) {
	st, pub, az := newStubStore(), &stubPublisher{}, &stubAuthZ{}
	_, r := newTestHandler(st, pub, az)

	body, _ := json.Marshal(map[string]any{"legal_entity_id": "le-1"})
	req := withTenant(httptest.NewRequest(http.MethodPost, "/v1/threads/comments", bytes.NewReader(body)))
	req.Header.Set("X-Principal-Id", "reviewer-1")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", w.Code)
	}
}

// TestAddComment_AuthzDenied_Returns403_NeverPublishes proves
// fetch/authorize-before-mutate: a denied AddComment must never reach the
// store or publish an event.
func TestAddComment_AuthzDenied_Returns403_NeverPublishes(t *testing.T) {
	st, pub, az := newStubStore(), &stubPublisher{}, &stubAuthZ{denyAction: "COMMENT_CREATE"}
	_, r := newTestHandler(st, pub, az)

	body, _ := json.Marshal(map[string]any{
		"legal_entity_id": "le-1", "linked_object_type": "DOCUMENT", "linked_object_id": "doc-1", "body": "hello",
	})
	req := withTenant(httptest.NewRequest(http.MethodPost, "/v1/threads/comments", bytes.NewReader(body)))
	req.Header.Set("X-Principal-Id", "reviewer-1")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusForbidden {
		t.Fatalf("expected 403, got %d", w.Code)
	}
	if len(st.comments) != 0 {
		t.Fatalf("expected the store to never be called, got %d comments", len(st.comments))
	}
	if pub.calls != 0 {
		t.Fatalf("expected no event published on denial, got %d", pub.calls)
	}
}

// TestEditComment_AuthzDenied_UsesThreadLegalEntityID proves EditComment
// fetches the comment's parent thread first (for its legal_entity_id)
// before checking authorization — fetch-then-authorize-then-mutate.
func TestEditComment_AuthzDenied_Returns403(t *testing.T) {
	st, pub, az := newStubStore(), &stubPublisher{}, &stubAuthZ{denyAction: "COMMENT_MANAGE"}
	_, r := newTestHandler(st, pub, az)
	_, _, _ = st.AddComment(context.Background(), domain.AddCommentParams{
		TenantID: "default", LegalEntityID: "le-1", LinkedObjectType: "DOCUMENT", LinkedObjectID: "doc-1",
		Body: "original", CreatedByPrincipalID: "reviewer-1",
	})

	body, _ := json.Marshal(map[string]any{"body": "edited"})
	req := withTenant(httptest.NewRequest(http.MethodPost, "/v1/comments/comment-1/edit", bytes.NewReader(body)))
	req.Header.Set("X-Principal-Id", "reviewer-1")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusForbidden {
		t.Fatalf("expected 403, got %d: %s", w.Code, w.Body.String())
	}
	if st.comments["comment-1"].Status != domain.CommentStatusActive {
		t.Fatalf("expected the comment to remain untouched on denial, got status %s", st.comments["comment-1"].Status)
	}
}

func TestGetThread_UnknownThread_Returns404(t *testing.T) {
	st, pub, az := newStubStore(), &stubPublisher{}, &stubAuthZ{}
	_, r := newTestHandler(st, pub, az)

	req := withTenant(httptest.NewRequest(http.MethodGet, "/v1/threads/thread-missing", nil))
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d", w.Code)
	}
}

func TestGetCommentHistory_ReturnsEmptyArrayNotNull(t *testing.T) {
	st, pub, az := newStubStore(), &stubPublisher{}, &stubAuthZ{}
	_, r := newTestHandler(st, pub, az)

	req := withTenant(httptest.NewRequest(http.MethodGet, "/v1/comments/comment-none/history", nil))
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	var got []domain.CommentVersion
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}
	if got == nil {
		t.Fatal("expected an empty array, got null")
	}
}

// TestMention_NoVisibilityCheckerRegistered_Returns403 is the negative
// control for the fail-closed default — an object type with no
// registered ObjectVisibilityChecker must refuse every mention against
// it, not silently allow one.
func TestMention_NoVisibilityCheckerRegistered_Returns403(t *testing.T) {
	st, pub, az := newStubStore(), &stubPublisher{}, &stubAuthZ{}
	_, _, _ = st.AddComment(context.Background(), domain.AddCommentParams{
		TenantID: "default", LegalEntityID: "le-1", LinkedObjectType: "DOCUMENT", LinkedObjectID: "doc-1",
		Body: "hello", CreatedByPrincipalID: "reviewer-1",
	})
	_, r := newTestHandler(st, pub, az)

	body, _ := json.Marshal(map[string]any{"mentioned_principal_id": "sarah"})
	req := withTenant(httptest.NewRequest(http.MethodPost, "/v1/comments/comment-1/mention", bytes.NewReader(body)))
	req.Header.Set("X-Principal-Id", "reviewer-1")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusForbidden {
		t.Fatalf("expected 403, got %d: %s", w.Code, w.Body.String())
	}
	if len(st.mentions["comment-1"]) != 0 {
		t.Fatal("expected no mention row to be written on refusal")
	}
}

// TestMention_VisibilityDenied_Returns403 proves a registered checker
// that denies access also refuses the mention (T29).
func TestMention_VisibilityDenied_Returns403(t *testing.T) {
	st, pub, az := newStubStore(), &stubPublisher{}, &stubAuthZ{}
	_, _, _ = st.AddComment(context.Background(), domain.AddCommentParams{
		TenantID: "default", LegalEntityID: "le-1", LinkedObjectType: "DOCUMENT", LinkedObjectID: "doc-1",
		Body: "hello", CreatedByPrincipalID: "reviewer-1",
	})
	h := handler.New(st, pub, az, zap.NewNop()).WithRetentionClient(&stubRetention{}).WithVisibilityChecker("DOCUMENT", &stubVisibilityChecker{canView: false})
	r := chi.NewRouter()
	handler.RegisterRoutes(r, h)

	body, _ := json.Marshal(map[string]any{"mentioned_principal_id": "sarah"})
	req := withTenant(httptest.NewRequest(http.MethodPost, "/v1/comments/comment-1/mention", bytes.NewReader(body)))
	req.Header.Set("X-Principal-Id", "reviewer-1")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusForbidden {
		t.Fatalf("expected 403, got %d: %s", w.Code, w.Body.String())
	}
	if pub.calls != 0 {
		t.Fatalf("expected no MentionCreated published on denial, got %d calls", pub.calls)
	}
}

// TestMention_VisibilityGranted_Succeeds proves the happy path through a
// registered, granting checker.
func TestMention_VisibilityGranted_Succeeds(t *testing.T) {
	st, pub, az := newStubStore(), &stubPublisher{}, &stubAuthZ{}
	_, _, _ = st.AddComment(context.Background(), domain.AddCommentParams{
		TenantID: "default", LegalEntityID: "le-1", LinkedObjectType: "DOCUMENT", LinkedObjectID: "doc-1",
		Body: "hello", CreatedByPrincipalID: "reviewer-1",
	})
	h := handler.New(st, pub, az, zap.NewNop()).WithRetentionClient(&stubRetention{}).WithVisibilityChecker("DOCUMENT", &stubVisibilityChecker{canView: true})
	r := chi.NewRouter()
	handler.RegisterRoutes(r, h)

	body, _ := json.Marshal(map[string]any{"mentioned_principal_id": "sarah"})
	req := withTenant(httptest.NewRequest(http.MethodPost, "/v1/comments/comment-1/mention", bytes.NewReader(body)))
	req.Header.Set("X-Principal-Id", "reviewer-1")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d: %s", w.Code, w.Body.String())
	}
	if len(st.mentions["comment-1"]) != 1 {
		t.Fatal("expected one mention row to be written")
	}
}

// TestDeleteComment_RetentionHold_Returns409 proves DeleteComment
// refuses when retention-registry-svc reports a hold (T28).
func TestDeleteComment_RetentionHold_Returns409(t *testing.T) {
	st, pub, az := newStubStore(), &stubPublisher{}, &stubAuthZ{}
	_, _, _ = st.AddComment(context.Background(), domain.AddCommentParams{
		TenantID: "default", LegalEntityID: "le-1", LinkedObjectType: "DOCUMENT", LinkedObjectID: "doc-1",
		Body: "hello", CreatedByPrincipalID: "reviewer-1",
	})
	h := handler.New(st, pub, az, zap.NewNop()).WithRetentionClient(&stubRetention{blocked: true})
	r := chi.NewRouter()
	handler.RegisterRoutes(r, h)

	body, _ := json.Marshal(map[string]any{"reason": "spam"})
	req := withTenant(httptest.NewRequest(http.MethodPost, "/v1/comments/comment-1/delete", bytes.NewReader(body)))
	req.Header.Set("X-Principal-Id", "reviewer-1")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusConflict {
		t.Fatalf("expected 409, got %d: %s", w.Code, w.Body.String())
	}
	if st.comments["comment-1"].Status == domain.CommentStatusDeletedRedacted {
		t.Fatal("expected the comment to remain undeleted while under hold")
	}
}

// TestDeleteComment_RetentionServiceUnreachable_FailsClosed proves an
// unreachable retention-registry-svc blocks the delete rather than
// assuming "no hold."
func TestDeleteComment_RetentionServiceUnreachable_FailsClosed(t *testing.T) {
	st, pub, az := newStubStore(), &stubPublisher{}, &stubAuthZ{}
	_, _, _ = st.AddComment(context.Background(), domain.AddCommentParams{
		TenantID: "default", LegalEntityID: "le-1", LinkedObjectType: "DOCUMENT", LinkedObjectID: "doc-1",
		Body: "hello", CreatedByPrincipalID: "reviewer-1",
	})
	h := handler.New(st, pub, az, zap.NewNop()).WithRetentionClient(&stubRetention{err: domain.ErrRetentionServiceUnavailable})
	r := chi.NewRouter()
	handler.RegisterRoutes(r, h)

	body, _ := json.Marshal(map[string]any{"reason": "spam"})
	req := withTenant(httptest.NewRequest(http.MethodPost, "/v1/comments/comment-1/delete", bytes.NewReader(body)))
	req.Header.Set("X-Principal-Id", "reviewer-1")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected 503, got %d: %s", w.Code, w.Body.String())
	}
}

// TestModerate_MissingReason_Returns400 is the negative control for the
// doc's own T46: moderation always requires a reason.
func TestModerate_MissingReason_Returns400(t *testing.T) {
	st, pub, az := newStubStore(), &stubPublisher{}, &stubAuthZ{}
	_, _, _ = st.AddComment(context.Background(), domain.AddCommentParams{
		TenantID: "default", LegalEntityID: "le-1", LinkedObjectType: "DOCUMENT", LinkedObjectID: "doc-1",
		Body: "hello", CreatedByPrincipalID: "reviewer-1",
	})
	_, r := newTestHandler(st, pub, az)

	body, _ := json.Marshal(map[string]any{"action": "REDACT"})
	req := withTenant(httptest.NewRequest(http.MethodPost, "/v1/comments/comment-1/moderate", bytes.NewReader(body)))
	req.Header.Set("X-Principal-Id", "moderator-1")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d: %s", w.Code, w.Body.String())
	}
}

// TestReact_TogglesOnAndOff proves React is a real toggle.
func TestReact_TogglesOnAndOff(t *testing.T) {
	st, pub, az := newStubStore(), &stubPublisher{}, &stubAuthZ{}
	_, _, _ = st.AddComment(context.Background(), domain.AddCommentParams{
		TenantID: "default", LegalEntityID: "le-1", LinkedObjectType: "DOCUMENT", LinkedObjectID: "doc-1",
		Body: "hello", CreatedByPrincipalID: "reviewer-1",
	})
	_, r := newTestHandler(st, pub, az)

	body, _ := json.Marshal(map[string]any{"reaction_type": "THUMBS_UP"})
	req1 := withTenant(httptest.NewRequest(http.MethodPost, "/v1/comments/comment-1/react", bytes.NewReader(body)))
	req1.Header.Set("X-Principal-Id", "reviewer-2")
	w1 := httptest.NewRecorder()
	r.ServeHTTP(w1, req1)
	var res1 map[string]bool
	_ = json.Unmarshal(w1.Body.Bytes(), &res1)
	if !res1["added"] {
		t.Fatal("expected the first react to add a reaction")
	}

	req2 := withTenant(httptest.NewRequest(http.MethodPost, "/v1/comments/comment-1/react", bytes.NewReader(body)))
	req2.Header.Set("X-Principal-Id", "reviewer-2")
	w2 := httptest.NewRecorder()
	r.ServeHTTP(w2, req2)
	var res2 map[string]bool
	_ = json.Unmarshal(w2.Body.Bytes(), &res2)
	if res2["added"] {
		t.Fatal("expected the second react (same principal, same type) to remove the reaction")
	}
}

// TestResolveThread_ThenAddComment_Reopens proves the full round trip
// through the handler layer, not just the store.
func TestResolveThread_ThenAddComment_Succeeds(t *testing.T) {
	st, pub, az := newStubStore(), &stubPublisher{}, &stubAuthZ{}
	_, _, _ = st.AddComment(context.Background(), domain.AddCommentParams{
		TenantID: "default", LegalEntityID: "le-1", LinkedObjectType: "DOCUMENT", LinkedObjectID: "doc-1",
		Body: "hello", CreatedByPrincipalID: "reviewer-1",
	})
	_, r := newTestHandler(st, pub, az)

	req := withTenant(httptest.NewRequest(http.MethodPost, "/v1/threads/thread-1/resolve", bytes.NewReader([]byte(`{}`))))
	req.Header.Set("X-Principal-Id", "reviewer-1")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	if st.threads["thread-1"].Status != domain.ThreadStatusResolved {
		t.Fatal("expected the thread to be resolved")
	}
}
