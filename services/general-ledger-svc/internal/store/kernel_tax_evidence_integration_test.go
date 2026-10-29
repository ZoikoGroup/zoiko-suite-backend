package store_test

import (
	"strings"
	"testing"
)

// taxRule is the invoice rule whose VAT line links to the tax determination the event carries.
func taxRule(version string) map[string]any {
	r := invoiceRule(version)
	content := r["content"].(map[string]any)
	content["line_templates"].([]any)[2].(map[string]any)["tax_detail"] = "$facts.tax_determination_id"
	content["line_templates"].([]any)[1].(map[string]any)["tax_detail"] = "TAXCODE-STD"
	r["test_pack"].([]any)[0].(map[string]any)["event"].(map[string]any)["facts"].(map[string]any)["tax_determination_id"] = "td-test"
	return r
}

func (e *kenv) activate(t *testing.T, body map[string]any, book string) {
	t.Helper()
	st, rs := e.call("POST", "/v1/posting-rule-sets", e.tenant, "builder", body)
	if st != 201 {
		t.Fatalf("%d %v", st, rs)
	}
	id := rs["posting_rule_set_id"].(string)
	if st, b := e.call("POST", "/v1/posting-rule-sets/"+id+"/test", e.tenant, "builder", map[string]any{"book_id": book}); st != 200 {
		t.Fatalf("test %d %v", st, b)
	}
	if st, b := e.call("POST", "/v1/posting-rule-sets/"+id+"/approve", e.tenant, "approver", map[string]any{}); st != 200 {
		t.Fatalf("approve %d %v", st, b)
	}
	if st, b := e.call("POST", "/v1/posting-rule-sets/"+id+"/activate", e.tenant, "releaser", map[string]any{}); st != 200 {
		t.Fatalf("activate %d %v", st, b)
	}
}

func (e *kenv) taxEvent(t *testing.T, src string, extraFacts map[string]any, evidence ...string) string {
	t.Helper()
	body := eventBody(e.entity, src, "120.00", "100.00", "20.00")
	for k, v := range extraFacts {
		body["facts"].(map[string]any)[k] = v
	}
	if len(evidence) > 0 {
		body["evidence_refs"] = evidence
	}
	st, ev := e.call("POST", "/v1/accounting-events", e.tenant, "ar-svc", body)
	if st != 201 {
		t.Fatalf("register %s: %d %v", src, st, ev)
	}
	return ev["accounting_event_id"].(string)
}

// §8.2 / §8.1: a line links to the tax determination it rests on, and a header to the evidence chain behind it.
func TestJournalLinesLinkTheirTaxDeterminationAndHeadersTheirEvidenceBundle(t *testing.T) {
	e := newKEnv(t)
	e.seedChart(t)
	book := e.activeBook(t, "STAT")
	e.activate(t, taxRule("1.0.0"), book)

	// the determination the rule links to must be on the event: without it nothing is posted
	noTax := e.taxEvent(t, "se-t0", nil)
	if st, b := e.post(noTax, book, nil); st != 422 || !strings.Contains(strings.ToLower(b["error"].(string)), "tax_detail_missing") {
		t.Fatalf("a missing determination: %d %v", st, b)
	}
	if n := e.count(t, `SELECT count(*) FROM journal_headers WHERE source_accounting_event_id=$1::uuid`, noTax); n != 0 {
		t.Fatalf("a refused posting leaves no journal, found %d", n)
	}

	ev := e.taxEvent(t, "se-t1", map[string]any{"tax_determination_id": "TD-100"}, "doc-2", "doc-1")
	res := e.mustPost(t, ev, book)
	jid := res["journal_id"].(string)
	line := func(code string) string {
		return e.scalar(t, `SELECT tax_detail_ref FROM journal_lines WHERE journal_id=$1::uuid AND account_code=$2`, jid, code)
	}
	if line("2200") != "TD-100" || line("4000") != "TAXCODE-STD" || line("1100") != "" {
		t.Fatalf("tax links: VAT=%q REV=%q AR=%q", line("2200"), line("4000"), line("1100"))
	}
	if e.count(t, `SELECT count(*) FROM journal_lines WHERE journal_id=$1::uuid AND account_code='1100' AND tax_detail_ref IS NULL`, jid) != 1 {
		t.Fatal("a line with no determination stores none, not an empty string")
	}

	// the header's bundle follows the event's references, whatever order they came in
	bundle := e.scalar(t, `SELECT evidence_bundle_id FROM journal_headers WHERE journal_id=$1::uuid`, jid)
	if !strings.HasPrefix(bundle, "ebnd_") || len(bundle) != 37 {
		t.Fatalf("bundle id %q", bundle)
	}
	if want := e.scalar(t, `SELECT evidence_bundle_digest(ARRAY['doc-1','doc-2'])`); bundle != want {
		t.Fatalf("bundle %q is not the digest of its references %q", bundle, want)
	}
	if e.scalar(t, `SELECT array_to_string(evidence_refs, ',') FROM journal_headers WHERE journal_id=$1::uuid`, jid) != "doc-2,doc-1" {
		t.Fatal("the journal keeps the references the event carried")
	}

	// the link is part of what is sealed: the seal has to cover it
	if len(e.scalar(t, `SELECT journal_hash FROM journal_headers WHERE journal_id=$1::uuid`, jid)) != 64 {
		t.Fatal("sealed")
	}
	wantRefused(t, "re-point a posted line at another determination", e.asApp(e.tenant, `UPDATE journal_lines SET tax_detail_ref='TD-OTHER' WHERE journal_id=$1::uuid AND account_code='2200'`, jid), "immutable")
	wantRefused(t, "move a posted journal to another bundle", e.asApp(e.tenant, `UPDATE journal_headers SET evidence_bundle_id='ebnd_other' WHERE journal_id=$1::uuid`, jid), "immutable")

	// the posting trace shows both
	st, tr := e.call("GET", "/v1/posting-traces?journal_id="+jid, e.tenant, "auditor", nil)
	if st != 200 {
		t.Fatalf("%d %v", st, tr)
	}
	j := tr["postings"].([]any)[0].(map[string]any)["journals"].([]any)[0].(map[string]any)
	if j["evidence_bundle_id"] != bundle {
		t.Fatalf("trace bundle %v", j["evidence_bundle_id"])
	}
	seen := map[string]string{}
	for _, l := range j["lines"].([]any) {
		m := l.(map[string]any)
		ref, _ := m["tax_detail_ref"].(string)
		seen[m["account_code"].(string)] = ref
	}
	if seen["2200"] != "TD-100" || seen["4000"] != "TAXCODE-STD" || seen["1100"] != "" {
		t.Fatalf("trace tax links %v", seen)
	}

	// a reversal neutralizes the same determination and stays in the same evidence bundle
	st, rev := e.call("POST", "/v1/journals/"+jid+"/reverse", e.tenant, "reviewer", map[string]any{"reason": "wrong customer", "correlation_id": "rev-t1", "correction_kind": "ERROR_CORRECTION"})
	if st != 201 {
		t.Fatalf("%d %v", st, rev)
	}
	rid := rev["journal_id"].(string)
	if e.scalar(t, `SELECT tax_detail_ref FROM journal_lines WHERE journal_id=$1::uuid AND account_code='2200'`, rid) != "TD-100" ||
		e.scalar(t, `SELECT evidence_bundle_id FROM journal_headers WHERE journal_id=$1::uuid`, rid) != bundle {
		t.Fatal("the reversal keeps the determination and the bundle")
	}

	// an event that names its evidence bundle keeps that name
	named := e.mustPost(t, e.taxEvent(t, "se-t2", map[string]any{"tax_determination_id": "TD-200", "evidence_bundle_id": " EB-2026-9 "}, "doc-9"), book)
	if e.scalar(t, `SELECT evidence_bundle_id FROM journal_headers WHERE journal_id=$1::uuid`, named["journal_id"].(string)) != "EB-2026-9" {
		t.Fatal("an explicit bundle id is kept, trimmed")
	}

	// an event with no evidence has no bundle
	bare := e.mustPost(t, e.taxEvent(t, "se-t3", map[string]any{"tax_determination_id": "TD-300"}), book)
	if e.count(t, `SELECT count(*) FROM journal_headers WHERE journal_id=$1::uuid AND evidence_bundle_id IS NULL AND evidence_refs IS NULL`, bare["journal_id"].(string)) != 1 {
		t.Fatal("a journal with no evidence has no bundle")
	}
}

// A manual journal's bundle is the set of references it carries; it follows them while the journal is a draft.
func TestManualJournalEvidenceBundleFollowsItsReferences(t *testing.T) {
	e := newKEnv(t)
	bundle := func(id string) string {
		return e.scalar(t, `SELECT evidence_bundle_id FROM journal_headers WHERE journal_id=$1::uuid`, id)
	}
	a := e.mustCreate(t, e.manualBody("eb-1", "STANDARD", "", 50, "doc-b", "doc-a"))
	b := e.mustCreate(t, e.manualBody("eb-2", "STANDARD", "", 70, "doc-a", "doc-b"))
	none := e.mustCreate(t, e.manualBody("eb-3", "STANDARD", "", 90))
	if bundle(a) == "" || bundle(a) != bundle(b) {
		t.Fatalf("the same references in any order are the same bundle: %q %q", bundle(a), bundle(b))
	}
	if bundle(none) != "" {
		t.Fatalf("no references, no bundle: %q", bundle(none))
	}

	// a draft's references can change, and its bundle goes with them
	if err := e.exec(`UPDATE journal_headers SET evidence_refs = ARRAY['doc-c'] WHERE journal_id=$1::uuid`, a); err != nil {
		t.Fatal(err)
	}
	if got, want := bundle(a), e.scalar(t, `SELECT evidence_bundle_digest(ARRAY['doc-c'])`); got != want || got == bundle(b) {
		t.Fatalf("the bundle follows a draft's references: %q want %q", got, want)
	}
	// a bundle somebody named is not recomputed away
	if err := e.exec(`UPDATE journal_headers SET evidence_bundle_id='EB-NAMED' WHERE journal_id=$1::uuid`, a); err != nil {
		t.Fatal(err)
	}
	if err := e.exec(`UPDATE journal_headers SET evidence_refs = ARRAY['doc-d'] WHERE journal_id=$1::uuid`, a); err != nil {
		t.Fatal(err)
	}
	if bundle(a) != "EB-NAMED" {
		t.Fatalf("a named bundle stays: %q", bundle(a))
	}
}
