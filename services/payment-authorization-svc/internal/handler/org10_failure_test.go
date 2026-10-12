package handler_test

import (
	"net/http"
	"testing"
	"time"

	"zoiko.io/payment-authorization-svc/internal/domain"
)

// An ORG-10 outage at request time used to be logged and ignored: the
// authorization was created with DestinationID == "", and every later
// re-check skipped an empty DestinationID. It now fails closed.
func TestRequestPaymentAuthorization_ORG10Unavailable_Returns503_NothingPersisted(t *testing.T) {
	prop := newStubProposal()
	sup := newStubSupplier()
	setupFrozenProposal(prop, sup, "prop-f1", "vendor-f1", time.Now().UTC(), 100)
	payee := newStubPayee()
	payee.failWith = domain.ErrPayeeDestinationServiceUnavailable
	st := newStubStore()
	pub := &stubPublisher{}
	r := newTestRouterWithPayee(st, pub, &stubAuthz{}, prop, sup, payee, &stubPolicy{})

	w := doRequest(r, http.MethodPost, "/ap10/authorizations/", domain.RequestAuthorizationRequest{ProposalID: "prop-f1"}, testTenant)
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected 503 when ORG-10 is unavailable, got %d: %s", w.Code, w.Body.String())
	}
	if len(st.auths) != 0 {
		t.Fatalf("no authorization may be persisted when ORG-10 lookup fails, found %d", len(st.auths))
	}
	if len(st.snapshots) != 0 {
		t.Fatalf("no payee snapshot may be persisted when ORG-10 lookup fails, found %d", len(st.snapshots))
	}
	if len(st.activeByProposal) != 0 {
		t.Fatalf("the proposal must not be marked as having an active authorization")
	}
	if pub.calls != 0 {
		t.Fatalf("no authorization event may be published, got %d", pub.calls)
	}

	// The failure is not sticky: once ORG-10 recovers the same request succeeds.
	payee.failWith = nil
	if a := requestAuthorization(t, r, "prop-f1"); a.Status != domain.StatusPending {
		t.Fatalf("expected PENDING after ORG-10 recovers, got %s", a.Status)
	}
}

// Any non-"no coverage" error fails closed, not just the named sentinel.
func TestRequestPaymentAuthorization_ORG10UnexpectedError_Returns503(t *testing.T) {
	prop := newStubProposal()
	sup := newStubSupplier()
	setupFrozenProposal(prop, sup, "prop-f2", "vendor-f2", time.Now().UTC(), 100)
	payee := newStubPayee()
	payee.failWith = transportErr{}
	st := newStubStore()
	r := newTestRouterWithPayee(st, &stubPublisher{}, &stubAuthz{}, prop, sup, payee, &stubPolicy{})

	w := doRequest(r, http.MethodPost, "/ap10/authorizations/", domain.RequestAuthorizationRequest{ProposalID: "prop-f2"}, testTenant)
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected 503 on an arbitrary ORG-10 error, got %d: %s", w.Code, w.Body.String())
	}
	if len(st.auths) != 0 {
		t.Fatalf("no authorization may be persisted, found %d", len(st.auths))
	}
}

type transportErr struct{}

func (transportErr) Error() string { return "context deadline exceeded" }

// An unpinned snapshot (payee had no ORG-10 coverage) stays valid while ORG-10
// still has none — the documented policy is unchanged.
func TestApprovePayment_UnpinnedStillUncovered_Succeeds(t *testing.T) {
	prop := newStubProposal()
	sup := newStubSupplier()
	setupFrozenProposal(prop, sup, "prop-f3", "vendor-f3", time.Now().UTC(), 100)
	r := newTestRouter(newStubStore(), &stubPublisher{}, &stubAuthz{sodRules: true}, prop, sup, &stubPolicy{})
	a := requestAuthorization(t, r, "prop-f3")

	w := doRequestAs(r, http.MethodPost, "/ap10/authorizations/"+a.AuthorizationID+"/approve", nil, testTenant, "principal-checker")
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200: unpinned with no ORG-10 coverage is still allowed, got %d: %s", w.Code, w.Body.String())
	}
}

// An unpinned authorization must not silently pass once ORG-10 has a
// destination for that payee.
func TestApprovePayment_UnpinnedButDestinationNowExists_Invalidated(t *testing.T) {
	prop := newStubProposal()
	sup := newStubSupplier()
	setupFrozenProposal(prop, sup, "prop-f4", "vendor-f4", time.Now().UTC(), 100)
	payee := newStubPayee()
	r := newTestRouterWithPayee(newStubStore(), &stubPublisher{}, &stubAuthz{sodRules: true}, prop, sup, payee, &stubPolicy{})
	a := requestAuthorization(t, r, "prop-f4") // no coverage → unpinned

	payee.set(testLegalEntity, "vendor-f4", "destination-new")

	w := doRequestAs(r, http.MethodPost, "/ap10/authorizations/"+a.AuthorizationID+"/approve", nil, testTenant, "principal-checker")
	if w.Code != http.StatusConflict {
		t.Fatalf("expected 409 for an unpinned authorization whose payee now has a destination, got %d: %s", w.Code, w.Body.String())
	}
	fetched, _ := newStubStoreFind(t, r, a.AuthorizationID, testTenant)
	if fetched.Status != domain.StatusInvalidated {
		t.Fatalf("expected INVALIDATED, got %s", fetched.Status)
	}
}

// An unpinned authorization is not approved while ORG-10 cannot be asked.
func TestApprovePayment_UnpinnedAndORG10Unavailable_Returns503(t *testing.T) {
	prop := newStubProposal()
	sup := newStubSupplier()
	setupFrozenProposal(prop, sup, "prop-f5", "vendor-f5", time.Now().UTC(), 100)
	payee := newStubPayee()
	r := newTestRouterWithPayee(newStubStore(), &stubPublisher{}, &stubAuthz{sodRules: true}, prop, sup, payee, &stubPolicy{})
	a := requestAuthorization(t, r, "prop-f5")

	payee.failWith = domain.ErrPayeeDestinationServiceUnavailable

	w := doRequestAs(r, http.MethodPost, "/ap10/authorizations/"+a.AuthorizationID+"/approve", nil, testTenant, "principal-checker")
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected 503 approving an unpinned authorization while ORG-10 is down, got %d: %s", w.Code, w.Body.String())
	}
	fetched, _ := newStubStoreFind(t, r, a.AuthorizationID, testTenant)
	if fetched.Status == domain.StatusApproved {
		t.Fatalf("authorization must not be APPROVED while ORG-10 cannot be consulted")
	}
}
