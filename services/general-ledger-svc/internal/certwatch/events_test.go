package certwatch

import (
	"context"
	"errors"
	"io"
	"sync"
	"testing"
	"time"

	"github.com/segmentio/kafka-go"

	"zoiko.io/general-ledger-svc/internal/certreliance"
	svcmiddleware "zoiko.io/general-ledger-svc/internal/middleware"
)

const (
	tenantA = "11111111-1111-1111-1111-111111111111"

	// data-quality-svc publishes the legacy envelope; reconciliation-engine-svc the canonical one.
	dqEvent = `{"event_id":"evt-1","event_type":"dqc.certification.reliance-withdrawn","tenant_id":"` + tenantA + `","source_service":"data-quality-svc",
		"payload":{"reliance_id":"dqr_1","run_id":"dqn_a","revocation_id":"dqx_1","relying_service":"general-ledger-svc","relying_object_type":"journal","relying_object_id":"j1"}}`
	recEvent = `{"id":"e-2","type":"com.zoikosuite.accounting.reconciliation-certification.reliance-withdrawn","tenantid":"` + tenantA + `",
		"data":{"reliance_id":"rrl_1","run_id":"rcr_b","revocation_id":"rcv_2","relying_service":"general-ledger-svc","relying_object_type":"accounting-posting","relying_object_id":"e:b:PRIMARY"}}`
)

func TestParseWithdrawal_BothProducersShapesAndNothingElse(t *testing.T) {
	w, ok, err := ParseWithdrawal([]byte(dqEvent))
	if err != nil || !ok || w.TenantID != tenantA || w.Ref.String() != certA || w.RelianceID != "dqr_1" || w.RevocationID != "dqx_1" {
		t.Fatalf("data-quality shape: %+v %v %v", w, ok, err)
	}
	w, ok, err = ParseWithdrawal([]byte(recEvent))
	if err != nil || !ok || w.TenantID != tenantA || w.Ref.String() != certB || w.RevocationID != "rcv_2" {
		t.Fatalf("reconciliation shape: %+v %v %v", w, ok, err)
	}
	// Not this service's business: no error, not relevant.
	for name, raw := range map[string]string{
		"another relying service": `{"event_type":"dqc.certification.reliance-withdrawn","tenant_id":"` + tenantA + `","payload":{"run_id":"dqn_a","relying_service":"tax-svc"}}`,
		"the revocation itself":   `{"event_type":"dqc.certification.revoked","tenant_id":"` + tenantA + `","payload":{"run_id":"dqn_a","relying_service":"general-ledger-svc"}}`,
		"a reliance recorded":     `{"type":"com.zoikosuite.accounting.reconciliation-certification.reliance-recorded","tenantid":"` + tenantA + `","data":{"run_id":"r","relying_service":"general-ledger-svc"}}`,
		"an unrelated event":      `{"type":"com.zoikosuite.accounting.journal.posted","tenantid":"` + tenantA + `","data":{}}`,
		"no type at all":          `{"tenant_id":"` + tenantA + `"}`,
	} {
		if _, ok, err := ParseWithdrawal([]byte(raw)); ok || err != nil {
			t.Fatalf("%s: relevant=%v err=%v", name, ok, err)
		}
	}
	// This service's, but unusable: an error (so it is logged), never a guess.
	for name, raw := range map[string]string{
		"not json":          `{{{`,
		"bad payload":       `{"event_type":"dqc.certification.reliance-withdrawn","tenant_id":"` + tenantA + `","payload":"nope"}`,
		"no payload":        `{"event_type":"dqc.certification.reliance-withdrawn","tenant_id":"` + tenantA + `"}`,
		"no tenant":         `{"event_type":"dqc.certification.reliance-withdrawn","payload":{"run_id":"dqn_a","relying_service":"general-ledger-svc"}}`,
		"tenant not a uuid": `{"event_type":"dqc.certification.reliance-withdrawn","tenant_id":"t1","payload":{"run_id":"dqn_a","relying_service":"general-ledger-svc"}}`,
		"no run":            `{"event_type":"dqc.certification.reliance-withdrawn","tenant_id":"` + tenantA + `","payload":{"relying_service":"general-ledger-svc"}}`,
		"run is a path":     `{"event_type":"dqc.certification.reliance-withdrawn","tenant_id":"` + tenantA + `","payload":{"run_id":"../../x","relying_service":"general-ledger-svc"}}`,
		"run has a query":   `{"event_type":"dqc.certification.reliance-withdrawn","tenant_id":"` + tenantA + `","payload":{"run_id":"a?b=1","relying_service":"general-ledger-svc"}}`,
	} {
		if _, _, err := ParseWithdrawal([]byte(raw)); err == nil {
			t.Fatalf("%s: accepted", name)
		}
	}
}

type eventStore struct {
	Store
	mu          sync.Mutex
	watchers    map[string][]Watched // by certification
	listErr     error
	withdrawals []string
	checks      map[string]string
}

func (s *eventStore) ListWatchers(_ context.Context, _ string, r certreliance.Ref) ([]Watched, error) {
	if s.listErr != nil {
		return nil, s.listErr
	}
	return s.watchers[r.String()], nil
}

func (s *eventStore) RecordCheck(_ context.Context, _, entity string, r certreliance.Ref, state string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.checks == nil {
		s.checks = map[string]string{}
	}
	s.checks[entity+"|"+r.String()] = state
	return nil
}

func (s *eventStore) RecordWithdrawal(_ context.Context, _, entity string, r certreliance.Ref, rev, _ string) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.withdrawals = append(s.withdrawals, entity+"|"+r.String()+"|"+rev)
	return 2, nil
}

func newReactor(st *eventStore, status StatusReader, az *fakeAuthz) *Reactor {
	return &Reactor{Store: st, Sweeper: &Sweeper{Store: st, Status: status}, Authz: az, Principal: "svc-sweeper"}
}

func wd() Withdrawal {
	w, _, _ := ParseWithdrawal([]byte(dqEvent))
	return w
}

func TestReactor_ARevokedCertificationIsCheckedAndFlaggedPerEntityTheEventIsOnlyATrigger(t *testing.T) {
	st := &eventStore{watchers: map[string][]Watched{certA: {{LegalEntityID: "e1", Ref: ref(certA)}, {LegalEntityID: "e2", Ref: ref(certA)}}}}
	status := &fakeStatus{by: map[string]certreliance.Status{certA: {State: certreliance.StateRevoked, RevocationID: "dqx_1", Reason: "false pass"}}}
	az := &fakeAuthz{deny: map[string]bool{"e2": true}}
	rx, err := newReactor(st, status, az).OnWithdrawal(context.Background(), wd())
	if err != nil {
		t.Fatal(err)
	}
	if rx.Watchers != 2 || rx.Swept != 1 || rx.Skipped != 1 || rx.NewFlags != 2 || rx.Failed != 0 {
		t.Fatalf("%+v", rx)
	}
	// Authorized per entity, as the configured principal, with the event's tenant on the context.
	if len(az.seen) != 2 || az.seen[0] != tenantA+"|svc-sweeper|e1|GL_CERTIFICATION_WATCH_SWEEP" || az.seen[1] != tenantA+"|svc-sweeper|e2|GL_CERTIFICATION_WATCH_SWEEP" {
		t.Fatalf("authorization: %v", az.seen)
	}
	// Only the authorized entity was acted on, and only because the ISSUING SERVICE said REVOKED.
	if len(st.withdrawals) != 1 || st.withdrawals[0] != "e1|"+certA+"|dqx_1" || len(status.asked) != 1 {
		t.Fatalf("withdrawals %v, status asked %v", st.withdrawals, status.asked)
	}
}

func TestReactor_AForgedOrMistakenEventCannotRaiseAFlag(t *testing.T) {
	// The event says withdrawn; the issuing service says the certification is in force. Nothing is flagged.
	st := &eventStore{watchers: map[string][]Watched{certA: {{LegalEntityID: "e1", Ref: ref(certA)}}}}
	status := &fakeStatus{by: map[string]certreliance.Status{certA: {State: certreliance.StateActive}}}
	rx, err := newReactor(st, status, &fakeAuthz{}).OnWithdrawal(context.Background(), wd())
	if err != nil || rx.NewFlags != 0 || len(st.withdrawals) != 0 || st.checks["e1|"+certA] != certreliance.StateActive {
		t.Fatalf("%+v %v withdrawals %v checks %v", rx, err, st.withdrawals, st.checks)
	}
	// An issuing service that cannot be reached is a failed check, never a flag and never "in force".
	status2 := &fakeStatus{errs: map[string]error{certA: certreliance.ErrUnavailable}}
	st2 := &eventStore{watchers: st.watchers}
	rx, err = newReactor(st2, status2, &fakeAuthz{}).OnWithdrawal(context.Background(), wd())
	if err != nil || rx.Failed != 1 || rx.NewFlags != 0 || len(st2.withdrawals) != 0 || len(st2.checks) != 0 {
		t.Fatalf("%+v %v checks %v", rx, err, st2.checks)
	}
}

func TestReactor_ACertificationNobodyWatchesAndMissingPrincipal(t *testing.T) {
	st := &eventStore{}
	status := &fakeStatus{}
	rx, err := newReactor(st, status, &fakeAuthz{}).OnWithdrawal(context.Background(), wd())
	if err != nil || rx.Watchers != 0 || len(status.asked) != 0 {
		t.Fatalf("%+v %v asked %v", rx, err, status.asked)
	}
	r := newReactor(st, status, &fakeAuthz{})
	r.Principal = ""
	if _, err := r.OnWithdrawal(context.Background(), wd()); !errors.Is(err, ErrInvalid) {
		t.Fatalf("no principal: %v", err)
	}
	r2 := newReactor(&eventStore{listErr: errors.New("db down")}, status, &fakeAuthz{})
	if _, err := r2.OnWithdrawal(context.Background(), wd()); err == nil {
		t.Fatal("a store failure was swallowed")
	}
}

func TestReactor_AnAlreadyWithdrawnCertificationIsRescannedNotAskedAgain(t *testing.T) {
	now := time.Now()
	st := &eventStore{watchers: map[string][]Watched{certA: {{LegalEntityID: "e1", Ref: ref(certA), WithdrawnAt: &now, RevocationID: "dqx_1", Reason: "r"}}}}
	status := &fakeStatus{}
	rx, err := newReactor(st, status, &fakeAuthz{}).OnWithdrawal(context.Background(), wd())
	if err != nil || len(status.asked) != 0 || rx.NewFlags != 2 || len(st.withdrawals) != 1 {
		t.Fatalf("%+v %v asked %v withdrawals %v", rx, err, status.asked, st.withdrawals)
	}
}

func TestOnWithdrawal_TenantTravelsOnTheContext(t *testing.T) {
	st := &eventStore{watchers: map[string][]Watched{certA: {{LegalEntityID: "e1", Ref: ref(certA)}}}}
	var seen string
	az := &ctxAuthz{fn: func(ctx context.Context) { seen = svcmiddleware.TenantFromContext(ctx) }}
	r := &Reactor{Store: st, Sweeper: &Sweeper{Store: st, Status: &fakeStatus{}}, Authz: az, Principal: "p"}
	if _, err := r.OnWithdrawal(context.Background(), wd()); err != nil || seen != tenantA {
		t.Fatalf("tenant on the context: %q %v", seen, err)
	}
}

type ctxAuthz struct{ fn func(context.Context) }

func (c *ctxAuthz) CheckAllowed(ctx context.Context, _, _, _ string) error { c.fn(ctx); return nil }

// ── the consumer ──────────────────────────────────────────────────────────────

type fakeReader struct {
	mu        sync.Mutex
	msgs      []kafka.Message
	fetchErrs []error // returned before the messages, one per call
	next      int
	committed []string
	closed    bool
	cancel    context.CancelFunc
}

func (f *fakeReader) FetchMessage(ctx context.Context) (kafka.Message, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.fetchErrs) > 0 {
		err := f.fetchErrs[0]
		f.fetchErrs = f.fetchErrs[1:]
		return kafka.Message{}, err
	}
	if f.next >= len(f.msgs) {
		return kafka.Message{}, io.EOF
	}
	m := f.msgs[f.next]
	f.next++
	return m, nil
}

func (f *fakeReader) CommitMessages(_ context.Context, msgs ...kafka.Message) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, m := range msgs {
		f.committed = append(f.committed, string(m.Value))
	}
	return nil
}

func (f *fakeReader) Close() error { f.closed = true; return nil }

func newConsumer(st *eventStore, status StatusReader) *Consumer {
	return &Consumer{Reactor: newReactor(st, status, &fakeAuthz{}), Attempts: 3, RetryDelay: time.Millisecond, ReadRetryDelay: time.Millisecond}
}

func TestConsumer_HandlesRelevantEventsAcknowledgesEverythingAndSurvivesABrokerBlip(t *testing.T) {
	st := &eventStore{watchers: map[string][]Watched{certA: {{LegalEntityID: "e1", Ref: ref(certA)}}, certB: {{LegalEntityID: "e1", Ref: ref(certB)}}}}
	status := &fakeStatus{by: map[string]certreliance.Status{
		certA: {State: certreliance.StateRevoked, RevocationID: "dqx_1", Reason: "r"},
		certB: {State: certreliance.StateRevoked, RevocationID: "rcv_2", Reason: "r"},
	}}
	irrelevant := `{"type":"com.zoikosuite.accounting.journal.posted","tenantid":"` + tenantA + `","data":{}}`
	rd := &fakeReader{
		fetchErrs: []error{errors.New("broker unreachable"), errors.New("still unreachable")},
		msgs:      []kafka.Message{{Value: []byte(irrelevant)}, {Value: []byte(dqEvent)}, {Value: []byte(`{{{ undecodable`)}, {Value: []byte(recEvent)}},
	}
	newConsumer(st, status).Run(context.Background(), rd)

	if !rd.closed {
		t.Fatal("the reader was not closed")
	}
	// Every delivery was acknowledged, in order, including the irrelevant and the undecodable: none may block the topic.
	if len(rd.committed) != 4 {
		t.Fatalf("committed %d of 4: %v", len(rd.committed), rd.committed)
	}
	// Both relevant events led to a check and a flag; the others did nothing.
	if len(st.withdrawals) != 2 || st.withdrawals[0] != "e1|"+certA+"|dqx_1" || st.withdrawals[1] != "e1|"+certB+"|rcv_2" {
		t.Fatalf("withdrawals: %v", st.withdrawals)
	}
}

func TestConsumer_AnEventThatCannotBeHandledIsAcknowledgedAfterTheRetriesAndLeftToTheSweep(t *testing.T) {
	st := &eventStore{listErr: errors.New("db down")}
	rd := &fakeReader{msgs: []kafka.Message{{Value: []byte(dqEvent)}, {Value: []byte(recEvent)}}}
	c := newConsumer(st, &fakeStatus{})
	c.Run(context.Background(), rd)
	// Head-of-line blocking would stop the feed on a persistent failure; the sweep is the backstop, so both are acknowledged.
	if len(rd.committed) != 2 {
		t.Fatalf("committed %d: a failing event blocked the topic", len(rd.committed))
	}
}

func TestConsumer_ACancelledRunDoesNotAcknowledgeWhatItDidNotFinish(t *testing.T) {
	st := &eventStore{listErr: errors.New("db down")}
	ctx, cancel := context.WithCancel(context.Background())
	c := newConsumer(st, &fakeStatus{})
	c.RetryDelay = time.Hour // park in the retry wait
	rd := &fakeReader{msgs: []kafka.Message{{Value: []byte(dqEvent)}}}
	done := make(chan struct{})
	go func() { c.Run(ctx, rd); close(done) }()
	time.Sleep(50 * time.Millisecond)
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not stop on cancellation")
	}
	if len(rd.committed) != 0 {
		t.Fatalf("an unfinished event was acknowledged: %v", rd.committed)
	}
}

func TestConsumer_RetriesBeforeGivingUp(t *testing.T) {
	calls := 0
	st := &flakyStore{eventStore: eventStore{watchers: map[string][]Watched{certA: {{LegalEntityID: "e1", Ref: ref(certA)}}}}, failFirst: 2, calls: &calls}
	status := &fakeStatus{by: map[string]certreliance.Status{certA: {State: certreliance.StateRevoked, RevocationID: "dqx_1", Reason: "r"}}}
	c := &Consumer{Reactor: &Reactor{Store: st, Sweeper: &Sweeper{Store: &st.eventStore, Status: status}, Authz: &fakeAuthz{}, Principal: "p"}, Attempts: 3, RetryDelay: time.Millisecond}
	if err := c.Handle(context.Background(), []byte(dqEvent)); err != nil {
		t.Fatal(err)
	}
	if calls != 3 || len(st.withdrawals) != 1 {
		t.Fatalf("lookups %d (want 3: two failures then success), withdrawals %v", calls, st.withdrawals)
	}
}

type flakyStore struct {
	eventStore
	failFirst int
	calls     *int
}

func (f *flakyStore) ListWatchers(ctx context.Context, tenant string, r certreliance.Ref) ([]Watched, error) {
	*f.calls++
	if *f.calls <= f.failFirst {
		return nil, errors.New("transient")
	}
	return f.eventStore.ListWatchers(ctx, tenant, r)
}
