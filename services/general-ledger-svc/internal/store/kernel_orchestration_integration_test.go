package store_test

import (
	"strings"
	"testing"
)

// §4, §37: one event can have a consequence in several books of an entity. Each posting is atomic by itself; the event is incomplete
// until every book that must carry it has posted, and a posted book never hides one that has not.
func TestMultiBookEventIsIncompleteUntilEveryRequiredBookHasPosted(t *testing.T) {
	e := newKEnv(t)
	e.seedChart(t)
	stat := e.activeBook(t, "STAT")
	e.activate(t, invoiceRule("1.0.0"), stat)

	mb := bookBody(e.entity, "MGMT")
	mb["book_type"] = "MANAGEMENT"
	st, b := e.call("POST", "/v1/accounting-books", e.tenant, "owner", mb)
	if st != 201 {
		t.Fatalf("%d %v", st, b)
	}
	mgmt := b["book_id"].(string)
	if st, b := e.call("POST", "/v1/accounting-books/"+mgmt+"/activate", e.tenant, "owner", map[string]any{}); st != 200 {
		t.Fatalf("%d %v", st, b)
	}
	// the management book has its own rule for the same event
	mr := invoiceRule("1.0.0")
	mr["name"], mr["book_type"] = "ar-invoice-mgmt", "MANAGEMENT"
	e.activate(t, mr, mgmt)

	status := func(ev string) map[string]any {
		t.Helper()
		st, s := e.call("GET", "/v1/accounting-events/"+ev+"/posting-status", e.tenant, "auditor", nil)
		if st != 200 {
			t.Fatalf("%d %v", st, s)
		}
		return s
	}
	books := func(s map[string]any) map[string]string {
		m := map[string]string{}
		for _, x := range s["books"].([]any) {
			b := x.(map[string]any)
			m[b["book_code"].(string)] = b["status"].(string)
		}
		return m
	}

	ev := e.registerEvent(t, "se-mb", "120.00", "100.00", "20.00")
	s := status(ev)
	if s["status"] != "INCOMPLETE" || s["required_books"].(float64) != 2 || s["posted_books"].(float64) != 0 || books(s)["STAT"] != "PENDING" || books(s)["MGMT"] != "PENDING" {
		t.Fatalf("registered, not posted anywhere: %v", s)
	}

	// posted in the statutory book only: the event is still incomplete and names the book that is missing
	e.mustPost(t, ev, stat)
	s = status(ev)
	out := s["outstanding_book_codes"].([]any)
	if s["status"] != "INCOMPLETE" || s["posted_books"].(float64) != 1 || len(out) != 1 || out[0] != "MGMT" || books(s)["STAT"] != "POSTED" || books(s)["MGMT"] != "PENDING" {
		t.Fatalf("a posted book must not hide the one that has not: %v", s)
	}
	// the close evidence says so too: a period with an event that has not posted everywhere is not ready, whatever each book says alone
	closeEvidence := func() map[string]any {
		t.Helper()
		st, c := e.call("GET", "/v1/close-evidence?legal_entity_id="+e.entity+"&fiscal_period=2026-03", e.tenant, "close-svc", nil)
		if st != 200 {
			t.Fatalf("%d %v", st, c)
		}
		return c
	}
	hasIssue := func(c map[string]any, prefix string) bool {
		for _, x := range c["blocking_issues"].([]any) {
			if strings.HasPrefix(x.(string), prefix) {
				return true
			}
		}
		return false
	}
	if c := closeEvidence(); c["ready"] != false || !hasIssue(c, "incomplete_events: 1 ") || c["incomplete_events"].(map[string]any)["accounting_event_ids"].([]any)[0] != ev {
		t.Fatalf("%v", c)
	}
	// the list an operator works from shows it, but not once it has aged out of the window they asked for
	list := func(q string) map[string]any {
		t.Helper()
		st, l := e.call("GET", "/v1/accounting-events/incomplete?legal_entity_id="+e.entity+q, e.tenant, "auditor", nil)
		if st != 200 {
			t.Fatalf("%d %v", st, l)
		}
		return l
	}
	if l := list(""); len(l["incomplete_events"].([]any)) != 1 || l["incomplete_events"].([]any)[0].(map[string]any)["accounting_event_id"] != ev {
		t.Fatalf("%v", l)
	}
	if l := list("&min_age_minutes=60"); len(l["incomplete_events"].([]any)) != 0 || l["events_scanned"].(float64) != 1 {
		t.Fatalf("an event registered a moment ago is not yet reported when a minimum age is asked for: %v", l)
	}

	// posted in both: complete, and no longer listed
	e.mustPost(t, ev, mgmt)
	s = status(ev)
	if s["status"] != "COMPLETE" || s["posted_books"].(float64) != 2 || s["outstanding_book_codes"] != nil || books(s)["MGMT"] != "POSTED" {
		t.Fatalf("%v", s)
	}
	if l := list(""); len(l["incomplete_events"].([]any)) != 0 {
		t.Fatalf("%v", l)
	}
	if c := closeEvidence(); hasIssue(c, "incomplete_events") || c["incomplete_events"].(map[string]any)["count"].(float64) != 0 {
		t.Fatalf("nothing is outstanding once both books have posted: %v", c)
	}

	// a failed attempt in one book is an exception in that book, and the event stays incomplete
	ev2 := e.registerEvent(t, "se-mb2", "120.00", "100.00", "20.00")
	e.mustPost(t, ev2, stat)
	if err := e.exec(`UPDATE chart_of_accounts SET status='INACTIVE' WHERE tenant_id = $1::uuid AND account_code = '2200'`, e.tenant); err != nil {
		t.Fatalf("deactivate the VAT account: %v", err)
	}
	if st, _ := e.post(ev2, mgmt, nil); st < 400 {
		t.Fatalf("the posting should have failed: %d", st)
	}
	s = status(ev2)
	if s["status"] != "INCOMPLETE" || books(s)["MGMT"] != "EXCEPTION" || books(s)["STAT"] != "POSTED" {
		t.Fatalf("%v", s)
	}
	for _, x := range s["books"].([]any) {
		if b := x.(map[string]any); b["book_code"] == "MGMT" && (b["last_failure"] == nil || b["last_failure"].(map[string]any)["status"] != "FAILED") {
			t.Fatalf("the failure is shown with the book: %v", b)
		}
	}

	// refusals
	if st, _ := e.call("GET", "/v1/accounting-events/00000000-0000-0000-0000-000000000000/posting-status", e.tenant, "auditor", nil); st != 404 {
		t.Fatalf("unknown event: %d", st)
	}
	if st, _ := e.call("GET", "/v1/accounting-events/incomplete", e.tenant, "auditor", nil); st != 400 {
		t.Fatalf("no entity: %d", st)
	}
	if st, _ := e.call("GET", "/v1/accounting-events/incomplete?legal_entity_id="+e.entity+"&limit=-1", e.tenant, "auditor", nil); st != 400 {
		t.Fatalf("a negative limit: %d", st)
	}
	if st, _ := e.call("GET", "/v1/accounting-events/"+ev+"/posting-status", "00000000-0000-0000-0000-00000000dead", "auditor", nil); st != 404 {
		t.Fatalf("another tenant: %d", st)
	}
	e.denied.Store("nosy|GL_POSTING_VIEW", true)
	if st, _ := e.call("GET", "/v1/accounting-events/"+ev+"/posting-status", e.tenant, "nosy", nil); st != 403 {
		t.Fatalf("without the right to see postings: %d", st)
	}
}
