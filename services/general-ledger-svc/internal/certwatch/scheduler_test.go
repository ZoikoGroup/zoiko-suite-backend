package certwatch

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"zoiko.io/general-ledger-svc/internal/certreliance"
	svcmiddleware "zoiko.io/general-ledger-svc/internal/middleware"
)

type fakeScopes struct {
	scopes []Scope
	err    error
	calls  int
}

func (f *fakeScopes) ListScopes(context.Context) ([]Scope, error) {
	f.calls++
	return f.scopes, f.err
}

type fakeLock struct {
	held     bool // another replica holds it
	err      error
	taken    int
	released int
}

func (f *fakeLock) TryLock(context.Context) (func(), bool, error) {
	if f.err != nil {
		return nil, false, f.err
	}
	if f.held {
		return nil, false, nil
	}
	f.taken++
	return func() { f.released++ }, true, nil
}

type fakeAuthz struct {
	deny  map[string]bool // by entity
	seen  []string        // tenant|principal|entity|action
	errOn map[string]error
}

func (f *fakeAuthz) CheckAllowed(ctx context.Context, principal, entity, action string) error {
	f.seen = append(f.seen, svcmiddleware.TenantFromContext(ctx)+"|"+principal+"|"+entity+"|"+action)
	if err := f.errOn[entity]; err != nil {
		return err
	}
	if f.deny[entity] {
		return errors.New("denied")
	}
	return nil
}

// perScopeStore answers the sweeper per entity and records which (tenant, entity) it was asked about.
type perScopeStore struct {
	Store
	mu       sync.Mutex
	watched  map[string][]Watched // by entity
	listErr  map[string]error
	asked    []string
	flagsFor map[string]int
}

func (p *perScopeStore) ListWatched(_ context.Context, tenant, entity string) ([]Watched, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.asked = append(p.asked, tenant+"|"+entity)
	if err := p.listErr[entity]; err != nil {
		return nil, err
	}
	return p.watched[entity], nil
}
func (p *perScopeStore) RecordCheck(context.Context, string, string, certreliance.Ref, string) error {
	return nil
}
func (p *perScopeStore) RecordWithdrawal(_ context.Context, _, entity string, _ certreliance.Ref, _, _ string) (int, error) {
	return p.flagsFor[entity], nil
}

func newScheduler(scopes []Scope, store Store, status StatusReader, az *fakeAuthz, lock *fakeLock) (*Scheduler, *fakeScopes) {
	fs := &fakeScopes{scopes: scopes}
	return &Scheduler{Sweeper: &Sweeper{Store: store, Status: status}, Scopes: fs, Authz: az, Lock: lock, Principal: "svc-sweeper", Interval: time.Hour}, fs
}

func TestScheduler_SweepsEveryScopeAsItsPrincipalWithTheTenantOnTheContext(t *testing.T) {
	st := &perScopeStore{watched: map[string][]Watched{
		"e1": {{LegalEntityID: "e1", Ref: ref(certA)}},
		"e2": {{LegalEntityID: "e2", Ref: ref(certB)}},
	}, flagsFor: map[string]int{"e1": 2}}
	status := &fakeStatus{by: map[string]certreliance.Status{
		certA: {State: certreliance.StateRevoked, RevocationID: "dqx_1", Reason: "false pass"},
		certB: {State: certreliance.StateActive},
	}}
	az, lock := &fakeAuthz{}, &fakeLock{}
	s, _ := newScheduler([]Scope{{"t1", "e1"}, {"t2", "e2"}}, st, status, az, lock)

	sum, err := s.RunOnce(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if sum.Scopes != 2 || sum.Swept != 2 || sum.Skipped != 0 || sum.Checked != 2 || sum.Withdrawn != 1 || sum.NewFlags != 2 || sum.Locked {
		t.Fatalf("%+v", sum)
	}
	// Each scope was authorized for the sweep permission AS THE CONFIGURED PRINCIPAL, with that scope's own tenant on the context.
	want := []string{"t1|svc-sweeper|e1|GL_CERTIFICATION_WATCH_SWEEP", "t2|svc-sweeper|e2|GL_CERTIFICATION_WATCH_SWEEP"}
	if len(az.seen) != 2 || az.seen[0] != want[0] || az.seen[1] != want[1] {
		t.Fatalf("authorization: %v", az.seen)
	}
	if len(st.asked) != 2 || st.asked[0] != "t1|e1" || st.asked[1] != "t2|e2" {
		t.Fatalf("swept: %v", st.asked)
	}
	if lock.taken != 1 || lock.released != 1 {
		t.Fatalf("lock taken %d released %d", lock.taken, lock.released)
	}
}

func TestScheduler_AScopeItMayNotSweepIsSkippedAndTheOthersStillRun(t *testing.T) {
	st := &perScopeStore{watched: map[string][]Watched{
		"e1": {{LegalEntityID: "e1", Ref: ref(certA)}}, "e2": {{LegalEntityID: "e2", Ref: ref(certB)}}, "e3": {{LegalEntityID: "e3", Ref: ref(certC)}},
	}, listErr: map[string]error{"e2": errors.New("store down")}}
	az := &fakeAuthz{deny: map[string]bool{"e1": true}}
	s, _ := newScheduler([]Scope{{"t", "e1"}, {"t", "e2"}, {"t", "e3"}}, st, &fakeStatus{}, az, &fakeLock{})

	sum, err := s.RunOnce(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	// e1 is denied (never swept), e2's store fails (the sweep errors), e3 is swept: one failure never stops the rest.
	if sum.Scopes != 3 || sum.Swept != 1 || sum.Skipped != 2 {
		t.Fatalf("%+v", sum)
	}
	for _, a := range st.asked {
		if a == "t|e1" {
			t.Fatal("a scope the principal is not authorized for was swept")
		}
	}
	if len(st.asked) != 2 || st.asked[1] != "t|e3" {
		t.Fatalf("swept: %v", st.asked)
	}
}

func TestScheduler_UncheckableCertificationsAreCountedNeverTreatedAsInForce(t *testing.T) {
	st := &perScopeStore{watched: map[string][]Watched{"e1": {{LegalEntityID: "e1", Ref: ref(certA)}, {LegalEntityID: "e1", Ref: ref(certB)}}}}
	status := &fakeStatus{errs: map[string]error{certA: certreliance.ErrUnavailable}, by: map[string]certreliance.Status{certB: {State: certreliance.StateActive}}}
	s, _ := newScheduler([]Scope{{"t", "e1"}}, st, status, &fakeAuthz{}, &fakeLock{})
	sum, err := s.RunOnce(context.Background())
	if err != nil || sum.Swept != 1 || sum.Checked != 2 || sum.Failed != 1 || sum.NewFlags != 0 {
		t.Fatalf("%+v %v", sum, err)
	}
}

func TestScheduler_AReplicaThatCannotTakeTheLockDoesNothing(t *testing.T) {
	st := &perScopeStore{}
	az, lock := &fakeAuthz{}, &fakeLock{held: true}
	s, scopes := newScheduler([]Scope{{"t", "e1"}}, st, &fakeStatus{}, az, lock)
	sum, err := s.RunOnce(context.Background())
	if err != nil || !sum.Locked || sum.Scopes != 0 || scopes.calls != 0 || len(az.seen) != 0 || len(st.asked) != 0 {
		t.Fatalf("%+v %v (scope scans %d, authz %v, swept %v)", sum, err, scopes.calls, az.seen, st.asked)
	}
	// A lock that errors is an error, not a silent skip, and nothing runs.
	s2, _ := newScheduler([]Scope{{"t", "e1"}}, st, &fakeStatus{}, az, &fakeLock{err: errors.New("db down")})
	if _, err := s2.RunOnce(context.Background()); err == nil || len(st.asked) != 0 {
		t.Fatalf("lock error: %v", err)
	}
}

func TestScheduler_TheLockIsReleasedEvenWhenTheRunFails(t *testing.T) {
	lock := &fakeLock{}
	s, scopes := newScheduler(nil, &perScopeStore{}, &fakeStatus{}, &fakeAuthz{}, lock)
	scopes.err = errors.New("scan failed")
	if _, err := s.RunOnce(context.Background()); err == nil {
		t.Fatal("a failed scope scan was not reported")
	}
	if lock.taken != 1 || lock.released != 1 {
		t.Fatalf("lock taken %d released %d", lock.taken, lock.released)
	}
}

func TestScheduler_WithoutAPrincipalItRefusesToRunAndDoesNotStart(t *testing.T) {
	lock := &fakeLock{}
	s, scopes := newScheduler([]Scope{{"t", "e1"}}, &perScopeStore{}, &fakeStatus{}, &fakeAuthz{}, lock)
	s.Principal = ""
	if _, err := s.RunOnce(context.Background()); !errors.Is(err, ErrInvalid) || lock.taken != 0 || scopes.calls != 0 {
		t.Fatalf("%v (lock %d, scans %d)", err, lock.taken, scopes.calls)
	}
	// Run returns at once, without a principal or with the interval off: there is nothing to wait for.
	done := make(chan struct{})
	go func() { s.Run(context.Background()); close(done) }()
	s2, _ := newScheduler(nil, &perScopeStore{}, &fakeStatus{}, &fakeAuthz{}, lock)
	s2.Interval = 0
	d2 := make(chan struct{})
	go func() { s2.Run(context.Background()); close(d2) }()
	for _, c := range []chan struct{}{done, d2} {
		select {
		case <-c:
		case <-time.After(2 * time.Second):
			t.Fatal("a scheduler that must not start blocked")
		}
	}
}

func TestScheduler_RunRepeatsOnTheIntervalAndStopsWhenCancelled(t *testing.T) {
	st := &perScopeStore{watched: map[string][]Watched{"e1": {{LegalEntityID: "e1", Ref: ref(certA)}}}}
	lock := &fakeLock{}
	s, scopes := newScheduler([]Scope{{"t", "e1"}}, st, &fakeStatus{}, &fakeAuthz{}, lock)
	s.Interval = 5 * time.Millisecond
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { s.Run(ctx); close(done) }()
	deadline := time.After(3 * time.Second)
	for {
		st.mu.Lock()
		n := len(st.asked)
		st.mu.Unlock()
		if n >= 3 {
			break
		}
		select {
		case <-deadline:
			t.Fatalf("only %d runs in 3s", n)
		case <-time.After(5 * time.Millisecond):
		}
	}
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not stop after cancellation")
	}
	if lock.taken != lock.released || scopes.calls < 3 {
		t.Fatalf("lock taken %d released %d, scans %d", lock.taken, lock.released, scopes.calls)
	}
}
