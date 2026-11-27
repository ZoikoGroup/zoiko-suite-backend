package certwatch

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/segmentio/kafka-go"

	"zoiko.io/general-ledger-svc/internal/certreliance"
)

// The instrumentation points report the right thing at each outcome.

type recObs struct {
	mu        sync.Mutex
	scheduled []time.Duration
	runs      []string
	scopes    []string
	unchecked int
	flags     map[string]int
	events    []string
	reactions []string
	errs      []string
}

func (o *recObs) lock() func() { o.mu.Lock(); return o.mu.Unlock }

func (o *recObs) SweepScheduled(d time.Duration) {
	defer o.lock()()
	o.scheduled = append(o.scheduled, d)
}
func (o *recObs) SweepRun(s string, _ time.Duration) { defer o.lock()(); o.runs = append(o.runs, s) }
func (o *recObs) SweepScope(s string)                { defer o.lock()(); o.scopes = append(o.scopes, s) }
func (o *recObs) CertificationsUnchecked(n int)      { defer o.lock()(); o.unchecked += n }
func (o *recObs) FlagsRaised(trigger string, n int) {
	defer o.lock()()
	if o.flags == nil {
		o.flags = map[string]int{}
	}
	o.flags[trigger] += n
}
func (o *recObs) WithdrawalEvent(s string)    { defer o.lock()(); o.events = append(o.events, s) }
func (o *recObs) WithdrawalReaction(s string) { defer o.lock()(); o.reactions = append(o.reactions, s) }
func (o *recObs) ConsumerError(s string)      { defer o.lock()(); o.errs = append(o.errs, s) }

func eq(a []string, b ...string) bool { return fmt.Sprint(a) == fmt.Sprint(b) }

func TestSchedulerReportsEachOutcome(t *testing.T) {
	watched := map[string][]Watched{"e1": {{LegalEntityID: "e1", Ref: ref(certA)}}, "e2": {{LegalEntityID: "e2", Ref: ref(certB)}}}
	revoked := map[string]certreliance.Status{certA: {State: certreliance.StateRevoked, RevocationID: "dqx_1", Reason: "r"}, certB: {State: certreliance.StateActive}}

	// ok: everything swept, flags counted under "schedule".
	o := &recObs{}
	st := &perScopeStore{watched: watched, flagsFor: map[string]int{"e1": 2}}
	s, _ := newScheduler([]Scope{{"t", "e1"}, {"t", "e2"}}, st, &fakeStatus{by: revoked}, &fakeAuthz{}, &fakeLock{})
	s.Observer = o
	if _, err := s.RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !eq(o.runs, RunOK) || !eq(o.scopes, ScopeSwept, ScopeSwept) || o.unchecked != 0 || o.flags[TriggerSchedule] != 2 {
		t.Fatalf("ok: %+v", o)
	}

	// partial: a scope the principal may not sweep, and a certification that cannot be checked.
	o = &recObs{}
	st = &perScopeStore{watched: watched}
	s, _ = newScheduler([]Scope{{"t", "e1"}, {"t", "e2"}}, st, &fakeStatus{errs: map[string]error{certB: certreliance.ErrUnavailable}}, &fakeAuthz{deny: map[string]bool{"e1": true}}, &fakeLock{})
	s.Observer = o
	if _, err := s.RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !eq(o.runs, RunPartial) || !eq(o.scopes, ScopeUnauthorized, ScopeSwept) || o.unchecked != 1 {
		t.Fatalf("partial: %+v", o)
	}

	// a scope whose sweep fails is "error", and the pass is partial.
	o = &recObs{}
	st = &perScopeStore{watched: watched, listErr: map[string]error{"e1": errors.New("store down")}}
	s, _ = newScheduler([]Scope{{"t", "e1"}}, st, &fakeStatus{}, &fakeAuthz{}, &fakeLock{})
	s.Observer = o
	_, _ = s.RunOnce(context.Background())
	if !eq(o.runs, RunPartial) || !eq(o.scopes, ScopeError) {
		t.Fatalf("scope error: %+v", o)
	}

	// locked: another replica holds it, nothing else is reported.
	o = &recObs{}
	s, _ = newScheduler([]Scope{{"t", "e1"}}, &perScopeStore{}, &fakeStatus{}, &fakeAuthz{}, &fakeLock{held: true})
	s.Observer = o
	_, _ = s.RunOnce(context.Background())
	if !eq(o.runs, RunLocked) || len(o.scopes) != 0 || o.unchecked != 0 || len(o.flags) != 0 {
		t.Fatalf("locked: %+v", o)
	}

	// error: the lock or the scope scan failed.
	for name, mk := range map[string]func() (*Scheduler, *recObs){
		"lock": func() (*Scheduler, *recObs) {
			o := &recObs{}
			s, _ := newScheduler(nil, &perScopeStore{}, &fakeStatus{}, &fakeAuthz{}, &fakeLock{err: errors.New("db")})
			s.Observer = o
			return s, o
		},
		"scan": func() (*Scheduler, *recObs) {
			o := &recObs{}
			s, sc := newScheduler(nil, &perScopeStore{}, &fakeStatus{}, &fakeAuthz{}, &fakeLock{})
			sc.err = errors.New("scan")
			s.Observer = o
			return s, o
		},
	} {
		s, o := mk()
		if _, err := s.RunOnce(context.Background()); err == nil || !eq(o.runs, RunError) {
			t.Fatalf("%s: err=%v runs=%v", name, err, o.runs)
		}
	}
}

func TestSchedulerReportsItsIntervalAndOffAsZero(t *testing.T) {
	o := &recObs{}
	s, _ := newScheduler(nil, &perScopeStore{}, &fakeStatus{}, &fakeAuthz{}, &fakeLock{})
	s.Observer, s.Interval = o, 0
	s.Run(context.Background())
	s2, _ := newScheduler(nil, &perScopeStore{}, &fakeStatus{}, &fakeAuthz{}, &fakeLock{})
	s2.Observer, s2.Principal = o, ""
	s2.Run(context.Background())
	if len(o.scheduled) != 2 || o.scheduled[0] != 0 || o.scheduled[1] != 0 {
		t.Fatalf("off must be reported as 0: %v", o.scheduled)
	}
	// On: the configured interval, once, then it runs and stops on cancellation.
	o2 := &recObs{}
	s3, _ := newScheduler([]Scope{}, &perScopeStore{}, &fakeStatus{}, &fakeAuthz{}, &fakeLock{})
	s3.Observer, s3.Interval = o2, time.Hour
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { s3.Run(ctx); close(done) }()
	time.Sleep(50 * time.Millisecond)
	cancel()
	<-done
	if len(o2.scheduled) != 1 || o2.scheduled[0] != time.Hour || len(o2.runs) == 0 {
		t.Fatalf("on: scheduled %v runs %v", o2.scheduled, o2.runs)
	}
}

func TestACancelledPassIsNeitherASuccessNorAFailure(t *testing.T) {
	o := &recObs{}
	st := &perScopeStore{watched: map[string][]Watched{"e1": {{LegalEntityID: "e1", Ref: ref(certA)}}}}
	s, _ := newScheduler([]Scope{{"t", "e1"}}, st, &fakeStatus{}, &fakeAuthz{}, &fakeLock{})
	s.Observer = o
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, _ = s.RunOnce(ctx)
	if len(o.runs) != 0 {
		t.Fatalf("a pass cut short by shutdown was reported as %v", o.runs)
	}
}

func TestReactorAndConsumerReportReactionsEventsAndBrokerErrors(t *testing.T) {
	o := &recObs{}
	st := &eventStore{watchers: map[string][]Watched{certA: {{LegalEntityID: "e1", Ref: ref(certA)}, {LegalEntityID: "e2", Ref: ref(certA)}}}}
	status := &fakeStatus{by: map[string]certreliance.Status{certA: {State: certreliance.StateRevoked, RevocationID: "dqx_1", Reason: "r"}}}
	r := newReactor(st, status, &fakeAuthz{deny: map[string]bool{"e2": true}})
	r.Observer = o
	if _, err := r.OnWithdrawal(context.Background(), wd()); err != nil {
		t.Fatal(err)
	}
	if !eq(o.reactions, ReactionSwept, ReactionUnauthorized) && !eq(o.reactions, ReactionUnauthorized, ReactionSwept) || o.flags[TriggerEvent] != 2 {
		t.Fatalf("reactions %v flags %v", o.reactions, o.flags)
	}
	// unwatched, failed.
	o2 := &recObs{}
	r2 := newReactor(&eventStore{}, status, &fakeAuthz{})
	r2.Observer = o2
	_, _ = r2.OnWithdrawal(context.Background(), wd())
	r3 := newReactor(&eventStore{watchers: st.watchers}, &fakeStatus{errs: map[string]error{certA: certreliance.ErrUnavailable}}, &fakeAuthz{})
	r3.Observer = o2
	_, _ = r3.OnWithdrawal(context.Background(), wd())
	if !eq(o2.reactions, ReactionUnwatched, ReactionFailed, ReactionFailed) {
		t.Fatalf("reactions: %v", o2.reactions)
	}

	// The consumer: irrelevant, malformed, handled, failed (after the retries).
	oc := &recObs{}
	c := newConsumer(&eventStore{watchers: st.watchers}, status)
	c.Observer = oc
	for _, raw := range []string{`{"type":"com.zoikosuite.accounting.journal.posted","tenantid":"` + tenantA + `","data":{}}`, `{{{`, dqEvent} {
		if err := c.Handle(context.Background(), []byte(raw)); err != nil {
			t.Fatal(err)
		}
	}
	cf := newConsumer(&eventStore{listErr: errors.New("db")}, status)
	cf.Observer = oc
	_ = cf.Handle(context.Background(), []byte(dqEvent))
	if !eq(oc.events, EventIrrelevant, EventMalformed, EventHandled, EventFailed) {
		t.Fatalf("events: %v", oc.events)
	}

	// Broker problems: a failed read, and a failed commit.
	ob := &recObs{}
	rd := &commitFailReader{fakeReader: fakeReader{fetchErrs: []error{errors.New("broker")}, msgs: []kafka.Message{{Value: []byte(dqEvent)}}}}
	cb := newConsumer(&eventStore{watchers: st.watchers}, status)
	cb.Observer = ob
	cb.Run(context.Background(), rd)
	if !eq(ob.errs, ErrRead, ErrCommit) {
		t.Fatalf("consumer errors: %v", ob.errs)
	}
}

type commitFailReader struct{ fakeReader }

func (c *commitFailReader) CommitMessages(context.Context, ...kafka.Message) error {
	return errors.New("commit refused")
}

// A nil Observer is a no-op everywhere: callers that do not care wire nothing.
func TestANilObserverIsSafeEverywhere(t *testing.T) {
	st := &eventStore{watchers: map[string][]Watched{certA: {{LegalEntityID: "e1", Ref: ref(certA)}}}}
	status := &fakeStatus{by: map[string]certreliance.Status{certA: {State: certreliance.StateRevoked, RevocationID: "x", Reason: "r"}}}
	r := newReactor(st, status, &fakeAuthz{})
	if _, err := r.OnWithdrawal(context.Background(), wd()); err != nil {
		t.Fatal(err)
	}
	c := newConsumer(st, status)
	if err := c.Handle(context.Background(), []byte(dqEvent)); err != nil {
		t.Fatal(err)
	}
	s, _ := newScheduler([]Scope{{"t", "e1"}}, &perScopeStore{watched: map[string][]Watched{"e1": {{LegalEntityID: "e1", Ref: ref(certA)}}}}, status, &fakeAuthz{}, &fakeLock{})
	if _, err := s.RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
}
