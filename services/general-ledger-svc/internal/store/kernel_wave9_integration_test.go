package store_test

import (
	"testing"

	"github.com/google/uuid"
)

// Entity- and book-scoped account versions (ACC-01 A1) against real Postgres as the ordinary role.

func (w *w2) scoped(code string, body map[string]any) (int, map[string]any) {
	return w.call("POST", "/v1/chart-of-accounts/"+code+"/scoped-versions", w.tenant, "chart-admin", body)
}

func (w *w2) accountAsOf(t *testing.T, code, query string) map[string]any {
	t.Helper()
	st, b := w.call("GET", "/v1/chart-of-accounts/"+code+"/as-of?"+query, w.tenant, "reader", nil)
	if st != 200 {
		t.Fatalf("as-of: %d %v", st, b)
	}
	return b
}

func TestTwoEntitiesHoldDifferentVersionsOfOneAccountAndTheMostSpecificWins(t *testing.T) {
	w := w2Setup(t)
	other := uuid.NewString()
	otherBook := w.mkOtherBook(t, other)
	w.backfillVersions(t)

	st, v1 := w.scoped("6000", map[string]any{"legal_entity_id": w.entity, "account_name": "Office costs (UK)", "effective_from": "2026-01-01", "reason": "local naming"})
	if st != 201 || v1["version"].(float64) != 1 {
		t.Fatalf("%d %v", st, v1)
	}
	// the entity sees its own name, another entity and the tenant keep theirs
	a := w.accountAsOf(t, "6000", "as_of=2026-03-10&legal_entity_id="+w.entity)
	if a["account_name"] != "Office costs (UK)" || a["name_scope"] != "ENTITY" || a["status_scope"] != "TENANT" {
		t.Fatalf("%v", a)
	}
	if b := w.accountAsOf(t, "6000", "as_of=2026-03-10&legal_entity_id="+other); b["account_name"] != "6000 account" || b["name_scope"] != "TENANT" {
		t.Fatalf("%v", b)
	}
	if c := w.accountAsOf(t, "6000", "as_of=2026-03-10"); c["account_name"] != "6000 account" || c["legal_entity_id"] != nil {
		t.Fatalf("the tenant answer is unchanged: %v", c)
	}
	// before the version took effect the entity sees the tenant name
	if b := w.accountAsOf(t, "6000", "as_of=2025-12-31&legal_entity_id="+w.entity); b["name_scope"] != "TENANT" {
		t.Fatalf("%v", b)
	}
	// a book version beats the entity version, field by field: it sets the name only, the entity status still applies
	if st, b := w.scoped("6000", map[string]any{"legal_entity_id": w.entity, "accounting_book_id": w.book, "account_name": "Office costs (STAT)", "effective_from": "2026-01-01", "reason": "statutory naming"}); st != 201 || b["version"].(float64) != 1 {
		t.Fatalf("each scope numbers its own versions: %d %v", st, b)
	}
	a = w.accountAsOf(t, "6000", "as_of=2026-03-10&legal_entity_id="+w.entity+"&book_id="+w.book)
	if a["account_name"] != "Office costs (STAT)" || a["name_scope"] != "BOOK" {
		t.Fatalf("%v", a)
	}
	// a newer version of the same scope closes the one before it; history is kept
	st, v2 := w.scoped("6000", map[string]any{"legal_entity_id": w.entity, "account_name": "Office and premises (UK)", "effective_from": "2026-04-01", "reason": "renamed"})
	if st != 201 || v2["version"].(float64) != 2 {
		t.Fatalf("%d %v", st, v2)
	}
	if a := w.accountAsOf(t, "6000", "as_of=2026-03-10&legal_entity_id="+w.entity); a["account_name"] != "Office costs (UK)" {
		t.Fatalf("history is reproducible: %v", a)
	}
	if a := w.accountAsOf(t, "6000", "as_of=2026-04-10&legal_entity_id="+w.entity); a["account_name"] != "Office and premises (UK)" {
		t.Fatalf("%v", a)
	}
	st, list := w.call("GET", "/v1/chart-of-accounts/6000/scoped-versions?legal_entity_id="+w.entity, w.tenant, "reader", nil)
	if st != 200 || list["total"].(float64) != 3 {
		t.Fatalf("%d %v", st, list)
	}
	_ = otherBook
}

func TestAScopedStatusClosesTheAccountInOneBookOnlyAndOnlyFromItsDate(t *testing.T) {
	w := w2Setup(t)
	other := uuid.NewString()
	otherBook := w.mkOtherBook(t, other)
	w.backfillVersions(t)
	for _, b := range []string{w.book, otherBook} {
		_ = b
	}
	// 4000 (revenue) is closed for the UK entity from 10 March
	if st, b := w.scoped("4000", map[string]any{"legal_entity_id": w.entity, "status": "INACTIVE", "effective_from": "2026-03-10", "reason": "revenue moves to 4100 in the UK"}); st != 201 {
		t.Fatalf("%d %v", st, b)
	}
	ok := w.eventAt(t, "AR.INVOICE_ISSUED", "CUST-1", "GBP", "2026-03-05", inv2("120.00", "100.00", "20.00"))
	if st, r := w.kenv.post(ok, w.book, nil); st != 201 {
		t.Fatalf("before the account closed: %d %v", st, r)
	}
	late := w.eventAt(t, "AR.INVOICE_ISSUED", "CUST-1", "GBP", "2026-03-12", inv2("120.00", "100.00", "20.00"))
	if st, r := w.kenv.post(late, w.book, nil); st != 422 || r["family"] != "AK-COA" {
		t.Fatalf("after it closed, the UK book cannot post to it: %d %v", st, r)
	}
	// the tenant account is still active, and so is it in the other entity
	if a := w.accountAsOf(t, "4000", "as_of=2026-03-12"); a["status"] != "ACTIVE" {
		t.Fatalf("%v", a)
	}
	if a := w.accountAsOf(t, "4000", "as_of=2026-03-12&legal_entity_id="+w.entity); a["status"] != "INACTIVE" || a["status_scope"] != "ENTITY" {
		t.Fatalf("%v", a)
	}
	if st, r := w.eligibility("4000", w.entity); st != 200 || r["eligible"] != false {
		t.Fatalf("the eligibility answer follows the entity: %d %v", st, r)
	}
	if st, r := w.eligibility("4000", other); st != 200 || r["eligible"] != true {
		t.Fatalf("%d %v", st, r)
	}
	// the legacy journal path applies the same scoped status
	body := w.manualBody("scoped-1", "STANDARD", "", 10)
	body["posting_date"], body["transaction_date"], body["fiscal_period"] = "2026-03-12", "2026-03-12", "2026-03"
	body["lines"] = []any{map[string]any{"account_code": "4000", "credit_amount": 10}, map[string]any{"account_code": "2000", "debit_amount": 10}}
	if st, r := w.call("POST", "/v1/journals", w.tenant, "preparer", body); st != 422 || r["error"] != "account_inactive" {
		t.Fatalf("%d %v", st, r)
	}
	// it is reopened from 20 March: the book posts again, and 12 March stays refused
	if st, b := w.scoped("4000", map[string]any{"legal_entity_id": w.entity, "status": "ACTIVE", "effective_from": "2026-03-20", "reason": "reopened"}); st != 201 {
		t.Fatalf("%d %v", st, b)
	}
	again := w.eventAt(t, "AR.INVOICE_ISSUED", "CUST-1", "GBP", "2026-03-25", inv2("60.00", "50.00", "10.00"))
	if st, r := w.kenv.post(again, w.book, nil); st != 201 {
		t.Fatalf("%d %v", st, r)
	}
	if st, r := w.kenv.post(late, w.book, nil); st != 422 {
		t.Fatalf("a posting dated inside the closed window stays refused: %d %v", st, r)
	}
}

func (w *w2) eligibility(code, entity string) (int, map[string]any) {
	return w.call("GET", "/v1/chart-of-accounts/"+code+"/posting-eligibility?legal_entity_id="+entity, w.tenant, "reader", nil)
}

func (w *w2) mkOtherBook(t *testing.T, entity string) string {
	t.Helper()
	st, r := w.call("POST", "/v1/accounting-books", w.tenant, "owner", bookBody(entity, "STAT-OTHER"))
	if st != 201 {
		t.Fatalf("%d %v", st, r)
	}
	id := r["book_id"].(string)
	if st, r = w.call("POST", "/v1/accounting-books/"+id+"/activate", w.tenant, "owner", map[string]any{}); st != 200 {
		t.Fatalf("%d %v", st, r)
	}
	return id
}

func TestScopedVersionRefusesWhatCannotBeKept(t *testing.T) {
	w := w2Setup(t)
	other := uuid.NewString()
	otherBook := w.mkOtherBook(t, other)
	w.backfillVersions(t)
	good := map[string]any{"legal_entity_id": w.entity, "account_name": "X", "effective_from": "2026-02-01", "reason": "r"}
	for what, body := range map[string]map[string]any{
		"no reason":              {"legal_entity_id": w.entity, "account_name": "X"},
		"nothing to change":      {"legal_entity_id": w.entity, "reason": "r"},
		"bad status":             {"legal_entity_id": w.entity, "status": "PAUSED", "reason": "r"},
		"not an entity":          {"legal_entity_id": "x", "account_name": "X", "reason": "r"},
		"empty name":             {"legal_entity_id": w.entity, "account_name": " ", "reason": "r"},
		"bad date":               {"legal_entity_id": w.entity, "account_name": "X", "reason": "r", "effective_from": "March"},
		"unknown parent":         {"legal_entity_id": w.entity, "parent_account_code": "NOPE", "reason": "r"},
		"book of another entity": {"legal_entity_id": w.entity, "accounting_book_id": otherBook, "account_name": "X", "reason": "r"},
	} {
		st, b := w.scoped("6000", body)
		if st != 400 && st != 422 {
			t.Fatalf("%s: %d %v", what, st, b)
		}
	}
	if st, _ := w.scoped("9999", good); st != 404 {
		t.Fatalf("unknown account: %d", st)
	}
	if st, _ := w.scoped("6000", good); st != 201 {
		t.Fatal(st)
	}
	back := map[string]any{"legal_entity_id": w.entity, "account_name": "Y", "effective_from": "2026-01-15", "reason": "r"}
	if st, b := w.scoped("6000", back); st != 422 {
		t.Fatalf("a version cannot start before the one it replaces: %d %v", st, b)
	}
	// a scoped hierarchy cannot loop
	if st, b := w.scoped("6950", map[string]any{"legal_entity_id": w.entity, "parent_account_code": "6900", "effective_from": "2026-02-01", "reason": "group under 6900"}); st != 201 {
		t.Fatalf("%d %v", st, b)
	}
	if st, b := w.scoped("6900", map[string]any{"legal_entity_id": w.entity, "parent_account_code": "6950", "effective_from": "2026-02-01", "reason": "loop"}); st != 422 {
		t.Fatalf("%d %v", st, b)
	}
	if st, b := w.scoped("6900", map[string]any{"legal_entity_id": other, "parent_account_code": "6950", "effective_from": "2026-02-01", "reason": "another entity has its own hierarchy"}); st != 201 {
		t.Fatalf("%d %v", st, b)
	}
	// the records are append-only and tenant isolated
	wantRefused(t, "rewrite a version", w.asApp(w.tenant, `UPDATE account_scope_versions SET account_name = 'Z' WHERE account_code = '6000'`), "")
	wantRefused(t, "delete a version", w.asApp(w.tenant, `DELETE FROM account_scope_versions WHERE account_code = '6000'`), "")
	if w.countAs(uuid.NewString(), `SELECT count(*) FROM account_scope_versions`) != 0 {
		t.Fatal("another tenant sees scoped versions")
	}
	w.denied.Store("analyst|COA_ACCOUNT_AMEND", true)
	if st, _ := w.call("POST", "/v1/chart-of-accounts/6000/scoped-versions", w.tenant, "analyst", good); st != 403 {
		t.Fatalf("%d", st)
	}
}

// backfillVersions gives the seeded chart the version-1 history the account API writes, so the as-of reads have a tenant version to lay a
// scoped one over.
func (w *w2) backfillVersions(t *testing.T) {
	t.Helper()
	if err := w.exec(`INSERT INTO account_versions (account_version_id, tenant_id, account_id, account_code, version, account_name, account_type, parent_account_id, is_control_account,
			direct_posting_restricted, status, effective_from, effective_to, change_kind, change_reason, created_at, created_by_principal_id)
		SELECT gen_random_uuid(), tenant_id, account_id, account_code, version, account_name, account_type, parent_account_id, is_control_account, direct_posting_restricted, status,
			TIMESTAMPTZ '2020-01-01', NULL, 'CREATED', 'seed', created_at, created_by_principal_id FROM chart_of_accounts WHERE tenant_id = $1::uuid
			AND NOT EXISTS (SELECT 1 FROM account_versions v WHERE v.account_id = chart_of_accounts.account_id)`, w.tenant); err != nil {
		t.Fatal(err)
	}
}
