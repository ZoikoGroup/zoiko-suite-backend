package handler_test

import (
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/google/uuid"

	"zoiko.io/delegated-authority-svc/internal/domain"
	"zoiko.io/delegated-authority-svc/internal/events"
)

// ── An idempotent replay must not be reported as a new grant ─────────────────

// The store has always been idempotent on (tenant_id, correlation_id), and the
// handler answered 201 Created either way. That made the idempotency
// undetectable by the caller: a resubmitted form got "Created" and the
// ORIGINAL grant's body back, so the console told an operator they had just
// handed someone their authority when in fact nothing had been written. Its
// own client code carried a branch for the 200 that no response ever took.
func TestCreate_ReplayAnswers200NotCreated(t *testing.T) {
	correlationID := uuid.NewString()
	store := newStubStore()
	r := newRouter(store, &stubAuthZ{})
	body := delegationBody("delegator-1", correlationID, time.Now(), time.Now().Add(24*time.Hour))

	first := doReq(r, http.MethodPost, "/v1/delegations/", body, "delegator-1")
	if first.Code != http.StatusCreated {
		t.Fatalf("first create: expected 201 got %d: %s", first.Code, first.Body.String())
	}

	second := doReq(r, http.MethodPost, "/v1/delegations/", body, "delegator-1")
	if second.Code != http.StatusOK {
		t.Fatalf("replay: expected 200 (nothing was written) got %d: %s", second.Code, second.Body.String())
	}

	var d1, d2 domain.DelegationGrant
	_ = json.NewDecoder(first.Body).Decode(&d1)
	_ = json.NewDecoder(second.Body).Decode(&d2)
	if d1.DelegationID != d2.DelegationID {
		t.Errorf("replay resolved to a different grant (%s vs %s)", d2.DelegationID, d1.DelegationID)
	}
}

// One grant, one event. If a replay emitted a second authority.delegated,
// every consumer would see an authority granted twice — and a consumer that
// counts live delegations would be permanently wrong.
func TestCreate_ReplayEmitsNoSecondEvent(t *testing.T) {
	correlationID := uuid.NewString()
	store := newStubStore()
	r := newRouter(store, &stubAuthZ{})
	body := delegationBody("delegator-1", correlationID, time.Now(), time.Now().Add(24*time.Hour))

	_ = doReq(r, http.MethodPost, "/v1/delegations/", body, "delegator-1")
	_ = doReq(r, http.MethodPost, "/v1/delegations/", body, "delegator-1")

	if n := store.countEvents(events.EventDelegated); n != 1 {
		t.Errorf("two submissions of one correlation_id produced %d authority.delegated events, want 1", n)
	}
}

// ── Revoking a grant that has already lapsed ─────────────────────────────────

// Expiry here is lazy: a delegation past its window stays ACTIVE in the row
// until some read observes the lapse. Revoke was the one path that did not
// sweep first, so it wrote REVOKED, revoked_at and revoked_by over a grant that
// had already run out — the register then named a principal as having withdrawn
// an authority at a moment when nobody withdrew anything, and published
// authority.revoked in place of authority.expired.
//
// After the sweep the row reads EXPIRED and the revoke correctly answers 409.
func TestRevoke_SweepsExpiryFirstSoALapsedGrantIsNotRecordedAsRevoked(t *testing.T) {
	store := newStubStore()
	r := newRouter(store, &stubAuthZ{})

	past := time.Now().Add(-48 * time.Hour)
	created := doReq(r, http.MethodPost, "/v1/delegations/",
		delegationBody("delegator-1", uuid.NewString(), past, past.Add(time.Hour)), "delegator-1")
	if created.Code != http.StatusCreated {
		t.Fatalf("setup: %d %s", created.Code, created.Body.String())
	}
	var d domain.DelegationGrant
	_ = json.NewDecoder(created.Body).Decode(&d)

	rr := doReq(r, http.MethodPost, "/v1/delegations/"+d.DelegationID+"/revoke", nil, "admin-1")
	if rr.Code != http.StatusConflict {
		t.Fatalf("expected 409 — the grant expired before anyone revoked it — got %d: %s", rr.Code, rr.Body.String())
	}
	if n := store.countEvents(events.EventRevoked); n != 0 {
		t.Errorf("published %d authority.revoked event(s) for a grant nobody revoked", n)
	}
	if n := store.countEvents(events.EventExpired); n != 1 {
		t.Errorf("expected exactly 1 authority.expired, got %d", n)
	}
}

// The ordinary case still works: an in-window grant revokes normally, and the
// sweep added to this path must not disturb it.
func TestRevoke_InWindowGrantStillRevokes(t *testing.T) {
	store := newStubStore()
	r := newRouter(store, &stubAuthZ{})
	d := createActiveDelegation(t, r)

	rr := doReq(r, http.MethodPost, "/v1/delegations/"+d.DelegationID+"/revoke", nil, "admin-1")
	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200 got %d: %s", rr.Code, rr.Body.String())
	}
	var updated domain.DelegationGrant
	_ = json.NewDecoder(rr.Body).Decode(&updated)
	if updated.Status != domain.DelegationStatusRevoked {
		t.Errorf("status = %q, want REVOKED", updated.Status)
	}
	if updated.RevokedByPrincipalID == nil || *updated.RevokedByPrincipalID != "admin-1" {
		t.Errorf("revoked_by = %v, want the caller", updated.RevokedByPrincipalID)
	}
	if n := store.countEvents(events.EventExpired); n != 0 {
		t.Errorf("an in-window revoke emitted %d authority.expired event(s)", n)
	}
}
