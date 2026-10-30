package store_test

import (
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
)

// Migration and cutover (standard §28) against real Postgres as the ordinary role: stage, validate, preview, approve, post, reconcile, freeze.

const extractHash = "0f1e2d3c4b5a69788796a5b4c3d2e1f00f1e2d3c4b5a69788796a5b4c3d2e1f0"

type migEnv struct{ *w2 }

func migSetup(t *testing.T) *migEnv {
	t.Helper()
	w := w2Setup(t)
	if err := w.exec(`INSERT INTO chart_of_accounts (account_id, tenant_id, account_code, account_name, account_type, created_by_principal_id) VALUES ($1,$2,'3000','Share capital','EQUITY','seed')`,
		uuid.NewString(), w.tenant); err != nil {
		t.Fatal(err)
	}
	return &migEnv{w}
}

func line(ref, acct, dr, cr, cp string) map[string]any {
	l := map[string]any{"source_ref": ref, "source_account": "OLD-" + acct, "account_code": acct}
	if dr != "" {
		l["debit"] = dr
	}
	if cr != "" {
		l["credit"] = cr
	}
	if cp != "" {
		l["counterparty_id"] = cp
	}
	return l
}

// openingBody is a balanced opening trial balance: receivables of 120 owed by two customers, 80 in the bank, 60 owed to a supplier, 140 of capital.
func (m *migEnv) openingBody(code string) map[string]any {
	return map[string]any{
		"legal_entity_id": m.entity, "book_id": m.book, "batch_code": code, "cutover_date": "2026-03-01", "source_system": "legacy-erp", "source_extract_ref": "extract-2026-02-28.csv",
		"source_extract_hash": extractHash, "mapping_ref": "chart-map-v3", "source_totals": map[string]any{"debit": "200.00", "credit": "200.00"},
		"control_totals": map[string]any{
			"AR":   map[string]any{"total": "120.00", "count": 2, "reference": "ar-aging-2026-02-28"},
			"BANK": map[string]any{"total": "80.00", "count": 1, "reference": "bank-rec-2026-02-28"},
			"AP":   map[string]any{"total": "60.00", "count": 1, "reference": "ap-aging-2026-02-28"},
		},
		"lines": []any{line("ar-1", "1100", "100.00", "", "CUST-1"), line("ar-2", "1100", "20.00", "", "CUST-2"), line("bank-1", "1000", "80.00", "", ""),
			line("ap-1", "2000", "", "60.00", "SUP-1"), line("eq-1", "3000", "", "140.00", "")},
	}
}

func (m *migEnv) stage(t *testing.T, body map[string]any) (int, map[string]any) {
	t.Helper()
	return m.call("POST", "/v1/migration-batches", m.tenant, "preparer", body)
}

func (m *migEnv) mustStage(t *testing.T, body map[string]any) string {
	t.Helper()
	st, b := m.stage(t, body)
	if st != 201 || b["status"] != "STAGED" {
		t.Fatalf("stage: %d %v", st, b)
	}
	return b["batch_id"].(string)
}

func (m *migEnv) step(id, action, who string, body any) (int, map[string]any) {
	return m.call("POST", "/v1/migration-batches/"+id+"/"+action, m.tenant, who, body)
}

func window(from, to time.Duration) map[string]any {
	now := time.Now().UTC()
	return map[string]any{"authority_ref": "MIG-AUTH-2026-01", "window_start": now.Add(from).Format(time.RFC3339), "window_end": now.Add(to).Format(time.RFC3339)}
}

func (m *migEnv) approved(t *testing.T, id string) {
	t.Helper()
	if st, b := m.step(id, "validate", "preparer", nil); st != 200 || b["valid"] != true {
		t.Fatalf("validate: %d %v", st, b)
	}
	if st, b := m.step(id, "approve", "authority", window(-time.Hour, 2*time.Hour)); st != 200 || b["status"] != "APPROVED" {
		t.Fatalf("approve: %d %v", st, b)
	}
}

func TestOpeningBalancesGoThroughStageValidateApprovePostReconcileAndAreFrozen(t *testing.T) {
	m := migSetup(t)
	body := m.openingBody("OB-2026")
	id := m.mustStage(t, body)

	// the same extract again is the same batch; a different extract under the same code is a conflict, never a quiet replacement
	if st, b := m.stage(t, body); st != 200 || b["batch_id"] != id {
		t.Fatalf("replay: %d %v", st, b)
	}
	other := m.openingBody("OB-2026")
	other["lines"].([]any)[0].(map[string]any)["debit"] = "101.00"
	if st, b := m.stage(t, other); st != 409 {
		t.Fatalf("a different extract under the same code: %d %v", st, b)
	}

	// nothing moves before it is validated; the preview shows what posting would do
	if st, b := m.step(id, "approve", "authority", window(-time.Hour, time.Hour)); st != 409 {
		t.Fatalf("approve before validate: %d %v", st, b)
	}
	if st, b := m.step(id, "post", "migration-poster", nil); st != 409 {
		t.Fatalf("post before approval: %d %v", st, b)
	}
	st, pv := m.call("GET", "/v1/migration-batches/"+id+"/preview", m.tenant, "reader", nil)
	an := pv["analysis"].(map[string]any)
	if st != 200 || pv["ready_to_approve"] != true || len(an["issues"].([]any)) != 0 || len(an["accounts"].([]any)) != 4 || an["total_debit"] != "200.00" {
		t.Fatalf("%d %v", st, pv)
	}
	for _, c := range an["controls"].([]any) {
		if c.(map[string]any)["match"] != true {
			t.Fatalf("%v", c)
		}
	}
	if st, b := m.step(id, "validate", "preparer", nil); st != 200 || b["valid"] != true || b["batch"].(map[string]any)["status"] != "VALIDATED" {
		t.Fatalf("%d %v", st, b)
	}

	// approval needs another person, a named authority and a window
	if st, b := m.step(id, "approve", "preparer", window(-time.Hour, time.Hour)); st != 403 || b["error"] != "separation_of_duties" {
		t.Fatalf("the preparer approving: %d %v", st, b)
	}
	if st, b := m.step(id, "approve", "authority", map[string]any{"authority_ref": " ", "window_start": time.Now().Format(time.RFC3339), "window_end": time.Now().Add(time.Hour).Format(time.RFC3339)}); st != 422 {
		t.Fatalf("no authority: %d %v", st, b)
	}
	if st, b := m.step(id, "approve", "authority", window(-3*time.Hour, -time.Hour)); st != 422 {
		t.Fatalf("a window that is already over: %d %v", st, b)
	}
	if st, b := m.step(id, "approve", "authority", window(time.Hour, 3*time.Hour)); st != 200 || b["status"] != "APPROVED" {
		t.Fatalf("a future window: %d %v", st, b)
	}
	// the window has not opened yet, so posting is refused and nothing is written
	if st, b := m.step(id, "post", "migration-poster", nil); st != 403 || b["code"] != "MIGRATION_WINDOW_CLOSED" {
		t.Fatalf("before the window: %d %v", st, b)
	}
	if n := m.count(t, `SELECT count(*) FROM journal_headers WHERE tenant_id = $1::uuid AND journal_type = 'OPENING'`, m.tenant); n != 0 {
		t.Fatalf("nothing posted: %d", n)
	}
	// the approval is immutable: a new window means a new batch
	wantRefused(t, "move the window", m.asApp(m.tenant, `UPDATE migration_batches SET window_start = now() - interval '1 day', window_end = now() + interval '1 day' WHERE batch_id = $1::uuid`, id), "")
	m.exec(`ALTER TABLE migration_batches DISABLE TRIGGER migration_batches_guard_trg`)
	m.exec(`UPDATE migration_batches SET window_start = now() - interval '1 hour', window_end = now() + interval '2 hours' WHERE batch_id = $1::uuid`, id) //nolint:errcheck // fixture: the window the approval is deemed to have set
	m.exec(`ALTER TABLE migration_batches ENABLE TRIGGER migration_batches_guard_trg`)

	// posting: one OPENING journal, atomic, with the batch's provenance, and a repeat returns the same one
	st, posted := m.step(id, "post", "migration-poster", nil)
	if st != 201 || posted["status"] != "POSTED" || posted["journal_number"] == "" || posted["journal_id"] == "" {
		t.Fatalf("post: %d %v", st, posted)
	}
	jid := posted["journal_id"].(string)
	if st, again := m.step(id, "post", "migration-poster", nil); st != 200 || again["journal_id"] != jid {
		t.Fatalf("repeat: %d %v", st, again)
	}
	if n := m.count(t, `SELECT count(*) FROM journal_headers WHERE tenant_id = $1::uuid AND journal_type = 'OPENING'`, m.tenant); n != 1 {
		t.Fatalf("one opening journal: %d", n)
	}
	if got := m.scalar(t, `SELECT journal_type || '/' || journal_purpose || '/' || posting_date::text FROM journal_headers WHERE journal_id = $1::uuid`, jid); got != "OPENING/OPENING_BALANCE/2026-03-01" {
		t.Fatalf("%s", got)
	}
	if got := m.scalar(t, `SELECT ((calculation_trace::jsonb)->>'mapping_ref') || '/' || ((calculation_trace::jsonb)->>'authority_ref') FROM posting_executions WHERE journal_id = $1::uuid`, jid); got != "chart-map-v3/MIG-AUTH-2026-01" {
		t.Fatalf("the journal's execution records the batch's provenance: %s", got)
	}
	if got := m.scalar(t, `SELECT count(*)::text FROM ledger_entries WHERE journal_id = $1::uuid AND counterparty_id IS NOT NULL AND control_type IN ('AR', 'AP')`, jid); got != "3" {
		t.Fatalf("open items keep their counterparty and control type: %s", got)
	}
	if got := m.scalar(t, `SELECT sum(debit_amount)::text || '/' || sum(credit_amount)::text FROM ledger_entries WHERE journal_id = $1::uuid`, jid); got != "200.00/200.00" {
		t.Fatalf("%s", got)
	}

	// reconcile to the source totals and the subledger control totals, which freezes the evidence
	st, rec := m.step(id, "reconcile", "migration-poster", nil)
	if st != 200 || rec["status"] != "RECONCILED" || len(rec["evidence_hash"].(string)) != 64 || rec["reconciliation"].(map[string]any)["status"] != "MATCHED" {
		t.Fatalf("reconcile: %d %v", st, rec)
	}
	if st, again := m.step(id, "reconcile", "migration-poster", nil); st != 200 || again["evidence_hash"] != rec["evidence_hash"] {
		t.Fatalf("repeat: %d %v", st, again)
	}
	st, ev := m.call("GET", "/v1/migration-batches/"+id+"/evidence", m.tenant, "auditor", nil)
	if st != 200 || ev["evidence_verified"] != true || ev["lines_verified"] != true {
		t.Fatalf("evidence: %d %v", st, ev)
	}
	var kinds []string
	for _, h := range ev["history"].([]any) {
		kinds = append(kinds, h.(map[string]any)["kind"].(string))
	}
	if strings.Join(kinds, ",") != "STAGED,VALIDATED,APPROVED,POSTED,RECONCILED" {
		t.Fatalf("history: %v", kinds)
	}
	if st, b := m.call("GET", "/v1/migration-batches/"+id+"/lines?limit=2&offset=1", m.tenant, "reader", nil); st != 200 || len(b["lines"].([]any)) != 2 {
		t.Fatalf("%d %v", st, b)
	}
	if st, b := m.call("GET", "/v1/migration-batches?status=RECONCILED&legal_entity_id="+m.entity, m.tenant, "reader", nil); st != 200 || b["total"].(float64) != 1 {
		t.Fatalf("%d %v", st, b)
	}

	// a frozen batch cannot be changed, withdrawn or posted again, by anyone, through any path
	if st, b := m.step(id, "cancel", "preparer", map[string]any{"reason": "oops"}); st != 409 {
		t.Fatalf("cancel a posted batch: %d %v", st, b)
	}
	wantRefused(t, "rewrite the mapping", m.asApp(m.tenant, `UPDATE migration_batches SET mapping_ref = 'other' WHERE batch_id = $1::uuid`, id), "")
	wantRefused(t, "rewrite a staged line", m.asApp(m.tenant, `UPDATE migration_batch_lines SET account_code = '9999' WHERE batch_id = $1::uuid`, id), "")
	wantRefused(t, "delete a staged line", m.asApp(m.tenant, `DELETE FROM migration_batch_lines WHERE batch_id = $1::uuid`, id), "")
	wantRefused(t, "delete the batch", m.asApp(m.tenant, `DELETE FROM migration_batches WHERE batch_id = $1::uuid`, id), "")
	wantRefused(t, "rewrite its history", m.asApp(m.tenant, `UPDATE migration_batch_events SET actor = 'x' WHERE batch_id = $1::uuid`, id), "")
	wantRefused(t, "reopen a frozen batch", m.asApp(m.tenant, `UPDATE migration_batches SET status = 'POSTED' WHERE batch_id = $1::uuid`, id), "")
	wantRefused(t, "approve one's own batch in SQL", m.asApp(m.tenant, `UPDATE migration_batches SET approved_by = created_by WHERE batch_id = $1::uuid`, id), "")

	// one live opening per book: a second opening is refused, and a correction names the batch it corrects
	second := m.mustStage(t, m.openingBody("OB-2026-AGAIN"))
	m.approved(t, second)
	if st, b := m.step(second, "post", "migration-poster", nil); st != 409 {
		t.Fatalf("a second opening for the same book: %d %v", st, b)
	}
	corr := map[string]any{"legal_entity_id": m.entity, "book_id": m.book, "batch_code": "OB-2026-FIX1", "batch_kind": "CORRECTION", "corrects_batch_id": id, "cutover_date": "2026-03-01",
		"source_system": "legacy-erp", "source_extract_ref": "fix-1.csv", "source_extract_hash": extractHash, "mapping_ref": "chart-map-v3",
		"source_totals": map[string]any{"debit": "10.00", "credit": "10.00"}, "control_totals": map[string]any{"BANK": map[string]any{"total": "10.00", "count": 1, "reference": "bank-fix"}},
		"lines": []any{line("fix-bank", "1000", "10.00", "", ""), line("fix-eq", "3000", "", "10.00", "")}}
	cid := m.mustStage(t, corr)
	m.approved(t, cid)
	if st, b := m.step(cid, "post", "migration-poster", nil); st != 201 || b["batch_kind"] != "CORRECTION" {
		t.Fatalf("the correction: %d %v", st, b)
	}
	if st, b := m.step(cid, "reconcile", "migration-poster", nil); st != 200 || b["status"] != "RECONCILED" {
		t.Fatalf("%d %v", st, b)
	}
	bad := map[string]any{}
	for k, v := range corr {
		bad[k] = v
	}
	bad["batch_code"], bad["corrects_batch_id"] = "OB-FIX-BAD", second
	if st, b := m.stage(t, bad); st != 422 {
		t.Fatalf("a correction of a batch that was never reconciled: %d %v", st, b)
	}
	if got := m.scalar(t, `SELECT sum(debit_amount - credit_amount)::text FROM ledger_entries WHERE tenant_id = $1::uuid AND book_id = 'STAT' AND journal_id IN (SELECT journal_id FROM journal_headers WHERE journal_type = 'OPENING')`, m.tenant); got != "0.00" {
		t.Fatalf("the opening books balance: %s", got)
	}
}

func TestAStagedExtractIsRefusedOrReportedNeverQuietlyFixed(t *testing.T) {
	m := migSetup(t)

	// schema problems stop the batch before anything is kept
	bad := m.openingBody("OB-SHAPE")
	bad["lines"].([]any)[1].(map[string]any)["source_ref"] = "ar-1"
	bad["lines"].([]any)[2].(map[string]any)["debit"] = "80.005"
	if st, b := m.stage(t, bad); st != 422 || b["code"] != "MIGRATION_INVALID" || len(b["facts"].(map[string]any)["issues"].([]any)) < 2 {
		t.Fatalf("%d %v", st, b)
	}
	noHash := m.openingBody("OB-NOHASH")
	noHash["source_extract_hash"] = "abc"
	if st, b := m.stage(t, noHash); st != 422 {
		t.Fatalf("%d %v", st, b)
	}
	if n := m.count(t, `SELECT count(*) FROM migration_batches WHERE tenant_id = $1::uuid`, m.tenant); n != 0 {
		t.Fatalf("nothing kept: %d", n)
	}

	// referential problems are reported on the batch: the account is not in the chart, the control account has no counterparty, the books do not balance
	dirty := m.openingBody("OB-DIRTY")
	lines := dirty["lines"].([]any)
	lines[0] = line("ar-1", "1100", "100.00", "", "")
	lines[1] = line("ar-2", "9999", "20.00", "", "CUST-2")
	lines[4] = line("eq-1", "3000", "", "139.00", "")
	id := m.mustStage(t, dirty)
	st, v := m.step(id, "validate", "preparer", nil)
	if st != 200 || v["valid"] != false || v["batch"].(map[string]any)["status"] != "STAGED" {
		t.Fatalf("%d %v", st, v)
	}
	var codes []string
	for _, i := range v["analysis"].(map[string]any)["issues"].([]any) {
		codes = append(codes, i.(map[string]any)["code"].(string))
	}
	got := strings.Join(codes, ",")
	for _, want := range []string{"COUNTERPARTY_REQUIRED", "ACCOUNT_UNAVAILABLE", "BATCH_UNBALANCED", "SOURCE_TOTALS_MISMATCH", "CONTROL_TOTAL_MISMATCH"} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %s in %s", want, got)
		}
	}
	// a batch with issues cannot be approved; it is cancelled, with a reason, and staged again from a corrected extract
	if st, b := m.step(id, "approve", "authority", window(-time.Hour, time.Hour)); st != 409 {
		t.Fatalf("%d %v", st, b)
	}
	if st, b := m.step(id, "cancel", "preparer", map[string]any{}); st != 422 {
		t.Fatalf("a reason is required: %d %v", st, b)
	}
	if st, b := m.step(id, "cancel", "preparer", map[string]any{"reason": "extract was cut before the last ledger run"}); st != 200 || b["status"] != "CANCELLED" {
		t.Fatalf("%d %v", st, b)
	}
	if st, b := m.step(id, "validate", "preparer", nil); st != 409 {
		t.Fatalf("a cancelled batch is final: %d %v", st, b)
	}
	wantRefused(t, "revive a cancelled batch", m.asApp(m.tenant, `UPDATE migration_batches SET status = 'STAGED' WHERE batch_id = $1::uuid`, id), "")
}

func TestMigrationCannotBypassThePeriodGateAuthorizationOrTenantIsolation(t *testing.T) {
	m := migSetup(t)
	id := m.mustStage(t, m.openingBody("OB-GATE"))
	m.approved(t, id)

	// the period of the cutover date is sealed: the migration is refused like any other posting, and the batch stays approved
	if st, b := m.seal("SEAL", "", ""); st != 201 {
		t.Fatalf("seal: %d %v", st, b)
	}
	if st, b := m.step(id, "post", "migration-poster", nil); st != 409 {
		t.Fatalf("a sealed period: %d %v", st, b)
	}
	if got := m.scalar(t, `SELECT status FROM migration_batches WHERE batch_id = $1::uuid`, id); got != "APPROVED" {
		t.Fatalf("%s", got)
	}
	if n := m.count(t, `SELECT count(*) FROM journal_headers WHERE tenant_id = $1::uuid AND journal_type = 'OPENING'`, m.tenant); n != 0 {
		t.Fatalf("nothing posted: %d", n)
	}

	// each step is its own authority
	for action, deny := range map[string]string{"post": "GL_MIGRATION_POST", "approve": "GL_MIGRATION_APPROVE", "validate": "GL_MIGRATION_STAGE", "reconcile": "GL_MIGRATION_RECONCILE"} {
		who := "denied-" + action
		m.denied.Store(who+"|"+deny, true)
		if st, _ := m.step(id, action, who, map[string]any{}); st != 403 {
			t.Fatalf("%s without %s: %d", action, deny, st)
		}
	}
	m.denied.Store("snoop|GL_MIGRATION_READ", true)
	if st, _ := m.call("GET", "/v1/migration-batches/"+id, m.tenant, "snoop", nil); st != 403 {
		t.Fatalf("%d", st)
	}
	m.denied.Store("stager|GL_MIGRATION_STAGE", true)
	if st, _ := m.call("POST", "/v1/migration-batches", m.tenant, "stager", m.openingBody("X")); st != 403 {
		t.Fatalf("%d", st)
	}

	// another tenant sees nothing, and the tables enforce it
	if st, _ := m.call("GET", "/v1/migration-batches/"+id, uuid.NewString(), "reader", nil); st != 404 {
		t.Fatalf("%d", st)
	}
	for _, q := range []string{"migration_batches", "migration_batch_lines", "migration_batch_events"} {
		if n := m.countAs(uuid.NewString(), fmt.Sprintf(`SELECT count(*) FROM %s`, q)); n != 0 {
			t.Fatalf("another tenant sees %s", q)
		}
	}
}

func TestSixWorkersPostingTheSameApprovedBatchCreateOneJournal(t *testing.T) {
	m := migSetup(t)
	id := m.mustStage(t, m.openingBody("OB-RACE"))
	m.approved(t, id)
	var wg sync.WaitGroup
	codes := make([]int, 6)
	for i := range codes {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			codes[i], _ = m.step(id, "post", "migration-poster", nil)
		}(i)
	}
	wg.Wait()
	created := 0
	for _, c := range codes {
		switch c {
		case 201:
			created++
		case 200:
		default:
			t.Fatalf("every worker gets the batch back: %v", codes)
		}
	}
	if created != 1 {
		t.Fatalf("exactly one worker posts it: %v", codes)
	}
	if n := m.count(t, `SELECT count(*) FROM journal_headers WHERE tenant_id = $1::uuid AND journal_type = 'OPENING'`, m.tenant); n != 1 {
		t.Fatalf("one opening journal: %d", n)
	}
	if n := m.count(t, `SELECT count(*) FROM ledger_entries WHERE tenant_id = $1::uuid AND book_id = 'STAT'`, m.tenant); n != 5 {
		t.Fatalf("five ledger entries, not a multiple: %d", n)
	}
}
