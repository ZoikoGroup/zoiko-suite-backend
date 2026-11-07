package store_test

import (
	"fmt"
	"sync"
	"testing"
	"time"
)

// recObs records what the kernel reports as operational signals (standard §38).
type recObs struct {
	mu         sync.Mutex
	commands   map[string]int
	exceptions map[string]int
	recon      map[string]int
	slowest    time.Duration
}

func (o *recObs) reset() {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.commands, o.exceptions, o.recon, o.slowest = map[string]int{}, map[string]int{}, map[string]int{}, 0
}

func (o *recObs) Command(name, outcome string, d time.Duration) {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.commands == nil {
		o.commands, o.exceptions, o.recon = map[string]int{}, map[string]int{}, map[string]int{}
	}
	o.commands[name+"/"+outcome]++
	if d > o.slowest {
		o.slowest = d
	}
}

func (o *recObs) Exception(family, code string) {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.exceptions == nil {
		o.commands, o.exceptions, o.recon = map[string]int{}, map[string]int{}, map[string]int{}
	}
	o.exceptions[family+"/"+code]++
}

func (o *recObs) ReconciliationResult(typ, status string) {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.recon == nil {
		o.commands, o.exceptions, o.recon = map[string]int{}, map[string]int{}, map[string]int{}
	}
	o.recon[typ+"/"+status]++
}

func (o *recObs) get(m string, k string) int {
	o.mu.Lock()
	defer o.mu.Unlock()
	switch m {
	case "c":
		return o.commands[k]
	case "e":
		return o.exceptions[k]
	}
	return o.recon[k]
}

func TestKernelCommandsAreObservedByAccountingOutcome(t *testing.T) {
	w := w2Setup(t)
	w.obs.reset()

	// intake and posting of a clean event: ok, then the idempotent repeat is a duplicate-prevention hit
	id := w.event(t, "AR.INVOICE_ISSUED", "CUST-1", inv("120.00", "100.00", "20.00"))
	if st, b := w.kenv.post(id, w.book, nil); st != 201 {
		t.Fatalf("%d %v", st, b)
	}
	if st, b := w.kenv.post(id, w.book, nil); st != 200 {
		t.Fatalf("%d %v", st, b)
	}
	if w.obs.get("c", "register_event/ok") != 1 || w.obs.get("c", "post_event/ok") != 1 || w.obs.get("c", "post_event/existing") != 1 {
		t.Fatalf("%v", w.obs.commands)
	}

	// an event no rule covers is an accounting refusal: counted as such, with its family and code, and nothing is written
	body := map[string]any{"event_type": "NOPE.NOTHING", "event_version": 1, "legal_entity_id": w.entity, "source_system": "subledger", "source_object_type": "doc", "source_object_id": "obs-1",
		"source_event_id": "obs-1", "occurred_at": "2026-03-10T09:00:00Z", "document_date": "2026-03-10T00:00:00Z", "transaction_currency": "GBP", "facts": map[string]any{},
		"idempotency_key": "idem-obs-1", "correlation_id": "c", "causation_id": "c"}
	st, ev := w.call("POST", "/v1/accounting-events", w.tenant, "subledger", body)
	if st != 201 {
		t.Fatalf("%d %v", st, ev)
	}
	st, res := w.kenv.post(ev["accounting_event_id"].(string), w.book, nil)
	if st != 422 || res["family"] != "AK-RULE" {
		t.Fatalf("%d %v", st, res)
	}
	if w.obs.get("c", "post_event/refused") != 1 || w.obs.get("e", fmt.Sprintf("AK-RULE/%v", res["code"])) != 1 {
		t.Fatalf("%v %v", w.obs.commands, w.obs.exceptions)
	}

	// an authorization denial is a refusal too (it is an accounting control saying no, not an outage)
	w.denied.Store("poster|GL_POSTING_EXECUTE", true)
	if st, _ := w.kenv.post(id, w.book, nil); st != 403 {
		t.Fatalf("%d", st)
	}
	if w.obs.get("c", "post_event/refused") != 2 {
		t.Fatalf("%v", w.obs.commands)
	}

	// a reconciliation run reports the status it ended in
	if st, b := w.recon("AR_GL", "120.00", "obs"); st != 201 {
		t.Fatalf("%d %v", st, b)
	}
	if w.obs.get("c", "reconciliation_run/ok") != 1 || w.obs.get("r", "AR_GL/MATCHED") != 1 {
		t.Fatalf("%v %v", w.obs.commands, w.obs.recon)
	}
	if w.obs.slowest <= 0 {
		t.Fatal("durations are measured")
	}
}
