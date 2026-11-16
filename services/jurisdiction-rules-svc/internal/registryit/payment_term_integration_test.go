//go:build integration

// REF-10 payment terms end to end: real Postgres (embedded), the real store and
// the real HTTP handlers. A term has immutable versions; replacing one in force is
// stated; and a due date names the term version and the calendar version that
// gave it, so it can be reproduced after either is amended.
package registryit_test

import (
	"fmt"
	"net/url"
	"strings"
	"sync"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func (c *calFx) newTerm(t *testing.T) string {
	t.Helper()
	code := "NT" + strings.ToUpper(uuid.NewString()[:6])
	st, r := c.e.do("POST", "/v1/admin/payment-terms", "steward-1", map[string]any{"term_code": code, "term_name": "Net terms " + code})
	mustStatus(t, 201, st, r)
	return code
}

// rule is a NET_DAYS version counting from the invoice date.
func (c *calFx) rule(days int, convention, from string) map[string]any {
	b := map[string]any{
		"basis_date_rule": "INVOICE_DATE", "due_method": "NET_DAYS", "due_days": days, "business_day_convention": convention,
		"rationale": "standard terms", "effective_from": from,
	}
	if convention != "NONE" {
		b["calendar_code"] = c.code
	}
	return b
}

func (c *calFx) draft(t *testing.T, term string, body map[string]any, want int) map[string]any {
	t.Helper()
	st, r := c.e.do("POST", "/v1/admin/payment-terms/"+term+"/versions", "steward-1", body)
	mustStatus(t, want, st, r)
	return r
}

func (c *calFx) tpost(t *testing.T, term string, n int, action, actor string, body any, want int) map[string]any {
	t.Helper()
	st, r := c.e.do("POST", fmt.Sprintf("/v1/admin/payment-terms/%s/versions/%d/%s", term, n, action), actor, body)
	mustStatus(t, want, st, r)
	return r
}

func (c *calFx) approve(t *testing.T, term string, n int) map[string]any {
	t.Helper()
	return c.tpost(t, term, n, "approve", "approver-1", nil, 200)
}

func (c *calFx) due(t *testing.T, term, query string, want int) map[string]any {
	t.Helper()
	st, r := c.e.do("GET", "/v1/payment-terms/"+term+"/due-date?"+query, "", nil)
	mustStatus(t, want, st, r)
	return r
}

func termOf(r map[string]any) map[string]any { m, _ := r["term"].(map[string]any); return m }

// calendarReady imports and publishes the calendar the terms are applied by.
func (c *calFx) calendarReady(t *testing.T) map[string]any {
	t.Helper()
	c.imp(t, "2026-2", gbHolidays(), 201)
	return c.publish(t, 1)
}

// ── drafting ────────────────────────────────────────────────────────────────

func TestPaymentTerm_AVersionIsDraftedWithItsFormulaAndDigest(t *testing.T) {
	c := newCalFx(t)
	c.calendarReady(t)
	term := c.newTerm(t)

	// A term registers once; the same again is a replay, a different one is a conflict.
	st, r := c.e.do("POST", "/v1/admin/payment-terms", "steward-1", map[string]any{"term_code": term, "term_name": "Net terms " + term})
	mustStatus(t, 200, st, r)
	st, r = c.e.do("POST", "/v1/admin/payment-terms", "steward-1", map[string]any{"term_code": term, "term_name": "Something else"})
	mustStatus(t, 409, st, r)
	st, r = c.e.do("POST", "/v1/admin/payment-terms", "steward-1", map[string]any{"term_code": "net30", "term_name": "x"})
	mustStatus(t, 400, st, r)
	assert.True(t, c.e.auth.pairs["payment_term.create"])

	body := c.rule(30, "MODIFIED_FOLLOWING", "2026-01-01")
	body["discount_percent"], body["discount_days"], body["grace_days"] = "2.0", 10, 5
	v := c.draft(t, term, body, 201)
	assert.EqualValues(t, 1, v["version"])
	assert.Equal(t, "DRAFT", v["status"])
	assert.Equal(t, "invoice date + 30 days, modified following business day by calendar "+c.code+"; 2% discount if paid within 10 days; 5 days grace", v["formula_text"])
	assert.Len(t, v["content_digest"], 64)
	assert.Equal(t, "2", v["discount_percent"], "the percent reads the same however it was written")
	_, approved := v["approved_by"]
	assert.False(t, approved)

	// The next version is numbered by the service.
	assert.EqualValues(t, 2, c.draft(t, term, c.rule(45, "NONE", "2026-07-01"), 201)["version"])
	got := func(path string) map[string]any {
		st, r := c.e.do("GET", path, "", nil)
		mustStatus(t, 200, st, r)
		return r
	}
	assert.Equal(t, term, got("/v1/payment-terms/" + term)["term_code"])
	assert.Len(t, got("/v1/payment-terms/" + term + "/versions")["versions"], 2)
	assert.EqualValues(t, 1, got("/v1/payment-terms/" + term + "/versions/1")["version"])
}

func TestPaymentTerm_ADraftReportsEveryProblemAndWritesNothing(t *testing.T) {
	c := newCalFx(t)
	term := c.newTerm(t)
	st, r := c.e.do("POST", "/v1/admin/payment-terms/"+term+"/versions", "steward-1", map[string]any{"due_method": "NET_WEEKS", "due_days": -1, "effective_from": "soon"})
	mustStatus(t, 400, st, r)
	assert.Equal(t, "invalid_payment_term", r["code"])
	assert.GreaterOrEqual(t, len(r["problems"].([]any)), 4, "every problem at once")

	// Beyond 90 days of credit a term must say it changes controlled credit policy.
	long := c.rule(120, "NONE", "2026-01-01")
	st, r = c.e.do("POST", "/v1/admin/payment-terms/"+term+"/versions", "steward-1", long)
	mustStatus(t, 400, st, r)
	assert.Contains(t, fmt.Sprint(r["problems"]), "controlled_credit")
	long["controlled_credit"] = true
	c.draft(t, term, long, 201)

	// A calendar that does not exist, and a term that does not.
	st, r = c.e.do("POST", "/v1/admin/payment-terms/"+term+"/versions", "steward-1", func() map[string]any {
		b := c.rule(30, "FOLLOWING", "2026-01-01")
		b["calendar_code"] = "no-such-calendar"
		return b
	}())
	mustStatus(t, 404, st, r)
	assert.Equal(t, "calendar_not_found", r["code"])
	st, r = c.e.do("POST", "/v1/admin/payment-terms/NOSUCH/versions", "steward-1", c.rule(30, "NONE", "2026-01-01"))
	mustStatus(t, 404, st, r)
	assert.Equal(t, "payment_term_not_found", r["code"])

	var n int
	require.NoError(t, pool.QueryRow(ctx, `SELECT count(*) FROM payment_term_versions v JOIN payment_terms t USING (term_id) WHERE t.term_code=$1`, term).Scan(&n))
	assert.Equal(t, 1, n, "only the good draft was written")

	st, r = c.e.do("POST", "/v1/admin/payment-terms/"+term+"/versions", "", c.rule(30, "NONE", "2026-01-01"))
	mustStatus(t, 401, st, r)
}

// ── approval ────────────────────────────────────────────────────────────────

func TestPaymentTerm_ApprovalIsByAnotherPrincipalAndAnnounced(t *testing.T) {
	c := newCalFx(t)
	c.calendarReady(t)
	term := c.newTerm(t)
	c.draft(t, term, c.rule(30, "FOLLOWING", "2026-01-01"), 201)

	r := c.tpost(t, term, 1, "approve", "steward-1", nil, 403)
	assert.Equal(t, "segregation_of_duties", r["code"], "the author cannot approve their own version")
	assert.Equal(t, 0, c.e.pub.count("payment-term.activated"))

	r = c.approve(t, term, 1)
	assert.Equal(t, "ACTIVE", r["status"])
	assert.Equal(t, "approver-1", r["approved_by"])
	assert.NotNil(t, r["approved_at"])
	assert.True(t, c.e.auth.pairs["payment_term.approve"])
	require.Equal(t, 1, c.e.pub.count("payment-term.activated"))
	ev := c.e.pub.payloadsOf("payment-term.activated")[0]
	assert.Equal(t, term, ev["term_code"])
	assert.EqualValues(t, 1, ev["term_version"])
	assert.Len(t, ev["content_digest"], 64)
	assert.Equal(t, c.code, ev["calendar_code"])

	// A repeat changes nothing and announces nothing more.
	c.approve(t, term, 1)
	assert.Equal(t, 1, c.e.pub.count("payment-term.activated"))

	// Concurrent approvals of one draft: one approves, the rest replay.
	c.draft(t, term, c.rule(45, "NONE", "2025-01-01"), 201) // version 2, before version 1: no overlap with [2026-01-01, ...)? it is open-ended, so it overlaps
	var wg sync.WaitGroup
	codes := make([]int, 6)
	start := make(chan struct{})
	term2 := c.newTerm(t)
	c.draft(t, term2, c.rule(30, "NONE", "2026-01-01"), 201)
	for i := range codes {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			codes[i], _ = c.e.do("POST", "/v1/admin/payment-terms/"+term2+"/versions/1/approve", fmt.Sprintf("approver-%d", i), nil)
		}(i)
	}
	close(start)
	wg.Wait()
	for i, code := range codes {
		assert.Equal(t, 200, code, "approval %d", i)
	}
	events := 0
	for _, p := range c.e.pub.payloadsOf("payment-term.activated") {
		if p["term_code"] == term2 {
			events++
		}
	}
	assert.Equal(t, 1, events, "exactly one approval is announced")
}

func TestPaymentTerm_ACalendarWithNoPublishedVersionCannotPutATermInForce(t *testing.T) {
	c := newCalFx(t) // the calendar exists but has no published version
	term := c.newTerm(t)
	c.draft(t, term, c.rule(30, "FOLLOWING", "2026-01-01"), 201)
	r := c.tpost(t, term, 1, "approve", "approver-1", nil, 409)
	assert.Equal(t, "calendar_not_ready", r["code"])
	c.calendarReady(t)
	c.approve(t, term, 1)
}

// ── supersession ────────────────────────────────────────────────────────────

func TestPaymentTerm_ReplacingATermInForceIsStated(t *testing.T) {
	c := newCalFx(t)
	term := c.newTerm(t)
	c.draft(t, term, c.rule(30, "NONE", "2026-01-01"), 201)
	c.approve(t, term, 1)
	c.draft(t, term, c.rule(45, "NONE", "2026-07-01"), 201)

	// Approving over a version in force is refused: it must be superseded.
	r := c.tpost(t, term, 2, "approve", "approver-1", nil, 409)
	assert.Equal(t, "supersession_required", r["code"])

	// The author cannot supersede. The version named must be the active one.
	assert.Equal(t, "segregation_of_duties", c.tpost(t, term, 2, "supersede", "steward-1", map[string]any{"supersedes_version": 1}, 403)["code"])
	assert.Equal(t, "invalid_supersession", c.tpost(t, term, 2, "supersede", "approver-1", map[string]any{"supersedes_version": 2}, 409)["code"], "not itself")
	c.tpost(t, term, 2, "supersede", "approver-1", map[string]any{"supersedes_version": 9}, 404)
	c.tpost(t, term, 2, "supersede", "approver-1", map[string]any{}, 400)
	// A version that would start before the one it replaces cannot take over from it.
	c.draft(t, term, c.rule(60, "NONE", "2025-06-01"), 201)
	assert.Equal(t, "invalid_supersession", c.tpost(t, term, 3, "supersede", "approver-1", map[string]any{"supersedes_version": 1}, 409)["code"])

	r = c.tpost(t, term, 2, "supersede", "approver-1", map[string]any{"supersedes_version": 1}, 200)
	next, old := r["version"].(map[string]any), r["superseded"].(map[string]any)
	assert.Equal(t, "ACTIVE", next["status"])
	assert.Equal(t, "SUPERSEDED", old["status"])
	assert.Equal(t, "2026-07-01", old["ends_on"], "the replaced version stops governing the day the new one starts")
	assert.Equal(t, next["term_version_id"], old["superseded_by_version_id"])
	assert.True(t, c.e.auth.pairs["payment_term.supersede"])

	assert.Equal(t, 1, c.e.pub.count("payment-term.superseded"))
	assert.Equal(t, 2, c.e.pub.count("payment-term.activated"))
	sup := c.e.pub.payloadsOf("payment-term.superseded")[0]
	assert.EqualValues(t, 1, sup["term_version"])
	assert.EqualValues(t, 2, sup["superseded_by_version"])
	assert.Equal(t, "2026-07-01", sup["ends_on"])

	// Repeating it is a replay: nothing more is announced.
	c.tpost(t, term, 2, "supersede", "approver-1", map[string]any{"supersedes_version": 1}, 200)
	assert.Equal(t, 1, c.e.pub.count("payment-term.superseded"))
	// The replaced version is over and cannot be superseded or approved again.
	assert.Equal(t, "payment_term_not_draft", c.tpost(t, term, 1, "approve", "approver-2", nil, 409)["code"])
}

func TestPaymentTerm_TwoDraftsCannotBothSupersedeOneVersion(t *testing.T) {
	c := newCalFx(t)
	term := c.newTerm(t)
	c.draft(t, term, c.rule(30, "NONE", "2026-01-01"), 201)
	c.approve(t, term, 1)
	c.draft(t, term, c.rule(45, "NONE", "2026-07-01"), 201)
	c.draft(t, term, c.rule(60, "NONE", "2026-09-01"), 201)

	var wg sync.WaitGroup
	codes := make([]int, 2)
	start := make(chan struct{})
	for i, n := range []int{2, 3} {
		wg.Add(1)
		go func(i, n int) {
			defer wg.Done()
			<-start
			codes[i], _ = c.e.do("POST", fmt.Sprintf("/v1/admin/payment-terms/%s/versions/%d/supersede", term, n), "approver-1", map[string]any{"supersedes_version": 1})
		}(i, n)
	}
	close(start)
	wg.Wait()
	ok, refused := 0, 0
	for _, code := range codes {
		switch code {
		case 200:
			ok++
		case 409:
			refused++
		default:
			t.Fatalf("unexpected status %d", code)
		}
	}
	assert.Equal(t, 1, ok, "exactly one draft supersedes the version")
	assert.Equal(t, 1, refused)
	var active int
	require.NoError(t, pool.QueryRow(ctx, `SELECT count(*) FROM payment_term_versions v JOIN payment_terms t USING (term_id) WHERE t.term_code=$1 AND v.status='ACTIVE'`, term).Scan(&active))
	assert.Equal(t, 1, active)
}

// ── retirement ──────────────────────────────────────────────────────────────

func TestPaymentTerm_RetiringWithdrawsADraftOrEndsAnActiveVersion(t *testing.T) {
	c := newCalFx(t)
	term := c.newTerm(t)
	c.draft(t, term, c.rule(30, "NONE", "2026-01-01"), 201)
	c.draft(t, term, c.rule(45, "NONE", "2026-02-01"), 201)

	// A reason is required.
	c.tpost(t, term, 2, "retire", "approver-1", map[string]any{}, 400)
	r := c.tpost(t, term, 2, "retire", "approver-1", map[string]any{"reason": "drafted in error"}, 200)
	assert.Equal(t, "RETIRED", r["status"])
	assert.Equal(t, "drafted in error", r["retire_reason"])
	assert.Equal(t, "approver-1", r["retired_by"])
	assert.True(t, c.e.auth.pairs["payment_term.retire"])
	assert.Equal(t, 1, c.e.pub.count("payment-term.retired"))
	c.tpost(t, term, 2, "retire", "approver-1", map[string]any{"reason": "again"}, 200)
	assert.Equal(t, 1, c.e.pub.count("payment-term.retired"), "a repeat announces nothing")

	// An active version stops governing on the day given; dates before it keep the rule.
	c.approve(t, term, 1)
	assert.Equal(t, "payment_term_not_draft", c.tpost(t, term, 2, "approve", "approver-1", nil, 409)["code"], "a retired draft cannot be approved")
	assert.Equal(t, "invalid_payment_term", c.tpost(t, term, 1, "retire", "approver-1", map[string]any{"reason": "x", "ends_on": "2026-01-01"}, 400)["code"])
	r = c.tpost(t, term, 1, "retire", "approver-1", map[string]any{"reason": "withdrawn by the board", "ends_on": "2026-06-01"}, 200)
	assert.Equal(t, "RETIRED", r["status"])
	assert.Equal(t, "2026-06-01", r["ends_on"])
	assert.Equal(t, "approver-1", r["approved_by"], "the approval is kept")

	c.due(t, term, "invoice_date=2026-05-01", 200)
	assert.Equal(t, "no_payment_term_version", c.due(t, term, "invoice_date=2026-06-01", 404)["code"])

	// A superseded version is already over.
	t2 := c.newTerm(t)
	c.draft(t, t2, c.rule(30, "NONE", "2026-01-01"), 201)
	c.approve(t, t2, 1)
	c.draft(t, t2, c.rule(45, "NONE", "2026-07-01"), 201)
	c.tpost(t, t2, 2, "supersede", "approver-1", map[string]any{"supersedes_version": 1}, 200)
	assert.Equal(t, "payment_term_not_active", c.tpost(t, t2, 1, "retire", "approver-1", map[string]any{"reason": "x"}, 409)["code"])
}

// ── CalculateDueDate and ExplainDueDate ─────────────────────────────────────

func TestPaymentTerm_DueDateNamesTheTermCalendarBasisAndConvention(t *testing.T) {
	c := newCalFx(t)
	cal := c.calendarReady(t)
	term := c.newTerm(t)
	v1 := c.draft(t, term, c.rule(30, "FOLLOWING", "2026-01-01"), 201)
	c.approve(t, term, 1)

	// 25 Nov + 30 = Christmas Day; then the weekend; then Boxing Day observed on the Monday.
	r := c.due(t, term, "invoice_date=2026-11-25", 200)
	assert.Equal(t, "2026-12-25", r["unadjusted_due_date"])
	assert.Equal(t, "2026-12-29", r["due_date"])
	assert.Equal(t, "INVOICE_DATE", r["basis_date_rule"])
	assert.Equal(t, "2026-11-25", r["basis_date"])
	assert.Equal(t, "FOLLOWING", r["adjustment_convention"])
	assert.Equal(t, true, r["adjusted"])
	assert.Len(t, r["skipped"], 4)
	tm := termOf(r)
	assert.Equal(t, term, tm["term_code"])
	assert.EqualValues(t, 1, tm["term_version"])
	assert.Equal(t, v1["term_version_id"], tm["term_version_id"])
	assert.Equal(t, v1["content_digest"], tm["content_digest"])
	assert.Equal(t, v1["formula_text"], tm["formula_text"])
	assert.Equal(t, false, tm["pinned"])
	cv := calendarOf(r)
	assert.EqualValues(t, 1, cv["calendar_version"])
	assert.Equal(t, cal["calendar_version_id"], cv["calendar_version_id"])
	assert.Equal(t, cal["content_digest"], cv["content_digest"])
	_, hasSteps := r["steps"]
	assert.False(t, hasSteps, "a calculation carries no explanation")

	// A business day is untouched.
	r = c.due(t, term, "invoice_date=2026-10-05", 200)
	assert.Equal(t, "2026-11-04", r["due_date"])
	assert.Equal(t, false, r["adjusted"])

	// The explanation is the calculation.
	st, ex := c.e.do("GET", "/v1/payment-terms/"+term+"/explain-due-date?invoice_date=2026-11-25", "", nil)
	mustStatus(t, 200, st, ex)
	assert.Equal(t, "2026-12-29", ex["due_date"])
	steps := fmt.Sprint(ex["steps"])
	for _, want := range []string{"basis date (INVOICE_DATE) = 2026-11-25", "2026-12-25 is not a business day (holiday)", "FOLLOWING convention: 2026-12-25 -> 2026-12-29", "due 2026-12-29"} {
		assert.Contains(t, steps, want)
	}

	// The same question always has the same answer.
	assert.Equal(t, c.due(t, term, "invoice_date=2026-11-25", 200), c.due(t, term, "invoice_date=2026-11-25", 200))
}

func TestPaymentTerm_EveryMethodGivesItsGoldenDueDate(t *testing.T) {
	c := newCalFx(t)
	c.calendarReady(t)
	for _, tc := range []struct {
		name     string
		body     map[string]any
		query    string
		unadj    string
		due      string
		grace    string
		discount string
	}{
		{"end of month plus 15", map[string]any{"due_method": "END_OF_MONTH_PLUS", "due_days": 15, "business_day_convention": "FOLLOWING", "calendar_code": c.code}, "invoice_date=2026-01-20", "2026-02-15", "2026-02-16", "", ""},
		{"end of month, February", map[string]any{"due_method": "END_OF_MONTH_PLUS", "due_days": 0, "business_day_convention": "NONE"}, "invoice_date=2026-02-10", "2026-02-28", "2026-02-28", "", ""},
		{"the 31st of next month", map[string]any{"due_method": "DAY_OF_NEXT_MONTH", "due_day_of_month": 31, "due_days": 0, "business_day_convention": "NONE"}, "invoice_date=2026-01-15", "2026-02-28", "2026-02-28", "", ""},
		{"the 10th of next month over the year end", map[string]any{"due_method": "DAY_OF_NEXT_MONTH", "due_day_of_month": 10, "due_days": 0, "business_day_convention": "FOLLOWING", "calendar_code": c.code}, "invoice_date=2026-12-20", "2027-01-10", "2027-01-11", "", ""},
		{"2/10 net 30 with grace", map[string]any{"due_days": 30, "business_day_convention": "FOLLOWING", "calendar_code": c.code, "discount_percent": "2", "discount_days": 10, "grace_days": 5}, "invoice_date=2026-10-01", "2026-10-31", "2026-11-02", "2026-11-07", "2026-10-12"},
	} {
		term := c.newTerm(t)
		body := c.rule(30, "NONE", "2026-01-01")
		for k, v := range tc.body {
			body[k] = v
		}
		if tc.body["business_day_convention"] == "NONE" {
			delete(body, "calendar_code")
		}
		c.draft(t, term, body, 201)
		c.approve(t, term, 1)
		r := c.due(t, term, tc.query, 200)
		assert.Equal(t, tc.unadj, r["unadjusted_due_date"], tc.name)
		assert.Equal(t, tc.due, r["due_date"], tc.name)
		if tc.grace != "" {
			assert.Equal(t, tc.grace, r["grace_until"], tc.name)
		}
		if tc.discount != "" {
			assert.Equal(t, tc.discount, r["discount"].(map[string]any)["until"], tc.name)
		}
	}
}

func TestREF10NP41_PaymentTerm_DueDateInputRefusals(t *testing.T) {
	c := newCalFx(t)
	term := c.newTerm(t)
	body := c.rule(30, "NONE", "2026-01-01")
	body["basis_date_rule"] = "RECEIPT_DATE"
	c.draft(t, term, body, 201)
	c.approve(t, term, 1)

	r := c.due(t, term, "invoice_date=2026-10-01", 400)
	assert.Equal(t, "basis_date_required", r["code"], "a term that counts from the receipt date needs it")
	assert.Contains(t, fmt.Sprint(r["detail"]), "receipt_date")
	r = c.due(t, term, "invoice_date=2026-10-01&receipt_date=2026-10-10", 200)
	assert.Equal(t, "2026-10-10", r["basis_date"])
	assert.Equal(t, "2026-11-09", r["due_date"])
	assert.Equal(t, "basis_date_required", c.due(t, term, "invoice_date=2026-10-01&delivery_date=2026-10-10", 400)["code"], "a delivery date is not a receipt date")

	for name, q := range map[string]string{
		"no invoice date":    "receipt_date=2026-10-10",
		"a bad invoice":      "invoice_date=tomorrow",
		"a bad receipt":      "invoice_date=2026-10-01&receipt_date=x",
		"a bad known_at":     "invoice_date=2026-10-01&known_at=yesterday",
		"a bad term pin":     "invoice_date=2026-10-01&term_version=0",
		"a bad calendar pin": "invoice_date=2026-10-01&calendar_version=x",
	} {
		st, r := c.e.do("GET", "/v1/payment-terms/"+term+"/due-date?"+q, "", nil)
		assert.Equal(t, 400, st, "%s: %v", name, r)
	}
	st, r := c.e.do("GET", "/v1/payment-terms/NOSUCH/due-date?invoice_date=2026-10-01", "", nil)
	assert.Equal(t, 404, st)
	assert.Equal(t, "payment_term_not_found", r["code"])
	assert.Equal(t, "no_payment_term_version", c.due(t, term, "invoice_date=2025-12-31&receipt_date=2025-12-31", 404)["code"], "before the version took effect")

	// A term applied by a calendar that no longer answers for the date is refused, not guessed.
	t2 := c.newTerm(t)
	c.calendarReady(t)
	c.draft(t, t2, c.rule(30, "FOLLOWING", "2025-01-01"), 201)
	c.approve(t, t2, 1)
	assert.Equal(t, "no_calendar_version", c.due(t, t2, "invoice_date=2025-12-01", 404)["code"], "the calendar's first version starts 2026-01-01")
}

// ── history ─────────────────────────────────────────────────────────────────

// A due date worked out under one term is never changed by a later term: ask as it
// was known then, or name the version.
func TestPaymentTerm_ASupersessionDoesNotRewriteHistory(t *testing.T) {
	c := newCalFx(t)
	term := c.newTerm(t)
	v1 := c.draft(t, term, c.rule(30, "NONE", "2026-01-01"), 201)
	c.approve(t, term, 1)
	assert.Equal(t, "2026-08-01", c.due(t, term, "invoice_date=2026-07-02", 200)["due_date"])
	v1Row := c.getVersion(t, term, 1)

	c.draft(t, term, c.rule(45, "NONE", "2026-07-01"), 201)
	c.tpost(t, term, 2, "supersede", "approver-1", map[string]any{"supersedes_version": 1}, 200)

	// Before the new version starts, the old rule; from it, the new one.
	assert.Equal(t, "2026-05-31", c.due(t, term, "invoice_date=2026-05-01", 200)["due_date"])
	now := c.due(t, term, "invoice_date=2026-07-02", 200)
	assert.Equal(t, "2026-08-16", now["due_date"])
	assert.EqualValues(t, 2, termOf(now)["term_version"])

	// As it was known before the new version was approved, the old rule still governed that date.
	v1Approved := url.QueryEscape(v1Row["approved_at"].(string))
	past := c.due(t, term, "invoice_date=2026-07-02&known_at="+v1Approved, 200)
	assert.Equal(t, "2026-08-01", past["due_date"], "the answer given then is reproducible by knowledge time")
	assert.EqualValues(t, 1, termOf(past)["term_version"])
	assert.Equal(t, v1["content_digest"], termOf(past)["content_digest"])

	// Or by naming the version the first answer named.
	pinned := c.due(t, term, "invoice_date=2026-07-02&term_version=1", 200)
	assert.Equal(t, "2026-08-01", pinned["due_date"])
	assert.Equal(t, true, termOf(pinned)["pinned"])
	// A pin cannot reach back before the version was approved, or to a draft.
	early := url.QueryEscape("2020-01-01T00:00:00Z")
	assert.Equal(t, "no_payment_term_version", c.due(t, term, "invoice_date=2026-07-02&term_version=1&known_at="+early, 404)["code"])
	c.draft(t, term, c.rule(60, "NONE", "2026-10-01"), 201)
	assert.Equal(t, "no_payment_term_version", c.due(t, term, "invoice_date=2026-10-02&term_version=3", 404)["code"])
	assert.Equal(t, "2026-11-16", c.due(t, term, "invoice_date=2026-10-02", 200)["due_date"], "a draft never takes over")
}

func (c *calFx) getVersion(t *testing.T, term string, n int) map[string]any {
	t.Helper()
	st, r := c.e.do("GET", fmt.Sprintf("/v1/payment-terms/%s/versions/%d", term, n), "", nil)
	mustStatus(t, 200, st, r)
	return r
}

// Retiring a version later does not change a due date asked as it was known before.
func TestPaymentTerm_ARetirementDoesNotRewriteHistory(t *testing.T) {
	c := newCalFx(t)
	term := c.newTerm(t)
	c.draft(t, term, c.rule(30, "NONE", "2026-01-01"), 201)
	v1 := c.approve(t, term, 1)
	c.tpost(t, term, 1, "retire", "approver-1", map[string]any{"reason": "withdrawn", "ends_on": "2026-06-01"}, 200)

	assert.Equal(t, "no_payment_term_version", c.due(t, term, "invoice_date=2026-07-02", 404)["code"])
	at := url.QueryEscape(v1["approved_at"].(string))
	past := c.due(t, term, "invoice_date=2026-07-02&known_at="+at, 200)
	assert.Equal(t, "2026-08-01", past["due_date"], "as known before the retirement, the term was still in force")
}

// A calendar amended later does not change a due date: ask as it was known then,
// or name the calendar version the first answer named.
func TestREF10NP40_PaymentTerm_ACalendarAmendmentDoesNotRewriteHistory(t *testing.T) {
	c := newCalFx(t)
	cal1 := c.calendarReady(t)
	term := c.newTerm(t)
	c.draft(t, term, c.rule(30, "FOLLOWING", "2026-01-01"), 201)
	c.approve(t, term, 1)
	first := c.due(t, term, "invoice_date=2026-11-25", 200)
	assert.Equal(t, "2026-12-29", first["due_date"])

	// The authority corrects the year: Christmas and Boxing Day are no longer observed.
	c.imp(t, "2026-3", []map[string]any{hol("2026-08-31", "Summer bank holiday", "")}, 201)
	c.publish(t, 2)
	now := c.due(t, term, "invoice_date=2026-11-25", 200)
	assert.Equal(t, "2026-12-25", now["due_date"], "asked now, the corrected calendar answers")
	assert.EqualValues(t, 2, calendarOf(now)["calendar_version"])

	// Known when the term was approved, before the correction was published.
	approvedAt := c.getVersion(t, term, 1)["approved_at"].(string)
	past := c.due(t, term, "invoice_date=2026-11-25&known_at="+url.QueryEscape(approvedAt), 200)
	assert.Equal(t, "2026-12-29", past["due_date"])
	assert.EqualValues(t, 1, calendarOf(past)["calendar_version"])

	pinned := c.due(t, term, "invoice_date=2026-11-25&calendar_version=1", 200)
	assert.Equal(t, "2026-12-29", pinned["due_date"])
	assert.Equal(t, true, calendarOf(pinned)["pinned"])
	assert.Equal(t, cal1["content_digest"], calendarOf(pinned)["content_digest"])
	assert.Equal(t, first["due_date"], pinned["due_date"])
}

// ── the database holds the line too ─────────────────────────────────────────

func (c *calFx) termID(t *testing.T, term string) string {
	t.Helper()
	var id string
	require.NoError(t, pool.QueryRow(ctx, `SELECT term_id::text FROM payment_terms WHERE term_code=$1`, term).Scan(&id))
	return id
}

// insertVersion inserts a version row directly, with overrides to the defaults.
func insertVersion(termID string, version int, over map[string]any) error {
	cols := map[string]any{
		"term_id": termID, "version": version, "status": "DRAFT", "basis_date_rule": "INVOICE_DATE", "due_method": "NET_DAYS", "due_days": 30,
		"business_day_convention": "NONE", "rationale": "r", "formula_text": "f", "content_digest": strings.Repeat("a", 64),
		"effective_from": "2026-01-01", "created_by": "author",
	}
	for k, v := range over {
		cols[k] = v
	}
	var names, holders []string
	var args []any
	for k, v := range cols {
		names = append(names, k)
		args = append(args, v)
		holders = append(holders, fmt.Sprintf("$%d", len(args)))
	}
	_, err := pool.Exec(ctx, `INSERT INTO payment_term_versions (`+strings.Join(names, ",")+`) VALUES (`+strings.Join(holders, ",")+`)`, args...)
	return err
}

func TestPaymentTerm_TheDatabaseKeepsTheRuleTheLifecycleAndTheOneInForce(t *testing.T) {
	c := newCalFx(t)
	term := c.newTerm(t)
	c.draft(t, term, c.rule(30, "NONE", "2026-01-01"), 201)
	c.approve(t, term, 1)
	c.draft(t, term, c.rule(45, "NONE", "2026-07-01"), 201)
	c.tpost(t, term, 2, "supersede", "approver-1", map[string]any{"supersedes_version": 1}, 200)
	c.draft(t, term, c.rule(60, "NONE", "2027-01-01"), 201) // version 3, a draft
	id := c.termID(t, term)

	for name, q := range map[string]string{
		"rewriting an approved version's days":   `UPDATE payment_term_versions SET due_days = 99 WHERE term_id = $1 AND version = 2`,
		"rewriting a draft's days":               `UPDATE payment_term_versions SET due_days = 99 WHERE term_id = $1 AND version = 3`,
		"rewriting a rationale":                  `UPDATE payment_term_versions SET rationale = 'x' WHERE term_id = $1 AND version = 3`,
		"rewriting the digest":                   `UPDATE payment_term_versions SET content_digest = repeat('b', 64) WHERE term_id = $1 AND version = 3`,
		"rewriting the start":                    `UPDATE payment_term_versions SET effective_from = '2026-02-01' WHERE term_id = $1 AND version = 3`,
		"deleting a version":                     `DELETE FROM payment_term_versions WHERE term_id = $1 AND version = 3`,
		"truncating versions":                    `TRUNCATE payment_term_versions CASCADE`,
		"un-superseding":                         `UPDATE payment_term_versions SET status = 'DRAFT' WHERE term_id = $1 AND version = 1`,
		"reactivating a superseded version":      `UPDATE payment_term_versions SET status = 'ACTIVE' WHERE term_id = $1 AND version = 1`,
		"retiring a superseded version":          `UPDATE payment_term_versions SET status = 'RETIRED', retired_at = now(), retired_by = 'x', retire_reason = 'x' WHERE term_id = $1 AND version = 1`,
		"moving the end of a superseded version": `UPDATE payment_term_versions SET ends_on = '2026-12-31' WHERE term_id = $1 AND version = 1`,
		"rewriting an approval":                  `UPDATE payment_term_versions SET approved_by = 'someone-else' WHERE term_id = $1 AND version = 2`,
		"approving as the author":                `UPDATE payment_term_versions SET status = 'ACTIVE', approved_at = now(), approved_by = created_by WHERE term_id = $1 AND version = 3`,
		"activating a draft with no approval":    `UPDATE payment_term_versions SET status = 'ACTIVE' WHERE term_id = $1 AND version = 3`,
		"skipping to superseded":                 `UPDATE payment_term_versions SET status = 'SUPERSEDED', superseded_at = now() WHERE term_id = $1 AND version = 3`,
		"approving over a version in force":      `UPDATE payment_term_versions SET status = 'ACTIVE', approved_at = now(), approved_by = 'approver-9' WHERE term_id = $1 AND version = 3 AND effective_from < '2100-01-01'`,
		"renaming a term":                        `UPDATE payment_terms SET term_name = 'x' WHERE term_id = $1`,
		"deleting a term":                        `DELETE FROM payment_terms WHERE term_id = $1`,
	} {
		_, err := pool.Exec(ctx, q, id)
		// "approving over a version in force" targets version 3 which starts 2027, after version 2
		// (open-ended): the exclusion constraint refuses it.
		assert.Error(t, err, name)
	}

	// Rows that break the rules of the shape, whoever writes them.
	for name, over := range map[string]map[string]any{
		"a convention with no calendar":      {"business_day_convention": "FOLLOWING"},
		"a calendar with no convention":      {"calendar_code": c.code},
		"beyond 90 days, not controlled":     {"due_days": 120},
		"a negative number of days":          {"due_days": -1},
		"an unknown method":                  {"due_method": "NET_WEEKS"},
		"a day of month on net days":         {"due_day_of_month": 10},
		"day of next month with days":        {"due_method": "DAY_OF_NEXT_MONTH", "due_day_of_month": 10, "due_days": 5},
		"day of next month with no day":      {"due_method": "DAY_OF_NEXT_MONTH", "due_days": 0},
		"half a discount":                    {"discount_percent": 2},
		"a discount over 100":                {"discount_percent": 101, "discount_days": 5},
		"too much grace":                     {"grace_days": 91},
		"no rationale":                       {"rationale": " "},
		"a digest that is no digest":         {"content_digest": "nope"},
		"an approved draft":                  {"approved_by": "x", "approved_at": "2026-01-01T00:00:00Z"},
		"an active version with no approval": {"status": "ACTIVE"},
		"an end before the start":            {"ends_on": "2025-01-01"},
		"an unknown status":                  {"status": "PENDING"},
		"a version number twice":             {},
	} {
		v := 50
		if name == "a version number twice" {
			v = 3
		}
		assert.Error(t, insertVersion(id, v, over), name)
	}

	// Two approved versions of one term cannot govern the same date, however they got there.
	err := insertVersion(id, 60, map[string]any{"status": "ACTIVE", "approved_at": "2026-01-01T00:00:00Z", "approved_by": "approver", "effective_from": "2026-09-01"})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "excl_ptv_one_in_force")
	// An unapproved one does not take part: drafts may overlap.
	assert.NoError(t, insertVersion(id, 61, map[string]any{"effective_from": "2026-09-01"}))
}

func TestMigration000031_DownIsRefusedWhileTermsExist(t *testing.T) {
	c := newCalFx(t)
	c.newTerm(t)
	_, err := pool.Exec(ctx, readMigration(t, "000031_payment_terms.down.sql"))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "000031 down refused")
	_, err = pool.Exec(ctx, readMigration(t, "000031_payment_terms.up.sql"))
	require.NoError(t, err, "the up migration is re-runnable")
}
