package store_test

import (
	"testing"

	"github.com/google/uuid"
)

// Cross-entity allocations against real Postgres as the ordinary role: the cost lands in the recipient entity's own ledger, the two
// entities are left with a paired intercompany receivable and payable, and everything is atomic.

type crossEnv struct {
	*w2
	entity2 string
	book2   string
}

// crossSetup extends the allocation setup with a second entity (own active book), the intercompany accounts and their mappings.
func crossSetup(t *testing.T) *crossEnv {
	t.Helper()
	w := w2Setup(t)
	w.allocSetup(t)
	c := &crossEnv{w2: w, entity2: uuid.NewString()}
	st, b := w.call("POST", "/v1/accounting-books", w.tenant, "owner", bookBody(c.entity2, "STAT"))
	if st != 201 {
		t.Fatalf("%d %v", st, b)
	}
	c.book2 = b["book_id"].(string)
	if st, b = w.call("POST", "/v1/accounting-books/"+c.book2+"/activate", w.tenant, "owner", map[string]any{}); st != 200 {
		t.Fatalf("%d %v", st, b)
	}
	for _, a := range [][2]string{{"1800", "ASSET"}, {"2800", "LIABILITY"}} {
		if err := w.exec(`INSERT INTO chart_of_accounts (account_id, tenant_id, account_code, account_name, account_type, created_by_principal_id) VALUES ($1,$2,$3,$4,$5,'seed')`,
			uuid.NewString(), w.tenant, a[0], a[0]+" account", a[1]); err != nil {
			t.Fatal(err)
		}
		// intercompany control accounts: a line without its counterparty is refused, so the allocation has to carry it
		if st, b := w.call("PUT", "/v1/account-applicability/"+a[0], w.tenant, "chart-admin", map[string]any{"control_type": "INTERCOMPANY", "counterparty_required": true}); st != 200 {
			t.Fatalf("%d %v", st, b)
		}
	}
	return c
}

func (c *crossEnv) mapIC(t *testing.T) {
	t.Helper()
	for k, code := range map[string]string{"ALLOCATION_IC_RECEIVABLE": "1800", "ALLOCATION_IC_PAYABLE": "2800"} {
		if err := c.exec(`INSERT INTO account_mappings (account_mapping_id, tenant_id, mapping_key, account_code, effective_from, created_by_principal_id) VALUES ($1,$2,$3,$4,'2026-01-01','seed')`,
			uuid.NewString(), c.tenant, k, code); err != nil {
			t.Fatal(err)
		}
	}
}

// crossRule is a 50/50 FIXED rule: A stays in the source entity, B is charged to the second entity.
func (c *crossEnv) crossRule(t *testing.T, code string, remote map[string]any) string {
	t.Helper()
	b := map[string]any{"key": "B", "account_code": "6102", "percent": "50", "dimensions": map[string]any{"cost_center": "CC-B"}}
	for k, v := range remote {
		b[k] = v
	}
	body := map[string]any{"rule_code": code, "effective_from": "2026-01-01", "content": map[string]any{"name": code, "source_accounts": []string{"6050"}, "driver": "FIXED",
		"recipients": []any{map[string]any{"key": "A", "account_code": "6101", "percent": "50", "dimensions": map[string]any{"cost_center": "CC-A"}}, b}}}
	st, r := c.call("POST", "/v1/allocation-rules", c.tenant, "author", body)
	if st != 201 {
		t.Fatalf("%d %v", st, r)
	}
	id := r["allocation_rule_id"].(string)
	if st, r := c.call("POST", "/v1/allocation-rules/"+id+"/approve", c.tenant, "approver", nil); st != 200 {
		t.Fatalf("%d %v", st, r)
	}
	return id
}

func (c *crossEnv) remoteNet(t *testing.T, entity, account string) string {
	t.Helper()
	return c.scalar(t, `SELECT COALESCE(sum(debit_amount - credit_amount), 0)::numeric(20,2)::text FROM ledger_entries WHERE tenant_id = $1::uuid AND legal_entity_id = $2::uuid AND book_id = 'STAT' AND account_code = $3`, c.tenant, entity, account)
}

func (c *crossEnv) runRule(rule string) (int, map[string]any) {
	return c.call("POST", "/v1/allocation-runs", c.tenant, "close-runner", map[string]any{"allocation_rule_id": rule, "book_id": c.book, "fiscal_period": "2026-03", "as_of": "2026-03-31"})
}

func TestCrossEntityAllocationChargesTheRecipientEntityAndLeavesAPairedIntercompanyBalance(t *testing.T) {
	c := crossSetup(t)
	c.mapIC(t)
	c.cost(t, "6050", "100.00", "2026-03-15")
	rule := c.crossRule(t, "X-IT", map[string]any{"legal_entity_id": c.entity2, "book_id": c.book2})

	st, run := c.runRule(rule)
	if st != 201 || run["status"] != "POSTED" {
		t.Fatalf("%d %v", st, run)
	}
	remote := run["remote_journals"].([]any)
	if len(remote) != 1 || remote[0].(map[string]any)["legal_entity_id"] != c.entity2 || remote[0].(map[string]any)["journal_number"] == "" {
		t.Fatalf("the recipient entity's journal is recorded on the run: %v", run)
	}
	// the source entity: pool relieved, its own share expensed, and a receivable from the other entity for the recharge
	if c.srcNet(t, "6050") != "0.00" || c.srcNet(t, "6101") != "50.00" || c.srcNet(t, "1800") != "50.00" {
		t.Fatalf("source: %s %s %s", c.srcNet(t, "6050"), c.srcNet(t, "6101"), c.srcNet(t, "1800"))
	}
	// the recipient entity: the cost in its own ledger with its dimension, against a payable to the source entity
	if c.remoteNet(t, c.entity2, "6102") != "50.00" || c.remoteNet(t, c.entity2, "2800") != "-50.00" {
		t.Fatalf("recipient: %s %s", c.remoteNet(t, c.entity2, "6102"), c.remoteNet(t, c.entity2, "2800"))
	}
	if got := c.scalar(t, `SELECT dimensions->>'cost_center' FROM ledger_entries WHERE tenant_id = $1::uuid AND legal_entity_id = $2::uuid AND account_code = '6102'`, c.tenant, c.entity2); got != "CC-B" {
		t.Fatalf("the cost carries the recipient dimension: %q", got)
	}
	// the other entity is never charged outside its own ledger
	if c.srcNet(t, "6102") != "0.00" {
		t.Fatalf("the source entity does not carry the recipient's cost: %s", c.srcNet(t, "6102"))
	}
	// the two intercompany lines name each other, so the consolidation can pair them
	if got := c.scalar(t, `SELECT counterparty_id FROM ledger_entries WHERE tenant_id = $1::uuid AND legal_entity_id = $2::uuid AND account_code = '1800'`, c.tenant, c.entity); got != c.entity2 {
		t.Fatalf("receivable counterparty %q", got)
	}
	if got := c.scalar(t, `SELECT counterparty_id FROM ledger_entries WHERE tenant_id = $1::uuid AND legal_entity_id = $2::uuid AND account_code = '2800'`, c.tenant, c.entity2); got != c.entity {
		t.Fatalf("payable counterparty %q", got)
	}
	if n := c.count(t, `SELECT count(*) FROM (SELECT journal_id FROM journal_lines WHERE tenant_id = $1::uuid GROUP BY 1 HAVING sum(debit_amount) <> sum(credit_amount)) x`, c.tenant); n != 0 {
		t.Fatalf("%d unbalanced journals", n)
	}
	if n := c.count(t, `SELECT count(*) FROM journal_headers WHERE tenant_id = $1::uuid AND legal_entity_id = $2::uuid AND journal_type = 'ALLOCATION'`, c.tenant, c.entity2); n != 1 {
		t.Fatalf("one allocation journal in the recipient entity: %d", n)
	}

	// idempotent: the same run, remote journals and all
	if st, again := c.runRule(rule); st != 200 || again["allocation_run_id"] != run["allocation_run_id"] || len(again["remote_journals"].([]any)) != 1 {
		t.Fatalf("%d %v", st, again)
	}

	// what the run posted in another entity's book is as immutable as the rest of it
	id := run["allocation_run_id"].(string)
	wantRefused(t, "rewrite the recipient journals", c.asApp(c.tenant, `UPDATE allocation_runs SET remote_journals = '[]'::jsonb WHERE allocation_run_id = $1::uuid`, id), "")

	// withdrawing the run reverses every journal, in both entities, together
	st, rev := c.call("POST", "/v1/allocation-runs/"+id+"/reverse", c.tenant, "close-runner", map[string]any{"reason": "wrong rule"})
	if st != 200 || rev["status"] != "REVERSED" || rev["remote_journals"].([]any)[0].(map[string]any)["reversal_journal_id"] == "" {
		t.Fatalf("%d %v", st, rev)
	}
	for _, acct := range []string{"6101", "1800", "6050"} {
		want := "0.00"
		if acct == "6050" {
			want = "100.00"
		}
		if got := c.srcNet(t, acct); got != want {
			t.Fatalf("source %s after reversal: %s", acct, got)
		}
	}
	if c.remoteNet(t, c.entity2, "6102") != "0.00" || c.remoteNet(t, c.entity2, "2800") != "0.00" {
		t.Fatal("the recipient entity is back to nothing")
	}
	if n := c.count(t, `SELECT count(*) FROM journal_headers WHERE tenant_id = $1::uuid AND legal_entity_id = $2::uuid AND status = 'REVERSED'`, c.tenant, c.entity2); n != 1 {
		t.Fatalf("%d", n)
	}
}

func TestCrossEntityAllocationIsAllOrNothing(t *testing.T) {
	c := crossSetup(t)
	c.cost(t, "6050", "100.00", "2026-03-15")
	rule := c.crossRule(t, "X-ATOMIC", map[string]any{"legal_entity_id": c.entity2, "book_id": c.book2})

	// no intercompany mapping: refused before anything is posted
	if st, b := c.runRule(rule); st != 422 || b["code"] != "ALLOCATION_ACCOUNT_UNAVAILABLE" {
		t.Fatalf("%d %v", st, b)
	}
	c.mapIC(t)

	// the recipient entity's period is sealed: the source entity's journal must not post either
	if st, b := c.call("POST", "/v1/period-seals", c.tenant, "close-svc", map[string]any{"legal_entity_id": c.entity2, "fiscal_period": "2026-03", "action": "SEAL"}); st != 200 && st != 201 {
		t.Fatalf("%d %v", st, b)
	}
	if st, b := c.runRule(rule); st != 409 {
		t.Fatalf("a sealed recipient period refuses the allocation: %d %v", st, b)
	}
	if c.srcNet(t, "6050") != "100.00" || c.srcNet(t, "6101") != "0.00" || c.srcNet(t, "1800") != "0.00" {
		t.Fatalf("nothing posted in the source entity either: %s %s %s", c.srcNet(t, "6050"), c.srcNet(t, "6101"), c.srcNet(t, "1800"))
	}
	if n := c.count(t, `SELECT count(*) FROM allocation_runs WHERE tenant_id = $1::uuid`, c.tenant); n != 0 {
		t.Fatalf("no run recorded: %d", n)
	}
}

func TestCrossEntityRecipientsAreValidated(t *testing.T) {
	c := crossSetup(t)
	c.mapIC(t)
	c.cost(t, "6050", "100.00", "2026-03-15")

	// the entity and its book come together
	body := func(extra map[string]any) map[string]any {
		b := map[string]any{"key": "B", "account_code": "6102", "percent": "50"}
		for k, v := range extra {
			b[k] = v
		}
		return map[string]any{"rule_code": "X-BAD" + uuid.NewString()[:6], "effective_from": "2026-01-01", "content": map[string]any{"name": "n", "source_accounts": []string{"6050"}, "driver": "FIXED",
			"recipients": []any{map[string]any{"key": "A", "account_code": "6101", "percent": "50"}, b}}}
	}
	for what, extra := range map[string]map[string]any{
		"an entity without its book": {"legal_entity_id": c.entity2},
		"a book without its entity":  {"book_id": c.book2},
		"not an entity id":           {"legal_entity_id": "x", "book_id": c.book2},
	} {
		if st, b := c.call("POST", "/v1/allocation-rules", c.tenant, "author", body(extra)); st != 422 {
			t.Fatalf("%s: %d %v", what, st, b)
		}
	}

	// the book must belong to the named entity, be of the same currency, and the recipient cannot be the source entity
	other := uuid.NewString()
	st, ub := c.call("POST", "/v1/accounting-books", c.tenant, "owner", func() map[string]any {
		b := bookBody(other, "STAT")
		b["functional_currency"] = "USD"
		return b
	}())
	if st != 201 {
		t.Fatalf("%d %v", st, ub)
	}
	usd := ub["book_id"].(string)
	if st, b := c.call("POST", "/v1/accounting-books/"+usd+"/activate", c.tenant, "owner", map[string]any{}); st != 200 {
		t.Fatalf("%d %v", st, b)
	}
	for what, tc := range map[string]struct {
		extra map[string]any
		code  string
	}{
		"a book of another entity":         {map[string]any{"legal_entity_id": c.entity2, "book_id": usd}, "ALLOCATION_RECIPIENT_BOOK"},
		"a book in another currency":       {map[string]any{"legal_entity_id": other, "book_id": usd}, "ALLOCATION_CURRENCY_MISMATCH"},
		"the source entity as a recipient": {map[string]any{"legal_entity_id": c.entity, "book_id": c.book}, "ALLOCATION_INVALID"},
	} {
		rule := c.crossRule(t, "X-"+uuid.NewString()[:8], tc.extra)
		if st, b := c.runRule(rule); st != 422 || b["code"] != tc.code {
			t.Fatalf("%s: %d %v", what, st, b)
		}
	}
	if c.srcNet(t, "6050") != "100.00" {
		t.Fatal("nothing posted by a refused run")
	}
}

// srcNet is the net debit of an account of the source entity, always with two decimals (an account with no entries is 0.00).
func (c *crossEnv) srcNet(t *testing.T, account string) string {
	t.Helper()
	return c.remoteNet(t, c.entity, account)
}
