package handler_test

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/google/uuid"

	"zoiko.io/privacy-consent-svc/internal/domain"
)

// An invalid notice lifecycle transition is a 409 whose error_code is
// INVALID_TRANSITION. It used to fall through the status-derived fallback and
// answer IDEMPOTENCY_MISMATCH, which is a different condition: a reused
// Idempotency-Key. These tests pin the two apart.

func newDraftNotice(t *testing.T, r http.Handler) *domain.NoticeVersion {
	t.Helper()
	w := doRequestWithPrincipal(r, http.MethodPost, "/privacy/notices", domain.CreateNoticeRequest{
		Locale: "en-US", Audience: "CUSTOMER", ContentHash: "sha256:abc123",
	}, testTenant, "principal-maker")
	if w.Code != http.StatusCreated {
		t.Fatalf("create notice: %d %s", w.Code, w.Body.String())
	}
	var v domain.NoticeVersion
	_ = json.Unmarshal(w.Body.Bytes(), &v)
	return &v
}

func noticePath(v *domain.NoticeVersion, verb string) string {
	return "/privacy/notices/" + v.NoticeID + "/versions/" + v.NoticeVersionID + "/" + verb
}

func TestInvalidNoticeTransitions_AnswerInvalidTransition(t *testing.T) {
	st := newStubStore()
	r := newTestRouter(st, &stubAuthz{}, grantingPurposeRegistry())

	// approve: the version is no longer DRAFT.
	approved := newDraftNotice(t, r)
	if w := doRequestWithPrincipal(r, http.MethodPost, noticePath(approved, "approve"), nil, testTenant, "principal-approver"); w.Code != http.StatusOK {
		t.Fatalf("precondition: approve: %d %s", w.Code, w.Body.String())
	}
	w := doRequestWithPrincipal(r, http.MethodPost, noticePath(approved, "approve"), nil, testTenant, "principal-approver-2")
	if w.Code != http.StatusConflict || errorCodeOf(t, w) != domain.ErrCodeInvalidTransition {
		t.Fatalf("approving an APPROVED version: want 409 %s, got %d %s", domain.ErrCodeInvalidTransition, w.Code, w.Body.String())
	}

	// publish: the version was never approved.
	draft := newDraftNotice(t, r)
	w = doRequestWithPrincipal(r, http.MethodPost, noticePath(draft, "publish"), nil, testTenant, "principal-publisher")
	if w.Code != http.StatusConflict || errorCodeOf(t, w) != domain.ErrCodeInvalidTransition {
		t.Fatalf("publishing a DRAFT version: want 409 %s, got %d %s", domain.ErrCodeInvalidTransition, w.Code, w.Body.String())
	}

	// withdraw: the version was never published.
	w = doRequestWithPrincipal(r, http.MethodPost, noticePath(draft, "withdraw"), nil, testTenant, "principal-publisher")
	if w.Code != http.StatusConflict || errorCodeOf(t, w) != domain.ErrCodeInvalidTransition {
		t.Fatalf("withdrawing a DRAFT version: want 409 %s, got %d %s", domain.ErrCodeInvalidTransition, w.Code, w.Body.String())
	}

	// The canonical latest-version routes refuse the same way.
	for _, verb := range []string{"publish", "withdraw"} {
		w = doRequestWithPrincipal(r, http.MethodPost, "/privacy/notices/"+draft.NoticeID+"/"+verb, nil, testTenant, "principal-publisher")
		if w.Code != http.StatusConflict || errorCodeOf(t, w) != domain.ErrCodeInvalidTransition {
			t.Fatalf("canonical %s of a DRAFT: want 409 %s, got %d %s", verb, domain.ErrCodeInvalidTransition, w.Code, w.Body.String())
		}
	}
}

// A genuine idempotency mismatch is still IDEMPOTENCY_MISMATCH — and it is a
// different answer from a refused transition on the very same status code.
func TestIdempotencyMismatch_StillAnswersIdempotencyMismatch(t *testing.T) {
	st := newStubStore()
	r := newTestRouter(st, &stubAuthz{}, grantingPurposeRegistry())
	key := uuid.NewString()
	hdr := map[string]string{"Idempotency-Key": key}

	body1 := domain.CreateNoticeRequest{Locale: "en-US", Audience: "CUSTOMER", ContentHash: "sha256:one"}
	body2 := domain.CreateNoticeRequest{Locale: "en-US", Audience: "CUSTOMER", ContentHash: "sha256:two"}
	if w := doRequestWithHeaders(r, http.MethodPost, "/privacy/notices", body1, testTenant, "principal-maker", hdr); w.Code != http.StatusCreated {
		t.Fatalf("first create: %d %s", w.Code, w.Body.String())
	}
	w := doRequestWithHeaders(r, http.MethodPost, "/privacy/notices", body2, testTenant, "principal-maker", hdr)
	if w.Code != http.StatusConflict || errorCodeOf(t, w) != domain.ErrCodeIdempotencyMismatch {
		t.Fatalf("same key, different body: want 409 %s, got %d %s", domain.ErrCodeIdempotencyMismatch, w.Code, w.Body.String())
	}
	if domain.ErrCodeIdempotencyMismatch == domain.ErrCodeInvalidTransition {
		t.Fatal("the two conditions must have distinct codes")
	}
}

// A refused transition does not consume its Idempotency-Key (the operation
// stored nothing), so the same key can be retried once the state allows it.
func TestInvalidNoticeTransition_DoesNotConsumeTheKey(t *testing.T) {
	st := newStubStore()
	r := newTestRouter(st, &stubAuthz{}, grantingPurposeRegistry())
	v := newDraftNotice(t, r)
	hdr := map[string]string{"Idempotency-Key": uuid.NewString()}

	w := doRequestWithHeaders(r, http.MethodPost, noticePath(v, "publish"), nil, testTenant, "principal-publisher", hdr)
	if w.Code != http.StatusConflict || errorCodeOf(t, w) != domain.ErrCodeInvalidTransition {
		t.Fatalf("publish of a DRAFT: %d %s", w.Code, w.Body.String())
	}
	if w := doRequestWithPrincipal(r, http.MethodPost, noticePath(v, "approve"), nil, testTenant, "principal-approver"); w.Code != http.StatusOK {
		t.Fatalf("approve: %d %s", w.Code, w.Body.String())
	}
	w = doRequestWithHeaders(r, http.MethodPost, noticePath(v, "publish"), nil, testTenant, "principal-publisher", hdr)
	if w.Code != http.StatusOK {
		t.Fatalf("retrying the same key after the state allows it: want 200, got %d %s", w.Code, w.Body.String())
	}
}

func TestValidNoticeTransitions_StillSucceed(t *testing.T) {
	st := newStubStore()
	r := newTestRouter(st, &stubAuthz{}, grantingPurposeRegistry())
	v := createPublishedNotice(t, r)
	w := doRequestWithPrincipal(r, http.MethodPost, noticePath(v, "withdraw"), nil, testTenant, "principal-publisher")
	if w.Code != http.StatusOK {
		t.Fatalf("withdraw of a PUBLISHED version: %d %s", w.Code, w.Body.String())
	}
}

// Authorization still precedes everything: a denied caller gets 403, never the
// 409 that would reveal the version's state, and the store is never reached.
func TestInvalidTransition_AuthorizationStillPrecedesLookupAndState(t *testing.T) {
	st := newStubStore()
	az := &stubAuthz{}
	r := newTestRouter(st, az, grantingPurposeRegistry())
	v := newDraftNotice(t, r) // a DRAFT: publishing it would be a 409 for an authorized caller

	az.deny = true
	calls := az.calls
	for _, verb := range []string{"approve", "publish", "withdraw"} {
		w := doRequestWithPrincipal(r, http.MethodPost, noticePath(v, verb), nil, testTenant, "principal-x")
		if w.Code != http.StatusForbidden || errorCodeOf(t, w) != domain.ErrCodeAuthorizationDenied {
			t.Fatalf("%s while unauthorized: want 403 %s, got %d %s", verb, domain.ErrCodeAuthorizationDenied, w.Code, w.Body.String())
		}
	}
	// Even an id that does not exist answers identically.
	w := doRequestWithPrincipal(r, http.MethodPost, "/privacy/notices/"+uuid.NewString()+"/versions/"+uuid.NewString()+"/publish", nil, testTenant, "principal-x")
	if w.Code != http.StatusForbidden {
		t.Fatalf("unknown id while unauthorized: want 403, got %d", w.Code)
	}
	if az.calls-calls != 4 {
		t.Fatalf("authorization must be consulted on every request, saw %d checks for 4 requests", az.calls-calls)
	}
}

// The pre-flight is unchanged for the transition routes: no principal and no
// tenant are still 401 before anything about the version is revealed.
func TestInvalidTransition_PreflightUnchanged(t *testing.T) {
	st := newStubStore()
	r := newTestRouter(st, &stubAuthz{}, grantingPurposeRegistry())
	v := newDraftNotice(t, r)

	w := doRequestWithHeaders(r, http.MethodPost, noticePath(v, "publish"), nil, testTenant, "x", map[string]string{"X-Principal-Id": ""})
	if w.Code != http.StatusUnauthorized || errorCodeOf(t, w) != domain.ErrCodeAuthenticationFailed {
		t.Fatalf("no principal: want 401 %s, got %d %s", domain.ErrCodeAuthenticationFailed, w.Code, w.Body.String())
	}
	w = doRequestWithPrincipal(r, http.MethodPost, noticePath(v, "publish"), nil, "", "principal-publisher")
	if w.Code != http.StatusUnauthorized || errorCodeOf(t, w) != domain.ErrCodeTenantScopeRequired {
		t.Fatalf("no tenant: want 401 %s, got %d %s", domain.ErrCodeTenantScopeRequired, w.Code, w.Body.String())
	}
}
