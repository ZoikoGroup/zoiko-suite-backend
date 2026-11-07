package store_test

import "testing"

// outboxCount counts the facts of one type on the outbox, whichever tenant they belong to.
func (e *kenv) outboxCount(t *testing.T, eventType string) int {
	t.Helper()
	return e.count(t, `SELECT count(*) FROM eventing_outbox WHERE event_type=$1`, eventType)
}

const (
	evProposed = "com.zoikosuite.accounting.journal.proposed"
	evApproved = "com.zoikosuite.accounting.journal.approved"
	evRejected = "com.zoikosuite.accounting.journal.rejected"
	evEventRej = "com.zoikosuite.accounting.kernel.event.rejected"
)

// §33: a manual journal raises JournalProposed when it goes to review, and JournalApproved or JournalRejected when the
// reviewer decides — once each, and only when the move happened.
func TestJournalReviewRaisesItsLifecycleEvents(t *testing.T) {
	e := newKEnv(t)
	call := func(path, who string, body map[string]any) (int, map[string]any) {
		return e.call("POST", path, e.tenant, who, body)
	}
	a := e.mustCreate(t, e.manualBody("lc-1", "STANDARD", "", 40))
	if st, b := call("/v1/journals/"+a+"/submit", "bob", map[string]any{}); st != 200 {
		t.Fatalf("%d %v", st, b)
	}
	if e.outboxCount(t, evProposed) != 1 || e.outboxCount(t, evApproved) != 0 {
		t.Fatal("submitting raises JournalProposed, and only that")
	}
	// a refused approval (the preparer) is no decision and raises nothing
	if st, _ := call("/v1/journals/"+a+"/approve", "preparer", map[string]any{}); st != 403 {
		t.Fatalf("self approval: %d", st)
	}
	if e.outboxCount(t, evApproved) != 0 {
		t.Fatal("a refused approval raises nothing")
	}
	if st, b := call("/v1/journals/"+a+"/approve", "carol", map[string]any{}); st != 200 {
		t.Fatalf("%d %v", st, b)
	}
	if e.outboxCount(t, evApproved) != 1 {
		t.Fatal("approving raises JournalApproved")
	}
	// the event says who decided, about which journal, and carries the journal's own correlation
	if e.count(t, `SELECT count(*) FROM eventing_outbox WHERE event_type=$1 AND aggregate_id=$2 AND convert_from(payload,'UTF8') LIKE '%carol%'`, evApproved, a) != 1 {
		t.Fatal("JournalApproved names the journal and the approver")
	}
	// approving again is refused and raises nothing more
	if st, _ := call("/v1/journals/"+a+"/approve", "dave", map[string]any{}); st == 200 {
		t.Fatal("an approved journal cannot be approved again")
	}
	if e.outboxCount(t, evApproved) != 1 || e.outboxCount(t, evProposed) != 1 {
		t.Fatal("a repeated decision raises no second event")
	}

	r := e.mustCreate(t, e.manualBody("lc-2", "STANDARD", "", 60))
	call("/v1/journals/"+r+"/submit", "bob", map[string]any{})
	if st, b := call("/v1/journals/"+r+"/reject", "carol", map[string]any{"reason": "wrong cost centre"}); st != 200 {
		t.Fatalf("%d %v", st, b)
	}
	if e.outboxCount(t, evRejected) != 1 || e.outboxCount(t, evProposed) != 2 {
		t.Fatal("rejecting raises JournalRejected")
	}
	if e.count(t, `SELECT count(*) FROM eventing_outbox WHERE event_type=$1 AND aggregate_id=$2 AND convert_from(payload,'UTF8') LIKE '%wrong cost centre%'`, evRejected, r) != 1 {
		t.Fatal("JournalRejected carries the reason")
	}
}

// §33: an accounting event the kernel refuses at intake raises AccountingEventRejected, and stores nothing.
func TestRefusedEventRaisesAccountingEventRejected(t *testing.T) {
	e := newKEnv(t)
	e.seedChart(t)
	e.activeBook(t, "STAT")

	bad := eventBody(e.entity, "se-bad", "120.00", "100.00", "20.00")
	delete(bad, "idempotency_key")
	if st, b := e.call("POST", "/v1/accounting-events", e.tenant, "ar-svc", bad); st < 400 || st >= 500 {
		t.Fatalf("an invalid event is refused: %d %v", st, b)
	}
	if e.outboxCount(t, evEventRej) != 1 {
		t.Fatal("a refused event raises AccountingEventRejected")
	}
	if e.count(t, `SELECT count(*) FROM accounting_events`) != 0 {
		t.Fatal("a rejected event is not stored")
	}
	if e.count(t, `SELECT count(*) FROM eventing_outbox WHERE event_type=$1 AND convert_from(payload,'UTF8') LIKE '%se-bad%' AND convert_from(payload,'UTF8') LIKE '%idempotency_key%'`, evEventRej) != 1 {
		t.Fatal("the rejection names the producer event and why")
	}

	// the same producer event registered again with other content is a conflict, and is rejected too
	ok := eventBody(e.entity, "se-ok", "120.00", "100.00", "20.00")
	if st, b := e.call("POST", "/v1/accounting-events", e.tenant, "ar-svc", ok); st != 201 {
		t.Fatalf("%d %v", st, b)
	}
	if e.outboxCount(t, evEventRej) != 1 {
		t.Fatal("an accepted event is not a rejection")
	}
	// an exact replay is not a rejection
	if st, _ := e.call("POST", "/v1/accounting-events", e.tenant, "ar-svc", ok); st != 200 {
		t.Fatalf("replay: %d", st)
	}
	clash := eventBody(e.entity, "se-ok", "130.00", "110.00", "20.00")
	if st, b := e.call("POST", "/v1/accounting-events", e.tenant, "ar-svc", clash); st != 422 {
		t.Fatalf("a conflicting replay: %d %v", st, b)
	}
	if e.outboxCount(t, evEventRej) != 2 || e.count(t, `SELECT count(*) FROM accounting_events`) != 1 {
		t.Fatal("a conflicting replay is rejected and the original stands")
	}
	if e.count(t, `SELECT count(*) FROM eventing_outbox WHERE event_type=$1 AND convert_from(payload,'UTF8') LIKE '%SOURCE_EVENT_CONFLICT%'`, evEventRej) != 1 {
		t.Fatal("the rejection carries the exception code")
	}
}
