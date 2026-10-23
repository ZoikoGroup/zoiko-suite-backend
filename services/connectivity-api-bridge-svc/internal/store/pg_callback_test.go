package store_test

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"zoiko.io/connectivity-api-bridge-svc/internal/outbound"
	"zoiko.io/connectivity-api-bridge-svc/internal/xicconn"
)

// These run the provider-callback pipeline against REAL Postgres as an ordinary role: the receipts table with its
// append-only and uniqueness rules, atomic application with the attempt, late-callback evidence on a FINAL attempt
// (which cannot be updated at all), row-level security and concurrency.

func (o *obEnv) now() time.Time {
	o.mu.Lock()
	defer o.mu.Unlock()
	return time.Now().UTC().Add(o.nowAdd)
}

func cbPayload(event, ref, status, clientRef string) string {
	return `{"event_id":"` + event + `","payment_id":"` + ref + `","status":"` + status + `","client_ref":"` + clientRef + `"}`
}

func (o *obEnv) callback(c *xicconn.Connection, body, secret string) *outbound.CallbackOutcome {
	if secret == "" {
		secret = pgCBSecret
	}
	ts := strconv.FormatInt(o.now().Unix(), 10)
	m := hmac.New(sha256.New, []byte(secret))
	m.Write([]byte(ts + "." + body))
	h := http.Header{}
	h.Set("Content-Type", "application/json")
	h.Set("X-Timestamp", ts)
	h.Set("X-Signature", hex.EncodeToString(m.Sum(nil)))
	return o.svc.ReceiveCallback(context.Background(), outbound.CallbackRequest{ConnectorKey: c.ConnectorID, ConnectionID: c.ConnectionID, OperationID: "payment-submit",
		Method: "POST", Header: h, Body: []byte(body), RemoteAddr: "198.51.100.7:4433", CorrelationID: "corr-pg-cb"})
}

func (o *obEnv) pending(t *testing.T, c *xicconn.Connection, ref string) *outbound.Attempt {
	t.Helper()
	a := o.reserve(t, c, ref)
	o.prov.set(func(obCall) (int, string, error) { return 202, `{"payment_id":"P-9"}`, nil })
	got, err := o.svc.Submit(tenantCtx(c.TenantID), obDomain, a.AttemptID, nil)
	if err != nil || got.State != outbound.StatePending || got.ProviderReference != "P-9" {
		t.Fatalf("%v %+v", err, got)
	}
	return got
}

func (o *obEnv) cbReady(t *testing.T) *xicconn.Connection {
	t.Helper()
	return o.active(t, cTenantA, o.contractWith(t, false, true), "acc-cb", cRefHMAC)
}

func TestCallbackPG_MigrationIsRerunnable(t *testing.T) {
	o := newObEnv(t)
	dir, names := migrationFiles(t)
	for _, n := range names {
		if strings.HasPrefix(n, "008") {
			b, err := os.ReadFile(filepath.Join(dir, n))
			if err != nil {
				t.Fatal(err)
			}
			if _, err := o.admin.Exec(context.Background(), string(b)); err != nil {
				t.Fatalf("008 applied a second time: %v", err)
			}
			return
		}
	}
	t.Fatal("no 008 migration")
}

func TestCallbackPG_AppliedAtomicallyWithItsReceiptAndEvents(t *testing.T) {
	o := newObEnv(t)
	c := o.cbReady(t)
	a := o.pending(t, c, "PAY-1")
	got := o.callback(c, cbPayload("evt-1", "P-9", "SETTLED", "PAY-1"), "")
	if got.Status != 200 || got.Receipt == nil || got.Receipt.State != outbound.ReceiptApplied {
		t.Fatalf("%+v xic=%v", got.Receipt, got.XIC)
	}
	att := o.get(t, a.AttemptID)
	if att.State != outbound.StateSettled || att.ResolutionMethod != outbound.MethodCallback || att.BodyRetained {
		t.Fatalf("%+v", att)
	}
	r, err := o.repo.GetReceipt(tenantCtx(cTenantA), got.Receipt.ReceiptID)
	if err != nil || r.AttemptID != a.AttemptID || r.AttemptStateBefore != "PENDING" || r.AttemptStateAfter != "SETTLED" || r.AuthResult != "OK" || r.BodyHash == "" || r.NeedsReconciliation {
		t.Fatalf("%v %+v", err, r)
	}
	for _, want := range []string{"callback.applied", "outbound.resolved"} {
		if n := o.countAs(cTenantA, `SELECT count(*) FROM eventing_outbox WHERE event_type=$1`, "com.zoikosuite.integration."+want); n != 1 {
			t.Errorf("%d %s events", n, want)
		}
	}
	events, _ := o.repo.Events(tenantCtx(cTenantA), a.AttemptID)
	var kinds []string
	for _, e := range events {
		kinds = append(kinds, e.Kind)
	}
	if strings.Join(kinds, ",") != "RESERVED,SUBMIT_STARTED,PENDING,RESOLVED" {
		t.Fatalf("%v", kinds)
	}
	// nothing from the body is stored anywhere: the receipt holds hashes and metadata
	if n := o.countAs(cTenantA, `SELECT count(*) FROM xic_callback_receipts WHERE to_jsonb(xic_callback_receipts)::text LIKE '%evt-1%'`); n != 0 {
		t.Fatalf("%d receipts carry the event id", n)
	}
	if n := o.countAs(cTenantA, `SELECT count(*) FROM eventing_outbox WHERE event_type LIKE '%callback%' AND convert_from(payload, 'UTF8') LIKE '%evt-1%'`); n != 0 {
		t.Fatalf("%d callback events carry the event id", n)
	}
}

func TestCallbackPG_ConcurrentIdenticalCallbacksApplyOnce(t *testing.T) {
	o := newObEnv(t)
	c := o.cbReady(t)
	a := o.pending(t, c, "PAY-1")
	var applied, dup, other int32
	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			r := o.callback(c, cbPayload("race", "P-9", "SETTLED", "PAY-1"), "")
			switch {
			case r.Status == 200 && !r.Duplicate:
				atomic.AddInt32(&applied, 1)
			case r.Status == 200 && r.Duplicate:
				atomic.AddInt32(&dup, 1)
			default:
				atomic.AddInt32(&other, 1)
				t.Errorf("%d %v %+v", r.Status, r.XIC, r.Receipt)
			}
		}()
	}
	wg.Wait()
	if applied != 1 || dup != 15 || other != 0 {
		t.Fatalf("applied=%d dup=%d other=%d", applied, dup, other)
	}
	if n := o.countAs(cTenantA, `SELECT count(*) FROM xic_callback_receipts WHERE state='APPLIED'`); n != 1 {
		t.Fatalf("%d applied receipts", n)
	}
	if n := o.countAs(cTenantA, `SELECT count(*) FROM xic_callback_receipts WHERE state='DUPLICATE'`); n != 15 {
		t.Fatalf("%d duplicate receipts", n)
	}
	if n := o.countAs(cTenantA, `SELECT count(*) FROM eventing_outbox WHERE event_type='com.zoikosuite.integration.outbound.resolved'`); n != 1 {
		t.Fatalf("%d resolutions", n)
	}
	if got := o.get(t, a.AttemptID); got.State != outbound.StateSettled {
		t.Fatalf("%+v", got)
	}
	// the database itself refuses a second APPLIED receipt for the same event
	wantDBError(t, "second applied receipt", o.execAs(cTenantA, `INSERT INTO xic_callback_receipts (receipt_id, tenant_id, connection_id, connector_id, operation_id, contract_version_id,
		attempt_id, state, auth_result, dedupe_key, body_hash, body_size, target_state, attempt_state_before, attempt_state_after)
		SELECT gen_random_uuid(), tenant_id, connection_id, connector_id, operation_id, contract_version_id, attempt_id, 'APPLIED', 'OK', dedupe_key, body_hash, 1, 'SETTLED', 'PENDING', 'SETTLED'
		FROM xic_callback_receipts WHERE state='APPLIED'`), "xic_cr_dedupe")
}

func TestCallbackPG_LateContradictionOnAFinalAttemptIsRecordedWithoutTouchingIt(t *testing.T) {
	o := newObEnv(t)
	c := o.cbReady(t)
	a := o.reserve(t, c, "PAY-1")
	o.prov.set(func(obCall) (int, string, error) { return 400, `{"error":"no"}`, nil })
	failed, err := o.svc.Submit(tenantCtx(cTenantA), obDomain, a.AttemptID, nil)
	if err != nil || failed.State != outbound.StateFailed {
		t.Fatalf("%v %+v", err, failed)
	}
	// a FINAL attempt cannot be updated at all (the database refuses it); the late callback is evidence beside it
	r := o.callback(c, cbPayload("late-1", "P-3", "SETTLED", "PAY-1"), "")
	if r.Status != 409 || r.Receipt == nil || r.Receipt.ReasonCode != outbound.CBLateContradicts || !r.Receipt.NeedsReconciliation {
		t.Fatalf("%d %+v %v", r.Status, r.Receipt, r.XIC)
	}
	got := o.get(t, a.AttemptID)
	if got.State != outbound.StateFailed || got.Version != failed.Version {
		t.Fatalf("history was rewritten: %+v", got)
	}
	l, err := o.repo.ListReceipts(tenantCtx(cTenantA), outbound.ReceiptFilter{NeedsReconciliation: true})
	if err != nil || len(l) != 1 || l[0].ReceiptID != r.Receipt.ReceiptID || l[0].AttemptStateBefore != "FAILED" {
		t.Fatalf("%v %+v", err, l)
	}
	for _, want := range []string{"callback.inconsistent", "callback.invalid"} {
		if n := o.countAs(cTenantA, `SELECT count(*) FROM eventing_outbox WHERE event_type=$1`, "com.zoikosuite.integration."+want); n != 1 {
			t.Errorf("%d %s events", n, want)
		}
	}
	// the same outcome told late is only a confirmation
	d := o.callback(c, cbPayload("late-2", "P-3", "Rejected", "PAY-1"), "")
	if d.Status != 200 || !d.Duplicate || d.Receipt.ReasonCode != outbound.CBAlreadyRecorded {
		t.Fatalf("%d %+v", d.Status, d.Receipt)
	}
}

func TestCallbackPG_ImpossibleTransitionAndUnmatchedCallbacksChangeNothing(t *testing.T) {
	o := newObEnv(t)
	c := o.cbReady(t)
	a := o.reserve(t, c, "PAY-RES") // RESERVED: nothing was submitted, so no callback can settle it
	r := o.callback(c, cbPayload("e1", "P-1", "SETTLED", "PAY-RES"), "")
	if r.Status != 409 || r.Receipt.ReasonCode != outbound.CBTransition {
		t.Fatalf("%d %+v", r.Status, r.Receipt)
	}
	if got := o.get(t, a.AttemptID); got.State != outbound.StateReserved {
		t.Fatalf("a valid signature forced a transition: %+v", got)
	}
	u := o.callback(c, `{"event_id":"e2","payment_id":"P-404","status":"SETTLED"}`, "")
	if u.Status != 409 || u.Receipt.ReasonCode != outbound.CBUncorrelated || u.RetryAfter == 0 {
		t.Fatalf("%d %+v", u.Status, u.Receipt)
	}
	// rejections did not claim their event ids: the same callback applies once it can
	o.prov.set(func(obCall) (int, string, error) { return 202, `{"payment_id":"P-1"}`, nil })
	if _, err := o.svc.Submit(tenantCtx(cTenantA), obDomain, a.AttemptID, nil); err != nil {
		t.Fatal(err)
	}
	if ok := o.callback(c, cbPayload("e1", "P-1", "SETTLED", "PAY-RES"), ""); ok.Status != 200 || ok.Receipt.State != outbound.ReceiptApplied {
		t.Fatalf("%d %+v", ok.Status, ok.Receipt)
	}
}

func TestCallbackPG_AuthenticationFailuresLeaveOnlyCappedMetadata(t *testing.T) {
	o := newObEnv(t)
	cfg := outbound.DefaultConfig()
	cfg.CallbackInvalidPerMin = 3
	o.svc.Cfg = cfg
	c := o.cbReady(t)
	a := o.pending(t, c, "PAY-1")
	for i := 0; i < 10; i++ {
		if r := o.callback(c, cbPayload("x"+strconv.Itoa(i), "P-9", "SETTLED", "PAY-1"), "wrong-secret"); r.Status != 401 || r.Receipt != nil {
			t.Fatalf("%d %+v", r.Status, r.Receipt)
		}
	}
	if n := o.countAs(cTenantA, `SELECT count(*) FROM xic_callback_receipts WHERE reason_code='AUTH_FAILED' AND auth_result='FAILED' AND attempt_id IS NULL`); n != 3 {
		t.Fatalf("%d evidence rows; the budget is 3", n)
	}
	if n := o.countAs(cTenantA, `SELECT count(*) FROM eventing_outbox WHERE event_type='com.zoikosuite.integration.callback.invalid'`); n != 3 {
		t.Fatalf("%d invalid events", n)
	}
	if got := o.get(t, a.AttemptID); got.State != outbound.StatePending {
		t.Fatalf("%+v", got)
	}
	// a stale timestamp on a valid signature is a replay
	ts := strconv.FormatInt(o.now().Add(-3*time.Hour).Unix(), 10)
	body := cbPayload("rp", "P-9", "SETTLED", "PAY-1")
	m := hmac.New(sha256.New, []byte(pgCBSecret))
	m.Write([]byte(ts + "." + body))
	h := http.Header{}
	h.Set("Content-Type", "application/json")
	h.Set("X-Timestamp", ts)
	h.Set("X-Signature", hex.EncodeToString(m.Sum(nil)))
	rr := o.svc.ReceiveCallback(context.Background(), outbound.CallbackRequest{ConnectorKey: c.ConnectorID, ConnectionID: c.ConnectionID, OperationID: "payment-submit",
		Method: "POST", Header: h, Body: []byte(body), RemoteAddr: "198.51.100.8:1", CorrelationID: "c"})
	if rr.Status != 401 || rr.Receipt == nil || rr.Receipt.AuthResult != "REPLAY" {
		t.Fatalf("%d %+v", rr.Status, rr.Receipt)
	}
}

// ---- what the database enforces on its own ---------------------------------------------------------------

func TestCallbackPG_ReceiptsAreAppendOnlyAndConstrained(t *testing.T) {
	o := newObEnv(t)
	c := o.cbReady(t)
	o.pending(t, c, "PAY-1")
	r := o.callback(c, cbPayload("e1", "P-9", "SETTLED", "PAY-1"), "")
	if r.Status != 200 {
		t.Fatalf("%d", r.Status)
	}
	wantDBError(t, "rewrite a receipt", o.execAs(cTenantA, `UPDATE xic_callback_receipts SET reason_code='X'`), "")
	wantDBError(t, "delete a receipt", o.execAs(cTenantA, `DELETE FROM xic_callback_receipts`), "")
	ins := func(cols, vals string) error {
		return o.execAs(cTenantA, `INSERT INTO xic_callback_receipts (receipt_id, tenant_id, connection_id, connector_id, operation_id, contract_version_id, body_hash, body_size, `+cols+`)
			SELECT gen_random_uuid(), tenant_id, connection_id, connector_id, operation_id, contract_version_id, repeat('a', 64), 1, `+vals+` FROM xic_callback_receipts LIMIT 1`)
	}
	wantDBError(t, "applied without an attempt", ins(`state, auth_result, dedupe_key, target_state, attempt_state_before, attempt_state_after`, `'APPLIED', 'OK', 'k1', 'SETTLED', 'PENDING', 'SETTLED'`), "xic_cr_applied")
	wantDBError(t, "rejected without a reason", ins(`state, auth_result`, `'REJECTED', 'FAILED'`), "xic_cr_rejected_reasoned")
	wantDBError(t, "duplicate pointing nowhere", ins(`state, auth_result, reason_code`, `'DUPLICATE', 'OK', 'DUPLICATE_EVENT'`), "xic_cr_duplicate_points_back")
	wantDBError(t, "inconsistency without its reason", ins(`state, auth_result, reason_code, needs_reconciliation`, `'REJECTED', 'OK', 'AUTH_FAILED', true`), "xic_cr_reconcile_reasoned")
	wantDBError(t, "unknown state", ins(`state, auth_result`, `'LOST', 'OK'`), "xic_cr_state_valid")
	wantDBError(t, "unknown auth result", ins(`state, auth_result, reason_code`, `'REJECTED', 'MAYBE', 'X'`), "xic_cr_auth_valid")
}

func TestCallbackPG_TenantIsolation(t *testing.T) {
	o := newObEnv(t)
	c := o.cbReady(t)
	o.pending(t, c, "PAY-1")
	r := o.callback(c, cbPayload("e1", "P-9", "SETTLED", "PAY-1"), "")
	b := tenantCtx(cTenantB)
	if _, err := o.repo.GetReceipt(b, r.Receipt.ReceiptID); err == nil {
		t.Fatal("tenant B read tenant A's receipt")
	}
	if l, _ := o.repo.ListReceipts(b, outbound.ReceiptFilter{}); len(l) != 0 {
		t.Fatal("tenant B lists tenant A's receipts")
	}
	if n := o.countAs(cTenantB, `SELECT count(*) FROM xic_callback_receipts`); n != 0 {
		t.Fatalf("%d", n)
	}
	if n := o.countAs("", `SELECT count(*) FROM xic_callback_receipts`); n != 0 {
		t.Fatalf("%d rows visible with no tenant", n)
	}
	o.wantRefused(t, "update", cTenantB, `UPDATE xic_callback_receipts SET reason_code='X'`)
	o.wantRefused(t, "delete", cTenantB, `DELETE FROM xic_callback_receipts`)
	wantDBError(t, "forged receipt", o.execAs(cTenantB, `INSERT INTO xic_callback_receipts (receipt_id, tenant_id, connection_id, connector_id, operation_id, contract_version_id,
		state, auth_result, reason_code, body_hash, body_size) VALUES (gen_random_uuid(), $1, $2::uuid, 'bank-x', 'payment-submit', $3::uuid, 'REJECTED', 'FAILED', 'X', repeat('a', 64), 1)`,
		cTenantA, c.ConnectionID, c.ContractVersionID), "row-level security")
	// a callback for tenant B's connection cannot find tenant A's attempt
	if _, err := o.repo.FindAttempt(b, c.ConnectionID, "payment-submit", "P-9", "PAY-1"); err == nil {
		t.Fatal("tenant B found tenant A's attempt")
	}
}

func TestCallbackPG_CallbackAndSubmitResponseRaceNeverOverwritesTheCallback(t *testing.T) {
	o := newObEnv(t)
	c := o.cbReady(t)
	a := o.reserve(t, c, "PAY-1")
	inflight, release := make(chan struct{}), make(chan struct{})
	o.prov.set(func(obCall) (int, string, error) {
		close(inflight)
		<-release
		return 202, `{"payment_id":"P-9"}`, nil
	})
	done := make(chan struct{})
	go func() {
		_, _ = o.svc.Submit(tenantCtx(cTenantA), obDomain, a.AttemptID, nil)
		close(done)
	}()
	<-inflight
	r := o.callback(c, cbPayload("evt-1", "P-9", "SETTLED", "PAY-1"), "")
	if r.Status != 200 || r.Receipt.AttemptStateBefore != "SUBMITTING" || r.Receipt.AttemptStateAfter != "SETTLED" {
		t.Fatalf("%d %+v", r.Status, r.Receipt)
	}
	close(release)
	<-done
	got := o.get(t, a.AttemptID)
	if got.State != outbound.StateSettled || got.ResolutionMethod != outbound.MethodCallback || got.SubmitCount != 1 {
		t.Fatalf("%+v", got)
	}
	if o.prov.posts() != 1 {
		t.Fatalf("%d submissions", o.prov.posts())
	}
}

var _ = json.Marshal
