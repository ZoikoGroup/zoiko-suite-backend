package recon_test

// Wave 8 chaos tests for reconciliation: store faults in the middle of Evaluate. A failed evaluation leaves the run
// OPEN and retryable, a re-evaluation produces the same variances once (repeat_count semantics), and reconciliation
// never changes a fact it read.

import (
	"context"
	"encoding/json"
	"errors"
	"sort"
	"testing"
	"time"

	"zoiko.io/connectivity-api-bridge-svc/internal/recon"
)

type chaosMode int

const (
	chaosOK         chaosMode = iota
	chaosFailBefore           // the write is not applied
	chaosFailAfter            // the write COMMITS but the caller sees an error
)

var errChaos = errors.New("chaos: injected store failure")

// chaosRepo overrides MutateRun (the claim, the evaluation commit and the release) and the fact reads.
type chaosRepo struct {
	recon.Repo
	mutN     int
	mutateFn func(n int, kind string) chaosMode
	readN    map[string]int
	readFn   func(name string, n int) error
}

func (c *chaosRepo) MutateRun(ctx context.Context, id string, fn func(tx *recon.RunTx) error) (*recon.Run, error) {
	c.mutN++
	n, hook := c.mutN, c.mutateFn
	mode := chaosOK
	wrapped := func(tx *recon.RunTx) error {
		if err := fn(tx); err != nil {
			return err
		}
		kind := ""
		if len(tx.Event) > 0 {
			kind = tx.Event[0].Kind
		}
		if hook != nil {
			mode = hook(n, kind)
		}
		if mode == chaosFailBefore {
			return errChaos
		}
		return nil
	}
	r, err := c.Repo.MutateRun(ctx, id, wrapped)
	if err == nil && mode == chaosFailAfter {
		return nil, errChaos
	}
	return r, err
}

func (c *chaosRepo) read(name string) error {
	if c.readN == nil {
		c.readN = map[string]int{}
	}
	c.readN[name]++
	if c.readFn != nil {
		return c.readFn(name, c.readN[name])
	}
	return nil
}

func (c *chaosRepo) InboundCounts(ctx context.Context, conn string, from, to time.Time) (map[string]int, error) {
	if err := c.read("inbound"); err != nil {
		return nil, err
	}
	return c.Repo.InboundCounts(ctx, conn, from, to)
}

func (c *chaosRepo) Attempts(ctx context.Context, conn string, from, to time.Time, limit int) ([]recon.AttemptFact, error) {
	if err := c.read("attempts"); err != nil {
		return nil, err
	}
	return c.Repo.Attempts(ctx, conn, from, to, limit)
}

func (c *chaosRepo) Batches(ctx context.Context, conn string, from, to time.Time, limit int) ([]recon.BatchFact, error) {
	if err := c.read("batches"); err != nil {
		return nil, err
	}
	return c.Repo.Batches(ctx, conn, from, to, limit)
}

// chaosFixture states exactly what the platform holds and returns the request that compares against it. Every basis
// that reads facts is included, so every read path is exercised.
func chaosFixture(e *env) recon.RunRequest {
	old := e.now.Add(-72 * time.Hour)
	d := 10
	e.repo.Inbound = map[string]int{"received": 10}
	e.repo.AttemptsF = []recon.AttemptFact{
		{AttemptID: "a-unknown", State: "UNKNOWN", CreatedAt: old, UpdatedAt: old},
		{AttemptID: "a-pending", State: "PENDING", CreatedAt: old, UpdatedAt: old},
		{AttemptID: "a-contra", State: "SETTLED", NeedsReconcile: true, LatestReceiptState: "FAILED", CreatedAt: old, UpdatedAt: old},
		{AttemptID: "a-fine", State: "SETTLED", LatestReceiptState: "SETTLED", CreatedAt: old, UpdatedAt: old},
	}
	e.repo.BatchesF = []recon.BatchFact{
		{BatchID: "b2", ExternalBatchID: "X2", State: "PARTIAL", DeclaredRows: &d, AdmittedRows: 8, RejectedRows: 1, VarianceRows: 1, ProcessingDone: true, ReceivedAt: e.now.Add(-48 * time.Hour)},
		{BatchID: "b3", ExternalBatchID: "X3", State: "PROCESSING", PendingRows: 3, ReceivedAt: e.now.Add(-48 * time.Hour)},
	}
	e.repo.Late = 3
	cut := t0.Add(12 * time.Hour)
	return e.req([]string{"COUNT", "STATE", "EVIDENCE", "TIME"}, &recon.External{Source: "PROVIDER_STATEMENT", Counts: map[string]int{"received": 11}, Cutoff: &cut})
}

// chaosFactsDigest is every fact the platform holds that reconciliation reads, byte for byte.
func chaosFactsDigest(r *recon.MemRepo) string {
	b, _ := json.Marshal(map[string]any{"in": r.Inbound, "out": r.Outbound, "refs": r.Refs, "attempts": r.AttemptsF, "batches": r.BatchesF, "late": r.Late})
	return string(b)
}

// chaosShape is the identity of the variances found: type, severity and fingerprint, sorted. Counters are separate.
func chaosShape(vs []recon.Variance) []string {
	var out []string
	for _, v := range vs {
		out = append(out, v.Type+"|"+v.Severity+"|"+v.Fingerprint)
	}
	sort.Strings(out)
	return out
}

func sameShape(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func chaosEventKinds(t *testing.T, e *env, id string) []string {
	t.Helper()
	evs, err := e.repo.RunEvents(ctx(tenantA), id)
	if err != nil {
		t.Fatal(err)
	}
	var k []string
	for _, ev := range evs {
		k = append(k, ev.Kind)
	}
	return k
}

// chaosReference is what a fault-free evaluation of the fixture finds.
func chaosReference(t *testing.T) (shape []string, state string) {
	t.Helper()
	e := newEnv()
	r := e.eval(t, e.run(t, chaosFixture(e)))
	vs := e.variances(t)
	for _, v := range vs {
		if v.RepeatCount != 1 {
			t.Fatalf("reference run repeated: %+v", v)
		}
	}
	return chaosShape(vs), r.State
}

func countOut(r *recon.MemRepo, typ string) int {
	n := 0
	for _, o := range r.Out {
		if o.Type == typ {
			n++
		}
	}
	return n
}

// 11a. The commit of the evaluation fails.
func TestChaos_EvaluateCommitFails_RunStaysOpenAndRetriesToTheSameVariancesOnce(t *testing.T) {
	wantShape, wantState := chaosReference(t)
	if len(wantShape) < 5 || wantState != recon.RunEscalated {
		t.Fatalf("fixture is too weak to prove anything: %v %s", wantShape, wantState)
	}

	t.Run("write not applied, twice, then recovers", func(t *testing.T) {
		e := newEnv()
		req := chaosFixture(e)
		r := e.run(t, req)
		digest := chaosFactsDigest(e.repo)
		before := chaosEventKinds(t, e, r.RunID)
		failures := 0
		e.svc.Repo = &chaosRepo{Repo: e.repo, mutateFn: func(_ int, kind string) chaosMode {
			if kind == "EVALUATED" && failures < 2 {
				failures++
				return chaosFailBefore
			}
			return chaosOK
		}}
		for i := 0; i < 2; i++ {
			if _, err := e.svc.Evaluate(ctx(tenantA), tenantA, r.RunID); !errors.Is(err, errChaos) {
				t.Fatalf("attempt %d: the failed commit must reach the caller: %v", i, err)
			}
			got, _ := e.repo.GetRun(ctx(tenantA), r.RunID)
			if got.State != recon.RunOpen || got.LeaseUntil == nil || got.EvaluatedAt != nil || got.VarianceCount != 0 {
				t.Fatalf("attempt %d: a failed evaluation leaves the run OPEN and leased: %+v", i, got)
			}
			if len(e.variances(t)) != 0 || countOut(e.repo, "integration.reconciliation.completed") != 0 {
				t.Fatalf("attempt %d: a failed commit left findings or events behind", i)
			}
			// no hot loop: while the lease holds nothing is due
			if n, _ := e.svc.DriveDue(context.Background(), 10); n != 0 {
				t.Fatalf("attempt %d: a leased run was driven again at once", i)
			}
			e.now = e.now.Add(6 * time.Minute) // past the lease
		}
		if n, err := e.svc.DriveDue(context.Background(), 10); n != 1 || err != nil {
			t.Fatalf("the worker did not retry the stranded run: %d %v", n, err)
		}
		got, _ := e.repo.GetRun(ctx(tenantA), r.RunID)
		vs := e.variances(t)
		if got.State != wantState || !sameShape(chaosShape(vs), wantShape) {
			t.Fatalf("re-evaluation differs from a fault-free one: %s %v", got.State, chaosShape(vs))
		}
		for _, v := range vs {
			if v.RepeatCount != 1 || v.RunID != r.RunID || v.LastRunID != r.RunID {
				t.Fatalf("a failed attempt was counted as a repeat: %+v", v)
			}
		}
		if n := countOut(e.repo, "integration.reconciliation.completed"); n != 1 {
			t.Fatalf("%d completed events", n)
		}
		if d := chaosFactsDigest(e.repo); d != digest {
			t.Fatalf("reconciliation changed a fact it read:\n%s\n%s", digest, d)
		}
		kinds := chaosEventKinds(t, e, r.RunID)
		if len(kinds) != len(before)+1 || kinds[len(kinds)-1] != "EVALUATED" || kinds[0] != "REQUESTED" {
			t.Fatalf("history %v", kinds)
		}
	})

	t.Run("write applied but error returned", func(t *testing.T) {
		e := newEnv()
		r := e.run(t, chaosFixture(e))
		digest := chaosFactsDigest(e.repo)
		failed := false
		e.svc.Repo = &chaosRepo{Repo: e.repo, mutateFn: func(_ int, kind string) chaosMode {
			if kind == "EVALUATED" && !failed {
				failed = true
				return chaosFailAfter
			}
			return chaosOK
		}}
		if _, err := e.svc.Evaluate(ctx(tenantA), tenantA, r.RunID); !errors.Is(err, errChaos) {
			t.Fatalf("%v", err)
		}
		committed, _ := e.repo.GetRun(ctx(tenantA), r.RunID)
		if committed.State != wantState || !sameShape(chaosShape(e.variances(t)), wantShape) {
			t.Fatalf("the committed evaluation is not visible: %+v", committed)
		}
		// the caller retries, and so does the worker: nothing is evaluated or counted a second time
		for i := 0; i < 3; i++ {
			e.now = e.now.Add(6 * time.Minute)
			got, err := e.svc.Evaluate(ctx(tenantA), tenantA, r.RunID)
			if err != nil || got.Version != committed.Version || got.State != committed.State {
				t.Fatalf("retry %d re-evaluated: %v %+v", i, err, got)
			}
			if n, _ := e.svc.DriveDue(context.Background(), 10); n != 0 {
				t.Fatalf("an evaluated run is due again")
			}
		}
		for _, v := range e.variances(t) {
			if v.RepeatCount != 1 {
				t.Fatalf("a retry of a committed evaluation counted as a repeat: %+v", v)
			}
		}
		if n := countOut(e.repo, "integration.reconciliation.completed"); n != 1 {
			t.Fatalf("%d completed events", n)
		}
		if kinds := chaosEventKinds(t, e, r.RunID); len(kinds) != 2 {
			t.Fatalf("history %v", kinds)
		}
		if chaosFactsDigest(e.repo) != digest {
			t.Fatal("reconciliation changed a fact it read")
		}
	})

	t.Run("the claim write fails", func(t *testing.T) {
		for _, mode := range []chaosMode{chaosFailBefore, chaosFailAfter} {
			e := newEnv()
			r := e.run(t, chaosFixture(e))
			failed := false
			e.svc.Repo = &chaosRepo{Repo: e.repo, mutateFn: func(n int, _ string) chaosMode {
				if n == 1 && !failed {
					failed = true
					return mode
				}
				return chaosOK
			}}
			if _, err := e.svc.Evaluate(ctx(tenantA), tenantA, r.RunID); !errors.Is(err, errChaos) {
				t.Fatalf("%v", err)
			}
			if got, _ := e.repo.GetRun(ctx(tenantA), r.RunID); got.State != recon.RunOpen || got.VarianceCount != 0 {
				t.Fatalf("a failed claim evaluated nothing: %+v", got)
			}
			e.now = e.now.Add(6 * time.Minute)
			if _, err := e.svc.Evaluate(ctx(tenantA), tenantA, r.RunID); err != nil {
				t.Fatal(err)
			}
			if !sameShape(chaosShape(e.variances(t)), wantShape) {
				t.Fatalf("mode %d: %v", mode, chaosShape(e.variances(t)))
			}
		}
	})
}

// 11b. The reads of the platform's facts fail in the middle of the comparison.
func TestChaos_FactReadFails_RunStaysOpenAndNothingIsWrittenOrFixed(t *testing.T) {
	wantShape, wantState := chaosReference(t)
	for _, read := range []string{"inbound", "attempts", "batches"} {
		for _, releaseFails := range []bool{false, true} {
			name := read + " read fails"
			if releaseFails {
				name += ", and so does releasing the lease"
			}
			t.Run(name, func(t *testing.T) {
				e := newEnv()
				r := e.run(t, chaosFixture(e))
				digest := chaosFactsDigest(e.repo)
				kinds0 := chaosEventKinds(t, e, r.RunID)
				broken := true
				cr := &chaosRepo{Repo: e.repo, readFn: func(name string, _ int) error {
					if broken && name == read {
						return errors.New("db down")
					}
					return nil
				}}
				if releaseFails {
					cr.mutateFn = func(n int, _ string) chaosMode {
						if broken && n == 2 { // claim is call 1, the release is call 2
							return chaosFailBefore
						}
						return chaosOK
					}
				}
				e.svc.Repo = cr
				if _, err := e.svc.Evaluate(ctx(tenantA), tenantA, r.RunID); !errors.Is(err, recon.ErrDependency) {
					t.Fatalf("%v", err)
				}
				got, _ := e.repo.GetRun(ctx(tenantA), r.RunID)
				if got.State != recon.RunOpen || got.EvaluatedAt != nil || got.Result != nil || got.VarianceCount != 0 {
					t.Fatalf("a failed read must leave the run OPEN and unevaluated: %+v", got)
				}
				if releaseFails != (got.LeaseUntil != nil) {
					t.Fatalf("lease: %+v", got.LeaseUntil)
				}
				if len(e.variances(t)) != 0 || len(e.repo.Out) != 1 || len(chaosEventKinds(t, e, r.RunID)) != len(kinds0) {
					t.Fatal("a failed comparison wrote findings, events or history")
				}
				// not due again until its backoff (or lease) has run out: no hot loop
				if n, _ := e.svc.DriveDue(context.Background(), 10); n != 0 {
					t.Fatal("hot loop: driven again with no time elapsed")
				}
				broken = false // the store recovers
				if releaseFails {
					e.now = e.now.Add(6 * time.Minute)
				} else {
					e.now = e.now.Add(31 * time.Second)
				}
				if n, err := e.svc.DriveDue(context.Background(), 10); n != 1 || err != nil {
					t.Fatalf("the worker did not retry: %d %v", n, err)
				}
				final, _ := e.repo.GetRun(ctx(tenantA), r.RunID)
				vs := e.variances(t)
				if final.State != wantState || !sameShape(chaosShape(vs), wantShape) {
					t.Fatalf("re-evaluation differs from a fault-free one: %s %v", final.State, chaosShape(vs))
				}
				for _, v := range vs {
					if v.RepeatCount != 1 {
						t.Fatalf("%+v", v)
					}
				}
				if chaosFactsDigest(e.repo) != digest {
					t.Fatal("reconciliation changed a fact it read")
				}
			})
		}
	}
}

// 11c. repeat_count grows once per RECORDED evaluation, whatever faults happened around it.
func TestChaos_RepeatCountCountsRecordedEvaluationsOnly(t *testing.T) {
	wantShape, _ := chaosReference(t)
	e := newEnv()
	req := chaosFixture(e)
	digest := chaosFactsDigest(e.repo)
	cr := &chaosRepo{Repo: e.repo}
	e.svc.Repo = cr
	evalFaults := 0
	cr.mutateFn = func(_ int, kind string) chaosMode {
		if kind != "EVALUATED" {
			return chaosOK
		}
		evalFaults++
		switch evalFaults {
		case 1, 2: // run 1: not applied twice
			return chaosFailBefore
		case 4: // run 2: applied, error returned
			return chaosFailAfter
		}
		return chaosOK
	}
	evaluate := func(r *recon.Run) {
		for i := 0; i < 5; i++ {
			e.now = e.now.Add(6 * time.Minute)
			if got, err := e.svc.Evaluate(ctx(tenantA), tenantA, r.RunID); err == nil && got.State != recon.RunOpen {
				return
			}
			if got, _ := e.repo.GetRun(ctx(tenantA), r.RunID); got.State != recon.RunOpen {
				return // committed although the caller saw an error
			}
		}
		t.Fatal("run never evaluated")
	}
	r1 := e.run(t, req)
	evaluate(r1)
	if vs := e.variances(t); !sameShape(chaosShape(vs), wantShape) || vs[0].RepeatCount != 1 {
		t.Fatalf("%+v", vs)
	}
	req2 := req
	req2.PeriodTo = req2.PeriodTo.Add(time.Hour)
	r2 := e.run(t, req2)
	evaluate(r2)
	vs := e.variances(t)
	if len(vs) != len(wantShape) {
		t.Fatalf("a repeated finding was stored again: %d variances, want %d", len(vs), len(wantShape))
	}
	for _, v := range vs {
		if v.RepeatCount != 2 || v.RunID != r1.RunID || v.LastRunID != r2.RunID {
			t.Fatalf("run 2 (one failed and one committed-with-error attempt) must count exactly once: %+v", v)
		}
	}
	// the caller of run 2 retries after the committed-with-error: still once
	if _, err := e.svc.Evaluate(ctx(tenantA), tenantA, r2.RunID); err != nil {
		t.Fatal(err)
	}
	for _, v := range e.variances(t) {
		if v.RepeatCount != 2 {
			t.Fatalf("a retry counted again: %+v", v)
		}
	}
	req3 := req
	req3.PeriodTo = req3.PeriodTo.Add(2 * time.Hour)
	r3 := e.run(t, req3)
	evaluate(r3)
	for _, v := range e.variances(t) {
		if v.RepeatCount != 3 || v.LastRunID != r3.RunID {
			t.Fatalf("%+v", v)
		}
	}
	if chaosFactsDigest(e.repo) != digest {
		t.Fatal("reconciliation changed a fact it read")
	}
	if n := countOut(e.repo, "integration.reconciliation.completed"); n != 3 {
		t.Fatalf("%d completed events for 3 runs", n)
	}
}
