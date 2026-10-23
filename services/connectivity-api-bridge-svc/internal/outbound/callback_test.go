package outbound_test

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"zoiko.io/connectivity-api-bridge-svc/internal/outbound"
	"zoiko.io/connectivity-api-bridge-svc/internal/xicconn"
	"zoiko.io/eventing/integration"
)

// ---- helpers ------------------------------------------------------------------------------------------

func cbBody(event, ref, status, clientRef string) string {
	return `{"event_id":"` + event + `","payment_id":"` + ref + `","status":"` + status + `","client_ref":"` + clientRef + `"}`
}

func cbSign(secret, ts, body string) string {
	m := hmac.New(sha256.New, []byte(secret))
	m.Write([]byte(ts + "." + body))
	return hex.EncodeToString(m.Sum(nil))
}

type cb struct {
	conn    *xicconn.Connection
	body    string
	secret  string
	tsOff   time.Duration
	headers map[string]string
	op      string
	method  string
	ip      string
}

func (e *env) callbackReq(d cb) outbound.CallbackRequest {
	if d.secret == "" {
		d.secret = cbSecret
	}
	if d.op == "" {
		d.op = "payment-submit"
	}
	if d.method == "" {
		d.method = "POST"
	}
	if d.ip == "" {
		d.ip = "198.51.100.7:4433"
	}
	ts := strconv.FormatInt(e.at().Add(d.tsOff).Unix(), 10)
	h := http.Header{}
	h.Set("Content-Type", "application/json")
	h.Set("X-Timestamp", ts)
	h.Set("X-Signature", cbSign(d.secret, ts, d.body))
	for k, v := range d.headers {
		if v == "" {
			h.Del(k)
		} else {
			h.Set(k, v)
		}
	}
	return outbound.CallbackRequest{ConnectorKey: d.conn.ConnectorID, ConnectionID: d.conn.ConnectionID, OperationID: d.op, Method: d.method, Header: h,
		Body: []byte(d.body), RemoteAddr: d.ip, CorrelationID: "corr-cb"}
}

func (e *env) callback(d cb) *outbound.CallbackOutcome {
	return e.svc.ReceiveCallback(context.Background(), e.callbackReq(d))
}

// pendingAttempt returns an attempt the provider accepted (202, reference P-9): PENDING, awaiting its callback.
func (e *env) pendingAttempt(c *xicconn.Connection, ref string) *outbound.Attempt {
	e.t.Helper()
	a := e.reserve(c, ref)
	e.prov.set(answer(202, `{"payment_id":"P-9"}`))
	got, err := e.svc.Submit(ctxFor(c.TenantID), domain, a.AttemptID, nil)
	if err != nil || got.State != outbound.StatePending || got.ProviderReference != "P-9" {
		e.t.Fatalf("%v %+v", err, got)
	}
	return got
}

func (e *env) receipts(tenant string) []outbound.Receipt {
	e.t.Helper()
	l, err := e.repo.ListReceipts(ctxFor(tenant), outbound.ReceiptFilter{})
	if err != nil {
		e.t.Fatal(err)
	}
	return l
}

func assertCB(t *testing.T, o *outbound.CallbackOutcome, status int, code integration.XICCode) {
	t.Helper()
	if o.Status != status {
		t.Fatalf("status %d (want %d): %+v xic=%v", o.Status, status, o.Receipt, o.XIC)
	}
	if code != "" && (o.XIC == nil || o.XIC.Code != code) {
		t.Fatalf("code %v (want %s)", o.XIC, code)
	}
}

// ---- applying a callback --------------------------------------------------------------------------------------

func TestCallback_AppliesAContractAllowedTransitionAndRecordsItsReceipt(t *testing.T) {
	e := newEnv(t)
	c := e.setup(variant{callback: true})
	a := e.pendingAttempt(c, "PAY-1")
	posts := e.prov.count()

	o := e.callback(cb{conn: c, body: cbBody("evt-1", "P-9", "SETTLED", "PAY-1")})
	assertCB(t, o, 200, "")
	if o.Receipt == nil || o.Receipt.State != outbound.ReceiptApplied || o.Receipt.AttemptStateBefore != outbound.StatePending || o.Receipt.AttemptStateAfter != outbound.StateSettled ||
		o.Receipt.AttemptID != a.AttemptID || o.Receipt.AuthResult != outbound.AuthOK || o.Receipt.NeedsReconciliation {
		t.Fatalf("%+v", o.Receipt)
	}
	got := e.get(tenantA, a.AttemptID)
	if got.State != outbound.StateSettled || got.ResolutionMethod != outbound.MethodCallback || got.ProviderReference != "P-9" || got.BodyRetained {
		t.Fatalf("%+v", got)
	}
	if e.prov.count() != posts {
		t.Fatal("applying a callback called the provider")
	}
	types := e.repo.Types()
	for _, want := range []string{"integration.callback.applied", "integration.outbound.resolved"} {
		if !hasType(types, want) {
			t.Fatalf("%s missing: %v", want, types)
		}
	}
	// the receipt holds hashes and metadata, never the body
	b, _ := json.Marshal(o.Receipt)
	if strings.Contains(string(b), "evt-1") || strings.Contains(string(b), "client_ref") || o.Receipt.BodyHash == "" {
		t.Fatalf("%s", b)
	}
}

func TestCallback_FailedAndPendingCallbacksUseTheSameDefinitions(t *testing.T) {
	e := newEnv(t)
	c := e.setup(variant{callback: true})
	a := e.unknown(c, "PAY-U") // UNKNOWN: the contract lets a callback drive it to PENDING, SETTLED or FAILED
	assertCB(t, e.callback(cb{conn: c, body: cbBody("e1", "P-5", "processing", "PAY-U")}), 200, "")
	if got := e.get(tenantA, a.AttemptID); got.State != outbound.StatePending || got.ProviderReference != "P-5" {
		t.Fatalf("%+v", got)
	}
	assertCB(t, e.callback(cb{conn: c, body: cbBody("e2", "P-5", "Declined", "PAY-U")}), 200, "")
	got := e.get(tenantA, a.AttemptID)
	if got.State != outbound.StateFailed || got.AmbiguityReason != "CALLBACK_REPORTED_FAILED" || got.ResolutionMethod != outbound.MethodCallback {
		t.Fatalf("%+v", got)
	}
}

// ---- authentication (I-16, NP-22) --------------------------------------------------------------------------

func TestCallback_AuthenticationFailureIsOneUniformAnswerAndOnlyCappedMetadataIsKept(t *testing.T) {
	e := newEnv(t)
	cfg := outbound.DefaultConfig()
	cfg.CallbackInvalidPerMin = 3
	e.svc.Cfg = cfg
	c := e.setup(variant{callback: true})
	a := e.pendingAttempt(c, "PAY-1")

	cases := map[string]cb{
		"bad secret":      {conn: c, body: cbBody("e1", "P-9", "SETTLED", "PAY-1"), secret: "wrong"},
		"no signature":    {conn: c, body: cbBody("e1", "P-9", "SETTLED", "PAY-1"), headers: map[string]string{"X-Signature": ""}},
		"forged":          {conn: c, body: cbBody("e1", "P-9", "SETTLED", "PAY-1"), headers: map[string]string{"X-Signature": "deadbeef"}},
		"body changed":    {conn: c, body: cbBody("e1", "P-9", "SETTLED", "PAY-1"), headers: map[string]string{"X-Signature": cbSign(cbSecret, "1", "x")}},
		"another route":   {conn: &xicconn.Connection{ConnectorID: "bank-x", ConnectionID: "00000000-0000-4000-8000-000000000000"}, body: cbBody("e1", "P-9", "SETTLED", "PAY-1")},
		"wrong connector": {conn: &xicconn.Connection{ConnectorID: "bank-y", ConnectionID: c.ConnectionID}, body: cbBody("e1", "P-9", "SETTLED", "PAY-1")},
		"wrong method":    {conn: c, body: cbBody("e1", "P-9", "SETTLED", "PAY-1"), method: "GET"},
		"unknown op":      {conn: c, body: cbBody("e1", "P-9", "SETTLED", "PAY-1"), op: "payment-status"},
		"inbound op":      {conn: c, body: cbBody("e1", "P-9", "SETTLED", "PAY-1"), op: "statement-push"},
	}
	var ref *outbound.CallbackOutcome
	for name, d := range cases {
		o := e.callback(d)
		assertCB(t, o, 401, integration.XICAuthenticationFailed)
		if o.Receipt != nil {
			t.Errorf("%s: the answer carries evidence", name)
		}
		if ref == nil {
			ref = o
		} else if o.XIC.Detail != ref.XIC.Detail || o.Detail != ref.Detail {
			t.Errorf("%s is distinguishable from the others", name)
		}
	}
	// evidence: only for connections that resolved, only metadata, and no more than the budget
	rs := e.receipts(tenantA)
	if len(rs) != 3 {
		t.Fatalf("%d receipts; the per-connection budget is 3 (and routes that never resolved leave none)", len(rs))
	}
	for _, r := range rs {
		if r.State != outbound.ReceiptRejected || r.ReasonCode != outbound.CBAuthFailed || r.AuthResult != outbound.AuthFailed || r.AttemptID != "" || r.SourceNetHash == "" {
			t.Fatalf("%+v", r)
		}
	}
	if n := countType(e.repo.Types(), "integration.callback.invalid"); n != 3 {
		t.Fatalf("%d invalid events", n)
	}
	if got := e.get(tenantA, a.AttemptID); got.State != outbound.StatePending {
		t.Fatalf("an unauthenticated callback moved the attempt: %+v", got)
	}
	// another tenant's credential does not open this connection
	if o := e.callback(cb{conn: c, body: cbBody("e9", "P-9", "SETTLED", "PAY-1"), secret: "another-tenants-secret"}); o.Status != 401 {
		t.Fatalf("%d", o.Status)
	}
}

func TestCallback_ValidSignatureOutsideTheReplayWindowIsRefusedAfterAuthentication(t *testing.T) {
	e := newEnv(t)
	c := e.setup(variant{callback: true})
	a := e.pendingAttempt(c, "PAY-1")
	o := e.callback(cb{conn: c, body: cbBody("e1", "P-9", "SETTLED", "PAY-1"), tsOff: -2 * time.Hour})
	assertCB(t, o, 401, integration.XICReplayWindowFailed)
	if o.Receipt == nil || o.Receipt.AuthResult != outbound.AuthReplay || o.Receipt.State != outbound.ReceiptRejected {
		t.Fatalf("%+v", o.Receipt)
	}
	if o := e.callback(cb{conn: c, body: cbBody("e1", "P-9", "SETTLED", "PAY-1"), tsOff: 2 * time.Hour}); o.Status != 401 {
		t.Fatalf("a timestamp from the future: %d", o.Status)
	}
	if got := e.get(tenantA, a.AttemptID); got.State != outbound.StatePending {
		t.Fatalf("%+v", got)
	}
	// a stale replay of an already-applied callback changes nothing, and neither does a fresh one
	assertCB(t, e.callback(cb{conn: c, body: cbBody("e1", "P-9", "SETTLED", "PAY-1")}), 200, "")
	assertCB(t, e.callback(cb{conn: c, body: cbBody("e1", "P-9", "SETTLED", "PAY-1"), tsOff: -2 * time.Hour}), 401, integration.XICReplayWindowFailed)
}

func TestCallback_VaultOutageIsRetryableAndNothingIsStored(t *testing.T) {
	e := newEnv(t)
	c := e.setup(variant{callback: true})
	e.pendingAttempt(c, "PAY-1")
	before := len(e.receipts(tenantA))
	e.secrets.down = true
	o := e.callback(cb{conn: c, body: cbBody("e1", "P-9", "SETTLED", "PAY-1")})
	if o.Status != 503 || o.RetryAfter == 0 || len(e.receipts(tenantA)) != before {
		t.Fatalf("%+v", o)
	}
	e.secrets.down = false
	assertCB(t, e.callback(cb{conn: c, body: cbBody("e1", "P-9", "SETTLED", "PAY-1")}), 200, "")
}

// ---- duplicates (NP-23) --------------------------------------------------------------------------------------

func TestCallback_DuplicateIsAcknowledgedDeterministicallyWithoutRepeatingTheEffect(t *testing.T) {
	e := newEnv(t)
	c := e.setup(variant{callback: true})
	a := e.pendingAttempt(c, "PAY-1")
	first := e.callback(cb{conn: c, body: cbBody("evt-1", "P-9", "SETTLED", "PAY-1")})
	assertCB(t, first, 200, "")
	versionAfter := e.get(tenantA, a.AttemptID).Version
	for i := 0; i < 3; i++ {
		d := e.callback(cb{conn: c, body: cbBody("evt-1", "P-9", "SETTLED", "PAY-1")})
		if d.Status != 200 || !d.Duplicate || d.Receipt.ReceiptID != first.Receipt.ReceiptID {
			t.Fatalf("%+v", d)
		}
	}
	if got := e.get(tenantA, a.AttemptID); got.Version != versionAfter {
		t.Fatalf("a duplicate touched the attempt: v%d -> v%d", versionAfter, got.Version)
	}
	if n := countType(e.repo.Types(), "integration.outbound.resolved"); n != 1 {
		t.Fatalf("%d resolutions", n)
	}
	var dups int
	for _, r := range e.receipts(tenantA) {
		if r.State == outbound.ReceiptDuplicate {
			dups++
			if r.OriginalReceiptID != first.Receipt.ReceiptID {
				t.Fatalf("%+v", r)
			}
		}
	}
	if dups != 3 {
		t.Fatalf("%d duplicate receipts", dups)
	}
	// the same event identifier with a different body is a conflict, not a repeat
	o := e.callback(cb{conn: c, body: cbBody("evt-1", "P-9", "FAILED", "PAY-1")})
	assertCB(t, o, 409, integration.XICDuplicateExchange)
	if got := e.get(tenantA, a.AttemptID); got.State != outbound.StateSettled {
		t.Fatalf("%+v", got)
	}
}

func TestCallback_ConcurrentIdenticalCallbacksApplyOnce(t *testing.T) {
	e := newEnv(t)
	c := e.setup(variant{callback: true})
	a := e.pendingAttempt(c, "PAY-1")
	var wg sync.WaitGroup
	var mu sync.Mutex
	var applied, dup, other int
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			o := e.callback(cb{conn: c, body: cbBody("race", "P-9", "SETTLED", "PAY-1")})
			mu.Lock()
			defer mu.Unlock()
			switch {
			case o.Status == 200 && !o.Duplicate:
				applied++
			case o.Status == 200 && o.Duplicate:
				dup++
			default:
				other++
			}
		}()
	}
	wg.Wait()
	if applied != 1 || other != 0 || dup != 19 {
		t.Fatalf("applied=%d dup=%d other=%d", applied, dup, other)
	}
	if n := countType(e.repo.Types(), "integration.outbound.resolved"); n != 1 {
		t.Fatalf("%d resolutions", n)
	}
	if got := e.get(tenantA, a.AttemptID); got.State != outbound.StateSettled {
		t.Fatalf("%+v", got)
	}
}

// ---- transitions (I-17) and ordering (NP-24, NP-25) --------------------------------------------------------

func TestCallback_CannotForceATransitionTheContractDoesNotAllow(t *testing.T) {
	e := newEnv(t)
	c := e.setup(variant{callback: true})
	a := e.reserve(c, "PAY-RES") // RESERVED: nothing has been submitted, so no callback can settle it
	o := e.callback(cb{conn: c, body: cbBody("e1", "P-1", "SETTLED", "PAY-RES")})
	assertCB(t, o, 409, integration.XICCallbackInvalid)
	if o.Receipt.ReasonCode != outbound.CBTransition || o.Receipt.State != outbound.ReceiptRejected {
		t.Fatalf("%+v", o.Receipt)
	}
	got := e.get(tenantA, a.AttemptID)
	if got.State != outbound.StateReserved || got.Version != a.Version {
		t.Fatalf("a cryptographically valid callback forced a transition: %+v", got)
	}
	// the rejection is evidence on the attempt and an event, and the same callback may be tried again later
	evs, _ := e.repo.Events(ctxFor(tenantA), a.AttemptID)
	if last := evs[len(evs)-1]; last.Kind != "CALLBACK_REJECTED" {
		t.Fatalf("%+v", last)
	}
	if !hasType(e.repo.Types(), "integration.callback.invalid") {
		t.Fatalf("%v", e.repo.Types())
	}
	// a callback for a status the contract defines but the attempt state forbids: PENDING cannot be told "pending"
	// as news, and a cancelled attempt cannot be settled
	if _, err := e.svc.Cancel(ctxFor(tenantA), other, a.AttemptID, "withdrawn"); err != nil {
		t.Fatal(err)
	}
	o = e.callback(cb{conn: c, body: cbBody("e2", "P-1", "SETTLED", "PAY-RES")})
	assertCB(t, o, 409, integration.XICCallbackInvalid)
	if o.Receipt.ReasonCode != outbound.CBLateContradicts || !o.Receipt.NeedsReconciliation {
		t.Fatalf("%+v", o.Receipt)
	}
	if got := e.get(tenantA, a.AttemptID); got.State != outbound.StateCancelled {
		t.Fatalf("%+v", got)
	}
}

func TestCallback_BeforeTheSubmitResponseAppliesToTheOriginalAttempt_NP24(t *testing.T) {
	e := newEnv(t)
	c := e.setup(variant{callback: true})
	a := e.reserve(c, "PAY-1")
	inflight, release := make(chan struct{}), make(chan struct{})
	e.prov.set(func(call) (int, string, error) {
		close(inflight)
		<-release
		return 202, `{"payment_id":"P-9"}`, nil
	})
	done := make(chan struct{})
	go func() {
		_, _ = e.svc.Submit(ctxFor(tenantA), domain, a.AttemptID, nil)
		close(done)
	}()
	<-inflight
	if got := e.get(tenantA, a.AttemptID); got.State != outbound.StateSubmitting || got.ProviderReference != "" {
		t.Fatalf("%+v", got)
	}
	// the provider's callback beats its own HTTP response; it echoes the request reference, so it finds its attempt
	o := e.callback(cb{conn: c, body: cbBody("evt-1", "P-9", "SETTLED", "PAY-1")})
	assertCB(t, o, 200, "")
	if o.Receipt.AttemptStateBefore != outbound.StateSubmitting || o.Receipt.AttemptStateAfter != outbound.StateSettled {
		t.Fatalf("%+v", o.Receipt)
	}
	close(release)
	<-done
	got := e.get(tenantA, a.AttemptID)
	if got.State != outbound.StateSettled || got.ResolutionMethod != outbound.MethodCallback || got.SubmitCount != 1 {
		t.Fatalf("the late submit response overwrote the callback: %+v", got)
	}
	if n := len(e.prov.posts()); n != 1 {
		t.Fatalf("%d submissions", n)
	}
}

func TestCallback_NoAttemptToApplyItToIsAskedToComeAgainNotGuessed(t *testing.T) {
	e := newEnv(t)
	c := e.setup(variant{callback: true})
	other := e.pendingAttempt(c, "PAY-1")
	o := e.callback(cb{conn: c, body: `{"event_id":"e1","payment_id":"P-404","status":"SETTLED"}`})
	assertCB(t, o, 409, integration.XICCallbackInvalid)
	if o.RetryAfter == 0 || o.Receipt.ReasonCode != outbound.CBUncorrelated || o.Receipt.AttemptID != "" {
		t.Fatalf("%+v", o)
	}
	// a callback whose reference contradicts the attempt it names is refused, not applied
	o = e.callback(cb{conn: c, body: cbBody("e2", "P-DIFFERENT", "SETTLED", "PAY-1")})
	assertCB(t, o, 409, integration.XICCallbackInvalid)
	if o.Receipt.ReasonCode != outbound.CBReferenceMismatch {
		t.Fatalf("%+v", o.Receipt)
	}
	if got := e.get(tenantA, other.AttemptID); got.State != outbound.StatePending {
		t.Fatalf("%+v", got)
	}
	// the rejected callback did not claim the event id: the same one is applied once the attempt can be found
	assertCB(t, e.callback(cb{conn: c, body: cbBody("e2", "P-9", "SETTLED", "PAY-1")}), 200, "")
}

func TestCallback_LateContradictionIsPreservedForReconciliationNotWrittenOverHistory_NP25(t *testing.T) {
	e := newEnv(t)
	c := e.setup(variant{callback: true})
	a := e.reserve(c, "PAY-1")
	e.prov.set(answer(400, `{"error":"no"}`))
	failed, _ := e.svc.Submit(ctxFor(tenantA), domain, a.AttemptID, nil)
	if failed.State != outbound.StateFailed {
		t.Fatalf("%+v", failed)
	}
	o := e.callback(cb{conn: c, body: cbBody("late-1", "P-3", "SETTLED", "PAY-1")})
	assertCB(t, o, 409, integration.XICCallbackInvalid)
	if o.Receipt.ReasonCode != outbound.CBLateContradicts || !o.Receipt.NeedsReconciliation || o.Receipt.AttemptStateBefore != outbound.StateFailed || o.Receipt.AttemptID != a.AttemptID {
		t.Fatalf("%+v", o.Receipt)
	}
	got := e.get(tenantA, a.AttemptID)
	if got.State != outbound.StateFailed || got.Version != failed.Version {
		t.Fatalf("history was rewritten: %+v", got)
	}
	for _, want := range []string{"integration.callback.inconsistent", "integration.callback.invalid"} {
		if !hasType(e.repo.Types(), want) {
			t.Fatalf("%s missing: %v", want, e.repo.Types())
		}
	}
	l, _ := e.repo.ListReceipts(ctxFor(tenantA), outbound.ReceiptFilter{NeedsReconciliation: true})
	if len(l) != 1 || l[0].ReceiptID != o.Receipt.ReceiptID {
		t.Fatalf("%+v", l)
	}
	// a late callback that AGREES with the record is only a confirmation
	d := e.callback(cb{conn: c, body: cbBody("late-2", "P-3", "Rejected", "PAY-1")})
	if d.Status != 200 || !d.Duplicate || d.Receipt.ReasonCode != outbound.CBAlreadyRecorded || d.Receipt.NeedsReconciliation {
		t.Fatalf("%+v", d.Receipt)
	}
	// and a stale "still processing" after the outcome is superseded, not a contradiction
	if d := e.callback(cb{conn: c, body: cbBody("late-3", "P-3", "PROCESSING", "PAY-1")}); d.Status != 200 || d.Receipt.NeedsReconciliation {
		t.Fatalf("%+v", d.Receipt)
	}
}

// ---- tenant and account claims (NP-26) ---------------------------------------------------------------------

func TestCallback_ClaimsAboutAnotherTenantOrAccountAreRejectedWithASecuritySignal_NP26(t *testing.T) {
	e := newEnv(t)
	c := e.activeConn(tenantA, e.publish(variant{callback: true, claims: true}, nil), "acc-a", refAPIKey)
	a := e.pendingAttempt(c, "PAY-1")
	for name, body := range map[string]string{
		"tenant":  `{"event_id":"e1","payment_id":"P-9","status":"SETTLED","client_ref":"PAY-1","tenant":"tenant-b"}`,
		"account": `{"event_id":"e2","payment_id":"P-9","status":"SETTLED","client_ref":"PAY-1","account":"someone-elses-account"}`,
	} {
		o := e.callback(cb{conn: c, body: body})
		assertCB(t, o, 403, integration.XICTenantScopeMismatch)
		if o.Receipt.ReasonCode != outbound.CBClaimMismatch {
			t.Fatalf("%s: %+v", name, o.Receipt)
		}
	}
	if got := e.get(tenantA, a.AttemptID); got.State != outbound.StatePending {
		t.Fatalf("%+v", got)
	}
	if n := countType(e.repo.Types(), "integration.security.callback-claim-mismatch"); n != 2 {
		t.Fatalf("%d signals", n)
	}
	// claims that agree with the connection are corroboration, and the callback applies
	good := `{"event_id":"e3","payment_id":"P-9","status":"SETTLED","client_ref":"PAY-1","tenant":"` + tenantA + `","account":"acc-a"}`
	assertCB(t, e.callback(cb{conn: c, body: good}), 200, "")
}

// ---- shape (exact schema, no guessing) ------------------------------------------------------------------------

func TestCallback_UnknownShapeIsRejectedNeverParsedPermissively(t *testing.T) {
	e := newEnv(t)
	c := e.setup(variant{callback: true})
	a := e.pendingAttempt(c, "PAY-1")
	type tc struct {
		d      cb
		status int
		code   integration.XICCode
		reason string
	}
	for name, x := range map[string]tc{
		"missing required field": {cb{conn: c, body: `{"event_id":"e1","payment_id":"P-9"}`}, 422, "", outbound.CBSchemaViolation},
		"not json":               {cb{conn: c, body: `payment settled`}, 422, "", outbound.CBSchemaViolation},
		"wrong field type":       {cb{conn: c, body: `{"event_id":1,"payment_id":"P-9","status":"SETTLED"}`}, 422, "", outbound.CBSchemaViolation},
		"unknown schema version": {cb{conn: c, body: cbBody("e1", "P-9", "SETTLED", "PAY-1"), headers: map[string]string{"X-Source-Schema-Version": "payment-cb@9"}}, 422, integration.XICSchemaVersionUnsupported, outbound.CBSchemaVersion},
		"compressed":             {cb{conn: c, body: cbBody("e1", "P-9", "SETTLED", "PAY-1"), headers: map[string]string{"Content-Encoding": "gzip"}}, 415, integration.XICSchemaVersionUnsupported, outbound.CBEncoding},
		"form media type":        {cb{conn: c, body: cbBody("e1", "P-9", "SETTLED", "PAY-1"), headers: map[string]string{"Content-Type": "application/x-www-form-urlencoded"}}, 415, integration.XICSchemaVersionUnsupported, outbound.CBMediaType},
		"status we do not know":  {cb{conn: c, body: cbBody("e1", "P-9", "ON_HOLD_FOR_REVIEW", "PAY-1")}, 422, integration.XICCallbackInvalid, outbound.CBStatusUnknown},
		"no event identifier":    {cb{conn: c, body: `{"payment_id":"P-9","status":"SETTLED","event_id":""}`}, 422, integration.XICCallbackInvalid, outbound.CBDedupeMissing},
	} {
		o := e.callback(x.d)
		assertCB(t, o, x.status, x.code)
		if o.Receipt == nil || o.Receipt.State != outbound.ReceiptRejected || o.Receipt.ReasonCode != x.reason {
			t.Errorf("%s: %+v", name, o.Receipt)
		}
	}
	if got := e.get(tenantA, a.AttemptID); got.State != outbound.StatePending || got.Version != a.Version {
		t.Fatalf("%+v", got)
	}
}

func TestCallback_ARedirectUrlInACallbackIsNeverFetchedOrStored_NP27(t *testing.T) {
	e := newEnv(t)
	c := e.setup(variant{callback: true})
	a := e.pendingAttempt(c, "PAY-1")
	calls := e.prov.count()
	body := `{"event_id":"e1","payment_id":"P-9","status":"SETTLED","client_ref":"PAY-1","redirect_url":"http://169.254.169.254/latest/meta-data","callback_url":"https://evil.example/steal"}`
	o := e.callback(cb{conn: c, body: body})
	assertCB(t, o, 200, "")
	if e.prov.count() != calls {
		t.Fatal("the service made a request because of data inside a callback")
	}
	for _, r := range e.receipts(tenantA) {
		b, _ := json.Marshal(r)
		if strings.Contains(string(b), "evil") || strings.Contains(string(b), "169.254") {
			t.Fatalf("receipt carries callback data: %s", b)
		}
	}
	for _, ev := range e.repo.Out {
		b, _ := json.Marshal(ev.Data)
		if strings.Contains(string(b), "evil") || strings.Contains(string(b), "169.254") {
			t.Fatalf("%s carries callback data: %s", ev.Type, b)
		}
	}
	if got := e.get(tenantA, a.AttemptID); got.State != outbound.StateSettled {
		t.Fatalf("%+v", got)
	}
}

// ---- connection state, limits, isolation ----------------------------------------------------------------------

func TestCallback_ConnectionNotActiveIsRecordedAndNotApplied(t *testing.T) {
	e := newEnv(t)
	c := e.setup(variant{callback: true})
	a := e.pendingAttempt(c, "PAY-1")
	if _, err := e.connSvc.Suspend(ctxFor(tenantA), operator, c.ConnectionID, e.mustConn(c).Version, "provider incident"); err != nil {
		t.Fatal(err)
	}
	o := e.callback(cb{conn: c, body: cbBody("e1", "P-9", "SETTLED", "PAY-1")})
	assertCB(t, o, 409, integration.XICConnectionNotActive)
	if o.Receipt == nil || o.Receipt.ReasonCode != outbound.CBConnectionInactive {
		t.Fatalf("%+v", o.Receipt)
	}
	if got := e.get(tenantA, a.AttemptID); got.State != outbound.StatePending {
		t.Fatalf("%+v", got)
	}
}

func TestCallback_LimitsAndSizes(t *testing.T) {
	e := newEnv(t)
	cfg := outbound.DefaultConfig()
	cfg.CallbackMaxBodyBytes = 512
	cfg.CallbackConnRatePerMin = 5
	e.svc.Cfg = cfg
	c := e.setup(variant{callback: true})
	e.pendingAttempt(c, "PAY-1")
	if o := e.callback(cb{conn: c, body: `{"event_id":"e1","payment_id":"P-9","status":"SETTLED","pad":"` + strings.Repeat("a", 1024) + `"}`}); o.Status != 413 {
		t.Fatalf("%d", o.Status)
	}
	var limited *outbound.CallbackOutcome
	for i := 0; i < 20 && limited == nil; i++ {
		if o := e.callback(cb{conn: c, body: cbBody("l"+strconv.Itoa(i), "P-9", "PROCESSING", "PAY-1"), secret: "wrong"}); o.Status == 429 {
			limited = o
		}
	}
	if limited == nil || limited.RetryAfter == 0 || limited.XIC == nil || limited.XIC.Code != integration.XICRateLimited {
		t.Fatalf("%+v", limited)
	}
}

func TestCallback_TenantsAreIsolated(t *testing.T) {
	e := newEnv(t)
	c := e.setup(variant{callback: true})
	a := e.pendingAttempt(c, "PAY-1")
	applied := e.callback(cb{conn: c, body: cbBody("evA", "P-9", "SETTLED", "PAY-1")})
	assertCB(t, applied, 200, "")
	// tenant B has its own connection to the same provider; its callbacks never reach tenant A's attempts, even
	// naming the same reference and request reference
	cB := e.activeConn(tenantB, e.publish(variant{callback: true}, nil), "acc-b", "0a000000-0000-4000-8000-0000000000c9")
	o := e.callback(cb{conn: cB, body: cbBody("evB", "P-9", "SETTLED", "PAY-1")})
	assertCB(t, o, 409, integration.XICCallbackInvalid)
	if o.Receipt.AttemptID != "" || o.Receipt.TenantID != tenantB || o.Receipt.ReasonCode != outbound.CBUncorrelated {
		t.Fatalf("%+v", o.Receipt)
	}
	if got := e.get(tenantA, a.AttemptID); got.State != outbound.StateSettled || got.Version != e.get(tenantA, a.AttemptID).Version {
		t.Fatalf("%+v", got)
	}
	for _, r := range e.receipts(tenantA) {
		if r.TenantID != tenantA {
			t.Fatalf("%+v", r)
		}
	}
	for _, r := range e.receipts(tenantB) {
		if r.TenantID != tenantB || r.AttemptID != "" {
			t.Fatalf("%+v", r)
		}
	}
	if _, err := e.repo.GetReceipt(ctxFor(tenantB), applied.Receipt.ReceiptID); err == nil {
		t.Fatal("tenant B read tenant A's receipt")
	}
	// A's secret in tenant B's route: the credential is bound per connection, so it is the connection's own
	// material that verifies; here the harness gives both the same material, so the callback authenticates for B
	// only and still finds nothing of A's
}
