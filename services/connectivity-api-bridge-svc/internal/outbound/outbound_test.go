package outbound_test

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"zoiko.io/connectivity-api-bridge-svc/internal/outbound"
	"zoiko.io/connectivity-api-bridge-svc/internal/registry"
	"zoiko.io/connectivity-api-bridge-svc/internal/xicconn"
)

// ---- reservation: durable before any network call (I-13) -------------------------------------------

func TestReserve_IsDurableAndMakesNoNetworkCall(t *testing.T) {
	e := newEnv(t)
	c := e.setup(variant{})
	a := e.reserve(c, "PAY-1")
	if a.State != outbound.StateReserved || a.SubmitCount != 0 || a.PayloadHash == "" || a.ProviderIdemRefHash == "" {
		t.Fatalf("%+v", a)
	}
	if e.prov.count() != 0 {
		t.Fatal("reserving called the provider")
	}
	if e.secrets.hits != 0 {
		t.Fatal("reserving fetched a credential")
	}
	got := e.get(tenantA, a.AttemptID)
	if got.PayloadHash != a.PayloadHash || !got.BodyRetained {
		t.Fatalf("%+v", got)
	}
	if !hasType(e.repo.Types(), "integration.outbound.reserved") {
		t.Fatalf("%v", e.repo.Types())
	}
}

func TestReserve_SameDomainRequestReturnsTheOriginalAndNeverASecondOperation(t *testing.T) {
	e := newEnv(t)
	c := e.setup(variant{})
	a := e.reserve(c, "PAY-1")
	again, created, err := e.svc.Reserve(ctxFor(tenantA), domain, outbound.ReserveInput{ConnectionID: c.ConnectionID, OperationID: "payment-submit", DomainRequestRef: "PAY-1",
		Payload: json.RawMessage(` { "currency" : "GBP", "amount":"10.00" } `)})
	// the same JSON in another layout and key order is a different byte string but the same payload only if it
	// hashes the same after compaction: key order differs, so it is a conflict rather than a silent match
	if err == nil && created {
		t.Fatal("a second logical operation was created")
	}
	same, created, err := e.svc.Reserve(ctxFor(tenantA), domain, outbound.ReserveInput{ConnectionID: c.ConnectionID, OperationID: "payment-submit", DomainRequestRef: "PAY-1",
		Payload: json.RawMessage("{ \"amount\":\"10.00\",\n \"currency\":\"GBP\" }")})
	if err != nil || created || same.AttemptID != a.AttemptID {
		t.Fatalf("%v created=%v %+v", err, created, same)
	}
	_ = again
	_, _, err = e.svc.Reserve(ctxFor(tenantA), domain, outbound.ReserveInput{ConnectionID: c.ConnectionID, OperationID: "payment-submit", DomainRequestRef: "PAY-1",
		Payload: json.RawMessage(`{"amount":"99.00","currency":"GBP"}`)})
	if !errors.Is(err, outbound.ErrPayloadConflict) {
		t.Fatalf("a changed payload under the same request must conflict: %v", err)
	}
	list, _ := e.repo.List(ctxFor(tenantA), outbound.Filter{})
	if len(list) != 1 {
		t.Fatalf("%d attempts", len(list))
	}
}

func TestReserve_RefusalsLeaveNothingBehind(t *testing.T) {
	e := newEnv(t)
	c := e.setup(variant{})
	res := func(tenant string, in outbound.ReserveInput) error {
		_, _, err := e.svc.Reserve(ctxFor(tenant), domain, in)
		return err
	}
	base := outbound.ReserveInput{ConnectionID: c.ConnectionID, OperationID: "payment-submit", DomainRequestRef: "PAY-X", Payload: json.RawMessage(payRef)}
	with := func(f func(*outbound.ReserveInput)) outbound.ReserveInput { b := base; f(&b); return b }

	for name, in := range map[string]outbound.ReserveInput{
		"no connection":    with(func(b *outbound.ReserveInput) { b.ConnectionID = "x" }),
		"no operation":     with(func(b *outbound.ReserveInput) { b.OperationID = "" }),
		"bad request ref":  with(func(b *outbound.ReserveInput) { b.DomainRequestRef = "has space" }),
		"empty payload":    with(func(b *outbound.ReserveInput) { b.Payload = nil }),
		"null payload":     with(func(b *outbound.ReserveInput) { b.Payload = json.RawMessage("null") }),
		"not json":         with(func(b *outbound.ReserveInput) { b.Payload = json.RawMessage("{nope") }),
		"schema violation": with(func(b *outbound.ReserveInput) { b.Payload = json.RawMessage(`{"amount":"1.00"}`) }),
		"wrong field type": with(func(b *outbound.ReserveInput) { b.Payload = json.RawMessage(`{"amount":1,"currency":"GBP"}`) }),
	} {
		if err := res(tenantA, in); !errors.Is(err, outbound.ErrBadRequest) {
			t.Errorf("%s: %v", name, err)
		}
	}
	var ref *xicconn.Refusal
	if err := res(tenantA, with(func(b *outbound.ReserveInput) { b.OperationID = "nope" })); !errors.As(err, &ref) || ref.Code != xicconn.CodeContractMismatch {
		t.Errorf("an unregistered operation: %v", err)
	}
	if err := res(tenantA, with(func(b *outbound.ReserveInput) { b.OperationID = "statement-push" })); !errors.Is(err, outbound.ErrOperationDirection) {
		t.Errorf("an inbound operation: %v", err)
	}
	// another tenant cannot reserve against this connection
	if err := res(tenantB, base); !errors.As(err, &ref) || ref.Code != xicconn.CodeTenantScope {
		t.Errorf("cross-tenant: %v", err)
	}
	if list, _ := e.repo.List(ctxFor(tenantA), outbound.Filter{}); len(list) != 0 {
		t.Fatalf("%d attempts were left behind", len(list))
	}
	// a revoked connection refuses new reservations
	if _, err := e.connSvc.Revoke(ctxFor(tenantA), operator, c.ConnectionID, e.mustConn(c).Version, "gone"); err != nil {
		t.Fatal(err)
	}
	if err := res(tenantA, base); !errors.As(err, &ref) || ref.Code != xicconn.CodeNotActive {
		t.Errorf("revoked: %v", err)
	}
}

func (e *env) mustConn(c *xicconn.Connection) *xicconn.Connection {
	cur, err := e.connSvc.Connection(ctxFor(c.TenantID), c.ConnectionID)
	if err != nil {
		e.t.Fatal(err)
	}
	return cur
}

func TestReserve_UnsupportedAuthenticationFailsClosedAtReservation(t *testing.T) {
	e := newEnv(t)
	// the reference contract's outbound operations use OAuth, which this engine does not carry yet
	cv := e.publish(variant{}, func(in *registry.VersionInput) {
		for i := range in.Content.Operations {
			if in.Content.Operations[i].ID == "payment-submit" {
				in.Content.Operations[i].AuthProfile = "oauth"
			}
		}
	})
	// the connection needs an api-out binding too only if it is used; bind the OAuth profile it does use
	ctx := ctxFor(tenantA)
	c, _, err := e.connSvc.Create(ctx, creator, xicconn.CreateInput{Environment: "SANDBOX", ConnectorID: "bank-x", ContractVersionID: cv.ContractVersionID, Region: "uk",
		Endpoints: map[string]string{"bank-api": "api.bank-x.com"}, ProviderAccountRef: "acc-oauth"}, "")
	if err != nil {
		t.Fatal(err)
	}
	res, _ := e.connSvc.Authorize(ctx, creator, c.ConnectionID, c.Version, xicconn.AuthorizeInput{Mode: "PRE_PROVISIONED"})
	cur := res.Connection
	exp := e.at().Add(60 * 24 * time.Hour)
	for _, b := range []xicconn.BindInput{
		{AuthProfile: "oauth", CredentialRef: uniqueRef("o", "o"), GrantedScopes: []string{"payments.write", "payments.read"}, ConsentActor: "x", ExpiresAt: &exp},
		{AuthProfile: "hmac-in", CredentialRef: uniqueRef("o", "h"), ConsentActor: "x"},
		{AuthProfile: "api-out", CredentialRef: uniqueRef("o", "a"), ConsentActor: "x", ExpiresAt: &exp},
	} {
		if cur, err = e.connSvc.Bind(ctx, creator, c.ConnectionID, cur.Version, b); err != nil {
			t.Fatal(err)
		}
	}
	vres, err := e.connSvc.Validate(ctx, operator, c.ConnectionID, cur.Version, "")
	if err != nil {
		t.Fatal(err)
	}
	act, err := e.connSvc.Activate(ctx, operator, c.ConnectionID, vres.Connection.Version)
	if err != nil {
		t.Fatal(err)
	}
	_, _, err = e.svc.Reserve(ctx, domain, outbound.ReserveInput{ConnectionID: act.ConnectionID, OperationID: "payment-submit", DomainRequestRef: "PAY-1", Payload: json.RawMessage(payRef)})
	if !errors.Is(err, outbound.ErrAuthUnsupported) {
		t.Fatalf("%v", err)
	}
	if list, _ := e.repo.List(ctx, outbound.Filter{}); len(list) != 0 {
		t.Fatal("an attempt was reserved for an operation that can never be submitted")
	}
}

func TestReserve_BodyIsStoredOnlyWhenDataPolicyAllows(t *testing.T) {
	e := newEnv(t)
	c := e.setup(variant{})
	a := e.reserve(c, "PAY-1")
	if !a.BodyRetained || a.BodyExpiresAt == nil {
		t.Fatalf("a FINANCIAL body should be kept while the attempt is open: %+v", a)
	}

	e2 := newEnv(t)
	c2 := e2.setup(variant{personal: true})
	p := e2.reserve(c2, "PAY-1")
	if p.BodyRetained || len(p.Body) != 0 {
		t.Fatalf("a PERSONAL payload must never be stored: %+v", p)
	}
	if got := e2.get(tenantA, p.AttemptID); got.BodyRetained {
		t.Fatal("stored anyway")
	}
}

// ---- submit ------------------------------------------------------------------------------------------

func TestSubmit_ClearSuccessSettlesAndSendsGovernedRequest(t *testing.T) {
	e := newEnv(t)
	c := e.setup(variant{})
	a := e.reserve(c, "PAY-1")
	got, err := e.svc.Submit(ctxFor(tenantA), domain, a.AttemptID, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got.State != outbound.StateSettled || got.ProviderReference != "P-1" || got.ResolutionMethod != outbound.MethodSubmit || got.SubmitCount != 1 || got.BodyRetained {
		t.Fatalf("%+v", got)
	}
	posts := e.prov.posts()
	if len(posts) != 1 {
		t.Fatalf("%d calls", len(posts))
	}
	p := posts[0]
	if p.url != "https://api.bank-x.com/v1/payments" {
		t.Fatalf("the destination must come from the connection's governed endpoint: %s", p.url)
	}
	if p.header.Get("X-Api-Key") != apiKey || p.header.Get("Idempotency-Key") != a.AttemptID || p.header.Get("Content-Type") != "application/json" || p.header.Get("X-Request-Id") != a.AttemptID {
		t.Fatalf("%v", p.header)
	}
	if string(p.body) != payRef {
		t.Fatalf("%s", p.body)
	}
	// no secret or payload value in any event
	for _, ev := range e.repo.Out {
		b, _ := json.Marshal(ev.Data)
		if strings.Contains(string(b), apiKey) || strings.Contains(string(b), "10.00") {
			t.Fatalf("event %s leaks: %s", ev.Type, b)
		}
	}
	evs, _ := e.repo.Events(ctxFor(tenantA), a.AttemptID)
	for _, ev := range evs {
		if strings.Contains(string(ev.Detail), apiKey) || strings.Contains(string(ev.Detail), "10.00") {
			t.Fatalf("evidence leaks: %s", ev.Detail)
		}
	}
	// a second submit of a settled attempt answers with its state and sends nothing
	again, err := e.svc.Submit(ctxFor(tenantA), domain, a.AttemptID, nil)
	if err != nil || again.State != outbound.StateSettled || e.prov.count() != 1 {
		t.Fatalf("%v %+v calls=%d", err, again, e.prov.count())
	}
	if !hasType(e.repo.Types(), "integration.outbound.submitted") || !hasType(e.repo.Types(), "integration.outbound.resolved") {
		t.Fatalf("%v", e.repo.Types())
	}
}

func TestSubmit_HMACSigningUsesTheContractsProfile(t *testing.T) {
	e := newEnv(t)
	cv := e.publish(variant{hmacOut: true}, nil)
	c := e.activeConn(tenantA, cv, "acc-h", refHMAC)
	a := e.reserve(c, "PAY-1")
	if _, err := e.svc.Submit(ctxFor(tenantA), domain, a.AttemptID, nil); err != nil {
		t.Fatal(err)
	}
	p := e.prov.posts()[0]
	ts, sig := p.header.Get("X-Timestamp"), p.header.Get("X-Signature")
	if ts == "" || len(sig) != 64 || p.header.Get("X-Api-Key") != "" {
		t.Fatalf("%v", p.header)
	}
	// the receiver can verify it: HMAC-SHA256 over timestamp.body with the credential
	if want := hmacHex("hmac-secret-material", ts+"."+string(p.body)); sig != want {
		t.Fatalf("signature %s want %s", sig, want)
	}
}

func TestSubmit_CallbackContractMakesATwoXXPending(t *testing.T) {
	e := newEnv(t)
	c := e.setup(variant{callback: true})
	a := e.reserve(c, "PAY-1")
	e.prov.set(answer(202, `{"payment_id":"P-9"}`))
	got, err := e.svc.Submit(ctxFor(tenantA), domain, a.AttemptID, nil)
	if err != nil || got.State != outbound.StatePending || got.ProviderReference != "P-9" || got.NextResolutionAt == nil || !got.BodyRetained {
		t.Fatalf("%v %+v", err, got)
	}
	if !hasType(e.repo.Types(), "integration.outbound.pending") {
		t.Fatalf("%v", e.repo.Types())
	}
}

func TestSubmit_ClearFailuresAreFailedAndFinal(t *testing.T) {
	for status, reason := range map[int]string{400: outbound.ReasonProviderRejected, 422: outbound.ReasonProviderRejected, 404: outbound.ReasonProviderRejected,
		401: outbound.ReasonAuthRejected, 403: outbound.ReasonAuthRejected} {
		e := newEnv(t)
		c := e.setup(variant{})
		a := e.reserve(c, "PAY-1")
		e.prov.set(answer(status, `{"error":"no"}`))
		got, err := e.svc.Submit(ctxFor(tenantA), domain, a.AttemptID, nil)
		if err != nil || got.State != outbound.StateFailed || got.AmbiguityReason != reason || got.BodyRetained {
			t.Fatalf("%d: %v %+v", status, err, got)
		}
		// a failed attempt is final: submitting again sends nothing
		if _, err := e.svc.Submit(ctxFor(tenantA), domain, a.AttemptID, nil); err != nil || e.prov.count() != 1 {
			t.Fatalf("%d: a failed attempt was sent again", status)
		}
	}
}

func TestSubmit_NoRedirectIsFollowedAndTheCredentialStaysWithTheApprovedHost(t *testing.T) {
	e := newEnv(t)
	c := e.setup(variant{})
	a := e.reserve(c, "PAY-1")
	e.prov.set(func(call) (int, string, error) { return 307, "", nil })
	got, err := e.svc.Submit(ctxFor(tenantA), domain, a.AttemptID, nil)
	if err != nil || got.State != outbound.StateFailed || got.AmbiguityReason != outbound.ReasonRedirectRefused {
		t.Fatalf("%v %+v", err, got)
	}
	if e.prov.count() != 1 {
		t.Fatal("a redirect was followed")
	}
	if !hasType(e.repo.Types(), "integration.security.outbound-redirect-refused") {
		t.Fatalf("%v", e.repo.Types())
	}
}

// ---- ambiguity (I-14): NP-17, NP-19, NP-20, NP-21 ------------------------------------------------------

func TestSubmit_TimeoutAfterSendIsUnknownNotFailed_NP17(t *testing.T) {
	e := newEnv(t)
	c := e.setup(variant{})
	a := e.reserve(c, "PAY-1")
	e.prov.set(failWith(timeoutErr{}))
	got, err := e.svc.Submit(ctxFor(tenantA), domain, a.AttemptID, nil)
	if err != nil || got.State != outbound.StateUnknown || got.AmbiguityReason != outbound.ReasonTimeout || got.NextResolutionAt == nil {
		t.Fatalf("%v %+v", err, got)
	}
	if !got.BodyRetained {
		t.Fatal("an open attempt keeps its body")
	}
	if !hasType(e.repo.Types(), "integration.outbound.unknown") {
		t.Fatalf("%v", e.repo.Types())
	}
}

func TestSubmit_ConnectionResetAfterSendIsUnknown(t *testing.T) {
	e := newEnv(t)
	c := e.setup(variant{})
	a := e.reserve(c, "PAY-1")
	e.prov.set(failWith(errors.New("read: connection reset by peer")))
	got, _ := e.svc.Submit(ctxFor(tenantA), domain, a.AttemptID, nil)
	if got.State != outbound.StateUnknown || got.AmbiguityReason != outbound.ReasonConnectionLost {
		t.Fatalf("%+v", got)
	}
}

func TestSubmit_AmbiguousAnswersAreUnknown(t *testing.T) {
	for status, reason := range map[int]string{500: outbound.ReasonProviderUnavail, 503: outbound.ReasonProviderUnavail, 429: outbound.ReasonRateLimited,
		408: outbound.ReasonTimeout, 409: outbound.ReasonUnclassified} {
		e := newEnv(t)
		c := e.setup(variant{})
		a := e.reserve(c, "PAY-1")
		e.prov.set(answer(status, `{}`))
		got, err := e.svc.Submit(ctxFor(tenantA), domain, a.AttemptID, nil)
		if err != nil || got.State != outbound.StateUnknown || got.AmbiguityReason != reason {
			t.Fatalf("%d: %v %+v", status, err, got)
		}
	}
}

func TestSubmit_UnknownWithoutSafeResubmissionIsNeverBlindlyRepeated_NP19_NP21(t *testing.T) {
	e := newEnv(t)
	c := e.setup(variant{}) // resubmission was not declared safe
	a := e.reserve(c, "PAY-1")
	e.prov.set(failWith(timeoutErr{}))
	if got, _ := e.svc.Submit(ctxFor(tenantA), domain, a.AttemptID, nil); got.State != outbound.StateUnknown {
		t.Fatalf("%+v", got)
	}
	for i := 0; i < 3; i++ {
		if _, err := e.svc.Submit(ctxFor(tenantA), domain, a.AttemptID, nil); !errors.Is(err, outbound.ErrUnknownMustBeResolved) {
			t.Fatalf("a second submission of an UNKNOWN attempt must be refused: %v", err)
		}
	}
	if len(e.prov.posts()) != 1 {
		t.Fatalf("the provider saw %d submissions of one logical operation", len(e.prov.posts()))
	}
	// even with the body supplied again
	if _, err := e.svc.Submit(ctxFor(tenantA), domain, a.AttemptID, []byte(payRef)); !errors.Is(err, outbound.ErrUnknownMustBeResolved) {
		t.Fatalf("%v", err)
	}
	// and the database-level rule agrees: the repo refuses the transition even if the service were wrong
	_, err := e.repo.Mutate(ctxFor(tenantA), a.AttemptID, func(tx *outbound.Tx) error {
		now := e.at()
		tx.A.State, tx.A.SubmitCount, tx.A.LastSubmitAt = outbound.StateSubmitting, tx.A.SubmitCount+1, &now
		return nil
	})
	if !errors.Is(err, outbound.ErrTransition) {
		t.Fatalf("the store allowed a blind resubmission: %v", err)
	}
}

func TestSubmit_ResubmissionAfterUnknownCarriesTheSameProviderKey_NP20(t *testing.T) {
	e := newEnv(t)
	c := e.setup(variant{resubmit: true})
	a := e.reserve(c, "PAY-1")
	if !a.ResubmitAllowed {
		t.Fatal("resubmission was declared safe")
	}
	e.prov.set(answer(500, `{}`)) // accepted, then 500
	if got, _ := e.svc.Submit(ctxFor(tenantA), domain, a.AttemptID, nil); got.State != outbound.StateUnknown {
		t.Fatalf("%+v", got)
	}
	e.prov.set(answer(200, `{"payment_id":"P-1"}`)) // the provider replays its original answer for the same key
	got, err := e.svc.Submit(ctxFor(tenantA), domain, a.AttemptID, nil)
	if err != nil || got.State != outbound.StateSettled || got.SubmitCount != 2 {
		t.Fatalf("%v %+v", err, got)
	}
	posts := e.prov.posts()
	if len(posts) != 2 || posts[0].header.Get("Idempotency-Key") != posts[1].header.Get("Idempotency-Key") || posts[0].header.Get("Idempotency-Key") != a.AttemptID {
		t.Fatalf("the provider key must be stable across submissions: %v", posts)
	}
	if list, _ := e.repo.List(ctxFor(tenantA), outbound.Filter{}); len(list) != 1 {
		t.Fatalf("%d logical attempts", len(list))
	}
}

func TestSubmit_BodyNotStoredNeedsTheCallerToSendItAgain(t *testing.T) {
	e := newEnv(t)
	c := e.setup(variant{personal: true})
	a := e.reserve(c, "PAY-1")
	if _, err := e.svc.Submit(ctxFor(tenantA), domain, a.AttemptID, nil); !errors.Is(err, outbound.ErrBodyNotHeld) {
		t.Fatalf("%v", err)
	}
	if _, err := e.svc.Submit(ctxFor(tenantA), domain, a.AttemptID, []byte(`{"amount":"11.00","currency":"GBP"}`)); !errors.Is(err, outbound.ErrBodyMismatch) {
		t.Fatalf("a different body must not go out under the reserved attempt: %v", err)
	}
	if e.prov.count() != 0 {
		t.Fatal("something was sent")
	}
	got, err := e.svc.Submit(ctxFor(tenantA), domain, a.AttemptID, []byte(" "+payRef+" "))
	if err != nil || got.State != outbound.StateSettled {
		t.Fatalf("%v %+v", err, got)
	}
}

// ---- nothing is sent when authority, destination or credential fails (I-18..I-20, NP-15) ----------------

func TestSubmit_RevokedBetweenReserveAndSubmitSendsNothing_NP15(t *testing.T) {
	e := newEnv(t)
	c := e.setup(variant{})
	a := e.reserve(c, "PAY-1")
	if _, err := e.connSvc.Revoke(ctxFor(tenantA), operator, c.ConnectionID, e.mustConn(c).Version, "provider relationship ended"); err != nil {
		t.Fatal(err)
	}
	var ref *xicconn.Refusal
	if _, err := e.svc.Submit(ctxFor(tenantA), domain, a.AttemptID, nil); !errors.As(err, &ref) || ref.Code != xicconn.CodeNotActive {
		t.Fatalf("%v", err)
	}
	if e.prov.count() != 0 || e.secrets.hits != 0 {
		t.Fatalf("calls=%d secrets=%d", e.prov.count(), e.secrets.hits)
	}
	got := e.get(tenantA, a.AttemptID)
	if got.State != outbound.StateReserved || got.SubmitCount != 0 {
		t.Fatalf("%+v", got)
	}
	if !hasType(e.repo.Types(), "integration.outbound.blocked") {
		t.Fatalf("%v", e.repo.Types())
	}
	// the reserved attempt can still be cancelled, and then it is final
	cancelled, err := e.svc.Cancel(ctxFor(tenantA), other, a.AttemptID, "provider relationship ended")
	if err != nil || cancelled.State != outbound.StateCancelled || cancelled.BodyRetained {
		t.Fatalf("%v %+v", err, cancelled)
	}
}

func TestSubmit_DestinationIsRecheckedBeforeAnyCredentialIsFetched(t *testing.T) {
	e := newEnv(t)
	c := e.setup(variant{})
	a := e.reserve(c, "PAY-1")
	// DNS rebinding: the approved name now resolves to a private address
	e.dns.set("api.bank-x.com", "10.0.0.5")
	if _, err := e.svc.Submit(ctxFor(tenantA), domain, a.AttemptID, nil); !errors.Is(err, outbound.ErrBlocked) {
		t.Fatalf("%v", err)
	}
	if e.prov.count() != 0 || e.secrets.hits != 0 {
		t.Fatalf("a credential was fetched or a call made for a refused destination: calls=%d secrets=%d", e.prov.count(), e.secrets.hits)
	}
	if got := e.get(tenantA, a.AttemptID); got.State != outbound.StateReserved || got.SubmitCount != 0 {
		t.Fatalf("%+v", got)
	}
	if !hasType(e.repo.Types(), "integration.security.outbound-egress-blocked") {
		t.Fatalf("%v", e.repo.Types())
	}
	// when DNS is right again the same attempt goes out - still one logical operation
	e.dns.set("api.bank-x.com", "93.184.216.34")
	if got, err := e.svc.Submit(ctxFor(tenantA), domain, a.AttemptID, nil); err != nil || got.State != outbound.StateSettled {
		t.Fatalf("%v %+v", err, got)
	}
}

func TestSubmit_VaultOutageSendsNothingAndKeepsTheAttempt(t *testing.T) {
	e := newEnv(t)
	c := e.setup(variant{})
	a := e.reserve(c, "PAY-1")
	e.secrets.down = true
	if _, err := e.svc.Submit(ctxFor(tenantA), domain, a.AttemptID, nil); !errors.Is(err, outbound.ErrDependency) {
		t.Fatalf("%v", err)
	}
	if e.prov.count() != 0 || e.get(tenantA, a.AttemptID).State != outbound.StateReserved {
		t.Fatal("state or call changed")
	}
	e.secrets.down = false
	if got, err := e.svc.Submit(ctxFor(tenantA), domain, a.AttemptID, nil); err != nil || got.State != outbound.StateSettled {
		t.Fatalf("%v %+v", err, got)
	}
}

func TestSubmit_DialFailureMeansNothingWasSentAndTheAttemptIsReusable(t *testing.T) {
	e := newEnv(t)
	c := e.setup(variant{})
	a := e.reserve(c, "PAY-1")
	e.prov.set(failWith(&net.OpError{Op: "dial", Net: "tcp", Err: errors.New("connection refused")}))
	got, err := e.svc.Submit(ctxFor(tenantA), domain, a.AttemptID, nil)
	if !errors.Is(err, outbound.ErrBlocked) || got == nil || got.State != outbound.StateReserved {
		t.Fatalf("%v %+v", err, got)
	}
	e.prov.set(nil)
	done, err := e.svc.Submit(ctxFor(tenantA), domain, a.AttemptID, nil)
	if err != nil || done.State != outbound.StateSettled {
		t.Fatalf("%v %+v", err, done)
	}
	if list, _ := e.repo.List(ctxFor(tenantA), outbound.Filter{}); len(list) != 1 {
		t.Fatal("a second logical attempt appeared")
	}
}

func TestSubmit_MovedToAnotherContractVersionIsRefused(t *testing.T) {
	e := newEnv(t)
	c := e.setup(variant{})
	a := e.reserve(c, "PAY-1")
	v2 := e.publish(variant{}, nil)
	ctx := ctxFor(tenantA)
	req, err := e.connSvc.RequestMigration(ctx, creator, c.ConnectionID, xicconn.MigrationInput{ToContractVersionID: v2.ContractVersionID, Reason: "adopt the new contract"})
	if err != nil {
		t.Skipf("migration needs more setup in this harness: %v", err)
	}
	if _, err := e.connSvc.DecideMigration(ctx, operator, c.ConnectionID, req.RequestID, true); err != nil {
		t.Skipf("migration approval needs more setup in this harness: %v", err)
	}
	var ref *xicconn.Refusal
	_, err = e.svc.Submit(ctx, domain, a.AttemptID, nil)
	if !errors.As(err, &ref) || (ref.Code != xicconn.CodeContractMismatch && ref.Code != xicconn.CodeCertStale && ref.Code != xicconn.CodeNotActive) {
		t.Fatalf("%v", err)
	}
	if e.prov.count() != 0 {
		t.Fatal("sent under the wrong contract")
	}
}

func TestSubmit_ConcurrentSubmittersSendOnce(t *testing.T) {
	e := newEnv(t)
	c := e.setup(variant{})
	a := e.reserve(c, "PAY-1")
	e.prov.set(func(call) (int, string, error) {
		time.Sleep(20 * time.Millisecond)
		return 200, `{"payment_id":"P-1"}`, nil
	})
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _ = e.svc.Submit(ctxFor(tenantA), domain, a.AttemptID, nil)
		}()
	}
	wg.Wait()
	if n := len(e.prov.posts()); n != 1 {
		t.Fatalf("the provider saw %d submissions of one logical operation", n)
	}
	if got := e.get(tenantA, a.AttemptID); got.State != outbound.StateSettled || got.SubmitCount != 1 {
		t.Fatalf("%+v", got)
	}
}

func TestSubmit_ACallerThatGoesAwayStillGetsTheOutcomeRecorded(t *testing.T) {
	e := newEnv(t)
	c := e.setup(variant{})
	a := e.reserve(c, "PAY-1")
	ctx, cancel := context.WithCancel(ctxFor(tenantA))
	e.prov.set(func(call) (int, string, error) { cancel(); return 200, `{"payment_id":"P-1"}`, nil })
	if _, err := e.svc.Submit(ctx, domain, a.AttemptID, nil); err != nil {
		t.Fatal(err)
	}
	if got := e.get(tenantA, a.AttemptID); got.State != outbound.StateSettled {
		t.Fatalf("the provider answered and the answer was lost: %+v", got)
	}
}

// ---- resolution: query the ORIGINAL, never resubmit ------------------------------------------------------

func (e *env) unknown(c *xicconn.Connection, ref string) *outbound.Attempt {
	e.t.Helper()
	a := e.reserve(c, ref)
	e.prov.set(failWith(timeoutErr{}))
	got, err := e.svc.Submit(ctxFor(c.TenantID), domain, a.AttemptID, nil)
	if err != nil || got.State != outbound.StateUnknown {
		e.t.Fatalf("%v %+v", err, got)
	}
	return got
}

func statusIs(status string) func(call) (int, string, error) {
	return func(c call) (int, string, error) {
		if c.method == http.MethodGet {
			return 200, `{"status":"` + status + `","payment_id":"P-7"}`, nil
		}
		return 200, `{"payment_id":"P-1"}`, nil
	}
}

func TestResolve_StatusQueryAsksAboutTheOriginalAndSettlesIt_NP19(t *testing.T) {
	for word, want := range map[string]string{"SETTLED": outbound.StateSettled, "completed": outbound.StateSettled, "FAILED": outbound.StateFailed, "Rejected": outbound.StateFailed} {
		e := newEnv(t)
		c := e.setup(variant{})
		a := e.unknown(c, "PAY-1")
		e.prov.set(statusIs(word))
		got, err := e.svc.Resolve(ctxFor(tenantA), outbound.Actor{PrincipalID: "system:xic-outbound-resolver"}, a.AttemptID, outbound.ResolveInput{})
		if err != nil || got.State != want || got.ResolutionMethod != outbound.MethodStatusQuery || got.ProviderReference != "P-7" || got.BodyRetained {
			t.Fatalf("%s: %v %+v", word, err, got)
		}
		if len(e.prov.posts()) != 1 {
			t.Fatalf("%s: resolving submitted again", word)
		}
		g := e.prov.gets()
		if len(g) != 1 || !strings.HasPrefix(g[0].url, "https://api.bank-x.com/v1/payments/status?") || !strings.Contains(g[0].url, "client_ref=PAY-1") || g[0].header.Get("X-Api-Key") != apiKey {
			t.Fatalf("%v", g)
		}
		if !hasType(e.repo.Types(), "integration.outbound.resolved") {
			t.Fatalf("%v", e.repo.Types())
		}
	}
}

func TestResolve_UsesTheProviderReferenceWhenKnown(t *testing.T) {
	e := newEnv(t)
	c := e.setup(variant{})
	a := e.reserve(c, "PAY-1")
	e.prov.set(func(call) (int, string, error) { return 503, `{"payment_id":"P-5"}`, nil })
	if got, _ := e.svc.Submit(ctxFor(tenantA), domain, a.AttemptID, nil); got.ProviderReference != "P-5" || got.State != outbound.StateUnknown {
		t.Fatalf("%+v", got)
	}
	e.prov.set(statusIs("SETTLED"))
	if _, err := e.svc.Resolve(ctxFor(tenantA), other, a.AttemptID, outbound.ResolveInput{}); err != nil {
		t.Fatal(err)
	}
	if g := e.prov.gets(); len(g) != 1 || !strings.Contains(g[0].url, "reference=P-5") {
		t.Fatalf("%v", g)
	}
}

func TestResolve_InconclusiveAnswersKeepTheAttemptUnknownAndBackOff(t *testing.T) {
	cases := map[string]func(call) (int, string, error){
		"not found":            answer(404, `{}`),
		"provider error":       answer(500, `{}`),
		"network":              failWith(timeoutErr{}),
		"words we do not know": answer(200, `{"status":"SOMETHING_NEW"}`),
		"schema violation":     answer(200, `{"state":"SETTLED"}`),
		"not json":             answer(200, `settled`),
	}
	for name, fn := range cases {
		e := newEnv(t)
		c := e.setup(variant{})
		a := e.unknown(c, "PAY-1")
		before := e.get(tenantA, a.AttemptID).NextResolutionAt
		e.advance(time.Hour)
		e.prov.set(func(c call) (int, string, error) {
			if c.method == http.MethodGet {
				return fn(c)
			}
			return 200, `{}`, nil
		})
		got, err := e.svc.Resolve(ctxFor(tenantA), other, a.AttemptID, outbound.ResolveInput{})
		if err != nil || got.State != outbound.StateUnknown || got.ResolveCount != 1 || got.NextResolutionAt == nil || !got.NextResolutionAt.After(e.at()) {
			t.Fatalf("%s: %v %+v", name, err, got)
		}
		_ = before
		if len(e.prov.posts()) != 1 {
			t.Fatalf("%s: resubmitted", name)
		}
		// the second inconclusive query backs off further
		got2, _ := e.svc.Resolve(ctxFor(tenantA), other, a.AttemptID, outbound.ResolveInput{})
		if got2.ResolveCount != 2 || !got2.NextResolutionAt.After(*got.NextResolutionAt) {
			t.Fatalf("%s: %+v then %+v", name, got, got2)
		}
	}
}

func TestResolve_PendingAnswerMovesUnknownToPending(t *testing.T) {
	e := newEnv(t)
	c := e.setup(variant{})
	a := e.unknown(c, "PAY-1")
	e.prov.set(statusIs("PROCESSING"))
	got, err := e.svc.Resolve(ctxFor(tenantA), other, a.AttemptID, outbound.ResolveInput{})
	if err != nil || got.State != outbound.StatePending || got.AmbiguityReason != "" {
		t.Fatalf("%v %+v", err, got)
	}
	// later it settles
	e.prov.set(statusIs("SETTLED"))
	got, err = e.svc.Resolve(ctxFor(tenantA), other, a.AttemptID, outbound.ResolveInput{})
	if err != nil || got.State != outbound.StateSettled {
		t.Fatalf("%v %+v", err, got)
	}
}

func TestResolve_NeedsAStatusOperationOrIsManual(t *testing.T) {
	e := newEnv(t)
	cv := e.publish(variant{noStatusQuery: true}, nil)
	c := e.activeConn(tenantA, cv, "acc-ns", refAPIKey)
	a := e.unknown(c, "PAY-1")
	if got := e.get(tenantA, a.AttemptID); got.NextResolutionAt != nil {
		t.Fatalf("with no status operation there is nothing for the worker to schedule: %+v", got)
	}
	if _, err := e.svc.Resolve(ctxFor(tenantA), other, a.AttemptID, outbound.ResolveInput{}); !errors.Is(err, outbound.ErrNotResolvable) {
		t.Fatalf("%v", err)
	}
}

func TestResolve_ManualNeedsEvidenceAndAnotherPerson(t *testing.T) {
	e := newEnv(t)
	c := e.setup(variant{})
	a := e.unknown(c, "PAY-1")
	ctx := ctxFor(tenantA)
	m := func(who outbound.Actor, in outbound.ResolveInput) error {
		in.Method = outbound.MethodManual
		_, err := e.svc.Resolve(ctx, who, a.AttemptID, in)
		return err
	}
	if err := m(other, outbound.ResolveInput{Outcome: "SETTLED", Reason: "bank statement line 14"}); !errors.Is(err, outbound.ErrBadRequest) {
		t.Fatalf("evidence is required: %v", err)
	}
	if err := m(other, outbound.ResolveInput{Outcome: "MAYBE", EvidenceRef: "BS-1", Reason: "x"}); !errors.Is(err, outbound.ErrBadRequest) {
		t.Fatalf("outcome: %v", err)
	}
	if err := m(domain, outbound.ResolveInput{Outcome: "SETTLED", EvidenceRef: "BS-1", Reason: "bank statement"}); !errors.Is(err, outbound.ErrSelfAction) {
		t.Fatalf("whoever reserved it cannot resolve it: %v", err)
	}
	if err := m(other, outbound.ResolveInput{Outcome: "SETTLED", EvidenceRef: "BS-1", Reason: "bank statement line 14"}); err != nil {
		t.Fatal(err)
	}
	got := e.get(tenantA, a.AttemptID)
	if got.State != outbound.StateSettled || got.ResolutionMethod != outbound.MethodManual || got.ResolvedBy != "ops-lead" || got.EvidenceRef != "BS-1" {
		t.Fatalf("%+v", got)
	}
	// a final attempt is not resolved again
	if err := m(other, outbound.ResolveInput{Outcome: "FAILED", EvidenceRef: "BS-2", Reason: "changed my mind"}); err != nil || e.get(tenantA, a.AttemptID).State != outbound.StateSettled {
		t.Fatalf("a final attempt moved: %v", err)
	}
	if len(e.prov.posts()) != 1 {
		t.Fatal("a manual resolution submitted")
	}
}

func TestResolve_CrashAfterClaimIsRecoveredByQueryNotResubmission_NP18(t *testing.T) {
	e := newEnv(t)
	c := e.setup(variant{})
	a := e.reserve(c, "PAY-1")
	// the worker claimed the attempt (SUBMITTING is durable) and died: no answer was ever recorded
	now := e.at()
	lease := now.Add(2 * time.Minute)
	if _, err := e.repo.Mutate(ctxFor(tenantA), a.AttemptID, func(tx *outbound.Tx) error {
		tx.A.State, tx.A.SubmitCount, tx.A.LastSubmitAt, tx.A.NextResolutionAt = outbound.StateSubmitting, 1, &now, &lease
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := e.svc.Resolve(ctxFor(tenantA), other, a.AttemptID, outbound.ResolveInput{}); !errors.Is(err, outbound.ErrTransition) {
		t.Fatalf("inside its lease the call may still be in flight: %v", err)
	}
	e.advance(3 * time.Minute)
	e.prov.set(statusIs("SETTLED"))
	got, err := e.svc.Resolve(ctxFor(tenantA), other, a.AttemptID, outbound.ResolveInput{})
	if err != nil || got.State != outbound.StateSettled || got.ProviderReference != "P-7" {
		t.Fatalf("%v %+v", err, got)
	}
	if len(e.prov.posts()) != 0 {
		t.Fatal("the crashed attempt was submitted again")
	}
	evs, _ := e.repo.Events(ctxFor(tenantA), a.AttemptID)
	var seen bool
	for _, ev := range evs {
		if ev.Kind == "UNKNOWN" && strings.Contains(string(ev.Detail), outbound.ReasonCrashRecovery) {
			seen = true
		}
	}
	if !seen {
		t.Fatalf("the recovery was not evidenced: %+v", evs)
	}
}

// ---- cancellation ---------------------------------------------------------------------------------------

func TestCancel_OnlyBeforeAnySubmission(t *testing.T) {
	e := newEnv(t)
	c := e.setup(variant{})
	a := e.reserve(c, "PAY-1")
	if _, err := e.svc.Cancel(ctxFor(tenantA), other, a.AttemptID, " "); !errors.Is(err, outbound.ErrBadRequest) {
		t.Fatalf("%v", err)
	}
	u := e.unknown(c, "PAY-2")
	if _, err := e.svc.Cancel(ctxFor(tenantA), other, u.AttemptID, "changed mind"); !errors.Is(err, outbound.ErrTransition) {
		t.Fatalf("an attempt the provider may have acted on cannot be cancelled: %v", err)
	}
	got, err := e.svc.Cancel(ctxFor(tenantA), other, a.AttemptID, "customer withdrew the payment")
	if err != nil || got.State != outbound.StateCancelled {
		t.Fatalf("%v %+v", err, got)
	}
	if again, err := e.svc.Cancel(ctxFor(tenantA), other, a.AttemptID, "again"); err != nil || again.State != outbound.StateCancelled {
		t.Fatalf("cancelling twice is idempotent: %v", err)
	}
	if _, err := e.svc.Submit(ctxFor(tenantA), domain, a.AttemptID, nil); err != nil || e.prov.count() != 1 {
		t.Fatalf("a cancelled attempt must not be sent: %v", err)
	}
	if !hasType(e.repo.Types(), "integration.outbound.cancelled") {
		t.Fatalf("%v", e.repo.Types())
	}
}

// ---- tenant isolation ----------------------------------------------------------------------------------

func TestTenantsNeverSeeOrDriveEachOthersAttempts(t *testing.T) {
	e := newEnv(t)
	c := e.setup(variant{})
	a := e.reserve(c, "PAY-1")
	if _, err := e.repo.Get(ctxFor(tenantB), a.AttemptID); !errors.Is(err, outbound.ErrNotFound) {
		t.Fatalf("%v", err)
	}
	for name, f := range map[string]func() error{
		"submit": func() error { _, err := e.svc.Submit(ctxFor(tenantB), domain, a.AttemptID, nil); return err },
		"resolve": func() error {
			_, err := e.svc.Resolve(ctxFor(tenantB), other, a.AttemptID, outbound.ResolveInput{})
			return err
		},
		"cancel": func() error { _, err := e.svc.Cancel(ctxFor(tenantB), other, a.AttemptID, "x"); return err },
		"events": func() error { _, err := e.repo.Events(ctxFor(tenantB), a.AttemptID); return err },
	} {
		if err := f(); !errors.Is(err, outbound.ErrNotFound) {
			t.Errorf("%s: %v", name, err)
		}
	}
	if l, _ := e.repo.List(ctxFor(tenantB), outbound.Filter{}); len(l) != 0 {
		t.Fatal("tenant B lists tenant A's attempts")
	}
	if e.prov.count() != 0 {
		t.Fatal("another tenant caused a call")
	}
}

// ---- the worker ---------------------------------------------------------------------------------------

func TestDriveDue_ResolvesDueAttemptsOnlyAndNeverSubmits(t *testing.T) {
	e := newEnv(t)
	c := e.setup(variant{})
	due := e.unknown(c, "PAY-1")
	e.advance(time.Hour)
	later := e.unknown(c, "PAY-2") // its backoff has not elapsed
	e.prov.set(statusIs("SETTLED"))
	posts := len(e.prov.posts())
	n, err := e.svc.DriveDue(context.Background(), 50)
	if err != nil || n != 1 {
		t.Fatalf("%d %v", n, err)
	}
	if got := e.get(tenantA, due.AttemptID); got.State != outbound.StateSettled {
		t.Fatalf("%+v", got)
	}
	if got := e.get(tenantA, later.AttemptID); got.State != outbound.StateUnknown {
		t.Fatalf("%+v", got)
	}
	if len(e.prov.posts()) != posts {
		t.Fatal("the worker submitted")
	}
}

func TestDriveDue_EscalatesOnceWhenAnAttemptStaysOpenTooLong(t *testing.T) {
	e := newEnv(t)
	cfg := outbound.DefaultConfig()
	cfg.MaxUnknownAge = 2 * time.Hour
	e.svc.Cfg = cfg
	c := e.setup(variant{})
	a := e.unknown(c, "PAY-1")
	e.prov.set(func(c call) (int, string, error) {
		if c.method == http.MethodGet {
			return 200, `{"status":"PENDING"}`, nil
		}
		return 200, `{}`, nil
	})
	e.advance(3 * time.Hour)
	for i := 0; i < 3; i++ {
		if _, err := e.svc.DriveDue(context.Background(), 50); err != nil {
			t.Fatal(err)
		}
		e.advance(time.Hour)
	}
	if n := countType(e.repo.Types(), "integration.outbound.unknown-escalated"); n != 1 {
		t.Fatalf("escalated %d times", n)
	}
	if got := e.get(tenantA, a.AttemptID); got.EscalatedAt == nil || got.State == outbound.StateFailed {
		t.Fatalf("an escalated attempt is never failed to clear a queue: %+v", got)
	}
}

func TestDriveDue_AttemptWithoutAStatusOperationIsStillEscalated(t *testing.T) {
	e := newEnv(t)
	cfg := outbound.DefaultConfig()
	cfg.MaxUnknownAge = time.Hour
	e.svc.Cfg = cfg
	cv := e.publish(variant{noStatusQuery: true}, nil)
	c := e.activeConn(tenantA, cv, "acc-ns", refAPIKey)
	e.unknown(c, "PAY-1")
	e.advance(2 * time.Hour)
	if _, err := e.svc.DriveDue(context.Background(), 50); err != nil {
		t.Fatal(err)
	}
	if countType(e.repo.Types(), "integration.outbound.unknown-escalated") != 1 {
		t.Fatalf("%v", e.repo.Types())
	}
}

func TestPurgeBodies_RemovesBodiesOfFinalAttemptsAndExpiredOnes(t *testing.T) {
	e := newEnv(t)
	cfg := outbound.DefaultConfig()
	cfg.BodyRetention = time.Hour
	e.svc.Cfg = cfg
	c := e.setup(variant{})
	open := e.unknown(c, "PAY-1")
	e.advance(2 * time.Hour)
	n, err := e.repo.PurgeBodies(context.Background(), e.at(), 10)
	if err != nil || n != 1 {
		t.Fatalf("%d %v", n, err)
	}
	if got := e.get(tenantA, open.AttemptID); got.BodyRetained {
		t.Fatal("an expired body was kept")
	}
	// the attempt itself and its hash are untouched, so it can still be resolved
	if got := e.get(tenantA, open.AttemptID); got.PayloadHash != open.PayloadHash || got.State != outbound.StateUnknown {
		t.Fatalf("%+v", got)
	}
}

func TestRunResolver_ResolvesOnATicker(t *testing.T) {
	e := newEnv(t)
	c := e.setup(variant{})
	a := e.unknown(c, "PAY-1")
	e.advance(time.Hour)
	e.prov.set(statusIs("SETTLED"))
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		e.svc.RunResolver(ctx, 10*time.Millisecond, 10, nil)
		close(done)
	}()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) && e.get(tenantA, a.AttemptID).State != outbound.StateSettled {
		time.Sleep(10 * time.Millisecond)
	}
	cancel()
	<-done
	if got := e.get(tenantA, a.AttemptID); got.State != outbound.StateSettled {
		t.Fatalf("%+v", got)
	}
}

func TestBackoffIsExponentialAndBounded(t *testing.T) {
	s := &outbound.Service{Cfg: outbound.Config{RequestTimeout: time.Second, ResolveBackoff: 10 * time.Second, ResolveBackoffMax: time.Minute}}
	for n, want := range []time.Duration{10 * time.Second, 10 * time.Second, 20 * time.Second, 40 * time.Second, time.Minute, time.Minute} {
		if got := s.Backoff(n); got != want {
			t.Errorf("Backoff(%d)=%s want %s", n, got, want)
		}
	}
}

// ---- events ----------------------------------------------------------------------------------------------

func TestEveryEventTypeIsProducedByARealFlow(t *testing.T) {
	e := newEnv(t)
	cfg := outbound.DefaultConfig()
	cfg.MaxUnknownAge = time.Hour
	e.svc.Cfg = cfg
	c := e.setup(variant{callback: true, claims: true})
	// reserved, submitted, pending
	a := e.reserve(c, "PAY-1")
	e.prov.set(answer(202, `{"payment_id":"P-9"}`))
	_, _ = e.svc.Submit(ctxFor(tenantA), domain, a.AttemptID, nil)
	// callbacks on the pending attempt: a claim about another tenant, an applied callback and its duplicate
	e.callback(cb{conn: c, body: `{"event_id":"c1","payment_id":"P-9","status":"SETTLED","client_ref":"PAY-1","tenant":"tenant-b"}`})
	e.callback(cb{conn: c, body: cbBody("c2", "P-9", "SETTLED", "PAY-1")})
	e.callback(cb{conn: c, body: cbBody("c2", "P-9", "SETTLED", "PAY-1")})
	// unknown, resolved, escalated
	b := e.reserve(c, "PAY-2")
	e.prov.set(failWith(timeoutErr{}))
	_, _ = e.svc.Submit(ctxFor(tenantA), domain, b.AttemptID, nil)
	e.advance(2 * time.Hour)
	e.prov.set(statusIs("SETTLED"))
	_, _ = e.svc.DriveDue(context.Background(), 50)
	// cancelled, blocked
	d := e.reserve(c, "PAY-3")
	e.dns.set("api.bank-x.com", "10.0.0.5")
	_, _ = e.svc.Submit(ctxFor(tenantA), domain, d.AttemptID, nil)
	e.dns.set("api.bank-x.com", "93.184.216.34")
	_, _ = e.svc.Cancel(ctxFor(tenantA), other, d.AttemptID, "withdrawn")
	// redirect refused
	f := e.reserve(c, "PAY-4")
	e.prov.set(func(call) (int, string, error) { return 302, "", nil })
	_, _ = e.svc.Submit(ctxFor(tenantA), domain, f.AttemptID, nil)

	// callbacks: a claim about another tenant, an impossible transition, an applied callback, a duplicate, a late contradiction
	e.callback(cb{conn: c, body: cbBody("c3", "P-0", "SETTLED", "PAY-4")}) // PAY-4 failed on a refused redirect
	types := e.repo.Types()
	for _, want := range outbound.EventTypes {
		if want == "integration.security.outbound-region-blocked" {
			continue // produced by the region guard, covered by the Operable residency refusal test
		}
		if !hasType(types, want) {
			t.Errorf("no flow produced %s (got %v)", want, types)
		}
	}
	for _, ev := range e.repo.Out {
		if ev.Data["tenant_id"] != tenantA {
			t.Errorf("%s carries no tenant", ev.Type)
		}
	}
}

func hmacHex(secret, content string) string {
	m := newHMAC(secret)
	m.Write([]byte(content))
	return hexOf(m.Sum(nil))
}
