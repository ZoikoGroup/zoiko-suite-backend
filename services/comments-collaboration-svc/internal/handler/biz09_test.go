package handler_test

import (
	"net/http"
	"testing"
)

// seededBy returns a store whose comment-1 was written by author-1, on thread-1.
func seededBy(restricted bool) *stubStore {
	st := seeded()
	st.comments["comment-1"].CreatedBy = "author-1"
	st.threads["thread-1"].Restricted = restricted
	return st
}

// A comment is attributable: only its author edits it. Before this, any holder of
// COMMENT_MANAGE could rewrite anyone's comment (the old text survived only in history).
func TestEdit_OnlyTheAuthorMayEdit(t *testing.T) {
	st := seededBy(false)
	r, pub := routerWith(st, &recordingAuthZ{})
	if w := do(r, http.MethodPost, "/v1/comments/comment-1/edit", map[string]string{"body": "rewritten"}, "someone-else", nil); w.Code != http.StatusForbidden {
		t.Fatalf("a non-author edit must be 403, got %d: %s", w.Code, w.Body.String())
	}
	if st.comments["comment-1"].CurrentBody != "hello" || pub.n() != 0 {
		t.Fatal("a refused edit must change nothing and publish nothing")
	}
	if w := do(r, http.MethodPost, "/v1/comments/comment-1/edit", map[string]string{"body": "mine"}, "author-1", nil); w.Code != http.StatusOK {
		t.Fatalf("the author's edit must succeed, got %d", w.Code)
	}
}

// Deleting someone else's comment is moderation and needs COMMENT_MODERATE; the author
// deletes their own with COMMENT_MANAGE.
func TestDelete_NonAuthorNeedsTheModerationAction(t *testing.T) {
	// Non-author with COMMENT_MANAGE only: refused.
	st := seededBy(false)
	az := &recordingAuthZ{deny: map[string]bool{"COMMENT_MODERATE": true}}
	r, _ := routerWith(st, az)
	if w := do(r, http.MethodPost, "/v1/comments/comment-1/delete", map[string]string{"reason": "spam"}, "someone-else", nil); w.Code != http.StatusForbidden {
		t.Fatalf("non-author without COMMENT_MODERATE must be 403, got %d", w.Code)
	}
	if st.comments["comment-1"].Status == "DELETED_REDACTED" {
		t.Fatal("a refused delete must leave the comment untouched")
	}
	if len(az.actions) != 1 || az.actions[0] != "COMMENT_MODERATE" {
		t.Fatalf("want a COMMENT_MODERATE check, got %v", az.actions)
	}

	// The author with COMMENT_MANAGE only: allowed.
	st2 := seededBy(false)
	r2, _ := routerWith(st2, &recordingAuthZ{deny: map[string]bool{"COMMENT_MODERATE": true}})
	if w := do(r2, http.MethodPost, "/v1/comments/comment-1/delete", map[string]string{"reason": "oops"}, "author-1", nil); w.Code != http.StatusOK {
		t.Fatalf("the author must be able to delete their own comment, got %d", w.Code)
	}

	// A moderator (COMMENT_MODERATE, no COMMENT_MANAGE) deleting someone else's: allowed.
	st3 := seededBy(false)
	r3, _ := routerWith(st3, &recordingAuthZ{deny: map[string]bool{"COMMENT_MANAGE": true}})
	if w := do(r3, http.MethodPost, "/v1/comments/comment-1/delete", map[string]string{"reason": "spam"}, "mod-1", nil); w.Code != http.StatusOK {
		t.Fatalf("a moderator must be able to delete another's comment, got %d", w.Code)
	}
}

// Moderation has its own action, kept apart from ordinary participation, and a moderator
// cannot moderate their own comment.
func TestModerate_UsesTheModerationActionAndRefusesSelfModeration(t *testing.T) {
	st := seededBy(false)
	az := &recordingAuthZ{deny: map[string]bool{"COMMENT_MODERATE": true}}
	r, pub := routerWith(st, az)
	body := map[string]string{"action": "HIDE", "reason": "abusive"}
	if w := do(r, http.MethodPost, "/v1/comments/comment-1/moderate", body, "mod-1", nil); w.Code != http.StatusForbidden {
		t.Fatalf("COMMENT_MANAGE alone must not allow moderation, got %d", w.Code)
	}
	if len(st.trail["comment-1"]) != 0 || pub.n() != 0 {
		t.Fatal("a refused moderation must leave no trail and publish nothing")
	}

	// A moderator who is the comment's author is refused.
	st2 := seededBy(false)
	r2, _ := routerWith(st2, &recordingAuthZ{})
	if w := do(r2, http.MethodPost, "/v1/comments/comment-1/moderate", body, "author-1", nil); w.Code != http.StatusForbidden {
		t.Fatalf("self-moderation must be 403, got %d", w.Code)
	}
	if len(st2.trail["comment-1"]) != 0 {
		t.Fatal("self-moderation must leave no trail")
	}

	// A different moderator is allowed.
	if w := do(r2, http.MethodPost, "/v1/comments/comment-1/moderate", body, "mod-1", nil); w.Code != http.StatusOK {
		t.Fatalf("another principal's moderation must succeed, got %d: %s", w.Code, w.Body.String())
	}
}

// On a restricted thread every write also needs COMMENT_READ_RESTRICTED. A principal who
// cannot read the thread must not be able to change it.
func TestRestrictedThread_WritesRequireRestrictedRead(t *testing.T) {
	type call struct {
		name, method, path string
		body               any
		principal          string
	}
	calls := []call{
		{"edit", http.MethodPost, "/v1/comments/comment-1/edit", map[string]string{"body": "x"}, "author-1"},
		{"delete", http.MethodPost, "/v1/comments/comment-1/delete", map[string]string{"reason": "x"}, "author-1"},
		{"react", http.MethodPost, "/v1/comments/comment-1/react", map[string]string{"reaction_type": "THUMBS_UP"}, "p-1"},
		{"moderate", http.MethodPost, "/v1/comments/comment-1/moderate", map[string]string{"action": "HIDE", "reason": "x"}, "mod-1"},
		{"resolve", http.MethodPost, "/v1/threads/thread-1/resolve", map[string]string{}, "p-1"},
		{"attach", http.MethodPost, "/v1/comments/comment-1/attachments", map[string]string{"linked_object_id": "doc-2"}, "p-1"},
	}
	for _, c := range calls {
		st := seededBy(true)
		az := &recordingAuthZ{deny: map[string]bool{"COMMENT_READ_RESTRICTED": true}}
		r, pub := routerWith(st, az)
		w := do(r, c.method, c.path, c.body, c.principal, nil)
		if w.Code != http.StatusForbidden {
			t.Errorf("%s on a restricted thread without COMMENT_READ_RESTRICTED: got %d, want 403", c.name, w.Code)
		}
		if pub.n() != 0 || st.comments["comment-1"].Status != "ACTIVE" || st.threads["thread-1"].Status != "ACTIVE" {
			t.Errorf("%s: a refused write changed state or published", c.name)
		}
		found := false
		for _, a := range az.actions {
			if a == "COMMENT_READ_RESTRICTED" {
				found = true
			}
		}
		if !found {
			t.Errorf("%s: COMMENT_READ_RESTRICTED was never checked (%v)", c.name, az.actions)
		}
	}

	// An ordinary thread does not ask for it.
	st := seededBy(false)
	az := &recordingAuthZ{deny: map[string]bool{"COMMENT_READ_RESTRICTED": true}}
	r, _ := routerWith(st, az)
	if w := do(r, http.MethodPost, "/v1/comments/comment-1/react", map[string]string{"reaction_type": "THUMBS_UP"}, "p-1", nil); w.Code != http.StatusOK {
		t.Fatalf("an ordinary thread must not need COMMENT_READ_RESTRICTED, got %d", w.Code)
	}
}

// Mention on a restricted thread still refuses with the same body as every other refusal.
func TestRestrictedThread_MentionWithoutRestrictedReadIsTheUniformRefusal(t *testing.T) {
	refusal := func(restrictedDenied bool) string {
		st := seededBy(true)
		az := &recordingAuthZ{deny: map[string]bool{"COMMENT_READ_RESTRICTED": restrictedDenied}}
		r, _ := routerWith(st, az)
		w := do(r, http.MethodPost, "/v1/comments/comment-1/mention", map[string]string{"mentioned_principal_id": "x"}, "p-1", nil)
		if restrictedDenied && w.Code != http.StatusForbidden {
			t.Fatalf("mention without restricted read: got %d, want 403", w.Code)
		}
		return w.Body.String()
	}
	denied := refusal(true)
	// Same body as the unknown-comment refusal.
	st := seededBy(true)
	r, _ := routerWith(st, &recordingAuthZ{})
	unknown := do(r, http.MethodPost, "/v1/comments/nope/mention", map[string]string{"mentioned_principal_id": "x"}, "p-1", nil).Body.String()
	if denied != unknown {
		t.Fatalf("refusals differ: %q vs %q", denied, unknown)
	}
}
