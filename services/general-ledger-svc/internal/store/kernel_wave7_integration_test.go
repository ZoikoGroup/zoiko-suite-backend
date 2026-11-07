package store_test

import (
	"testing"

	"github.com/google/uuid"
)

// Wave 7 (group policy adjustments and capital consolidation) against real Postgres as the ordinary role.

// capitalSetup extends the Wave 5 group: the US membership is replaced from 2025-06-01 by one that carries the capital consolidation
// (the UK holds the investment on account 1600 against the equity on 3000, 80% owned), the chart gains goodwill and non-controlling
// interest accounts, and the UK has paid 3200 for its 80% of the 4000 (translated) equity.
func capitalSetup(t *testing.T) *cenv { return capitalSetupWith(t, nil) }

// capitalSetupWith is capitalSetup with extra fields on the US membership (an equity rate date, for instance).
func capitalSetupWith(t *testing.T, extra map[string]any) *cenv {
	t.Helper()
	c := consolSetup(t)
	for _, a := range [][2]string{{"1600", "ASSET"}, {"1700", "ASSET"}, {"3950", "EQUITY"}} {
		if err := c.exec(`INSERT INTO chart_of_accounts (account_id, tenant_id, account_code, account_name, account_type, created_by_principal_id) VALUES ($1,$2,$3,$4,$5,'seed')`,
			uuid.NewString(), c.tenant, a[0], a[0]+" account", a[1]); err != nil {
			t.Fatal(err)
		}
	}
	for k, code := range map[string]string{"GOODWILL": "1700", "NCI_EQUITY": "3950"} {
		if err := c.exec(`INSERT INTO account_mappings (account_mapping_id, tenant_id, mapping_key, account_code, effective_from, created_by_principal_id) VALUES ($1,$2,$3,$4,'2026-01-01','seed')`,
			uuid.NewString(), c.tenant, k, code); err != nil {
			t.Fatal(err)
		}
	}
	member := c.scalar(t, `SELECT member_id::text FROM consolidation_members WHERE tenant_id = $1::uuid AND legal_entity_id = $2::uuid`, c.tenant, c.us)
	if st, b := c.call("POST", "/v1/consolidation-groups/"+c.groupID+"/members/"+member+"/end", c.tenant, "group-finance", map[string]any{"effective_to": "2025-06-01"}); st != 200 {
		t.Fatalf("%d %v", st, b)
	}
	body := map[string]any{"legal_entity_id": c.us, "source_book_id": c.usBook, "ownership_pct": "80",
		"effective_from": "2025-06-01", "investor_entity_id": c.uk, "investment_account": "1600", "equity_accounts": []string{"3000"}}
	for k, v := range extra {
		body[k] = v
	}
	if st, b := c.call("POST", "/v1/consolidation-groups/"+c.groupID+"/members", c.tenant, "group-finance", body); st != 201 {
		t.Fatalf("%d %v", st, b)
	}
	if st, r := c.call("PUT", "/v1/consolidation-groups/"+c.groupID+"/mappings", c.tenant, "group-finance", map[string]any{
		"account_map": identity("1000", "1500", "2500", "3000", "4000", "4500", "5000", "6000", "1600"), "ic_roles": icRoles}); st != 201 {
		t.Fatalf("%d %v", st, r)
	}
	c.seed(t, c.uk, "STAT-UK", "GBP", "2026-02-01", []string{"1600", "3200.00", "0"}, []string{"1000", "0", "3200.00"})
	return c
}

func adjBody(group, desc string, amt string) map[string]any {
	return map[string]any{"group_id": group, "fiscal_period": "2026-03", "description": desc, "policy_ref": "GP-12 harmonise depreciation", "evidence_ref": "memo-2026-03",
		"lines": []any{map[string]any{"account_code": "6000", "amount": amt}, map[string]any{"account_code": "1000", "amount": "-" + amt}}}
}

func TestCapitalConsolidationAndGroupAdjustmentsReachTheGroupBookAndCertify(t *testing.T) {
	c := capitalSetup(t)
	c.seal(t, c.uk, "2026-03")
	c.seal(t, c.us, "2026-03")

	// an adjustment must balance, say which policy and evidence it rests on, and use accounts of the chart
	call := func(method, path, who string, body any) (int, map[string]any) {
		return c.call(method, path, c.tenant, who, body)
	}
	path := "/v1/consolidation-groups/" + c.groupID + "/adjustments"
	unbalanced := adjBody(c.groupID, "d", "40.00")
	unbalanced["lines"].([]any)[1].(map[string]any)["amount"] = "-39.00"
	if st, b := call("POST", path, "group-finance", unbalanced); st != 422 || b["code"] != "ADJUSTMENT_UNBALANCED" {
		t.Fatalf("%d %v", st, b)
	}
	noEvidence := adjBody(c.groupID, "d", "40.00")
	noEvidence["evidence_ref"] = " "
	if st, b := call("POST", path, "group-finance", noEvidence); st != 422 {
		t.Fatalf("%d %v", st, b)
	}
	unknown := adjBody(c.groupID, "d", "40.00")
	unknown["lines"].([]any)[0].(map[string]any)["account_code"] = "9999"
	if st, b := call("POST", path, "group-finance", unknown); st != 422 || b["code"] != "GROUP_ACCOUNT_UNAVAILABLE" {
		t.Fatalf("%d %v", st, b)
	}
	st, adj := call("POST", path, "group-finance", adjBody(c.groupID, "harmonise depreciation", "40.00"))
	if st != 201 || adj["status"] != "PROPOSED" {
		t.Fatalf("%d %v", st, adj)
	}
	adjID := adj["adjustment_id"].(string)
	if st, b := call("POST", "/v1/consolidation-adjustments/"+adjID+"/approve", "group-finance", nil); st != 403 || b["error"] != "separation_of_duties" {
		t.Fatalf("the author cannot approve: %d %v", st, b)
	}
	st, pending := call("POST", path, "group-finance", adjBody(c.groupID, "not yet approved", "5.00"))
	if st != 201 {
		t.Fatalf("%d %v", st, pending)
	}
	if st, b := call("POST", "/v1/consolidation-adjustments/"+adjID+"/approve", "group-controller", nil); st != 200 || b["status"] != "APPROVED" || b["approved_by"] != "group-controller" {
		t.Fatalf("%d %v", st, b)
	}

	run := c.mustRun(t, "2026-03")
	id := run["consolidation_run_id"].(string)
	if run["blocking_count"].(float64) != 0 || run["adjustment_journal_number"] == "" {
		t.Fatalf("%v", run["population"].(map[string]any)["exceptions"])
	}
	pop := run["population"].(map[string]any)
	if len(pop["capital_eliminations"].([]any)) != 1 || len(pop["group_adjustments"].([]any)) != 1 || pop["group_adjustments"].([]any)[0].(map[string]any)["adjustment_id"] != adjID {
		t.Fatalf("only the approved adjustment is applied, and the run records it: %v", pop)
	}
	ce := pop["capital_eliminations"].([]any)[0].(map[string]any)
	if ce["non_controlling_interest"] != "800.00" || ce["goodwill"] != "0.00" || ce["investment"] != "3200.00" || ce["eliminated_equity"] != "4000.00" {
		t.Fatalf("%v", ce)
	}
	// cash 6720 + 4320 less the 40 adjustment; equity 10000; the minority 800; the expense the adjustment booked
	wantBook(t, c.groupBook(t), map[string]string{"1000": "11000.00", "3000": "-10000.00", "3950": "-800.00", "4000": "-234.00", "3900": "-5.00", "6990": "-1.00", "6000": "40.00"})
	if n := c.count(t, `SELECT count(*) FROM journal_headers WHERE tenant_id = $1::uuid AND legal_entity_id = $2::uuid AND journal_type = 'ADJUSTMENT' AND journal_purpose = 'GROUP_ADJUSTMENT'`, c.tenant, c.group); n != 1 {
		t.Fatalf("%d", n)
	}

	// an adjustment that is in the group figures is withdrawn only after the run is; a proposed one can go at once; withdrawn is final
	if st, b := call("POST", "/v1/consolidation-adjustments/"+adjID+"/withdraw", "group-finance", map[string]any{"reason": "x"}); st != 422 || b["code"] != "ADJUSTMENT_APPLIED" {
		t.Fatalf("%d %v", st, b)
	}
	if st, b := call("POST", "/v1/consolidation-adjustments/"+pending["adjustment_id"].(string)+"/withdraw", "group-finance", map[string]any{}); st != 422 {
		t.Fatalf("a reason is required: %d %v", st, b)
	}
	if st, b := call("POST", "/v1/consolidation-adjustments/"+pending["adjustment_id"].(string)+"/withdraw", "group-finance", map[string]any{"reason": "not needed"}); st != 200 || b["status"] != "WITHDRAWN" {
		t.Fatalf("%d %v", st, b)
	}
	if st, _ := call("POST", "/v1/consolidation-adjustments/"+pending["adjustment_id"].(string)+"/approve", "group-controller", nil); st != 409 {
		t.Fatalf("a withdrawn adjustment is final: %d", st)
	}
	if st, b := call("GET", path+"?fiscal_period=2026-03", "reader", nil); st != 200 || b["total"].(float64) != 2 {
		t.Fatalf("%d %v", st, b)
	}

	// the group book carries exactly the run, adjustments and capital consolidation included
	if st, cert := call("POST", "/v1/consolidation-runs/"+id+"/certify", "group-controller", nil); st != 200 || cert["status"] != "CERTIFIED" {
		t.Fatalf("%d %v", st, cert)
	}

	// superseding reverses all three journals; the adjustment can then be withdrawn and the period consolidated without it
	if st, b := call("POST", "/v1/consolidation-runs/"+id+"/supersede", "group-controller", map[string]any{"reason": "adjustment withdrawn"}); st != 200 {
		t.Fatalf("%d %v", st, b)
	}
	wantBook(t, c.groupBook(t), map[string]string{})
	if n := c.count(t, `SELECT count(*) FROM journal_headers WHERE tenant_id = $1::uuid AND legal_entity_id = $2::uuid AND status = 'REVERSED'`, c.tenant, c.group); n != 3 {
		t.Fatalf("translation, elimination and adjustment journals are all reversed: %d", n)
	}
	if st, b := call("POST", "/v1/consolidation-adjustments/"+adjID+"/withdraw", "group-finance", map[string]any{"reason": "superseded policy"}); st != 200 {
		t.Fatalf("%d %v", st, b)
	}
	run2 := c.mustRun(t, "2026-03")
	if run2["adjustment_journal_id"] != nil && run2["adjustment_journal_id"] != "" {
		t.Fatalf("a withdrawn adjustment is not applied: %v", run2)
	}
	wantBook(t, c.groupBook(t), map[string]string{"1000": "11040.00", "3000": "-10000.00", "3950": "-800.00", "4000": "-234.00", "3900": "-5.00", "6990": "-1.00"})
}

func TestCapitalConsolidationNeedsItsAccountsAndBlocksWhatItCannotEliminate(t *testing.T) {
	c := capitalSetup(t)
	c.seal(t, c.uk, "2026-03")
	c.seal(t, c.us, "2026-03")
	// no goodwill or non-controlling interest account: the run says so and posts nothing
	if err := c.exec(`DELETE FROM account_mappings WHERE tenant_id = $1::uuid AND mapping_key = 'NCI_EQUITY'`, c.tenant); err != nil {
		t.Fatal(err)
	}
	if st, b := c.run("2026-03"); st != 422 || b["code"] != "CONSOLIDATION_ACCOUNT_UNAVAILABLE" {
		t.Fatalf("%d %v", st, b)
	}
	if len(c.groupBook(t)) != 0 {
		t.Fatal("nothing posted")
	}
	// membership validation: the capital fields come together and name another entity
	path := "/v1/consolidation-groups/" + c.groupID + "/members"
	for what, body := range map[string]map[string]any{
		"investment account alone":  {"legal_entity_id": c.uk, "source_book_id": c.ukBook, "ownership_pct": "100", "effective_from": "2030-01-01", "investment_account": "1600"},
		"investor is the entity":    {"legal_entity_id": c.uk, "source_book_id": c.ukBook, "ownership_pct": "100", "effective_from": "2030-01-01", "investment_account": "1600", "investor_entity_id": c.uk, "equity_accounts": []string{"3000"}},
		"investor is not an entity": {"legal_entity_id": c.uk, "source_book_id": c.ukBook, "ownership_pct": "100", "effective_from": "2030-01-01", "investment_account": "1600", "investor_entity_id": "x", "equity_accounts": []string{"3000"}},
	} {
		if st, b := c.call("POST", path, c.tenant, "group-finance", body); st != 422 {
			t.Fatalf("%s: %d %v", what, st, b)
		}
	}
	// records are immutable and tenant isolated
	adjSt, adj := c.call("POST", "/v1/consolidation-groups/"+c.groupID+"/adjustments", c.tenant, "group-finance", adjBody(c.groupID, "d", "1.00"))
	if adjSt != 201 {
		t.Fatalf("%d %v", adjSt, adj)
	}
	id := adj["adjustment_id"].(string)
	wantRefused(t, "rewrite the lines", c.asApp(c.tenant, `UPDATE consolidation_adjustments SET lines = '[]'::jsonb WHERE adjustment_id = $1::uuid`, id), "")
	wantRefused(t, "delete an adjustment", c.asApp(c.tenant, `DELETE FROM consolidation_adjustments WHERE adjustment_id = $1::uuid`, id), "")
	wantRefused(t, "approve as the author", c.asApp(c.tenant, `UPDATE consolidation_adjustments SET status = 'APPROVED', approved_by = created_by, approved_at = now() WHERE adjustment_id = $1::uuid`, id), "")
	wantRefused(t, "edit the capital terms of a membership", c.asApp(c.tenant, `UPDATE consolidation_members SET investment_account = '9999' WHERE legal_entity_id = $1::uuid AND effective_to IS NULL`, c.us), "")
	if c.countAs(uuid.NewString(), `SELECT count(*) FROM consolidation_adjustments`) != 0 {
		t.Fatal("another tenant sees adjustments")
	}
	c.denied.Store("analyst|GL_CONSOLIDATION_MANAGE", true)
	if st, _ := c.call("POST", "/v1/consolidation-groups/"+c.groupID+"/adjustments", c.tenant, "analyst", map[string]any{}); st != 403 {
		t.Fatalf("%d", st)
	}
	c.denied.Store("preparer|GL_CONSOLIDATION_CERTIFY", true)
	if st, _ := c.call("POST", "/v1/consolidation-adjustments/"+id+"/approve", c.tenant, "preparer", nil); st != 403 {
		t.Fatalf("%d", st)
	}
}

func TestAdjustmentSubmittedByAnotherServiceIsIdempotentOnItsSourceReference(t *testing.T) {
	c := consolSetup(t)
	path := "/v1/consolidation-groups/" + c.groupID + "/adjustments"
	body := func(amt string) map[string]any {
		b := adjBody(c.groupID, "from consolidation-svc", amt)
		b["source_ref"] = "cons-adj-1"
		return b
	}
	st, first := c.call("POST", path, c.tenant, "poster", body("25.00"))
	if st != 201 || first["source_ref"] != "cons-adj-1" {
		t.Fatalf("%d %v", st, first)
	}
	st, again := c.call("POST", path, c.tenant, "poster", body("25.00"))
	if st != 200 || again["adjustment_id"] != first["adjustment_id"] || again["existing"] != true {
		t.Fatalf("a retry is the same adjustment: %d %v", st, again)
	}
	if st, b := c.call("POST", path, c.tenant, "poster", body("26.00")); st != 409 {
		t.Fatalf("the same reference with other content is a conflict: %d %v", st, b)
	}
	if n := c.count(t, `SELECT count(*) FROM consolidation_adjustments WHERE tenant_id = $1::uuid`, c.tenant); n != 1 {
		t.Fatalf("%d", n)
	}
	// the group is found by its reporting entity
	st, gl := c.call("GET", "/v1/consolidation-groups?parent_entity_id="+c.group, c.tenant, "reader", nil)
	if st != 200 || gl["total"].(float64) != 1 {
		t.Fatalf("%d %v", st, gl)
	}
	if st, gl = c.call("GET", "/v1/consolidation-groups?parent_entity_id="+c.uk, c.tenant, "reader", nil); st != 200 || gl["total"].(float64) != 0 {
		t.Fatalf("%d %v", st, gl)
	}
}
