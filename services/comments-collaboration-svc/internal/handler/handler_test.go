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
	threads  map[string]*domain.CommentThread
	comments map[string]*domain.Comment
	history  map[string][]domain.CommentVersion

	addCommentErr  error
	editCommentErr error
}

func newStubStore() *stubStore {
	return &stubStore{
		threads:  make(map[string]*domain.CommentThread),
		comments: make(map[string]*domain.Comment),
		history:  make(map[string][]domain.CommentVersion),
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

type stubPublisher struct{ calls int }

func (p *stubPublisher) PublishCommentAdded(_ context.Context, _, _, _, _ string, _ domain.Comment, _ domain.CommentThread) {
	p.calls++
}
func (p *stubPublisher) PublishCommentEdited(_ context.Context, _, _, _, _ string, _ domain.Comment) {
	p.calls++
}

type stubAuthZ struct{ denyAction string }

func (a *stubAuthZ) CheckAllowed(_ context.Context, _, _, actionType string) error {
	if actionType == a.denyAction {
		return context.DeadlineExceeded
	}
	return nil
}

func newTestHandler(st *stubStore, pub *stubPublisher, az *stubAuthZ) (*handler.Handler, *chi.Mux) {
	h := handler.New(st, pub, az, zap.NewNop())
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
