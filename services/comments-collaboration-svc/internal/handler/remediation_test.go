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

	"zoiko.io/comments-collaboration-svc/internal/authz"
	"zoiko.io/comments-collaboration-svc/internal/domain"
	"zoiko.io/comments-collaboration-svc/internal/handler"
	"zoiko.io/comments-collaboration-svc/internal/store"
)

// recordingAuthZ records every action asked of authorization-svc and answers
// per action: deny[action] → ErrAuthorizationDenied, down → unavailable.
type recordingAuthZ struct {
	actions []string
	deny    map[string]bool
	down    bool
}

func (a *recordingAuthZ) CheckAllowed(_ context.Context, _, _, action string) error {
	a.actions = append(a.actions, action)
	if a.down {
		return authz.ErrAuthzServiceUnavailable
	}
	if a.deny[action] {
		return authz.ErrAuthorizationDenied
	}
	return nil
}

func seeded() *stubStore {
	st := newStubStore()
	_, _, _ = st.AddComment(context.Background(), domain.AddCommentParams{
		TenantID: "default", LegalEntityID: "le-1", LinkedObjectType: "DOCUMENT", LinkedObjectID: "doc-1",
		Body: "hello", CreatedByPrincipalID: "reviewer-1",
	})
	return st
}

func routerWith(st store.Store, az handler.AuthZClient, opts ...func(*handler.Handler)) (*chi.Mux, *stubPublisher) {
	pub := &stubPublisher{}
	h := handler.New(st, pub, az, zap.NewNop()).WithRetentionClient(&stubRetention{})
	for _, o := range opts {
		o(h)
	}
	r := chi.NewRouter()
	handler.RegisterRoutes(r, h)
	return r, pub
}

func do(r http.Handler, method, path string, body any, principal string, hdr map[string]string) *httptest.ResponseRecorder {
	var buf bytes.Buffer
	if body != nil {
		_ = json.NewEncoder(&buf).Encode(body)
	}
	req := withTenant(httptest.NewRequest(method, path, &buf))
	if principal != "" {
		req.Header.Set("X-Principal-Id", principal)
	}
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w
}

var readRoutes = []string{
	"/v1/threads/thread-1",
	"/v1/threads/thread-1/comments",
	"/v1/comments/comment-1/history",
	"/v1/comments/comment-1/mentions",
	"/v1/comments/comment-1/moderation-trail",
}

// ── read authorization ───────────────────────────────────────────────────────

func TestReads_Authorized_UseCommentRead(t *testing.T) {
	for _, path := range readRoutes {
		az := &recordingAuthZ{}
		r, _ := routerWith(seeded(), az)
		w := do(r, http.MethodGet, path, nil, "reader-1", nil)
		if w.Code != http.StatusOK {
			t.Fatalf("%s: want 200, got %d: %s", path, w.Code, w.Body.String())
		}
		if len(az.actions) != 1 || az.actions[0] != "COMMENT_READ" {
			t.Fatalf("%s: want exactly one COMMENT_READ check, got %v", path, az.actions)
		}
	}
}

func TestReads_MissingPrincipal_Returns401(t *testing.T) {
	for _, path := range readRoutes {
		az := &recordingAuthZ{}
		r, _ := routerWith(seeded(), az)
		if w := do(r, http.MethodGet, path, nil, "", nil); w.Code != http.StatusUnauthorized {
			t.Fatalf("%s: want 401, got %d", path, w.Code)
		}
		if len(az.actions) != 0 {
			t.Fatalf("%s: authorization must not be consulted without a principal", path)
		}
	}
}

func TestReads_Unauthorized_Returns403_AndNoData(t *testing.T) {
	for _, path := range readRoutes {
		r, _ := routerWith(seeded(), &recordingAuthZ{deny: map[string]bool{"COMMENT_READ": true}})
		w := do(r, http.MethodGet, path, nil, "reader-1", nil)
		if w.Code != http.StatusForbidden {
			t.Fatalf("%s: want 403, got %d", path, w.Code)
		}
		if bytes.Contains(w.Body.Bytes(), []byte("hello")) {
			t.Fatalf("%s: a denied read must not leak content", path)
		}
	}
}

func TestReads_AuthzUnavailable_FailsClosed503(t *testing.T) {
	for _, path := range readRoutes {
		r, _ := routerWith(seeded(), &recordingAuthZ{down: true})
		if w := do(r, http.MethodGet, path, nil, "reader-1", nil); w.Code != http.StatusServiceUnavailable {
			t.Fatalf("%s: want 503, got %d", path, w.Code)
		}
	}
}

func TestReads_RestrictedThread_RequiresRestrictedRead(t *testing.T) {
	for _, path := range readRoutes {
		// Holder of COMMENT_READ only: refused.
		st := seeded()
		st.threads["thread-1"].Restricted = true
		az := &recordingAuthZ{deny: map[string]bool{"COMMENT_READ_RESTRICTED": true}}
		r, _ := routerWith(st, az)
		if w := do(r, http.MethodGet, path, nil, "reader-1", nil); w.Code != http.StatusForbidden {
			t.Fatalf("%s: restricted thread without COMMENT_READ_RESTRICTED must be 403, got %d", path, w.Code)
		}
		if len(az.actions) != 1 || az.actions[0] != "COMMENT_READ_RESTRICTED" {
			t.Fatalf("%s: want COMMENT_READ_RESTRICTED check, got %v", path, az.actions)
		}
		// Holder of COMMENT_READ_RESTRICTED: allowed.
		st2 := seeded()
		st2.threads["thread-1"].Restricted = true
		r2, _ := routerWith(st2, &recordingAuthZ{deny: map[string]bool{"COMMENT_READ": true}})
		if w := do(r2, http.MethodGet, path, nil, "reader-1", nil); w.Code != http.StatusOK {
			t.Fatalf("%s: restricted reader must be allowed, got %d", path, w.Code)
		}
	}
}

func TestReads_UnknownOrOtherTenantObject_Returns404_WithoutAuthz(t *testing.T) {
	for _, path := range []string{"/v1/threads/nope", "/v1/threads/nope/comments", "/v1/comments/nope/history", "/v1/comments/nope/mentions", "/v1/comments/nope/moderation-trail"} {
		az := &recordingAuthZ{}
		r, _ := routerWith(newStubStore(), az)
		if w := do(r, http.MethodGet, path, nil, "reader-1", nil); w.Code != http.StatusNotFound {
			t.Fatalf("%s: want 404, got %d", path, w.Code)
		}
	}
}

// ── mention: uniform refusal, no existence oracle ────────────────────────────

func TestMention_AllRefusalsAreIndistinguishable(t *testing.T) {
	type tc struct {
		name    string
		st      *stubStore
		az      *recordingAuthZ
		checker handler.ObjectVisibilityChecker
		comment string
	}
	cases := []tc{
		{"unknown comment", seeded(), &recordingAuthZ{}, &stubVisibilityChecker{canView: true}, "comment-nope"},
		{"caller not authorized", seeded(), &recordingAuthZ{deny: map[string]bool{"COMMENT_MANAGE": true}}, &stubVisibilityChecker{canView: true}, "comment-1"},
		{"no checker registered", seeded(), &recordingAuthZ{}, nil, "comment-1"},
		{"mentioned principal cannot view", seeded(), &recordingAuthZ{}, &stubVisibilityChecker{canView: false}, "comment-1"},
	}
	var first string
	for i, c := range cases {
		var opts []func(*handler.Handler)
		if c.checker != nil {
			chk := c.checker
			opts = append(opts, func(h *handler.Handler) { h.WithVisibilityChecker("DOCUMENT", chk) })
		}
		r, pub := routerWith(c.st, c.az, opts...)
		w := do(r, http.MethodPost, "/v1/comments/"+c.comment+"/mention", map[string]string{"mentioned_principal_id": "sarah"}, "reviewer-1", nil)
		if w.Code != http.StatusForbidden {
			t.Fatalf("%s: want 403, got %d", c.name, w.Code)
		}
		if i == 0 {
			first = w.Body.String()
		} else if w.Body.String() != first {
			t.Fatalf("%s: refusal body %q differs from %q — an existence oracle", c.name, w.Body.String(), first)
		}
		if len(c.st.mentions["comment-1"]) != 0 || pub.calls != 0 {
			t.Fatalf("%s: a refusal must create no mention and publish nothing", c.name)
		}
	}
}

func TestMention_DependencyFailuresFailClosed503(t *testing.T) {
	r, _ := routerWith(seeded(), &recordingAuthZ{down: true})
	if w := do(r, http.MethodPost, "/v1/comments/comment-1/mention", map[string]string{"mentioned_principal_id": "s"}, "p", nil); w.Code != http.StatusServiceUnavailable {
		t.Fatalf("authz down: want 503, got %d", w.Code)
	}
	st := seeded()
	r2, _ := routerWith(st, &recordingAuthZ{}, func(h *handler.Handler) {
		h.WithVisibilityChecker("DOCUMENT", &stubVisibilityChecker{err: context.DeadlineExceeded})
	})
	w := do(r2, http.MethodPost, "/v1/comments/comment-1/mention", map[string]string{"mentioned_principal_id": "s"}, "p", nil)
	if w.Code != http.StatusServiceUnavailable || len(st.mentions["comment-1"]) != 0 {
		t.Fatalf("visibility check down: want 503 and no row, got %d", w.Code)
	}
}

// ── moderation action ────────────────────────────────────────────────────────

func TestModerate_ActionValidation(t *testing.T) {
	for name, tc := range map[string]struct {
		body map[string]string
		want int
	}{
		"valid action and reason": {map[string]string{"action": "REDACT", "reason": "spam"}, http.StatusOK},
		"missing action":          {map[string]string{"reason": "spam"}, http.StatusBadRequest},
		"missing reason":          {map[string]string{"action": "REDACT"}, http.StatusBadRequest},
	} {
		st := seeded()
		r, pub := routerWith(st, &recordingAuthZ{})
		w := do(r, http.MethodPost, "/v1/comments/comment-1/moderate", tc.body, "moderator-1", nil)
		if w.Code != tc.want {
			t.Fatalf("%s: want %d, got %d: %s", name, tc.want, w.Code, w.Body.String())
		}
		if tc.want != http.StatusOK && (len(st.trail["comment-1"]) != 0 || pub.calls != 0) {
			t.Fatalf("%s: a rejected moderation must write no trail and publish nothing", name)
		}
	}
}

// ── restricted flag ──────────────────────────────────────────────────────────

func TestAddComment_Restricted_UsesRestrictedAction(t *testing.T) {
	az := &recordingAuthZ{}
	r, _ := routerWith(newStubStore(), az)
	w := do(r, http.MethodPost, "/v1/threads/comments", map[string]any{
		"legal_entity_id": "le-1", "linked_object_type": "DOCUMENT", "linked_object_id": "d", "body": "x", "restricted": true,
	}, "p", nil)
	if w.Code != http.StatusCreated || len(az.actions) != 1 || az.actions[0] != "COMMENT_READ_RESTRICTED" {
		t.Fatalf("want 201 via COMMENT_READ_RESTRICTED, got %d %v", w.Code, az.actions)
	}
}

func TestAddComment_RestrictionMismatch_Returns409(t *testing.T) {
	st := newStubStore()
	st.addCommentErr = domain.ErrThreadRestrictionMismatch
	r, pub := routerWith(st, &recordingAuthZ{})
	w := do(r, http.MethodPost, "/v1/threads/comments", map[string]any{
		"legal_entity_id": "le-1", "linked_object_type": "DOCUMENT", "linked_object_id": "d", "body": "x", "restricted": true,
	}, "p", nil)
	if w.Code != http.StatusConflict || pub.calls != 0 {
		t.Fatalf("want 409 and no event, got %d (%d events)", w.Code, pub.calls)
	}
}

// ── idempotency wiring ───────────────────────────────────────────────────────

// idemStore mimics the persistent store's contract (claim → replay/conflict)
// so the handler's wiring — header, hashing, replay marker, no repeat event —
// is tested here; the real transactional behaviour is in the store tests.
type idemStore struct {
	*stubStore
	seen map[string]string
	n    int
}

func (s *idemStore) AddComment(ctx context.Context, p domain.AddCommentParams) (*domain.Comment, *domain.CommentThread, error) {
	if p.Idem != nil {
		if h, ok := s.seen[p.Idem.Key]; ok {
			if h != p.Idem.RequestHash {
				return nil, nil, domain.ErrIdempotencyConflict
			}
			p.Idem.Replayed = true
		} else {
			s.seen[p.Idem.Key] = p.Idem.RequestHash
		}
	}
	s.n++
	return s.stubStore.AddComment(ctx, p)
}

func TestIdempotency_HandlerWiring(t *testing.T) {
	st := &idemStore{stubStore: newStubStore(), seen: map[string]string{}}
	r, pub := routerWith(st, &recordingAuthZ{})
	body := map[string]any{"legal_entity_id": "le-1", "linked_object_type": "DOCUMENT", "linked_object_id": "d", "body": "x"}
	hdr := map[string]string{"Idempotency-Key": "k1"}

	w1 := do(r, http.MethodPost, "/v1/threads/comments", body, "p", hdr)
	w2 := do(r, http.MethodPost, "/v1/threads/comments", body, "p", hdr)
	if w1.Code != http.StatusCreated || w2.Code != http.StatusCreated {
		t.Fatalf("want 201/201, got %d/%d", w1.Code, w2.Code)
	}
	if w1.Header().Get("Idempotent-Replayed") != "" || w2.Header().Get("Idempotent-Replayed") != "true" {
		t.Fatalf("only the replay is marked: %q / %q", w1.Header().Get("Idempotent-Replayed"), w2.Header().Get("Idempotent-Replayed"))
	}
	if pub.calls != 1 {
		t.Fatalf("a replay must not publish a second event, got %d", pub.calls)
	}

	body["body"] = "different"
	if w := do(r, http.MethodPost, "/v1/threads/comments", body, "p", hdr); w.Code != http.StatusConflict {
		t.Fatalf("same key, different request: want 409, got %d", w.Code)
	}
}
