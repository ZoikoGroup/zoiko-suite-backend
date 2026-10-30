package store_test

import "testing"

// Historical-rate translation of a member's equity against real Postgres as the ordinary role.

func TestEquityAtTheHistoricalRateIsResolvedRecordedAndNeverSubstituted(t *testing.T) {
	c := capitalSetupWith(t, map[string]any{"equity_rate_date": "2025-06-01"})
	c.seal(t, c.uk, "2026-03")
	c.seal(t, c.us, "2026-03")

	// the only USD rates are the March closing and average: the acquisition-date rate is missing, so the run stops and posts nothing
	st, b := c.run("2026-03")
	if st != 422 || b["code"] != "CONSOLIDATION_RATE_UNAVAILABLE" {
		t.Fatalf("a missing historical rate is never replaced by the closing rate: %d %v", st, b)
	}
	if len(c.groupBook(t)) != 0 {
		t.Fatal("nothing posted")
	}

	// a spot rate three days off the date is within the spot policy; the run uses it, and records which rate and date
	if st, r := c.rate("USD", "GBP", "0.75", "SPOT", "2025-05-30T12:00:00Z", "ecb", "s-may25"); st != 201 {
		t.Fatalf("%d %v", st, r)
	}
	run := c.mustRun(t, "2026-03")
	pop := run["population"].(map[string]any)
	if run["blocking_count"].(float64) != 0 {
		t.Fatalf("%v", pop["exceptions"])
	}
	var eqRate map[string]any
	for _, r := range pop["rates"].([]any) {
		if m := r.(map[string]any); m["class"] == "EQ" {
			eqRate = m
		}
	}
	if eqRate == nil || eqRate["rate"] != "0.75" || eqRate["policy_id"] != "SPOT_ON_DATE" {
		t.Fatalf("the rate used is part of the record: %v", pop["rates"])
	}
	// 5000 USD of share capital arose at 0.75 = 3750; the UK paid 3200 for 80%: the minority is 750 and the difference is goodwill
	ce := pop["capital_eliminations"].([]any)[0].(map[string]any)
	if ce["eliminated_equity"] != "3750.00" || ce["non_controlling_interest"] != "750.00" || ce["goodwill"] != "200.00" || ce["investment"] != "3200.00" {
		t.Fatalf("%v", ce)
	}
	var eqLine map[string]any
	for _, l := range pop["lines"].([]any) {
		if m := l.(map[string]any); m["local_account"] == "3000" && m["legal_entity_id"] == c.us {
			eqLine = m
		}
	}
	if eqLine == nil || eqLine["rate_class"] != "EQ" || eqLine["translated_net"] != "-3750.00" {
		t.Fatalf("%v", eqLine)
	}
	// certifying still works: the group book agrees with the run, historical equity and all
	if st, cert := c.call("POST", "/v1/consolidation-runs/"+run["consolidation_run_id"].(string)+"/certify", c.tenant, "group-controller", nil); st != 200 || cert["status"] != "CERTIFIED" {
		t.Fatalf("%d %v", st, cert)
	}
}

func TestTheEquityRateDateIsValidatedAndImmutable(t *testing.T) {
	c := capitalSetup(t)
	path := "/v1/consolidation-groups/" + c.groupID + "/members"
	base := func() map[string]any {
		return map[string]any{"legal_entity_id": c.uk, "source_book_id": c.ukBook, "ownership_pct": "100", "effective_from": "2030-01-01"}
	}
	withCapital := func(m map[string]any) map[string]any {
		m["investment_account"], m["investor_entity_id"], m["equity_accounts"] = "1600", c.us, []string{"3000"}
		return m
	}
	noCapital := base()
	noCapital["equity_rate_date"] = "2029-12-31"
	after := withCapital(base())
	after["equity_rate_date"] = "2030-02-01"
	garbage := withCapital(base())
	garbage["equity_rate_date"] = "yesterday"
	for what, body := range map[string]map[string]any{
		"a rate date without capital terms":  noCapital,
		"a rate date after the member joins": after,
		"a rate date that is not a date":     garbage,
	} {
		if st, b := c.call("POST", path, c.tenant, "group-finance", body); st != 422 {
			t.Fatalf("%s: %d %v", what, st, b)
		}
	}
	wantRefused(t, "SQL: a rate date on a membership with no equity accounts",
		c.asApp(c.tenant, `UPDATE consolidation_members SET equity_accounts = '[]'::jsonb WHERE legal_entity_id = $1::uuid AND effective_to IS NULL`, c.us), "")
	wantRefused(t, "edit the equity rate date of a membership",
		c.asApp(c.tenant, `UPDATE consolidation_members SET equity_rate_date = '2020-01-01' WHERE legal_entity_id = $1::uuid AND effective_to IS NULL`, c.us), "")
}
