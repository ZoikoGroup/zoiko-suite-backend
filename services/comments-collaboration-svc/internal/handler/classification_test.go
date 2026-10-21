package handler_test

import (
	"context"
	"net/http"
	"testing"

	"zoiko.io/comments-collaboration-svc/internal/domain"
)

// capturingStore records the params the handler passes to AddComment.
type capturingStore struct {
	*stubStore
	got   []domain.AddCommentParams
	calls int
}

func (s *capturingStore) AddComment(ctx context.Context, p domain.AddCommentParams) (*domain.Comment, *domain.CommentThread, error) {
	s.calls++
	s.got = append(s.got, p)
	return s.stubStore.AddComment(ctx, p)
}

func addBody(classification string) map[string]any {
	b := map[string]any{"legal_entity_id": "le-1", "linked_object_type": "DOCUMENT", "linked_object_id": "d", "body": "x"}
	if classification != "" {
		b["classification"] = classification
	}
	return b
}

func TestAddComment_Classification_PassedToTheStore(t *testing.T) {
	for _, tier := range []string{"PUBLIC", "INTERNAL", "CONFIDENTIAL", "RESTRICTED"} {
		st := &capturingStore{stubStore: newStubStore()}
		r, _ := routerWith(st, &recordingAuthZ{})
		if w := do(r, http.MethodPost, "/v1/threads/comments", addBody(tier), "p", nil); w.Code != http.StatusCreated {
			t.Fatalf("%s: want 201, got %d: %s", tier, w.Code, w.Body.String())
		}
		if len(st.got) != 1 || st.got[0].Classification != tier {
			t.Fatalf("%s: store received %+v", tier, st.got)
		}
	}
}

func TestAddComment_Classification_InvalidIs400_AndNeverReachesTheStore(t *testing.T) {
	for _, bad := range []string{"SECRET", "internal", "TOP-SECRET"} {
		st := &capturingStore{stubStore: newStubStore()}
		az := &recordingAuthZ{}
		r, pub := routerWith(st, az)
		w := do(r, http.MethodPost, "/v1/threads/comments", addBody(bad), "p", nil)
		if w.Code != http.StatusBadRequest {
			t.Fatalf("%q: want 400, got %d", bad, w.Code)
		}
		if st.calls != 0 || pub.calls != 0 {
			t.Fatalf("%q: an invalid classification must write nothing and publish nothing", bad)
		}
	}
}

func TestAddComment_Classification_StoreRefusalsMapToStatus(t *testing.T) {
	for name, tc := range map[string]struct {
		err  error
		want int
	}{
		"required on a new thread":  {domain.ErrClassificationRequired, http.StatusBadRequest},
		"differs from the thread's": {domain.ErrClassificationMismatch, http.StatusConflict},
	} {
		st := newStubStore()
		st.addCommentErr = tc.err
		r, pub := routerWith(st, &recordingAuthZ{})
		w := do(r, http.MethodPost, "/v1/threads/comments", addBody("INTERNAL"), "p", nil)
		if w.Code != tc.want || pub.calls != 0 {
			t.Fatalf("%s: want %d and no event, got %d (%d events)", name, tc.want, w.Code, pub.calls)
		}
	}
}

func TestAddComment_Classification_DoesNotChangeAuthorization(t *testing.T) {
	// Classification is lineage, not an access decision: it must not alter
	// which action is asked of authorization-svc (BIZ-02: "never replaces IAM").
	az := &recordingAuthZ{}
	r, _ := routerWith(newStubStore(), az)
	do(r, http.MethodPost, "/v1/threads/comments", addBody("RESTRICTED"), "p", nil)
	if len(az.actions) != 1 || az.actions[0] != "COMMENT_CREATE" {
		t.Fatalf("want exactly COMMENT_CREATE, got %v", az.actions)
	}
}
