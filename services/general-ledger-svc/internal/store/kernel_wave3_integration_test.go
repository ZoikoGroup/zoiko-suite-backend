package store_test

import (
	"fmt"
	"sync"
	"testing"
)

// Wave 3 (period close / FX) against real Postgres as the ordinary role.

func (w *w2) seal(action, authority, reason string) (int, map[string]any) {
	b := map[string]any{"legal_entity_id": w.entity, "fiscal_period": "2026-03", "action": action}
	if authority != "" {
		b["authority_ref"] = authority
	}
	if reason != "" {
		b["reason"] = reason
	}
	return w.call("POST", "/v1/period-seals", w.tenant, "close-svc", b)
}

func TestSealedPeriodRefusesEveryPostingPath(t *testing.T) {
	w := w2Setup(t)
	first := w.post(t, "AR.INVOICE_ISSUED", "CUST-1", inv("120.00", "100.00", "20.00"))

	st, seal := w.seal("SEAL", "", "")
	if st != 201 || seal["status"] != "SEALED" || seal["ledger_watermark"].(float64) < 3 {
		t.Fatalf("%d %v", st, seal)
	}
	if st, again := w.seal("SEAL", "", ""); st != 200 || again["existing"] != true {
		t.Fatalf("sealing twice is a repeat: %d %v", st, again)
	}

	// the kernel's posting path
	ev := w.event(t, "AR.INVOICE_ISSUED", "CUST-1", inv("60.00", "50.00", "10.00"))
	st, b := w.kenv.post(ev, w.book, nil)
	if st != 409 || b["family"] != "AK-PERIOD" || b["code"] != "PERIOD_SEALED" {
		t.Fatalf("%d %v", st, b)
	}
	// the ordinary journal path: a manual journal in the period cannot be posted
	manual := map[string]any{"tenant_id": w.tenant, "legal_entity_id": w.entity, "fiscal_period": "2026-03", "description": "late", "correlation_id": "late-1", "journal_type": "STANDARD",
		"transaction_date": "2026-03-10", "posting_date": "2026-03-10", "currency_code": "GBP",
		"lines": []any{map[string]any{"account_code": "6000", "debit_amount": 5}, map[string]any{"account_code": "2300", "credit_amount": 5}}}
	st, j := w.call("POST", "/v1/journals", w.tenant, "preparer", manual)
	if st != 201 {
		t.Fatalf("%d %v", st, j)
	}
	jid := j["journal_id"].(string)
	if st, _ := w.call("POST", "/v1/journals/"+jid+"/validate", w.tenant, "preparer", map[string]any{}); st != 200 {
		t.Fatal("validate")
	}
	if st, b := w.call("POST", "/v1/journals/"+jid+"/post", w.tenant, "preparer", map[string]any{}); st != 412 || b["error"] != "period_locked" {
		t.Fatalf("a sealed period refuses the journal path too: %d %v", st, b)
	}
	if w.scalar(t, `SELECT status FROM journal_headers WHERE journal_id=$1::uuid`, jid) != "VALIDATED" || w.count(t, `SELECT count(*) FROM ledger_entries WHERE journal_id=$1::uuid`, jid) != 0 {
		t.Fatal("and leaves it unposted")
	}
	// a reversal dated into the sealed period is refused; one dated after the period is not
	if st, b := w.call("POST", "/v1/journals/"+first["journal_id"].(string)+"/reverse", w.tenant, "reviewer", map[string]any{"reason": "x", "correlation_id": "rv-1", "posting_date": "2026-03-20"}); st != 412 {
		t.Fatalf("%d %v", st, b)
	}
	if st, b := w.call("POST", "/v1/journals/"+first["journal_id"].(string)+"/reverse", w.tenant, "reviewer", map[string]any{"reason": "x", "correlation_id": "rv-2", "posting_date": "2026-04-02"}); st != 201 {
		t.Fatalf("the next period is open: %d %v", st, b)
	}
	// the ledger never moved: no entry in the sealed period is newer than the watermark
	if w.count(t, `SELECT count(*) FROM ledger_entries WHERE posting_date BETWEEN '2026-03-01' AND '2026-03-31' AND entry_seq > $1`, int64(seal["ledger_watermark"].(float64))) != 0 {
		t.Fatal("nothing slipped under the seal")
	}

	// reopening needs the authority that permits it, and every step is on record
	if st, _ := w.seal("REOPEN", "", ""); st != 422 {
		t.Fatalf("no authority, no reopen: %d", st)
	}
	if st, r := w.seal("REOPEN", "AUTH-77", "restated invoice"); st != 200 || r["status"] != "REOPENED" || r["reopen_authority"] != "AUTH-77" {
		t.Fatalf("%d %v", st, r)
	}
	if st, _ := w.seal("SEAL", "", ""); st != 409 {
		t.Fatalf("a reopened period is re-sealed, not sealed: %d", st)
	}
	if st, b := w.kenv.post(ev, w.book, nil); st != 201 {
		t.Fatalf("posting is possible again while reopened: %d %v", st, b)
	}
	st, re := w.seal("RESEAL", "", "")
	if st != 200 || re["status"] != "SEALED" || re["seal_count"].(float64) != 2 || re["entries_since_reopen"].(float64) < 3 {
		t.Fatalf("the reopen window's activity is counted: %d %v", st, re)
	}
	st, hist := w.call("GET", "/v1/period-seals/"+w.entity+"/2026-03", w.tenant, "auditor", nil)
	h := hist["history"].([]any)
	if st != 200 || len(h) != 3 || h[0].(map[string]any)["kind"] != "SEAL" || h[1].(map[string]any)["kind"] != "REOPEN" || h[2].(map[string]any)["kind"] != "RESEAL" {
		t.Fatalf("%d %v", st, hist)
	}
	wantRefused(t, "delete a seal", w.asApp(w.tenant, `DELETE FROM period_seals`), "never deleted")
	wantRefused(t, "rewrite the seal history", w.asApp(w.tenant, `UPDATE period_seal_events SET actor='x'`), "append-only")
	w.denied.Store("clerk|GL_PERIOD_REOPEN", true)
	if st, _ := w.call("POST", "/v1/period-seals", w.tenant, "clerk", map[string]any{"legal_entity_id": w.entity, "fiscal_period": "2026-03", "action": "REOPEN", "authority_ref": "a", "reason": "r"}); st != 403 {
		t.Fatalf("reopening is its own permission: %d", st)
	}
	if st, _ := w.call("POST", "/v1/period-seals", w.tenant, "close-svc", map[string]any{"legal_entity_id": w.entity, "fiscal_period": "2026-03", "action": "BURN"}); st != 400 {
		t.Fatalf("%d", st)
	}
}

func TestNoPostingSlipsIntoAPeriodAfterItsWatermark(t *testing.T) {
	w := w2Setup(t)
	ids := make([]string, 40)
	for i := range ids {
		ids[i] = w.event(t, "AR.INVOICE_ISSUED", fmt.Sprintf("CUST-%d", i%4), inv("120.00", "100.00", "20.00"))
	}
	var wg sync.WaitGroup
	var mu sync.Mutex
	posted, refused := 0, 0
	for _, id := range ids {
		wg.Add(1)
		go func(id string) {
			defer wg.Done()
			st, b := w.kenv.post(id, w.book, nil)
			mu.Lock()
			defer mu.Unlock()
			switch {
			case st == 201:
				posted++
			case st == 409 && b["code"] == "PERIOD_SEALED":
				refused++
			default:
				t.Errorf("unexpected %d %v", st, b)
			}
		}(id)
	}
	// the seal lands while the burst is in flight
	var sealed map[string]any
	var sealStatus int
	wg.Add(1)
	go func() {
		defer wg.Done()
		sealStatus, sealed = w.seal("SEAL", "", "")
	}()
	wg.Wait()
	if sealStatus != 201 {
		t.Fatalf("%d %v", sealStatus, sealed)
	}
	wm := int64(sealed["ledger_watermark"].(float64))
	if posted+refused != 40 {
		t.Fatalf("posted %d refused %d", posted, refused)
	}
	if w.count(t, `SELECT count(*) FROM ledger_entries WHERE posting_date BETWEEN '2026-03-01' AND '2026-03-31' AND entry_seq > $1`, wm) != 0 {
		t.Fatal("an entry was written into the period after its close watermark")
	}
	if w.count(t, `SELECT count(*) FROM journal_headers`) != posted {
		t.Fatal("what was refused left nothing behind")
	}
	// the same burst again, after the seal: nothing new posts
	for _, id := range ids {
		if st, _ := w.kenv.post(id, w.book, nil); st != 200 && st != 409 {
			t.Fatalf("%d", st)
		}
	}
	if w.count(t, `SELECT count(*) FROM journal_headers`) != posted {
		t.Fatal("nothing new after the seal")
	}
}
