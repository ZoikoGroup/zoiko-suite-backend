package batch_test

// Wave 8 chaos tests for the batch engine: store faults in the middle of row admission, and two workers racing on
// one batch while faults are injected. Nothing lost, nothing duplicated, nothing silently fixed.

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"zoiko.io/connectivity-api-bridge-svc/internal/batch"
	"zoiko.io/connectivity-api-bridge-svc/internal/inbound"
)

type chaosMode int

const (
	chaosOK         chaosMode = iota
	chaosFailBefore           // the write is not applied
	chaosFailAfter            // the write COMMITS but the caller sees an error
)

var errChaos = errors.New("chaos: injected store failure")

// chaosInbound wraps the inbound repo the batch engine admits rows through (Insert is the row's exchange write).
type chaosInbound struct {
	inbound.Repo
	mu       sync.Mutex
	n        int
	insertFn func(n int) chaosMode
}

func (c *chaosInbound) Insert(ctx context.Context, e *inbound.Exchange, ev inbound.Event, out []inbound.OutEvent) (*inbound.Exchange, error) {
	c.mu.Lock()
	c.n++
	n, fn := c.n, c.insertFn
	c.mu.Unlock()
	mode := chaosOK
	if fn != nil {
		mode = fn(n)
	}
	if mode == chaosFailBefore {
		return nil, errChaos
	}
	ex, err := c.Repo.Insert(ctx, e, ev, out)
	if err == nil && mode == chaosFailAfter {
		return nil, errChaos
	}
	return ex, err
}

// chaosBatchRepo wraps the batch repo: AddRows (the progress save) and Mutate (the batch record).
type chaosBatchRepo struct {
	batch.Repo
	mu       sync.Mutex
	addN     int
	mutN     int
	addFn    func(n int) chaosMode
	mutateFn func(n int, kind string) chaosMode
}

func (c *chaosBatchRepo) AddRows(ctx context.Context, rows []batch.Row) error {
	c.mu.Lock()
	c.addN++
	n, fn := c.addN, c.addFn
	c.mu.Unlock()
	mode := chaosOK
	if fn != nil {
		mode = fn(n)
	}
	if mode == chaosFailBefore {
		return errChaos
	}
	err := c.Repo.AddRows(ctx, rows)
	if err == nil && mode == chaosFailAfter {
		return errChaos
	}
	return err
}

func (c *chaosBatchRepo) Mutate(ctx context.Context, id string, fn func(tx *batch.Tx) error) (*batch.Batch, error) {
	c.mu.Lock()
	c.mutN++
	n, hook := c.mutN, c.mutateFn
	c.mu.Unlock()
	mode := chaosOK
	wrapped := func(tx *batch.Tx) error {
		if err := fn(tx); err != nil {
			return err
		}
		kind := ""
		if tx.Event != nil {
			kind = tx.Event.Kind
		}
		if hook != nil {
			mode = hook(n, kind)
		}
		if mode == chaosFailBefore {
			return errChaos
		}
		return nil
	}
	b, err := c.Repo.Mutate(ctx, id, wrapped)
	if err == nil && mode == chaosFailAfter {
		return nil, errChaos
	}
	return b, err
}

func chaosChain(t *testing.T, evs []batch.Event) string {
	t.Helper()
	if len(evs) == 0 {
		t.Fatal("a batch without history")
	}
	cur := evs[0].To
	for i, ev := range evs[1:] {
		if ev.From != cur {
			t.Fatalf("history gap before event %d (%s): in %s, event says from %s", i+1, ev.Kind, cur, ev.From)
		}
		cur = ev.To
	}
	return cur
}

func chaosPrefix(t *testing.T, before, after []batch.Event) {
	t.Helper()
	if len(after) < len(before) {
		t.Fatalf("history lost events: %d -> %d", len(before), len(after))
	}
	for i := range before {
		b, _ := json.Marshal(before[i])
		a, _ := json.Marshal(after[i])
		if string(b) != string(a) {
			t.Fatalf("history event %d rewritten: %s -> %s", i, b, a)
		}
	}
}

// chaosFileDigest is what the delivered files were: it must never change under any fault.
func (e *env) chaosFileDigest(id string) string {
	e.t.Helper()
	fs, err := e.repo.Files(ctxFor(tenantA), id, true)
	if err != nil {
		e.t.Fatal(err)
	}
	d := ""
	for _, f := range fs {
		d += f.Name + ":" + f.ActualSHA256 + ":" + f.DeclaredSHA256 + ":" + sha(f.Content) + ";"
	}
	return d
}

func (e *env) events(id string) []batch.Event {
	e.t.Helper()
	evs, err := e.repo.Events(ctxFor(tenantA), id)
	if err != nil {
		e.t.Fatal(err)
	}
	return evs
}

// chaosRowIdentity is what a row is: number, hash and the exchange it became. States change legitimately; these never.
func chaosRowIdentity(rs []batch.Row) map[int]string {
	m := map[int]string{}
	for _, r := range rs {
		m[r.RowNo] = r.RowID + "|" + r.SourceRowHash + "|" + r.ExchangeID
	}
	return m
}

// chaosAssertOnce proves the hard invariants after recovery: exactly n rows, none skipped, none duplicated, every
// row's exchange delivered to the domain exactly once, and the batch complete and reconciled.
func (e *env) chaosAssertOnce(id string, n int) []batch.Row {
	e.t.Helper()
	t := e.t
	rs := e.rowsOf(tenantA, id)
	if len(rs) != n {
		t.Fatalf("%d rows for %d in the file", len(rs), n)
	}
	exch := map[string]bool{}
	for i, r := range rs {
		if r.RowNo != i+1 {
			t.Fatalf("row %d is missing or out of order (got %d)", i+1, r.RowNo)
		}
		if r.ExchangeID == "" {
			t.Fatalf("row %d never reached the pipeline: %+v", r.RowNo, r)
		}
		x, err := e.inRepo.Get(ctxFor(tenantA), r.ExchangeID)
		if err != nil || x.State != inbound.StateHandedOff || x.DomainOutcome != inbound.DomainAccepted {
			t.Fatalf("row %d's exchange was not delivered: %v %+v", r.RowNo, err, x)
		}
		if exch[r.ExchangeID] {
			t.Fatalf("two rows share exchange %s", r.ExchangeID)
		}
		exch[r.ExchangeID] = true
	}
	all, _ := e.inRepo.List(ctxFor(tenantA), inbound.Filter{})
	real := 0
	for _, x := range all {
		if x.State != inbound.StateDuplicate {
			real++
		}
	}
	if real != n {
		t.Fatalf("%d admitted exchanges for %d rows", real, n)
	}
	if hits := atomic.LoadInt32(&e.domHits); int(hits) != n {
		t.Fatalf("%d domain deliveries for %d rows", hits, n)
	}
	b := e.get(tenantA, id)
	if !b.ProcessingDone || b.AdmittedRows != n || b.ParsedRows != n || b.State != batch.StateReconciled {
		t.Fatalf("batch not complete and reconciled: %+v", b)
	}
	return rs
}

// recover heals the stores and drives the batch to the end, the way the worker would.
func (e *env) chaosRecover(id string) {
	e.t.Helper()
	for i := 0; i < 5; i++ {
		e.advance(3 * time.Minute) // past the backoff and any lease
		if _, err := e.svc.DriveDue(context.Background(), 10); err != nil {
			e.t.Fatalf("recovery run %d: %v", i, err)
		}
		if e.get(tenantA, id).ProcessingDone {
			break
		}
	}
	e.settle(e.get(tenantA, id))
}

// 9. A store fault in the middle of row processing, plain and commit-then-error, on each write the engine makes.
func TestChaos_RowProcessingFault_BatchNotCompleteThenResumesWithEachRowOnce(t *testing.T) {
	const n = 35
	type fault struct {
		name         string
		inbound      func(k int) chaosMode
		addRows      func(k int) chaosMode
		notDoneAfter bool // the fault leaves the batch visibly incomplete
	}
	on := func(k int, m chaosMode) func(int) chaosMode {
		return func(i int) chaosMode {
			if i == k {
				return m
			}
			return chaosOK
		}
	}
	faults := []fault{
		{"row insert fails, not applied", on(17, chaosFailBefore), nil, true},
		{"row insert commits then errors", on(17, chaosFailAfter), nil, true},
		{"progress save fails, not applied", nil, on(2, chaosFailBefore), true},
		{"progress save commits then errors", nil, on(2, chaosFailAfter), true},
		{"first row insert commits then errors", on(1, chaosFailAfter), nil, true},
		{"last row insert commits then errors", on(n, chaosFailAfter), nil, true},
	}
	for _, f := range faults {
		t.Run(f.name, func(t *testing.T) {
			e := newEnv(t)
			cfg := batch.DefaultConfig()
			cfg.RowChunk = 10
			e.svc.Cfg = cfg
			c := e.activeConn(tenantA, e.publish(nil), "acc-a")
			e.target()
			o := e.deliver(simple(c, "B-CH", n))
			id := o.Batch.BatchID
			digest := e.chaosFileDigest(id)
			received := e.events(id)

			ci := &chaosInbound{Repo: e.inRepo, insertFn: f.inbound}
			cb := &chaosBatchRepo{Repo: e.repo, addFn: f.addRows}
			e.in.Repo, e.svc.Repo = ci, cb
			if _, err := e.svc.Process(context.Background(), tenantA, id); err == nil {
				t.Fatal("the injected failure was swallowed")
			}
			b := e.get(tenantA, id)
			if f.notDoneAfter && (b.ProcessingDone || b.State == batch.StateReconciled || b.State == batch.StatePartial) {
				t.Fatalf("a batch interrupted mid-rows must not be complete: %+v", b)
			}
			if b.NextProcessAt == nil {
				t.Fatalf("an interrupted batch must be scheduled to retry: %+v", b)
			}
			if b.State == batch.StateRejected || b.State == batch.StateQuarantined {
				t.Fatalf("a storage fault must never reject or quarantine a batch: %+v", b)
			}
			saved := chaosRowIdentity(e.rowsOf(tenantA, id))
			if len(saved) >= n {
				t.Fatalf("%d rows saved at the fault", len(saved))
			}
			// nothing is delivered for rows that were never durably admitted; the domain is not touched while rows are admitted
			if atomic.LoadInt32(&e.domHits) != 0 {
				t.Fatal("rows were delivered before processing finished")
			}

			e.in.Repo, e.svc.Repo = e.inRepo, e.repo // the store recovers
			e.chaosRecover(id)
			rs := e.chaosAssertOnce(id, n)

			// nothing silently fixed: the files are the files, earlier rows are the same rows, history only grew
			if d := e.chaosFileDigest(id); d != digest {
				t.Fatalf("delivered file content changed: %s -> %s", digest, d)
			}
			after := chaosRowIdentity(rs)
			for no, was := range saved {
				if after[no] != was {
					t.Fatalf("row %d was rewritten by recovery: %s -> %s", no, was, after[no])
				}
			}
			evs := e.events(id)
			chaosChain(t, evs)
			chaosPrefix(t, received, evs)
			if evs[len(evs)-1].To != batch.StateReconciled {
				t.Fatalf("history ends in %s", evs[len(evs)-1].To)
			}
		})
	}
}

// 9b. The final batch record write (ROWS_ADMITTED) fails.
func TestChaos_RowsAdmittedRecordFails_ResumeAdmitsNothingTwice(t *testing.T) {
	for _, mode := range []chaosMode{chaosFailBefore, chaosFailAfter} {
		name := "write not applied"
		if mode == chaosFailAfter {
			name = "write applied but error returned"
		}
		t.Run(name, func(t *testing.T) {
			const n = 25
			e := newEnv(t)
			c := e.activeConn(tenantA, e.publish(nil), "acc-a")
			e.target()
			o := e.deliver(simple(c, "B-FIN", n))
			id := o.Batch.BatchID
			failed := false
			e.svc.Repo = &chaosBatchRepo{Repo: e.repo, mutateFn: func(_ int, kind string) chaosMode {
				if kind == "ROWS_ADMITTED" && !failed {
					failed = true
					return mode
				}
				return chaosOK
			}}
			if _, err := e.svc.Process(context.Background(), tenantA, id); err == nil {
				t.Fatal("the failed record was swallowed")
			}
			if b := e.get(tenantA, id); mode == chaosFailBefore && b.ProcessingDone {
				t.Fatalf("not applied, yet marked complete: %+v", b)
			}
			e.svc.Repo = e.repo
			e.chaosRecover(id)
			rs := e.chaosAssertOnce(id, n)
			for _, r := range rs {
				if r.State != batch.RowAccepted {
					t.Fatalf("row %d is %s after a clean recovery", r.RowNo, r.State)
				}
			}
			evs := e.events(id)
			chaosChain(t, evs)
		})
	}
}

// BUG (non-test code, see report): a row whose exchange committed but whose row record was lost is, on resume,
// re-admitted through the dedupe path and recorded as a DUPLICATE row pointing at its own exchange. It is then never
// refreshed, so the batch is reported RECONCILED with accepted_rows short and duplicate_rows > 0 although the file had
// no duplicate. The row is delivered once (the hard invariants hold) but its report is wrong.
func TestChaos_BUG_ResumedRowIsNotReportedAsADuplicateOfItself(t *testing.T) {
	const n = 20
	e := newEnv(t)
	cfg := batch.DefaultConfig()
	cfg.RowChunk = 10
	e.svc.Cfg = cfg
	c := e.activeConn(tenantA, e.publish(nil), "acc-a")
	e.target()
	o := e.deliver(simple(c, "B-DUP", n))
	id := o.Batch.BatchID
	e.in.Repo = &chaosInbound{Repo: e.inRepo, insertFn: func(k int) chaosMode {
		if k == 7 {
			return chaosFailAfter
		}
		return chaosOK
	}}
	if _, err := e.svc.Process(context.Background(), tenantA, id); err == nil {
		t.Fatal("the injected failure was swallowed")
	}
	e.in.Repo = e.inRepo
	e.chaosRecover(id)
	e.chaosAssertOnce(id, n)
	b := e.get(tenantA, id)
	if b.DuplicateRows != 0 || b.AcceptedRows != n {
		t.Fatalf("the file had no duplicate rows, yet accepted=%d duplicate=%d (batch %s)", b.AcceptedRows, b.DuplicateRows, b.State)
	}
	for _, r := range e.rowsOf(tenantA, id) {
		if r.State == batch.RowDuplicate {
			t.Fatalf("row %d is reported as a duplicate of its own exchange: %+v", r.RowNo, r)
		}
	}
}

// 10. Two workers process the same batch concurrently while faults are injected.
func TestChaos_ConcurrentWorkersWithFaults_EachRowAdmittedOnce(t *testing.T) {
	const n = 60
	e := newEnv(t)
	cfg := batch.DefaultConfig()
	cfg.RowChunk = 7
	e.svc.Cfg = cfg
	c := e.activeConn(tenantA, e.publish(nil), "acc-a")
	e.target()
	o := e.deliver(simple(c, "B-CC2", n))
	id := o.Batch.BatchID
	digest := e.chaosFileDigest(id)
	received := e.events(id)

	var healthy atomic.Bool
	e.in.Repo = &chaosInbound{Repo: e.inRepo, insertFn: func(k int) chaosMode {
		if healthy.Load() {
			return chaosOK
		}
		switch {
		case k%9 == 4:
			return chaosFailBefore
		case k%13 == 6:
			return chaosFailAfter
		}
		return chaosOK
	}}
	e.svc.Repo = &chaosBatchRepo{Repo: e.repo, addFn: func(k int) chaosMode {
		if healthy.Load() {
			return chaosOK
		}
		if k%5 == 2 {
			return chaosFailAfter
		}
		return chaosOK
	}}

	for round := 0; round < 40 && !e.get(tenantA, id).ProcessingDone; round++ {
		if round == 6 {
			healthy.Store(true) // the faults stop eventually
		}
		e.advance(3 * time.Minute)
		var wg sync.WaitGroup
		for w := 0; w < 4; w++ {
			wg.Add(1)
			go func(w int) {
				defer wg.Done()
				if w%2 == 0 {
					_, _ = e.svc.Process(context.Background(), tenantA, id)
				} else {
					_, _ = e.svc.DriveDue(context.Background(), 10)
				}
			}(w)
		}
		wg.Wait()
	}
	if !e.get(tenantA, id).ProcessingDone {
		t.Fatalf("the batch never completed: %+v", e.get(tenantA, id))
	}
	e.in.Repo, e.svc.Repo = e.inRepo, e.repo
	e.settle(e.get(tenantA, id))
	rs := e.chaosAssertOnce(id, n)
	if d := e.chaosFileDigest(id); d != digest {
		t.Fatalf("delivered file content changed")
	}
	seen := map[string]bool{}
	for _, r := range rs {
		if seen[r.SourceRowHash] {
			t.Fatalf("row hash %s admitted twice", r.SourceRowHash)
		}
		seen[r.SourceRowHash] = true
	}
	evs := e.events(id)
	chaosChain(t, evs)
	chaosPrefix(t, received, evs)
}
