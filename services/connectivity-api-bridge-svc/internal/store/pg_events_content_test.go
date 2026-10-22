package store_test

import (
	"strings"
	"testing"

	"zoiko.io/connectivity-api-bridge-svc/internal/outbound"
)

// The outbox stores each event as a bytea envelope. A check written as payload::text compares against the HEX of
// that envelope and can never find a secret, whatever the event holds. These tests read the real content
// (convert_from) and carry a positive control, so a check that cannot see event content fails loudly instead of passing.

func contentCount(o interface {
	countAs(string, string, ...any) int
}, where string) int {
	return o.countAs(cTenantA, `SELECT count(*) FROM eventing_outbox WHERE `+where)
}

func TestEvents_OutboundAndCallbackEventsHoldNoSecretsOrPayloadValues(t *testing.T) {
	o := newObEnv(t)
	c := o.cbReady(t)
	a := o.pending(t, c, "PAY-1")
	if r := o.callback(c, cbPayload("evt-secret-id", "P-9", "SETTLED", "PAY-1"), ""); r.Status != 200 {
		t.Fatalf("%d", r.Status)
	}
	o.callback(c, cbPayload("evt-bad", "P-9", "SETTLED", "PAY-1"), "wrong-secret") // an invalid-callback event
	if _, err := o.svc.Cancel(tenantCtx(cTenantA), obOps, o.reserve(t, c, "PAY-2").AttemptID, "withdrawn"); err != nil {
		t.Fatal(err)
	}
	_ = a

	// positive controls: the check can see what the events carry
	for _, must := range []string{`"attempt_id"`, `"connection_id"`, `"tenant_id"`, a.AttemptID} {
		if n := contentCount(o, `convert_from(payload, 'UTF8') LIKE '%`+must+`%'`); n == 0 {
			t.Fatalf("the check cannot see %s in any event: it would pass for the wrong reason", must)
		}
	}
	// and what they must never carry
	for _, never := range []string{obKey, pgCBSecret, "10.00", "evt-secret-id", "evt-bad", "wrong-secret", "GBP"} {
		if n := contentCount(o, `event_type LIKE '%integration%' AND convert_from(payload, 'UTF8') LIKE '%`+never+`%'`); n != 0 {
			t.Errorf("%d events carry %q", n, never)
		}
	}
	// the evidence rows too
	for _, never := range []string{obKey, pgCBSecret, "10.00", "evt-secret-id"} {
		if n := o.countAs(cTenantA, `SELECT count(*) FROM xic_outbound_events WHERE detail::text LIKE '%`+never+`%'`); n != 0 {
			t.Errorf("%d attempt events carry %q", n, never)
		}
		if n := o.countAs(cTenantA, `SELECT count(*) FROM xic_callback_receipts WHERE to_jsonb(xic_callback_receipts)::text LIKE '%`+never+`%'`); n != 0 {
			t.Errorf("%d receipts carry %q", n, never)
		}
	}
	_ = outbound.StateSettled
	_ = strings.ToLower
}

func TestEvents_InboundEventsHoldNoSecretsOrPayloadValues(t *testing.T) {
	g := newGwEnv(t)
	_, c := g.active(t)
	g.target(t)
	if o := g.deliver(c, body("evt-1"), "k1", pgHMAC); o.Status != 202 {
		t.Fatalf("%d", o.Status)
	}
	g.drive(t)
	g.deliver(c, body("evt-1"), "k1", pgHMAC)                                     // a duplicate
	g.deliver(c, strings.Replace(body("evt-2"), "10.5", "77.7", 1), "k2", pgHMAC) // another delivery
	for _, must := range []string{`"exchange_id"`, `"connection_id"`, `"tenant_id"`} {
		if n := contentCount(g, `convert_from(payload, 'UTF8') LIKE '%`+must+`%'`); n == 0 {
			t.Fatalf("the check cannot see %s in any event: it would pass for the wrong reason", must)
		}
	}
	for _, never := range []string{pgHMAC, "10.5", "10.50", "77.7", "evt-1", "evt-2", "acc-a"} {
		if n := contentCount(g, `event_type LIKE '%integration%' AND convert_from(payload, 'UTF8') LIKE '%`+never+`%'`); n != 0 {
			t.Errorf("%d events carry %q", n, never)
		}
	}
}
