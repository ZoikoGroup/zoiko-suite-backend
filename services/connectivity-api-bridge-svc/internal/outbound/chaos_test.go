package outbound_test

// Wave 8 chaos tests: composed and mid-flight faults. Nothing is lost, nothing is duplicated, nothing is silently
// fixed. Faults are injected by wrapping the Repo: a write may fail before it is applied (chaosFailBefore) or after
// it COMMITS while the caller sees an error (chaosFailAfter, the nastiest case).

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"zoiko.io/connectivity-api-bridge-svc/internal/outbound"
)

type chaosMode int

const (
	chaosOK chaosMode = iota
	chaosFailBefore
	chaosFailAfter
)

var errChaos = errors.New("chaos: injected store failure")

// chaosRepo embeds the real repo and overrides Reserve and Mutate. The hooks get the 1-based call number (and for
// Mutate the kind of event the transaction wrote, "" when it wrote none).
type chaosRepo struct {
	outbound.Repo
	mu        sync.Mutex
	reserveN  int
	mutateN   int
	reserveFn func(n int) chaosMode
	mutateFn  func(n int, kind string) chaosMode
}

func (c *chaosRepo) Reserve(ctx context.Context, a *outbound.Attempt, ev outbound.Event, out []outbound.OutEvent) (*outbound.Attempt, error) {
	c.mu.Lock()
	c.reserveN++
	n, fn := c.reserveN, c.reserveFn
	c.mu.Unlock()
	mode := chaosOK
	if fn != nil {
		mode = fn(n)
	}
	if mode == chaosFailBefore {
		return nil, errChaos
	}
	ex, err := c.Repo.Reserve(ctx, a, ev, out)
	if err == nil && mode == chaosFailAfter {
		return nil, errChaos
	}
	return ex, err
}

func (c *chaosRepo) Mutate(ctx context.Context, id string, fn func(tx *outbound.Tx) error) (*outbound.Attempt, error) {
	c.mu.Lock()
	c.mutateN++
	n, hook := c.mutateN, c.mutateFn
	c.mu.Unlock()
	mode := chaosOK
	wrapped := func(tx *outbound.Tx) error {
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
	a, err := c.Repo.Mutate(ctx, id, wrapped)
	if err == nil && mode == chaosFailAfter {
		return nil, errChaos
	}
	return a, err
}

// chaosChain proves the event list is a gapless chain from RESERVED and returns the state it ends in.
func chaosChain(t *testing.T, evs []outbound.Event) string {
	t.Helper()
	if len(evs) == 0 || evs[0].Kind != "RESERVED" || evs[0].To != outbound.StateReserved {
		t.Fatalf("history does not start at RESERVED: %+v", evs)
	}
	cur := evs[0].To
	for i, ev := range evs[1:] {
		if ev.From != cur {
			t.Fatalf("history has a gap before event %d (%s): was in %s, event says from %s", i+1, ev.Kind, cur, ev.From)
		}
		cur = ev.To
	}
	return cur
}

// chaosPrefix proves history is append-only: what was recorded before the fault is byte-for-byte unchanged.
func chaosPrefix(t *testing.T, before, after []outbound.Event) {
	t.Helper()
	if len(after) < len(before) {
		t.Fatalf("history lost events: %d -> %d", len(before), len(after))
	}
	for i := range before {
		b, _ := json.Marshal(before[i])
		a, _ := json.Marshal(after[i])
		if string(b) != string(a) {
			t.Fatalf("history event %d was rewritten: %s -> %s", i, b, a)
		}
	}
}

func (e *env) events(id string) []outbound.Event {
	e.t.Helper()
	evs, err := e.repo.Events(ctxFor(tenantA), id)
	if err != nil {
		e.t.Fatal(err)
	}
	return evs
}

// chaosImmutable proves the facts of an attempt that no fault may change.
func chaosImmutable(t *testing.T, was, now *outbound.Attempt) {
	t.Helper()
	if was.AttemptID != now.AttemptID || was.PayloadHash != now.PayloadHash || was.ProviderIdemRefHash != now.ProviderIdemRefHash ||
		was.DomainRequestRef != now.DomainRequestRef || was.ContractVersionID != now.ContractVersionID || was.ConnectionID != now.ConnectionID {
		t.Fatalf("an immutable fact of the attempt changed: %+v -> %+v", was, now)
	}
}

// 1. The provider accepted (2xx) but the write recording the answer fails.
func TestChaos_ProviderAcceptedButResultWriteFails_NeverFailed_ResolvedWithoutSecondSubmit(t *testing.T) {
	for _, applied := range []bool{false, true} {
		name := "write not applied"
		if applied {
			name = "write applied but error returned"
		}
		t.Run(name, func(t *testing.T) {
			e := newEnv(t)
			c := e.setup(variant{})
			a := e.reserve(c, "PAY-1")
			var healthy atomic.Bool
			cr := &chaosRepo{Repo: e.repo, mutateFn: func(_ int, kind string) chaosMode {
				if healthy.Load() || kind == "SUBMIT_STARTED" {
					return chaosOK
				}
				if applied {
					return chaosFailAfter
				}
				return chaosFailBefore
			}}
			e.svc.Repo = cr
			e.prov.set(answer(200, `{"payment_id":"P-1"}`))
			reserved := e.events(a.AttemptID)

			if _, err := e.svc.Submit(ctxFor(tenantA), domain, a.AttemptID, nil); err == nil {
				t.Fatal("a failed result write was swallowed: the caller must see a retryable error")
			}
			got := e.get(tenantA, a.AttemptID)
			if got.State == outbound.StateFailed {
				t.Fatalf("a provider 2xx with a lost write must never be FAILED: %+v", got)
			}
			want := outbound.StateSubmitting
			if applied {
				want = outbound.StateSettled
			}
			if got.State != want {
				t.Fatalf("state %s, want %s", got.State, want)
			}
			// the store is still down: more callers must not send again
			_, _ = e.svc.Submit(ctxFor(tenantA), domain, a.AttemptID, nil)
			if n := len(e.prov.posts()); n != 1 {
				t.Fatalf("the provider was hit %d times", n)
			}

			healthy.Store(true)
			e.advance(3 * time.Minute) // past the submit lease
			e.prov.set(statusIs("SETTLED"))
			if applied {
				if r, err := e.svc.Resolve(ctxFor(tenantA), other, a.AttemptID, outbound.ResolveInput{}); err != nil || r.State != outbound.StateSettled {
					t.Fatalf("%v %+v", err, r)
				}
				if len(e.prov.gets()) != 0 {
					t.Fatal("a final attempt was queried again")
				}
			} else {
				n, err := e.svc.DriveDue(context.Background(), 10)
				if err != nil || n != 1 {
					t.Fatalf("the worker did not pick up the stranded attempt: %d %v", n, err)
				}
			}
			_, _ = e.svc.Submit(ctxFor(tenantA), domain, a.AttemptID, nil)
			fin := e.get(tenantA, a.AttemptID)
			if fin.State != outbound.StateSettled || fin.SubmitCount != 1 {
				t.Fatalf("not resolved to the true outcome: %+v", fin)
			}
			if !applied && fin.ResolutionMethod != outbound.MethodStatusQuery {
				t.Fatalf("resolved by %s", fin.ResolutionMethod)
			}
			if n := len(e.prov.posts()); n != 1 {
				t.Fatalf("recovery submitted again: %d POSTs", n)
			}
			evs := e.events(a.AttemptID)
			if st := chaosChain(t, evs); st != outbound.StateSettled {
				t.Fatalf("history ends in %s", st)
			}
			chaosPrefix(t, reserved, evs)
			chaosImmutable(t, a, fin)
			if countType(e.repo.Types(), "integration.outbound.resolved") != 1 {
				t.Fatalf("resolved event count: %v", e.repo.Types())
			}
		})
	}
}

// 2. The process dies between Reserve and Submit.
func TestChaos_ProcessDiesBetweenReserveAndSubmit_NewInstanceSendsExactlyOnce(t *testing.T) {
	e := newEnv(t)
	c := e.setup(variant{})
	a := e.reserve(c, "PAY-1")
	reserved := e.events(a.AttemptID)
	// the old instance is gone; a brand-new one over the same store takes over
	fresh := &outbound.Service{Repo: e.repo, Conns: connAdapter{e.connSvc}, Secrets: e.secrets, Egress: e.svc.Egress, Cfg: outbound.DefaultConfig(),
		Clock: func() time.Time { return e.at() }, HTTP: &http.Client{Transport: e.prov}}
	if got := e.get(tenantA, a.AttemptID); got.State != outbound.StateReserved || got.SubmitCount != 0 || len(e.prov.calls) != 0 {
		t.Fatalf("a reserved attempt must survive the process: %+v", got)
	}
	got, err := fresh.Submit(ctxFor(tenantA), domain, a.AttemptID, nil)
	if err != nil || got.State != outbound.StateSettled {
		t.Fatalf("%v %+v", err, got)
	}
	// the zombie old instance and a duplicate delivery of the same command both lose
	_, _ = e.svc.Submit(ctxFor(tenantA), domain, a.AttemptID, nil)
	_, _ = fresh.Submit(ctxFor(tenantA), domain, a.AttemptID, nil)
	if n := len(e.prov.posts()); n != 1 {
		t.Fatalf("provider hit %d times", n)
	}
	evs := e.events(a.AttemptID)
	if chaosChain(t, evs) != outbound.StateSettled {
		t.Fatalf("%+v", evs)
	}
	chaosPrefix(t, reserved, evs)
	chaosImmutable(t, a, got)
}

// 3. A flapping provider with 20 callers hammering one attempt.
func TestChaos_FlappingProviderWithConcurrentCallers_OneAttemptSameKeyGaplessHistory(t *testing.T) {
	e := newEnv(t)
	c := e.setup(variant{resubmit: true})
	a := e.reserve(c, "PAY-1")
	reserved := e.events(a.AttemptID)
	var postN int32
	e.prov.set(func(c call) (int, string, error) {
		time.Sleep(2 * time.Millisecond)
		if c.method == http.MethodGet {
			return 503, `{}`, nil // the status endpoint is down throughout: only resubmission can finish this
		}
		switch atomic.AddInt32(&postN, 1) {
		case 1:
			return 0, "", errors.New("read: connection reset by peer")
		case 2:
			return 503, `{}`, nil
		case 3:
			return 0, "", timeoutErr{}
		}
		return 200, `{"payment_id":"P-1"}`, nil
	})
	var mu sync.Mutex
	var observed [][]*outbound.Attempt
	deadline := time.Now().Add(20 * time.Second)
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			var mine []*outbound.Attempt
			for time.Now().Before(deadline) {
				r, _ := e.svc.Submit(ctxFor(tenantA), domain, a.AttemptID, nil)
				if r != nil {
					mine = append(mine, r)
				}
				if r2, _ := e.svc.Resolve(ctxFor(tenantA), other, a.AttemptID, outbound.ResolveInput{}); r2 != nil {
					mine = append(mine, r2)
				}
				if r != nil && !r.Open() {
					break
				}
				time.Sleep(time.Millisecond)
			}
			mu.Lock()
			observed = append(observed, mine)
			mu.Unlock()
		}()
	}
	wg.Wait()
	fin := e.get(tenantA, a.AttemptID)
	if fin.State != outbound.StateSettled {
		t.Fatalf("did not converge: %+v", fin)
	}
	posts := e.prov.posts()
	if len(posts) != 4 || fin.SubmitCount != 4 {
		t.Fatalf("each flap is one resubmission of the same attempt: %d POSTs, submit_count %d", len(posts), fin.SubmitCount)
	}
	for _, p := range posts {
		if p.header.Get("Idempotency-Key") != a.AttemptID {
			t.Fatalf("a resubmission carried another provider key: %q", p.header.Get("Idempotency-Key"))
		}
	}
	for _, mine := range observed {
		var last int64
		for _, r := range mine {
			if r.AttemptID != a.AttemptID || r.Version < last || r.Version > fin.Version || r.SubmitCount > fin.SubmitCount {
				t.Fatalf("a caller saw a state inconsistent with the record: %+v (final v%d)", r, fin.Version)
			}
			if !r.Open() && (r.State != fin.State || r.Version != fin.Version) {
				t.Fatalf("a caller saw a final state that is not the final record: %+v vs %+v", r, fin)
			}
			last = r.Version
		}
	}
	evs := e.events(a.AttemptID)
	if chaosChain(t, evs) != outbound.StateSettled {
		t.Fatalf("%+v", evs)
	}
	if int64(len(evs)) != fin.Version {
		t.Fatalf("a transition was lost: %d events for version %d", len(evs), fin.Version)
	}
	started, unknown := 0, 0
	for _, ev := range evs {
		switch ev.Kind {
		case "SUBMIT_STARTED":
			started++
		case "UNKNOWN":
			unknown++
		}
	}
	if started != 4 || unknown != 3 {
		t.Fatalf("started %d unknown %d", started, unknown)
	}
	chaosPrefix(t, reserved, evs)
	chaosImmutable(t, a, fin)
	if list, _ := e.repo.List(ctxFor(tenantA), outbound.Filter{}); len(list) != 1 {
		t.Fatalf("%d attempts", len(list))
	}
}

// 4. 100 goroutines reserve the same logical request while the store fails some Reserve calls.
func TestChaos_ConcurrentReserveWithStoreFaults_OneAttemptOneId(t *testing.T) {
	e := newEnv(t)
	c := e.setup(variant{})
	cr := &chaosRepo{Repo: e.repo, reserveFn: func(n int) chaosMode {
		switch {
		case n > 18:
			return chaosOK
		case n%3 == 0:
			return chaosFailAfter // committed, caller sees an error
		}
		return chaosFailBefore
	}}
	e.svc.Repo = cr
	var mu sync.Mutex
	ids := map[string]int{}
	created := 0
	var wg sync.WaitGroup
	for i := 0; i < 100; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for try := 0; try < 100; try++ {
				a, cre, err := e.svc.Reserve(ctxFor(tenantA), domain, outbound.ReserveInput{ConnectionID: c.ConnectionID, OperationID: "payment-submit", DomainRequestRef: "PAY-1", Payload: json.RawMessage(payRef)})
				if err != nil {
					continue
				}
				mu.Lock()
				ids[a.AttemptID]++
				if cre {
					created++
				}
				mu.Unlock()
				return
			}
			t.Error("a caller never got an answer")
		}()
	}
	wg.Wait()
	list, _ := e.repo.List(ctxFor(tenantA), outbound.Filter{})
	if len(list) != 1 {
		t.Fatalf("%d attempts exist for one logical request", len(list))
	}
	if len(ids) != 1 || ids[list[0].AttemptID] != 100 {
		t.Fatalf("callers were handed different attempt ids: %v (stored %s)", ids, list[0].AttemptID)
	}
	if created > 1 {
		t.Fatalf("%d callers were told they created it", created)
	}
	if n := countType(e.repo.Types(), "integration.outbound.reserved"); n != 1 {
		t.Fatalf("%d reserved events", n)
	}
	evs := e.events(list[0].AttemptID)
	if len(evs) != 1 || chaosChain(t, evs) != outbound.StateReserved {
		t.Fatalf("%+v", evs)
	}
	if len(e.prov.calls) != 0 {
		t.Fatal("reserve made a network call")
	}
}

// 5. A 429/503 storm from the status endpoint.
func TestChaos_StatusQueryStorm_BacksOffNeverHotLoopsNeverFailsAnUnknown(t *testing.T) {
	e := newEnv(t)
	c := e.setup(variant{})
	a := e.unknown(c, "PAY-1")
	posts := len(e.prov.posts())
	storm := 0
	e.prov.set(func(c call) (int, string, error) {
		if c.method != http.MethodGet {
			return 200, `{}`, nil
		}
		storm++
		if storm%2 == 0 {
			return 429, `{}`, nil
		}
		return 503, `{}`, nil
	})
	max := e.svc.Backoff(1000)
	prevNext, prevGap := *e.get(tenantA, a.AttemptID).NextResolutionAt, time.Duration(0)
	capped := false
	for i := 0; i < 15; i++ {
		e.advance(prevNext.Sub(e.at()))
		before := len(e.prov.gets())
		if n, err := e.svc.DriveDue(context.Background(), 50); err != nil || n != 1 {
			t.Fatalf("iteration %d: %d %v", i, n, err)
		}
		if len(e.prov.gets())-before != 1 {
			t.Fatalf("one due attempt must cost exactly one query")
		}
		got := e.get(tenantA, a.AttemptID)
		if got.State != outbound.StateUnknown {
			t.Fatalf("the storm moved an UNKNOWN attempt to %s", got.State)
		}
		next := *got.NextResolutionAt
		gap := next.Sub(e.at())
		if !next.After(prevNext) || gap < prevGap || gap > max || gap != e.svc.Backoff(got.ResolveCount+1) {
			t.Fatalf("backoff broke at %d: gap %v (prev %v, max %v) resolve_count %d", i, gap, prevGap, max, got.ResolveCount)
		}
		capped = capped || gap == max
		// no time has passed: a second tick must not query again
		g := len(e.prov.gets())
		if n, _ := e.svc.DriveDue(context.Background(), 50); n != 0 || len(e.prov.gets()) != g {
			t.Fatalf("hot loop: the attempt was driven again with no time elapsed (%d)", n)
		}
		prevNext, prevGap = next, gap
	}
	if !capped {
		t.Fatal("backoff never reached its bound")
	}
	if len(e.prov.posts()) != posts {
		t.Fatal("the storm caused a submission")
	}
	evs := e.events(a.AttemptID)
	for _, ev := range evs {
		if ev.To == outbound.StateFailed {
			t.Fatalf("FAILED was recorded to shorten the queue: %+v", ev)
		}
	}
	if chaosChain(t, evs) != outbound.StateUnknown {
		t.Fatalf("%+v", evs)
	}
	// the storm ends: the true outcome still arrives
	e.prov.set(statusIs("SETTLED"))
	e.advance(max)
	if n, err := e.svc.DriveDue(context.Background(), 50); err != nil || n != 1 {
		t.Fatalf("%d %v", n, err)
	}
	fin := e.get(tenantA, a.AttemptID)
	if fin.State != outbound.StateSettled || fin.SubmitCount != 1 {
		t.Fatalf("%+v", fin)
	}
	chaosImmutable(t, a, fin)
}
