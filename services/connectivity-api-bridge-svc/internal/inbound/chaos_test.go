package inbound_test

// Wave 8 chaos tests for the inbound gateway: store faults during Receive, a failing/hanging domain during handoff,
// racing handoff workers and a burst past the rate limit. Nothing lost, nothing duplicated, nothing silently fixed.

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"zoiko.io/connectivity-api-bridge-svc/internal/inbound"
)

type chaosMode int

const (
	chaosOK         chaosMode = iota
	chaosFailBefore           // the write is not applied
	chaosFailAfter            // the write COMMITS but the caller sees an error
)

var errChaos = errors.New("chaos: injected store failure")

type chaosRepo struct {
	inbound.Repo
	mu       sync.Mutex
	insertN  int
	dupN     int
	mutateN  int
	insertFn func(n int) chaosMode
	dupFn    func(n int) chaosMode
	mutateFn func(n int, kind string) chaosMode
}

func (c *chaosRepo) Insert(ctx context.Context, e *inbound.Exchange, ev inbound.Event, out []inbound.OutEvent) (*inbound.Exchange, error) {
	c.mu.Lock()
	c.insertN++
	n, fn := c.insertN, c.insertFn
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

func (c *chaosRepo) InsertDuplicate(ctx context.Context, e *inbound.Exchange, ev inbound.Event, out []inbound.OutEvent) error {
	c.mu.Lock()
	c.dupN++
	n, fn := c.dupN, c.dupFn
	c.mu.Unlock()
	mode := chaosOK
	if fn != nil {
		mode = fn(n)
	}
	if mode == chaosFailBefore {
		return errChaos
	}
	err := c.Repo.InsertDuplicate(ctx, e, ev, out)
	if err == nil && mode == chaosFailAfter {
		return errChaos
	}
	return err
}

func (c *chaosRepo) Mutate(ctx context.Context, id string, fn func(tx *inbound.Tx) error) (*inbound.Exchange, error) {
	c.mu.Lock()
	c.mutateN++
	n, hook := c.mutateN, c.mutateFn
	c.mu.Unlock()
	mode := chaosOK
	wrapped := func(tx *inbound.Tx) error {
		if err := fn(tx); err != nil {
			return err
		}
		kind := ""
		if tx.NewEvent != nil {
			kind = tx.NewEvent.Kind
		}
		if hook != nil {
			mode = hook(n, kind)
		}
		if mode == chaosFailBefore {
			return errChaos
		}
		return nil
	}
	x, err := c.Repo.Mutate(ctx, id, wrapped)
	if err == nil && mode == chaosFailAfter {
		return nil, errChaos
	}
	return x, err
}

// chaosChain proves the exchange history is a gapless chain and returns the state it ends in.
func chaosChain(t *testing.T, evs []inbound.Event) string {
	t.Helper()
	if len(evs) == 0 {
		t.Fatal("an exchange without history")
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

func chaosPrefix(t *testing.T, before, after []inbound.Event) {
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

// chaosFacts is the part of an exchange nothing may change after it was recorded.
func chaosFacts(x *inbound.Exchange) string {
	return x.ExchangeID + "|" + x.BodyHash + "|" + x.CanonicalHash + "|" + x.SourceRefHash + "|" + x.ConnectionID + "|" + x.ContractVersionID + "|" + x.MappingVersionID
}

// chaosDomain is a domain stub that dedupes by Idempotency-Key but ALSO counts every raw request. act decides what
// the n-th raw request does: "500", "reset" (applied, then hang up), "hang" (applied, then never answer), "slow", "ok".
type chaosDomain struct {
	srv     *httptest.Server
	mu      sync.Mutex
	raw     []domainReq
	effects map[string]int // key -> raw requests that reached the domain with it
	act     func(n int) string
}

func newChaosDomain(t *testing.T, act func(n int) string) *chaosDomain {
	d := &chaosDomain{effects: map[string]int{}, act: act}
	d.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		d.mu.Lock()
		d.raw = append(d.raw, domainReq{Header: r.Header.Clone(), Body: string(b)})
		n := len(d.raw)
		d.mu.Unlock()
		action := "ok"
		if d.act != nil {
			action = d.act(n)
		}
		if action == "500" {
			w.WriteHeader(500)
			return
		}
		d.mu.Lock()
		d.effects[r.Header.Get("Idempotency-Key")]++
		d.mu.Unlock()
		switch action {
		case "reset":
			if hj, ok := w.(http.Hijacker); ok {
				conn, _, _ := hj.Hijack()
				conn.Close()
				return
			}
		case "hang":
			select {
			case <-r.Context().Done():
			case <-time.After(3 * time.Second):
			}
			return
		case "slow":
			time.Sleep(50 * time.Millisecond)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(200)
		_, _ = w.Write([]byte(`{"id":"domain-obj-7"}`))
	}))
	t.Cleanup(d.srv.Close)
	return d
}

func (d *chaosDomain) rawCount() int { d.mu.Lock(); defer d.mu.Unlock(); return len(d.raw) }

// distinct is how many distinct effects the domain applied (it dedupes by key).
func (d *chaosDomain) distinct() int { d.mu.Lock(); defer d.mu.Unlock(); return len(d.effects) }

func (d *chaosDomain) keys() []string {
	d.mu.Lock()
	defer d.mu.Unlock()
	var ks []string
	for _, r := range d.raw {
		ks = append(ks, r.Header.Get("Idempotency-Key"))
	}
	return ks
}

func (e *env) chaosList() (all, originals []inbound.Exchange) {
	e.t.Helper()
	all, err := e.repo.List(ctxFor(tenantA), inbound.Filter{})
	if err != nil {
		e.t.Fatal(err)
	}
	for _, x := range all {
		if x.State != inbound.StateDuplicate {
			originals = append(originals, x)
		}
	}
	return
}

// 6. The store fails during Receive; the sender retries the same event.
func TestChaos_InsertFailsDuringReceive_SenderRetriesToExactlyOneExchangeAndOneDelivery(t *testing.T) {
	for _, mode := range []chaosMode{chaosFailBefore, chaosFailAfter} {
		name := "write not applied"
		if mode == chaosFailAfter {
			name = "write applied but error returned"
		}
		t.Run(name, func(t *testing.T) {
			e := newEnv(t)
			v := e.publish(nil)
			c := e.activeConn(tenantA, v, "acc-a", refHMAC)
			dom := newChaosDomain(t, nil)
			e.target(dom.srv.URL)
			e.gw.Handoff.HTTP = dom.srv.Client()
			cr := &chaosRepo{Repo: e.repo,
				insertFn: func(n int) chaosMode {
					if n == 1 {
						return mode
					}
					return chaosOK
				},
				dupFn: func(n int) chaosMode { // the duplicate row's own write flaps once as well
					if n == 1 {
						return chaosFailBefore
					}
					return chaosOK
				}}
			e.gw.Repo = cr

			o1 := e.deliver(ok(c, "evt-1"))
			if o1.Status != http.StatusServiceUnavailable || o1.RetryAfter <= 0 || o1.Exchange != nil {
				t.Fatalf("the sender must get a retryable failure: %+v", o1)
			}
			all, _ := e.chaosList()
			var original inbound.Exchange
			var before []inbound.Event
			if mode == chaosFailBefore {
				if len(all) != 0 {
					t.Fatalf("a refused receive left %d rows behind", len(all))
				}
				e.driveAll()
				if dom.rawCount() != 0 {
					t.Fatal("something was delivered to the domain for a delivery the store refused")
				}
			} else {
				if len(all) != 1 {
					t.Fatalf("the committed write is lost: %d rows", len(all))
				}
				original = all[0]
				before, _ = e.repo.Events(ctxFor(tenantA), original.ExchangeID)
			}

			// the sender retries the same event until it is told
			var final *inbound.Outcome
			for try := 0; try < 4 && final == nil; try++ {
				o := e.deliver(ok(c, "evt-1"))
				if o.Status == http.StatusServiceUnavailable {
					if o.RetryAfter <= 0 || o.Exchange != nil {
						t.Fatalf("%+v", o)
					}
					continue
				}
				final = o
			}
			if final == nil {
				t.Fatal("the sender never got an answer")
			}
			if mode == chaosFailAfter {
				if final.Status != http.StatusOK || !final.Duplicate || final.Exchange == nil || final.Exchange.ExchangeID != original.ExchangeID {
					t.Fatalf("the retry must be recognised as a duplicate of the original: %+v", final)
				}
			} else if final.Status != http.StatusAccepted || final.Duplicate {
				t.Fatalf("%+v", final)
			}
			e.driveAll()
			e.driveAll()
			// the sender, unsure, retries again after success: still the same single effect
			var again *inbound.Outcome
			for try := 0; try < 4; try++ {
				if again = e.deliver(ok(c, "evt-1")); again.Status != http.StatusServiceUnavailable {
					break
				}
			}
			if !again.Duplicate {
				t.Fatalf("%+v", again)
			}
			e.driveAll()
			all, originals := e.chaosList()
			if len(originals) != 1 || originals[0].State != inbound.StateHandedOff || originals[0].DomainOutcome != inbound.DomainAccepted {
				t.Fatalf("want exactly one handed-off exchange: %+v", originals)
			}
			if dom.rawCount() != 1 || dom.distinct() != 1 {
				t.Fatalf("domain saw %d requests / %d effects for one event", dom.rawCount(), dom.distinct())
			}
			if k := dom.keys(); k[0] != originals[0].ExchangeID {
				t.Fatalf("idempotency key %q is not the exchange id", k[0])
			}
			for _, x := range all {
				if x.State == inbound.StateDuplicate && x.OriginalExchangeID != originals[0].ExchangeID {
					t.Fatalf("a duplicate row points at %q", x.OriginalExchangeID)
				}
			}
			if n := countType(e.repo.Types(), "integration.inbound.accepted"); n != 1 {
				t.Fatalf("%d accepted events", n)
			}
			evs, _ := e.repo.Events(ctxFor(tenantA), originals[0].ExchangeID)
			if chaosChain(t, evs) != inbound.StateHandedOff {
				t.Fatalf("%+v", evs)
			}
			if mode == chaosFailAfter {
				chaosPrefix(t, before, evs)
				if chaosFacts(&original) != chaosFacts(&originals[0]) {
					t.Fatalf("recorded facts changed: %s -> %s", chaosFacts(&original), chaosFacts(&originals[0]))
				}
			}
		})
	}
}

// 7a. The domain is down in three different ways, then recovers.
func TestChaos_DomainDownThenRecovers_NeverLostSameKeyDeliveredOnce(t *testing.T) {
	e := newEnv(t)
	v := e.publish(nil)
	c := e.activeConn(tenantA, v, "acc-a", refHMAC)
	dom := newChaosDomain(t, func(n int) string {
		return []string{"500", "reset", "hang", "ok"}[min(n, 4)-1]
	})
	e.target(dom.srv.URL)
	// no keep-alive: the standard transport silently replays a request that carries an Idempotency-Key when a reused
	// connection resets, which would blur which raw request this script is answering
	tr := dom.srv.Client().Transport.(*http.Transport).Clone()
	tr.DisableKeepAlives = true
	e.gw.Handoff.HTTP = &http.Client{Transport: tr, Timeout: 200 * time.Millisecond}
	o := e.deliver(ok(c, "evt-1"))
	if o.Status != http.StatusAccepted {
		t.Fatalf("%+v", o)
	}
	id := o.Exchange.ExchangeID
	first := e.exchange(tenantA, id)
	evs0, _ := e.repo.Events(ctxFor(tenantA), id)

	for step := 1; step <= 3; step++ {
		e.advance(16 * time.Minute)
		if n, _ := e.gw.DriveDue(context.Background(), 10); n != 1 {
			t.Fatalf("step %d: %d driven", step, n)
		}
		x := e.exchange(tenantA, id)
		if x.State != inbound.StateValidated || x.DomainOutcome != inbound.DomainUnknown || x.NextAttemptAt == nil || x.HandoffAttempts != step {
			t.Fatalf("step %d: the exchange must stay retryable, never lost or failed: %+v", step, x)
		}
		if chaosFacts(first) != chaosFacts(x) {
			t.Fatalf("step %d: recorded facts changed", step)
		}
		if !x.NextAttemptAt.After(e.at()) {
			t.Fatalf("step %d: no backoff before the next attempt", step)
		}
		// no time passes: a second tick does nothing
		if n, _ := e.gw.DriveDue(context.Background(), 10); n != 0 {
			t.Fatalf("step %d: hot loop", step)
		}
	}
	e.advance(16 * time.Minute)
	if n, err := e.gw.DriveDue(context.Background(), 10); n != 1 || err != nil {
		t.Fatalf("%d %v", n, err)
	}
	x := e.exchange(tenantA, id)
	if x.State != inbound.StateHandedOff || x.DomainOutcome != inbound.DomainAccepted || x.HandoffAttempts != 4 {
		t.Fatalf("%+v", x)
	}
	for i := 0; i < 3; i++ { // after success the engine never sends again
		e.advance(time.Hour)
		e.driveAll()
	}
	if dom.rawCount() != 4 || dom.distinct() != 1 {
		t.Fatalf("raw %d effects %d: the engine double-sent after success or lost an attempt", dom.rawCount(), dom.distinct())
	}
	for i, k := range dom.keys() {
		if k != id {
			t.Fatalf("attempt %d carried key %q, want the exchange id", i+1, k)
		}
	}
	evs, _ := e.repo.Events(ctxFor(tenantA), id)
	if chaosChain(t, evs) != inbound.StateHandedOff {
		t.Fatalf("%+v", evs)
	}
	chaosPrefix(t, evs0, evs)
	if hasType(e.repo.Types(), "integration.inbound.quarantined") {
		t.Fatal("a transport failure was turned into a quarantine")
	}
}

// 7b. Two (many) handoff workers race on the same exchange.
func TestChaos_RacingHandoffWorkers_ExactlyOneDelivery(t *testing.T) {
	e := newEnv(t)
	v := e.publish(nil)
	c := e.activeConn(tenantA, v, "acc-a", refHMAC)
	dom := newChaosDomain(t, func(int) string { return "slow" })
	e.target(dom.srv.URL)
	e.gw.Handoff.HTTP = dom.srv.Client()
	o := e.deliver(ok(c, "evt-1"))
	ref := inbound.DueRef{TenantID: tenantA, ExchangeID: o.Exchange.ExchangeID}
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			if i%2 == 0 {
				_, _ = e.gw.Drive(context.Background(), ref)
			} else {
				_, _ = e.gw.DriveDue(context.Background(), 10)
			}
		}(i)
	}
	wg.Wait()
	x := e.exchange(tenantA, ref.ExchangeID)
	if x.State != inbound.StateHandedOff || x.HandoffAttempts != 1 {
		t.Fatalf("%+v", x)
	}
	for i := 0; i < 4; i++ { // late workers, after the lease has lapsed
		e.advance(2 * time.Minute)
		_, _ = e.gw.Drive(context.Background(), ref)
		e.driveAll()
	}
	if dom.rawCount() != 1 || dom.distinct() != 1 {
		t.Fatalf("raw %d effects %d: racing workers delivered more than once", dom.rawCount(), dom.distinct())
	}
	if n := countType(e.repo.Types(), "integration.inbound.handed-off"); n != 1 {
		t.Fatalf("%d handed-off events", n)
	}
	evs, _ := e.repo.Events(ctxFor(tenantA), ref.ExchangeID)
	if chaosChain(t, evs) != inbound.StateHandedOff {
		t.Fatalf("%+v", evs)
	}
}

// 7c. The domain accepted, but the write recording that acceptance fails.
func TestChaos_DomainAcceptedButRecordWriteFails_ResentWithSameKeyAtMostOnceMore(t *testing.T) {
	for _, mode := range []chaosMode{chaosFailBefore, chaosFailAfter} {
		name := "write not applied"
		want := 2 // the engine could not know, so it asks again with the same key
		if mode == chaosFailAfter {
			name, want = "write applied but error returned", 1
		}
		t.Run(name, func(t *testing.T) {
			e := newEnv(t)
			v := e.publish(nil)
			c := e.activeConn(tenantA, v, "acc-a", refHMAC)
			dom := newChaosDomain(t, nil)
			e.target(dom.srv.URL)
			e.gw.Handoff.HTTP = dom.srv.Client()
			o := e.deliver(ok(c, "evt-1"))
			id := o.Exchange.ExchangeID
			failed := false
			e.gw.Repo = &chaosRepo{Repo: e.repo, mutateFn: func(_ int, kind string) chaosMode {
				if kind == "HANDED_OFF" && !failed {
					failed = true
					return mode
				}
				return chaosOK
			}}
			if _, err := e.gw.Drive(context.Background(), inbound.DueRef{TenantID: tenantA, ExchangeID: id}); err == nil {
				t.Fatal("the failed record was swallowed")
			}
			if x := e.exchange(tenantA, id); x.State == inbound.StateQuarantined || x.State == inbound.StateRejected {
				t.Fatalf("a lost write must not demote the exchange: %+v", x)
			}
			for i := 0; i < 3; i++ {
				e.advance(2 * time.Minute) // past the claim lease
				e.driveAll()
			}
			x := e.exchange(tenantA, id)
			if x.State != inbound.StateHandedOff || x.DomainOutcome != inbound.DomainAccepted {
				t.Fatalf("%+v", x)
			}
			if dom.distinct() != 1 || dom.rawCount() != want {
				t.Fatalf("raw %d (want %d) effects %d", dom.rawCount(), want, dom.distinct())
			}
			for _, k := range dom.keys() {
				if k != id {
					t.Fatalf("key %q", k)
				}
			}
			evs, _ := e.repo.Events(ctxFor(tenantA), id)
			if chaosChain(t, evs) != inbound.StateHandedOff {
				t.Fatalf("%+v", evs)
			}
		})
	}
}

// 8. A burst past the per-connection rate limit, with store faults on top.
func TestChaos_BurstPastRateLimit_EveryRequestRecordedOrCleanlyRefused(t *testing.T) {
	const limit, burst = 20, 60
	e := newEnv(t)
	e.gw.Cfg.ConnRatePerMin = limit
	v := e.publish(nil)
	c := e.activeConn(tenantA, v, "acc-a", refHMAC)
	dom := newChaosDomain(t, nil)
	e.target(dom.srv.URL)
	e.gw.Repo = &chaosRepo{Repo: e.repo, insertFn: func(n int) chaosMode {
		switch {
		case n%4 == 0:
			return chaosFailBefore
		case n%7 == 0:
			return chaosFailAfter
		}
		return chaosOK
	}}
	reqs := make([]inbound.Request, burst)
	for i := range reqs {
		reqs[i] = e.request(ok(c, "evt-"+itoa(i)))
	}
	outs := make([]*inbound.Outcome, burst)
	var wg sync.WaitGroup
	for i := range reqs {
		wg.Add(1)
		go func(i int) { defer wg.Done(); outs[i] = e.gw.Receive(context.Background(), reqs[i]) }(i)
	}
	wg.Wait()
	var accepted, limited, unavailable int
	for _, o := range outs {
		switch o.Status {
		case http.StatusAccepted:
			accepted++
			if o.Exchange == nil {
				t.Fatalf("an accepted answer without an exchange: %+v", o)
			}
			if _, err := e.repo.Get(ctxFor(tenantA), o.Exchange.ExchangeID); err != nil {
				t.Fatalf("the sender was told accepted but nothing is durable: %v", err)
			}
		case http.StatusTooManyRequests:
			limited++
			if o.RetryAfter <= 0 || o.Exchange != nil || o.XIC == nil {
				t.Fatalf("a refusal must be clean and carry Retry-After: %+v", o)
			}
		case http.StatusServiceUnavailable:
			unavailable++
			if o.RetryAfter <= 0 || o.Exchange != nil {
				t.Fatalf("%+v", o)
			}
		default:
			t.Fatalf("unexpected answer %+v", o)
		}
	}
	if accepted+unavailable != limit || limited != burst-limit {
		t.Fatalf("accepted %d unavailable %d limited %d: the limiter let the wrong number through", accepted, unavailable, limited)
	}
	all, _ := e.chaosList()
	if len(all) < accepted || len(all) > accepted+unavailable {
		t.Fatalf("%d rows for %d accepted + %d unavailable", len(all), accepted, unavailable)
	}
	for _, x := range all {
		evs, _ := e.repo.Events(ctxFor(tenantA), x.ExchangeID)
		if len(evs) != 1 || evs[0].Kind != "ACCEPTED" || x.State != inbound.StateValidated {
			t.Fatalf("a half-recorded exchange: %+v %+v", x, evs)
		}
	}
	if n := countType(e.repo.Types(), "integration.inbound.accepted"); n != len(all) {
		t.Fatalf("%d accepted events for %d exchanges", n, len(all))
	}
	if dom.rawCount() != 0 {
		t.Fatal("receiving delivered to the domain")
	}
}
