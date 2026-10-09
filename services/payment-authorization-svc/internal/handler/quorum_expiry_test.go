package handler_test

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

	"zoiko.io/payment-authorization-svc/internal/domain"
	"zoiko.io/payment-authorization-svc/internal/expiry"
	"zoiko.io/payment-authorization-svc/internal/handler"
	"zoiko.io/payment-authorization-svc/internal/middleware"
)

// clock is a settable test clock for expiry.
type clock struct{ t time.Time }

func (c *clock) now() time.Time { return c.t }

// routerWith builds a router with a controllable clock and options.
func routerWith(st *stubStore, az *stubAuthz, prop *stubProposal, sup *stubSupplier, payee *stubPayee, pol *stubPolicy, opts handler.Options) chi.Router {
	h := handler.New(st, &stubPublisher{}, az, prop, sup, payee, pol, zap.NewNop()).WithOptions(opts)
	r := chi.NewRouter()
	r.Use(middleware.TenantContext())
	handler.RegisterRoutes(r, h)
	return r
}

func postKeyed(r http.Handler, path, key, principal string, body interface{}) *httptest.ResponseRecorder {
	var buf bytes.Buffer
	if body != nil {
		_ = json.NewEncoder(&buf).Encode(body)
	}
	req := httptest.NewRequest(http.MethodPost, path, &buf)
	req.Header.Set("X-Tenant-Id", testTenant)
	req.Header.Set("X-Principal-Id", principal)
	if key != "" {
		req.Header.Set("Idempotency-Key", key)
	}
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w
}

func errorCode(t *testing.T, w *httptest.ResponseRecorder) string {
	t.Helper()
	var e struct {
		Code string `json:"code"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &e)
	return e.Code
}

func highValueSetup(t *testing.T, proposalID, vendor string) (*stubStore, *stubAuthz, *stubProposal, *stubSupplier, *stubPayee) {
	t.Helper()
	prop, sup := newStubProposal(), newStubSupplier()
	setupFrozenProposal(prop, sup, proposalID, vendor, time.Now().UTC(), 100)
	return newStubStore(), &stubAuthz{sodRules: true}, prop, sup, newStubPayee()
}

// ── quorum ───────────────────────────────────────────────────────────────────

func TestQuorum_HighValue_NeedsTwoDistinctSigners(t *testing.T) {
	st, az, prop, sup, payee := highValueSetup(t, "prop-q1", "vendor-q1")
	r := routerWith(st, az, prop, sup, payee, &stubPolicy{result: "APPROVAL_REQUIRED"}, handler.Options{})
	a := requestAuthorization(t, r, "prop-q1")
	path := "/ap10/authorizations/" + a.AuthorizationID + "/approve"

	w := postKeyed(r, path, "k1", "signer-1", nil)
	if w.Code != http.StatusAccepted {
		t.Fatalf("first signature: expected 202, got %d: %s", w.Code, w.Body.String())
	}
	if got := st.auths[a.AuthorizationID].Status; got != domain.StatusPending {
		t.Fatalf("expected still PENDING after one of two signatures, got %s", got)
	}

	// The same person cannot sign twice (negative-path: shared credentials / quorum bypass).
	w = postKeyed(r, path, "k2", "signer-1", nil)
	if w.Code != http.StatusConflict || errorCode(t, w) != handler.CodeAlreadySigned {
		t.Fatalf("expected 409 ALREADY_SIGNED, got %d %s", w.Code, w.Body.String())
	}
	if len(st.signatures[a.AuthorizationID]) != 1 {
		t.Fatalf("a repeat signature must not be recorded, got %d", len(st.signatures[a.AuthorizationID]))
	}

	// The proposal's preparer can never be a signer.
	w = postKeyed(r, path, "k3", testPreparer, nil)
	if w.Code != http.StatusForbidden {
		t.Fatalf("expected the preparer to be refused, got %d: %s", w.Code, w.Body.String())
	}

	w = postKeyed(r, path, "k4", "signer-2", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("second distinct signer: expected 200, got %d: %s", w.Code, w.Body.String())
	}
	got := st.auths[a.AuthorizationID]
	if got.Status != domain.StatusApproved || got.SignatureCount != 2 || got.RequiredSignatures != 2 {
		t.Fatalf("expected APPROVED 2/2, got %s %d/%d", got.Status, got.SignatureCount, got.RequiredSignatures)
	}
}

func TestQuorum_NormalPayment_SingleSigner(t *testing.T) {
	st, az, prop, sup, payee := highValueSetup(t, "prop-q2", "vendor-q2")
	r := routerWith(st, az, prop, sup, payee, &stubPolicy{}, handler.Options{})
	a := requestAuthorization(t, r, "prop-q2")
	if w := postKeyed(r, "/ap10/authorizations/"+a.AuthorizationID+"/approve", "k1", "signer-1", nil); w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
}

func TestQuorum_Configurable(t *testing.T) {
	st, az, prop, sup, payee := highValueSetup(t, "prop-q3", "vendor-q3")
	r := routerWith(st, az, prop, sup, payee, &stubPolicy{result: "APPROVAL_REQUIRED"}, handler.Options{HighValueSignatures: 3})
	a := requestAuthorization(t, r, "prop-q3")
	path := "/ap10/authorizations/" + a.AuthorizationID + "/approve"
	for i, signer := range []string{"s1", "s2"} {
		if w := postKeyed(r, path, "k"+signer, signer, nil); w.Code != http.StatusAccepted {
			t.Fatalf("signature %d: expected 202, got %d", i+1, w.Code)
		}
	}
	if w := postKeyed(r, path, "ks3", "s3", nil); w.Code != http.StatusOK {
		t.Fatalf("third signature: expected 200, got %d: %s", w.Code, w.Body.String())
	}
}

// ── idempotency ──────────────────────────────────────────────────────────────

func TestIdempotency_KeyRequiredOnRequestApproveConsume(t *testing.T) {
	st, az, prop, sup, payee := highValueSetup(t, "prop-i1", "vendor-i1")
	r := routerWith(st, az, prop, sup, payee, &stubPolicy{}, handler.Options{})

	w := postKeyed(r, "/ap10/authorizations/", "", "maker", domain.RequestAuthorizationRequest{ProposalID: "prop-i1"})
	if w.Code != http.StatusBadRequest || errorCode(t, w) != handler.CodeIdempotencyRequired {
		t.Fatalf("request: expected 400 IDEMPOTENCY_KEY_REQUIRED, got %d %s", w.Code, w.Body.String())
	}
	a := requestAuthorization(t, r, "prop-i1")
	for _, cmd := range []string{"approve", "consume"} {
		w = postKeyed(r, "/ap10/authorizations/"+a.AuthorizationID+"/"+cmd, "", "signer-1", nil)
		if w.Code != http.StatusBadRequest || errorCode(t, w) != handler.CodeIdempotencyRequired {
			t.Fatalf("%s: expected 400 IDEMPOTENCY_KEY_REQUIRED, got %d %s", cmd, w.Code, w.Body.String())
		}
	}
}

// TestIdempotency_ReplayReturnsStoredResult_AndDoesNotRunAgain: a retry of a
// completed approve with the same key replays the stored response and does
// not record a second signature.
func TestIdempotency_ReplayReturnsStoredResult_AndDoesNotRunAgain(t *testing.T) {
	st, az, prop, sup, payee := highValueSetup(t, "prop-i2", "vendor-i2")
	r := routerWith(st, az, prop, sup, payee, &stubPolicy{result: "APPROVAL_REQUIRED"}, handler.Options{})
	a := requestAuthorization(t, r, "prop-i2")
	path := "/ap10/authorizations/" + a.AuthorizationID + "/approve"

	first := postKeyed(r, path, "same-key", "signer-1", nil)
	if first.Code != http.StatusAccepted {
		t.Fatalf("first: expected 202, got %d", first.Code)
	}
	replay := postKeyed(r, path, "same-key", "signer-1", nil)
	if replay.Code != http.StatusAccepted || replay.Header().Get("Idempotent-Replay") != "true" {
		t.Fatalf("replay: expected 202 with Idempotent-Replay, got %d %v", replay.Code, replay.Header())
	}
	if replay.Body.String() != first.Body.String() {
		t.Fatalf("replay body differs from the original")
	}
	if len(st.signatures[a.AuthorizationID]) != 1 {
		t.Fatalf("a replay must not record another signature, got %d", len(st.signatures[a.AuthorizationID]))
	}
}

func TestIdempotency_SameKeyDifferentRequest_Rejected(t *testing.T) {
	st, az, prop, sup, payee := highValueSetup(t, "prop-i3", "vendor-i3")
	r := routerWith(st, az, prop, sup, payee, &stubPolicy{result: "APPROVAL_REQUIRED"}, handler.Options{})
	a := requestAuthorization(t, r, "prop-i3")
	path := "/ap10/authorizations/" + a.AuthorizationID + "/approve"

	postKeyed(r, path, "shared", "signer-1", nil)
	w := postKeyed(r, path, "shared", "signer-2", nil) // different principal = different request
	if w.Code != http.StatusUnprocessableEntity || errorCode(t, w) != handler.CodeIdempotencyReused {
		t.Fatalf("expected 422 IDEMPOTENCY_KEY_REUSED, got %d %s", w.Code, w.Body.String())
	}
	if len(st.signatures[a.AuthorizationID]) != 1 {
		t.Fatalf("the rejected request must not sign")
	}
}

// ── expiry ───────────────────────────────────────────────────────────────────

func TestExpiry_ExpiresAtSetOnRequest(t *testing.T) {
	st, az, prop, sup, payee := highValueSetup(t, "prop-e1", "vendor-e1")
	c := &clock{t: time.Date(2026, 10, 8, 9, 0, 0, 0, time.UTC)}
	r := routerWith(st, az, prop, sup, payee, &stubPolicy{}, handler.Options{AuthorizationTTL: 6 * time.Hour, Now: c.now})
	a := requestAuthorization(t, r, "prop-e1")
	want := c.t.Add(6 * time.Hour)
	if a.ExpiresAt == nil || !a.ExpiresAt.Equal(want) {
		t.Fatalf("expected expires_at %v, got %v", want, a.ExpiresAt)
	}
}

func TestExpiry_ApproveAfterExpiry_RefusedAndExpired(t *testing.T) {
	st, az, prop, sup, payee := highValueSetup(t, "prop-e2", "vendor-e2")
	c := &clock{t: time.Now().UTC()}
	r := routerWith(st, az, prop, sup, payee, &stubPolicy{}, handler.Options{AuthorizationTTL: time.Hour, Now: c.now})
	a := requestAuthorization(t, r, "prop-e2")

	c.t = c.t.Add(2 * time.Hour)
	w := postKeyed(r, "/ap10/authorizations/"+a.AuthorizationID+"/approve", "k1", "signer-1", nil)
	if w.Code != http.StatusConflict || errorCode(t, w) != handler.CodeAuthorizationExpired {
		t.Fatalf("expected 409 AUTHORIZATION_EXPIRED, got %d %s", w.Code, w.Body.String())
	}
	if got := st.auths[a.AuthorizationID].Status; got != domain.StatusExpired {
		t.Fatalf("expected the overdue authorization marked EXPIRED, got %s", got)
	}
}

func TestExpiry_ConsumeAfterExpiry_Refused(t *testing.T) {
	st, az, prop, sup, payee := highValueSetup(t, "prop-e3", "vendor-e3")
	c := &clock{t: time.Now().UTC()}
	r := routerWith(st, az, prop, sup, payee, &stubPolicy{}, handler.Options{AuthorizationTTL: time.Hour, Now: c.now})
	a := requestAuthorization(t, r, "prop-e3")
	if w := postKeyed(r, "/ap10/authorizations/"+a.AuthorizationID+"/approve", "k1", "signer-1", nil); w.Code != http.StatusOK {
		t.Fatalf("approve: %d %s", w.Code, w.Body.String())
	}

	c.t = c.t.Add(2 * time.Hour)
	w := postKeyed(r, "/ap10/authorizations/"+a.AuthorizationID+"/consume", "k2", "runner", nil)
	if w.Code != http.StatusConflict || errorCode(t, w) != handler.CodeAuthorizationExpired {
		t.Fatalf("expected 409 AUTHORIZATION_EXPIRED on consume, got %d %s", w.Code, w.Body.String())
	}
	if st.auths[a.AuthorizationID].ConsumedAt != nil {
		t.Fatalf("an expired authorization must not be consumed")
	}
}

func TestExpiry_Validate_ReportsExpired(t *testing.T) {
	st, az, prop, sup, payee := highValueSetup(t, "prop-e4", "vendor-e4")
	c := &clock{t: time.Now().UTC()}
	r := routerWith(st, az, prop, sup, payee, &stubPolicy{}, handler.Options{AuthorizationTTL: time.Hour, Now: c.now})
	a := requestAuthorization(t, r, "prop-e4")
	postKeyed(r, "/ap10/authorizations/"+a.AuthorizationID+"/approve", "k1", "signer-1", nil)

	c.t = c.t.Add(2 * time.Hour)
	w := doRequest(r, http.MethodGet, "/ap10/authorizations/"+a.AuthorizationID+"/validate", nil, testTenant)
	var resp struct {
		Valid bool `json:"valid"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &resp)
	if resp.Valid {
		t.Fatalf("an expired authorization must not validate: %s", w.Body.String())
	}
}

// TestExpiry_Sweeper expires overdue authorizations through the store (one
// event each) and leaves live ones alone.
func TestExpiry_Sweeper(t *testing.T) {
	st := newStubStore()
	past, future := time.Now().Add(-time.Minute), time.Now().Add(time.Hour)
	mk := func(proposal string, exp *time.Time, status domain.AuthorizationStatus) string {
		a, _ := st.RequestAuthorization(context.Background(), testTenant, domain.PaymentAuthorization{ProposalID: proposal, ExpiresAt: exp}, nil)
		a.Status = status
		return a.AuthorizationID
	}
	overduePending := mk("p-1", &past, domain.StatusPending)
	overdueApproved := mk("p-2", &past, domain.StatusApproved)
	live := mk("p-3", &future, domain.StatusPending)
	consumed := mk("p-4", &past, domain.StatusConsumed)
	neverExpires := mk("p-5", nil, domain.StatusPending)

	n := expiry.New(st, time.Minute, 100, zap.NewNop()).RunOnce(context.Background())
	if n != 2 {
		t.Fatalf("expected 2 expired, got %d", n)
	}
	for id, want := range map[string]domain.AuthorizationStatus{
		overduePending: domain.StatusExpired, overdueApproved: domain.StatusExpired,
		live: domain.StatusPending, consumed: domain.StatusConsumed, neverExpires: domain.StatusPending,
	} {
		if got := st.auths[id].Status; got != want {
			t.Errorf("authorization %s: expected %s, got %s", id, want, got)
		}
	}
	if len(st.events[overduePending]) == 0 || st.events[overduePending][len(st.events[overduePending])-1].EventType != domain.EventAuthorizationExpired {
		t.Errorf("expected an Expired event on the swept authorization")
	}
}

// ── stale version ────────────────────────────────────────────────────────────

func TestExpectedVersion_StaleRefused_CurrentAccepted(t *testing.T) {
	st, az, prop, sup, payee := highValueSetup(t, "prop-v1", "vendor-v1")
	r := routerWith(st, az, prop, sup, payee, &stubPolicy{}, handler.Options{})
	a := requestAuthorization(t, r, "prop-v1")
	path := "/ap10/authorizations/" + a.AuthorizationID + "/approve"

	stale := a.Version + 5
	w := postKeyed(r, path, "k1", "signer-1", domain.ApproveRequest{ExpectedVersion: &stale})
	if w.Code != http.StatusConflict || errorCode(t, w) != handler.CodeStaleVersion {
		t.Fatalf("expected 409 STALE_VERSION, got %d %s", w.Code, w.Body.String())
	}
	cur := a.Version
	if w := postKeyed(r, path, "k2", "signer-1", domain.ApproveRequest{ExpectedVersion: &cur}); w.Code != http.StatusOK {
		t.Fatalf("expected 200 with the current version, got %d: %s", w.Code, w.Body.String())
	}

}

// ── payee-bank changer cannot authorize ─────────────────────────────────────

func TestBankChanger_CannotApprove(t *testing.T) {
	for _, role := range []string{"proposer", "verifier", "approver"} {
		t.Run(role, func(t *testing.T) {
			st, az, prop, sup, payee := highValueSetup(t, "prop-b-"+role, "vendor-b-"+role)
			payee.set(testLegalEntity, "vendor-b-"+role, "dest-1")
			payee.setChangedBy(testLegalEntity, "vendor-b-"+role, role, "bank-changer")
			r := routerWith(st, az, prop, sup, payee, &stubPolicy{}, handler.Options{})
			a := requestAuthorization(t, r, "prop-b-"+role)

			w := postKeyed(r, "/ap10/authorizations/"+a.AuthorizationID+"/approve", "k1", "bank-changer", nil)
			if w.Code != http.StatusForbidden || errorCode(t, w) != handler.CodeSoDConflict {
				t.Fatalf("expected 403 SOD_CONFLICT, got %d %s", w.Code, w.Body.String())
			}
			if st.auths[a.AuthorizationID].Status != domain.StatusPending || len(st.signatures[a.AuthorizationID]) != 0 {
				t.Fatalf("a bank changer's attempt must not sign anything")
			}
			// an independent signer is fine
			if w := postKeyed(r, "/ap10/authorizations/"+a.AuthorizationID+"/approve", "k2", "independent", nil); w.Code != http.StatusOK {
				t.Fatalf("independent signer: expected 200, got %d: %s", w.Code, w.Body.String())
			}
		})
	}
}

func TestBankChanger_CannotRequest(t *testing.T) {
	st, az, prop, sup, payee := highValueSetup(t, "prop-b4", "vendor-b4")
	payee.set(testLegalEntity, "vendor-b4", "dest-1")
	payee.setChangedBy(testLegalEntity, "vendor-b4", "approver", "bank-changer")
	r := routerWith(st, az, prop, sup, payee, &stubPolicy{}, handler.Options{})

	w := postKeyed(r, "/ap10/authorizations/", "k1", "bank-changer", domain.RequestAuthorizationRequest{ProposalID: "prop-b4"})
	if w.Code != http.StatusForbidden || errorCode(t, w) != handler.CodeSoDConflict {
		t.Fatalf("expected 403 SOD_CONFLICT, got %d %s", w.Code, w.Body.String())
	}
	if len(st.auths) != 0 {
		t.Fatalf("no authorization may be created for a bank changer")
	}
}

func TestBankChanger_Org10Down_FailsClosed(t *testing.T) {
	st, az, prop, sup, payee := highValueSetup(t, "prop-b5", "vendor-b5")
	payee.set(testLegalEntity, "vendor-b5", "dest-1")
	r := routerWith(st, az, prop, sup, payee, &stubPolicy{}, handler.Options{})
	a := requestAuthorization(t, r, "prop-b5")

	payee.failWith = domain.ErrPayeeDestinationServiceUnavailable
	w := postKeyed(r, "/ap10/authorizations/"+a.AuthorizationID+"/approve", "k1", "signer-1", nil)
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected 503 when ORG-10 cannot be asked, got %d: %s", w.Code, w.Body.String())
	}
	if len(st.signatures[a.AuthorizationID]) != 0 {
		t.Fatalf("nothing may be signed while ORG-10 is unavailable")
	}
}

// ── stable codes ─────────────────────────────────────────────────────────────

func TestErrorCodes_PresentOnInvalidation(t *testing.T) {
	st, az, prop, sup, payee := highValueSetup(t, "prop-c1", "vendor-c1")
	r := routerWith(st, az, prop, sup, payee, &stubPolicy{}, handler.Options{})
	a := requestAuthorization(t, r, "prop-c1")
	prop.fingerprints["prop-c1"] = "fp-changed"
	w := postKeyed(r, "/ap10/authorizations/"+a.AuthorizationID+"/approve", "k1", "signer-1", nil)
	if w.Code != http.StatusConflict || errorCode(t, w) != handler.CodeAuthorizationInvalid {
		t.Fatalf("expected 409 AUTHORIZATION_INVALIDATED, got %d %s", w.Code, w.Body.String())
	}
}
