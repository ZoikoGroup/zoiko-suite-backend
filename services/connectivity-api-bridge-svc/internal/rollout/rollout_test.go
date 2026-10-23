package rollout

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"zoiko.io/connectivity-api-bridge-svc/internal/middleware"
	"zoiko.io/connectivity-api-bridge-svc/internal/xicconn"
)

const tenantA, tenantB = "tenant-a", "tenant-b"

type fakeConns struct {
	mu    sync.Mutex
	conns map[string]*xicconn.Connection
	cert  string
}

func (f *fakeConns) Connection(_ context.Context, id string) (*xicconn.Connection, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	c, ok := f.conns[id]
	if !ok {
		return nil, xicconn.ErrNotFound
	}
	cc := *c
	return &cc, nil
}

func (f *fakeConns) CertificationStatusOf(context.Context, *xicconn.Connection) (*xicconn.CertStatus, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return &xicconn.CertStatus{State: f.cert}, nil
}

type clock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *clock) now() time.Time { c.mu.Lock(); defer c.mu.Unlock(); return c.t }
func (c *clock) add(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

type rig struct {
	svc   *Service
	repo  *MemRepo
	conns *fakeConns
	clk   *clock
	gate  *Gate
}

func newRig(t *testing.T) *rig {
	t.Helper()
	repo := NewMemRepo()
	conns := &fakeConns{cert: xicconn.CertValid, conns: map[string]*xicconn.Connection{
		"conn-1":  {ConnectionID: "conn-1", TenantID: tenantA, Environment: xicconn.EnvProduction, ConnectorID: "bank-x", Status: xicconn.StatusActive},
		"conn-sb": {ConnectionID: "conn-sb", TenantID: tenantA, Environment: xicconn.EnvSandbox, ConnectorID: "bank-x", Status: xicconn.StatusActive},
		"conn-b":  {ConnectionID: "conn-b", TenantID: tenantB, Environment: xicconn.EnvProduction, ConnectorID: "bank-x", Status: xicconn.StatusActive},
	}}
	clk := &clock{t: time.Date(2026, 10, 1, 9, 0, 0, 0, time.UTC)}
	svc := &Service{Repo: repo, Conns: conns, Clock: clk.now}
	return &rig{svc: svc, repo: repo, conns: conns, clk: clk, gate: &Gate{Repo: repo}}
}

func ctxA() context.Context { return middleware.WithTenant(context.Background(), tenantA) }
func ctxB() context.Context { return middleware.WithTenant(context.Background(), tenantB) }

var (
	alice = Actor{PrincipalID: "alice"}
	bob   = Actor{PrincipalID: "bob"}
)

func (r *rig) planned(t *testing.T) *Rollout {
	t.Helper()
	ro, err := r.svc.Create(ctxA(), alice, "conn-1", Terms{PilotOperations: []string{"pay", "status"}, PilotDailyCap: 3, MinSoakHours: 24, MinSuccess: 5, MaxFailurePct: 10})
	if err != nil {
		t.Fatal(err)
	}
	return ro
}

func (r *rig) advance(t *testing.T, id string, atts []string) *Rollout {
	t.Helper()
	q, err := r.svc.RequestAdvance(ctxA(), alice, id, atts, "go")
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	if _, err := r.svc.Decide(ctxA(), bob, q.RequestID, true, "ok"); err != nil {
		t.Fatalf("decide: %v", err)
	}
	ro, err := r.repo.GetRollout(ctxA(), id)
	if err != nil {
		t.Fatal(err)
	}
	return ro
}

func (r *rig) pilot(t *testing.T) *Rollout {
	ro := r.planned(t)
	return r.advance(t, ro.RolloutID, Attestations)
}

func conn() *xicconn.Connection {
	return &xicconn.Connection{ConnectionID: "conn-1", TenantID: tenantA}
}

func admit(r *rig, ctx context.Context, op string) error {
	return r.gate.Admit(ctx, conn(), op, r.clk.now())
}

func refusal(t *testing.T, err error, code xicconn.RefusalCode) {
	t.Helper()
	var ref *xicconn.Refusal
	if !errors.As(err, &ref) || ref.Code != code {
		t.Fatalf("want refusal %s, got %v", code, err)
	}
}

func TestCreateRules(t *testing.T) {
	r := newRig(t)
	if _, err := r.svc.Create(ctxA(), alice, "conn-sb", Terms{PilotDailyCap: 5}); !errors.Is(err, ErrBadRequest) {
		t.Fatalf("a sandbox connection is not rolled out in stages: %v", err)
	}
	if _, err := r.svc.Create(ctxA(), alice, "conn-b", Terms{PilotDailyCap: 5}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("another tenant's connection reads as missing: %v", err)
	}
	if _, err := r.svc.Create(ctxA(), alice, "nope", Terms{PilotDailyCap: 5}); !errors.Is(err, ErrNotFound) {
		t.Fatal(err)
	}
	for _, tm := range []Terms{{}, {PilotDailyCap: -1}, {PilotDailyCap: 1 << 30}, {PilotDailyCap: 5, MaxFailurePct: 101}, {PilotDailyCap: 5, MinSuccess: -1}} {
		if _, err := r.svc.Create(ctxA(), alice, "conn-1", tm); !errors.Is(err, ErrBadRequest) {
			t.Fatalf("%+v should be refused: %v", tm, err)
		}
	}
	ro := r.planned(t)
	if ro.Stage != StagePlanned || len(ro.PilotOps) != 2 {
		t.Fatalf("%+v", ro)
	}
	if _, err := r.svc.Create(ctxA(), alice, "conn-1", Terms{PilotDailyCap: 5}); !errors.Is(err, ErrExists) {
		t.Fatalf("one live rollout per connection: %v", err)
	}
	if _, err := r.repo.GetRollout(ctxB(), ro.RolloutID); !errors.Is(err, ErrNotFound) {
		t.Fatal("another tenant must not read the rollout")
	}
}

func TestAdvanceNeedsAttestationsEvidenceAndASecondPerson(t *testing.T) {
	r := newRig(t)
	ro := r.planned(t)
	if _, err := r.svc.RequestAdvance(ctxA(), alice, ro.RolloutID, []string{AttSLOAlerts}, ""); !errors.Is(err, ErrBadRequest) {
		t.Fatalf("all four attestations start a pilot: %v", err)
	}
	if _, err := r.svc.RequestAdvance(ctxA(), alice, ro.RolloutID, []string{"VIBES"}, ""); !errors.Is(err, ErrBadRequest) {
		t.Fatal(err)
	}
	r.conns.cert = xicconn.CertStale
	_, err := r.svc.RequestAdvance(ctxA(), alice, ro.RolloutID, Attestations, "")
	var nr *NotReadyError
	if !errors.As(err, &nr) || !errors.Is(err, ErrNotReady) || nr.Report.Ready {
		t.Fatalf("a stale certification blocks the pilot: %v", err)
	}
	r.conns.cert = xicconn.CertValid
	r.repo.HighF = 1
	if _, err := r.svc.RequestAdvance(ctxA(), alice, ro.RolloutID, Attestations, ""); !errors.Is(err, ErrNotReady) {
		t.Fatalf("an open HIGH variance blocks the pilot: %v", err)
	}
	r.repo.HighF, r.repo.UnknownF = 0, 2
	if _, err := r.svc.RequestAdvance(ctxA(), alice, ro.RolloutID, Attestations, ""); !errors.Is(err, ErrNotReady) {
		t.Fatalf("an overdue UNKNOWN blocks the pilot: %v", err)
	}
	r.repo.UnknownF = 0
	q, err := r.svc.RequestAdvance(ctxA(), alice, ro.RolloutID, Attestations, "start")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.svc.RequestAdvance(ctxA(), alice, ro.RolloutID, Attestations, ""); err == nil {
		t.Fatal("a second open request for the same edge is refused")
	}
	if _, err := r.svc.Decide(ctxA(), alice, q.RequestID, true, ""); !errors.Is(err, ErrSelfAction) {
		t.Fatalf("the requester does not approve: %v", err)
	}
	if _, err := r.svc.Decide(ctxA(), alice, q.RequestID, false, ""); !errors.Is(err, ErrSelfAction) {
		t.Fatalf("nor reject: %v", err)
	}
	if got, _ := r.repo.GetRollout(ctxA(), ro.RolloutID); got.Stage != StagePlanned {
		t.Fatal("nothing moved")
	}
	// the evidence is recomputed at the decision
	r.repo.HighF = 1
	if _, err := r.svc.Decide(ctxA(), bob, q.RequestID, true, ""); !errors.Is(err, ErrNotReady) {
		t.Fatalf("approval does not rest on stale evidence: %v", err)
	}
	r.repo.HighF = 0
	done, err := r.svc.Decide(ctxA(), bob, q.RequestID, true, "ok")
	if err != nil || done.Status != ReqUsed {
		t.Fatalf("%+v %v", done, err)
	}
	got, _ := r.repo.GetRollout(ctxA(), ro.RolloutID)
	if got.Stage != StagePilot {
		t.Fatalf("stage %s", got.Stage)
	}
	// deciding again changes nothing
	if again, err := r.svc.Decide(ctxA(), bob, q.RequestID, true, ""); err != nil || again.Status != ReqUsed {
		t.Fatalf("%+v %v", again, err)
	}
	// another tenant cannot decide it
	if _, err := r.svc.Decide(ctxB(), bob, q.RequestID, true, ""); !errors.Is(err, ErrNotFound) {
		t.Fatalf("cross-tenant decide: %v", err)
	}
}

func TestRejectAndExpiry(t *testing.T) {
	r := newRig(t)
	ro := r.planned(t)
	q, _ := r.svc.RequestAdvance(ctxA(), alice, ro.RolloutID, Attestations, "")
	rej, err := r.svc.Decide(ctxA(), bob, q.RequestID, false, "no")
	if err != nil || rej.Status != ReqRejected {
		t.Fatalf("%+v %v", rej, err)
	}
	if _, err := r.svc.Decide(ctxA(), bob, q.RequestID, true, ""); !errors.Is(err, ErrState) {
		t.Fatalf("a rejected request is final: %v", err)
	}
	q2, err := r.svc.RequestAdvance(ctxA(), alice, ro.RolloutID, Attestations, "")
	if err != nil {
		t.Fatal(err)
	}
	r.clk.add(73 * time.Hour)
	if _, err := r.svc.Decide(ctxA(), bob, q2.RequestID, true, ""); !errors.Is(err, ErrState) {
		t.Fatalf("an expired request is not approved: %v", err)
	}
	if got, _ := r.repo.GetRequest(ctxA(), q2.RequestID); got.Status != ReqExpired {
		t.Fatalf("status %s", got.Status)
	}
}

func TestApprovalSurvivesACrashBeforeItIsApplied(t *testing.T) {
	r := newRig(t)
	ro := r.planned(t)
	q, _ := r.svc.RequestAdvance(ctxA(), alice, ro.RolloutID, Attestations, "")
	crash := true
	r.repo.Fail = func(op string) error {
		if op == "MutateRollout" && crash {
			return errors.New("connection reset")
		}
		return nil
	}
	if _, err := r.svc.Decide(ctxA(), bob, q.RequestID, true, ""); err == nil {
		t.Fatal("the crash is reported")
	}
	if got, _ := r.repo.GetRequest(ctxA(), q.RequestID); got.Status != ReqApproved {
		t.Fatalf("the approval is durable: %s", got.Status)
	}
	if got, _ := r.repo.GetRollout(ctxA(), ro.RolloutID); got.Stage != StagePlanned {
		t.Fatal("the stage did not move on a failed apply")
	}
	crash = false
	done, err := r.svc.Decide(ctxA(), bob, q.RequestID, true, "")
	if err != nil || done.Status != ReqUsed {
		t.Fatalf("a repeat finishes it: %+v %v", done, err)
	}
	if got, _ := r.repo.GetRollout(ctxA(), ro.RolloutID); got.Stage != StagePilot {
		t.Fatalf("stage %s", got.Stage)
	}
}

func TestGateStages(t *testing.T) {
	r := newRig(t)
	if err := admit(r, ctxA(), "pay"); err != nil {
		t.Fatalf("a connection with no rollout is not gated unless one is required: %v", err)
	}
	req := &Gate{Repo: r.repo, Required: true}
	refusal(t, req.Admit(ctxA(), conn(), "pay", r.clk.now()), xicconn.CodeNotActive)

	ro := r.planned(t)
	refusal(t, admit(r, ctxA(), "pay"), xicconn.CodeNotActive) // PLANNED carries nothing
	ro = r.advance(t, ro.RolloutID, Attestations)
	if ro.Stage != StagePilot {
		t.Fatal(ro.Stage)
	}
	refusal(t, admit(r, ctxA(), "refund"), xicconn.CodeNotActive) // not in the pilot
	for i := 0; i < 3; i++ {
		if err := admit(r, ctxA(), "pay"); err != nil {
			t.Fatalf("within the budget: %v", err)
		}
	}
	refusal(t, admit(r, ctxA(), "pay"), xicconn.CodeRateLimited)
	// a re-check of admitted work takes nothing, and a spent budget does not refuse it
	if err := admit(r, xicconn.WithoutBudget(ctxA()), "pay"); err != nil {
		t.Fatalf("a submit of a reserved attempt is not a new admission: %v", err)
	}
	if n, _ := r.repo.UsedToday(ctxA(), ro.RolloutID, r.clk.now()); n != 3 {
		t.Fatalf("budget used %d", n)
	}
	// the next day starts a fresh budget
	r.clk.add(24 * time.Hour)
	if err := admit(r, ctxA(), "status"); err != nil {
		t.Fatalf("a new day: %v", err)
	}
	// a failing read fails closed
	r.repo.Fail = func(op string) error {
		if op == "LatestRolloutFor" {
			return errors.New("db down")
		}
		return nil
	}
	if err := admit(r, ctxA(), "pay"); !errors.Is(err, xicconn.ErrDependency) {
		t.Fatalf("an unreadable stage must not admit: %v", err)
	}
	r.repo.Fail = nil

	if _, err := r.svc.Rollback(ctxA(), bob, ro.RolloutID, "provider incident", 0); err != nil {
		t.Fatal(err)
	}
	refusal(t, admit(r, ctxA(), "pay"), xicconn.CodeNotActive)
	refusal(t, admit(r, xicconn.WithoutBudget(ctxA()), "pay"), xicconn.CodeNotActive) // a rolled-back rollout carries nothing at all
	// and not being required must not turn a rolled-back connection back on
	if err := (&Gate{Repo: r.repo}).Admit(ctxA(), conn(), "pay", r.clk.now()); err == nil {
		t.Fatal("a rolled-back connection must stay blocked even when rollouts are not required")
	}
}

func TestGeneralNeedsSoakVolumeAndCleanOutcomes(t *testing.T) {
	r := newRig(t)
	ro := r.pilot(t)
	atts := []string{AttNoP0P1}
	try := func() error { _, err := r.svc.RequestAdvance(ctxA(), alice, ro.RolloutID, atts, ""); return err }
	if err := try(); !errors.Is(err, ErrNotReady) {
		t.Fatalf("a pilot that has not soaked is not general: %v", err)
	}
	r.clk.add(25 * time.Hour)
	r.repo.OutcomesF = Outcomes{Success: 3}
	if err := try(); !errors.Is(err, ErrNotReady) {
		t.Fatalf("too few exchanges: %v", err)
	}
	r.repo.OutcomesF = Outcomes{Success: 6, Failure: 1, Unknown: 1}
	if err := try(); !errors.Is(err, ErrNotReady) {
		t.Fatalf("25%% failures against a 10%% limit: %v", err)
	}
	r.repo.OutcomesF = Outcomes{Success: 20, Failure: 1}
	if _, err := r.svc.RequestAdvance(ctxA(), alice, ro.RolloutID, nil, ""); !errors.Is(err, ErrBadRequest) {
		t.Fatalf("no open P0/P1 defects must be attested: %v", err)
	}
	got := r.advance(t, ro.RolloutID, atts)
	if got.Stage != StageGeneral {
		t.Fatal(got.Stage)
	}
	if err := admit(r, ctxA(), "refund"); err != nil {
		t.Fatalf("general carries everything: %v", err)
	}
	// nothing moves past GENERAL
	if _, err := r.svc.RequestAdvance(ctxA(), alice, ro.RolloutID, atts, ""); !errors.Is(err, ErrState) {
		t.Fatal(err)
	}
}

func TestRollback(t *testing.T) {
	r := newRig(t)
	ro := r.pilot(t)
	if _, err := r.svc.Rollback(ctxA(), bob, ro.RolloutID, "  ", 0); !errors.Is(err, ErrBadRequest) {
		t.Fatalf("a reason is required: %v", err)
	}
	if _, err := r.svc.Rollback(ctxA(), bob, ro.RolloutID, "x", ro.Version+5); !errors.Is(err, ErrVersionConflict) {
		t.Fatal(err)
	}
	if _, err := r.svc.Rollback(ctxB(), bob, ro.RolloutID, "x", 0); !errors.Is(err, ErrNotFound) {
		t.Fatalf("another tenant cannot roll it back: %v", err)
	}
	// no second person is needed for the safe direction
	got, err := r.svc.Rollback(ctxA(), alice, ro.RolloutID, "provider incident", 0)
	if err != nil || got.Stage != StageRolledBack || got.RolledBackBy != "alice" {
		t.Fatalf("%+v %v", got, err)
	}
	if _, err := r.svc.Rollback(ctxA(), alice, ro.RolloutID, "again", 0); !errors.Is(err, ErrState) {
		t.Fatalf("final: %v", err)
	}
	// a rolled-back rollout cannot be advanced, but a new one can be planned
	if _, err := r.svc.RequestAdvance(ctxA(), alice, ro.RolloutID, Attestations, ""); !errors.Is(err, ErrState) {
		t.Fatal(err)
	}
	if _, err := r.svc.Create(ctxA(), alice, "conn-1", Terms{PilotDailyCap: 2}); err != nil {
		t.Fatalf("a new rollout after a rollback: %v", err)
	}
}

func TestPilotRollsItselfBack(t *testing.T) {
	cases := []struct {
		name  string
		setup func(r *rig)
		back  bool
	}{
		{"high variance", func(r *rig) { r.repo.HighF = 1 }, true},
		{"overdue unknown", func(r *rig) { r.repo.UnknownF = 1 }, true},
		{"failure rate", func(r *rig) { r.repo.OutcomesF = Outcomes{Success: 6, Failure: 6} }, true},
		{"too few outcomes to judge", func(r *rig) { r.repo.OutcomesF = Outcomes{Failure: 3} }, false},
		{"healthy", func(r *rig) { r.repo.OutcomesF = Outcomes{Success: 50, Failure: 1} }, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r := newRig(t)
			ro := r.pilot(t)
			c.setup(r)
			n, err := r.svc.EvaluatePilots(context.Background(), 10)
			if err != nil || n != 1 {
				t.Fatalf("%d %v", n, err)
			}
			got, _ := r.repo.GetRollout(ctxA(), ro.RolloutID)
			if (got.Stage == StageRolledBack) != c.back {
				t.Fatalf("stage %s", got.Stage)
			}
			if c.back {
				if !strings.HasPrefix(got.RolledBackBy, "system:") || !strings.HasPrefix(got.RollbackReason, "AUTO") {
					t.Fatalf("%+v", got)
				}
				refusal(t, admit(r, ctxA(), "pay"), xicconn.CodeNotActive)
			}
		})
	}
	// general and planned rollouts are not evaluated
	r := newRig(t)
	r.planned(t)
	r.repo.HighF = 1
	if n, _ := r.svc.EvaluatePilots(context.Background(), 10); n != 0 {
		t.Fatalf("only pilots are evaluated: %d", n)
	}
}

func TestConcurrentAdmissionsNeverExceedTheBudget(t *testing.T) {
	r := newRig(t)
	r.pilot(t)
	var wg sync.WaitGroup
	var ok, refused int
	var mu sync.Mutex
	for i := 0; i < 60; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			err := admit(r, ctxA(), "pay")
			mu.Lock()
			defer mu.Unlock()
			if err == nil {
				ok++
			} else {
				refused++
			}
		}()
	}
	wg.Wait()
	if ok != 3 || refused != 57 {
		t.Fatalf("admitted %d refused %d", ok, refused)
	}
}

// ── legacy migration ───────────────────────────────────────────────────────────

func hash(i int) string { return fmt.Sprintf("%064x", i+1) }

func (r *rig) legacyShadowing(t *testing.T) *Legacy { return r.legacyShadowingOn(t, "conn-1") }

func (r *rig) legacyShadowingOn(t *testing.T, connID string) *Legacy {
	t.Helper()
	l, err := r.svc.RegisterLegacy(ctxA(), alice, LegacyInput{Name: "bank-x-payments", OwningService: "bank-integration-svc", Direction: "outbound"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.svc.MapLegacy(ctxA(), alice, l.LegacyID, connID, 0); err != nil {
		t.Fatal(err)
	}
	if _, err := r.svc.StartShadow(ctxA(), alice, l.LegacyID, 0); err != nil {
		t.Fatal(err)
	}
	got, _ := r.repo.GetLegacy(ctxA(), l.LegacyID)
	return got
}

func (r *rig) register(t *testing.T, id string, n int, outcome string) {
	t.Helper()
	var in []LegacyExchange
	for i := 0; i < n; i++ {
		in = append(in, LegacyExchange{ExternalRef: fmt.Sprintf("ext-%d", i), DomainRequestRef: fmt.Sprintf("req-%d", i), Outcome: outcome,
			PayloadHash: hash(i), OccurredAt: r.clk.now().Add(-time.Hour)})
		r.repo.Counterpart[fmt.Sprintf("req-%d", i)] = "SETTLED"
	}
	if _, _, err := r.svc.RegisterExchanges(ctxA(), alice, id, in); err != nil {
		t.Fatal(err)
	}
}

func TestLegacyInventoryRules(t *testing.T) {
	r := newRig(t)
	for _, in := range []LegacyInput{{Name: "x", OwningService: "s", Direction: "OUTBOUND"}, {Name: "good-name", OwningService: "", Direction: "OUTBOUND"},
		{Name: "good-name", OwningService: "s", Direction: "SIDEWAYS"}} {
		if _, err := r.svc.RegisterLegacy(ctxA(), alice, in); !errors.Is(err, ErrBadRequest) {
			t.Fatalf("%+v: %v", in, err)
		}
	}
	l, err := r.svc.RegisterLegacy(ctxA(), alice, LegacyInput{Name: "bank-x-payments", OwningService: "bank", Direction: "OUTBOUND"})
	if err != nil || l.State != LegacyDiscovered {
		t.Fatalf("%+v %v", l, err)
	}
	if _, err := r.svc.RegisterLegacy(ctxA(), alice, LegacyInput{Name: "bank-x-payments", OwningService: "bank", Direction: "OUTBOUND"}); !errors.Is(err, ErrExists) {
		t.Fatalf("one entry per name: %v", err)
	}
	if _, err := r.svc.MapLegacy(ctxA(), alice, l.LegacyID, "conn-b", 0); !errors.Is(err, ErrNotFound) {
		t.Fatalf("another tenant's connection reads as missing: %v", err)
	}
	if _, err := r.svc.StartShadow(ctxA(), alice, l.LegacyID, 0); !errors.Is(err, ErrState) {
		t.Fatalf("shadow needs a mapping: %v", err)
	}
	if _, _, err := r.svc.RegisterExchanges(ctxA(), alice, l.LegacyID, []LegacyExchange{{ExternalRef: "a", Outcome: "SETTLED", PayloadHash: hash(1), OccurredAt: r.clk.now()}}); !errors.Is(err, ErrState) {
		t.Fatalf("nothing registers before it is mapped: %v", err)
	}
	if _, err := r.svc.MapLegacy(ctxA(), alice, l.LegacyID, "conn-1", 99); !errors.Is(err, ErrVersionConflict) {
		t.Fatal(err)
	}
	if _, err := r.repo.GetLegacy(ctxB(), l.LegacyID); !errors.Is(err, ErrNotFound) {
		t.Fatal("cross-tenant read")
	}
}

func TestRegistrationIsAppendOnlyAndStrict(t *testing.T) {
	r := newRig(t)
	l := r.legacyShadowing(t)
	good := LegacyExchange{ExternalRef: "ext-1", DomainRequestRef: "req-1", Outcome: "PENDING", PayloadHash: hash(1), OccurredAt: r.clk.now().Add(-time.Minute)}
	st, dup, err := r.svc.RegisterExchanges(ctxA(), alice, l.LegacyID, []LegacyExchange{good})
	if err != nil || st != 1 || dup != 0 {
		t.Fatalf("%d %d %v", st, dup, err)
	}
	if st, dup, _ = r.svc.RegisterExchanges(ctxA(), alice, l.LegacyID, []LegacyExchange{good}); st != 0 || dup != 1 {
		t.Fatalf("the same registration again is a no-op: %d %d", st, dup)
	}
	// the outcome moving on is a new registration, not an edit
	settled := good
	settled.Outcome = "SETTLED"
	if st, _, err = r.svc.RegisterExchanges(ctxA(), alice, l.LegacyID, []LegacyExchange{settled}); err != nil || st != 1 {
		t.Fatalf("%d %v", st, err)
	}
	// the same reference with different content is refused, and nothing of that call is stored
	forged := good
	forged.PayloadHash = hash(99)
	fresh := LegacyExchange{ExternalRef: "ext-2", Outcome: "SETTLED", PayloadHash: hash(2), OccurredAt: r.clk.now().Add(-time.Minute)}
	if _, _, err = r.svc.RegisterExchanges(ctxA(), alice, l.LegacyID, []LegacyExchange{fresh, forged}); !errors.Is(err, ErrConflict) {
		t.Fatalf("%v", err)
	}
	if got, _ := r.repo.LatestExchanges(ctxA(), l.LegacyID, r.clk.now().Add(-time.Hour), r.clk.now(), 10); len(got) != 1 || got[0].Outcome != "SETTLED" {
		t.Fatalf("a refused call stores nothing: %+v", got)
	}
	bads := []LegacyExchange{
		{ExternalRef: "", Outcome: "SETTLED", PayloadHash: hash(1), OccurredAt: r.clk.now()},
		{ExternalRef: "a b", Outcome: "SETTLED", PayloadHash: hash(1), OccurredAt: r.clk.now()},
		{ExternalRef: "ok", Outcome: "DONE", PayloadHash: hash(1), OccurredAt: r.clk.now()},
		{ExternalRef: "ok", Outcome: "SETTLED", PayloadHash: "xyz", OccurredAt: r.clk.now()},
		{ExternalRef: "ok", Outcome: "SETTLED", PayloadHash: hash(1), OccurredAt: r.clk.now().Add(time.Hour)},
		{ExternalRef: "ok", Outcome: "SETTLED", PayloadHash: hash(1)},
	}
	for i, b := range bads {
		if _, _, err := r.svc.RegisterExchanges(ctxA(), alice, l.LegacyID, []LegacyExchange{b}); !errors.Is(err, ErrBadRequest) {
			t.Fatalf("bad[%d]: %v", i, err)
		}
	}
	if _, _, err := r.svc.RegisterExchanges(ctxA(), alice, l.LegacyID, nil); !errors.Is(err, ErrBadRequest) {
		t.Fatal(err)
	}
	if _, _, err := r.svc.RegisterExchanges(ctxB(), alice, l.LegacyID, []LegacyExchange{good}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("another tenant cannot register: %v", err)
	}
}

func TestParityAndCutover(t *testing.T) {
	r := newRig(t)
	l := r.legacyShadowing(t)
	r.register(t, l.LegacyID, 25, "SETTLED")
	win := ParityInput{From: r.clk.now().Add(-2 * time.Hour), To: r.clk.now()}
	if _, err := r.svc.RunParity(ctxA(), alice, l.LegacyID, ParityInput{From: win.To, To: win.From}); !errors.Is(err, ErrBadRequest) {
		t.Fatal(err)
	}
	// a hole on the platform side fails parity and says where
	delete(r.repo.Counterpart, "req-3")
	r.repo.Counterpart["req-4"] = "FAILED"
	p, err := r.svc.RunParity(ctxA(), alice, l.LegacyID, win)
	if err != nil {
		t.Fatal(err)
	}
	if p.Passed || p.Compared != 25 || p.MissingInXIC != 1 || p.OutcomeDiffers != 1 || p.Matched != 23 {
		t.Fatalf("%+v", p)
	}
	if !strings.Contains(string(p.Samples), "req-3") || !strings.Contains(string(p.Samples), "OUTCOME_DIFFERS") {
		t.Fatalf("samples: %s", p.Samples)
	}
	r.clk.add(25 * time.Hour) // soaked, but the only run failed
	if _, err := r.svc.RequestLegacyAdvance(ctxA(), alice, l.LegacyID, ""); !errors.Is(err, ErrNotReady) {
		t.Fatalf("a failed parity run does not cut over: %v", err)
	}
	// repaired, rerun inside the window
	r.repo.Counterpart["req-3"], r.repo.Counterpart["req-4"] = "SETTLED", "SETTLED"
	win = ParityInput{From: r.clk.now().Add(-27 * time.Hour), To: r.clk.now()}
	p, err = r.svc.RunParity(ctxA(), alice, l.LegacyID, win)
	if err != nil || !p.Passed {
		t.Fatalf("%+v %v", p, err)
	}
	rep, to, err := r.svc.LegacyReadiness(ctxA(), l.LegacyID)
	if err != nil || to != LegacyCutover || !rep.Ready {
		t.Fatalf("%+v %s %v", rep, to, err)
	}
	q, err := r.svc.RequestLegacyAdvance(ctxA(), alice, l.LegacyID, "cut over")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.svc.Decide(ctxA(), alice, q.RequestID, true, ""); !errors.Is(err, ErrSelfAction) {
		t.Fatalf("a second person decides the cutover: %v", err)
	}
	if got, _ := r.repo.GetLegacy(ctxA(), l.LegacyID); got.State != LegacyShadow {
		t.Fatal("nothing moved")
	}
	// parity too old at the decision is not trusted
	r.clk.add(25 * time.Hour)
	if _, err := r.svc.Decide(ctxA(), bob, q.RequestID, true, ""); !errors.Is(err, ErrNotReady) {
		t.Fatalf("a stale parity run is not evidence: %v", err)
	}
	if _, err := r.svc.RunParity(ctxA(), alice, l.LegacyID, ParityInput{From: r.clk.now().Add(-200 * time.Hour), To: r.clk.now()}); err != nil {
		t.Fatal(err)
	}
	if _, err := r.svc.Decide(ctxA(), bob, q.RequestID, true, ""); err != nil {
		t.Fatal(err)
	}
	if got, _ := r.repo.GetLegacy(ctxA(), l.LegacyID); got.State != LegacyCutover {
		t.Fatalf("state %s", got.State)
	}
	// after cutover the legacy path is not supposed to send
	if _, _, err := r.svc.RegisterExchanges(ctxA(), alice, l.LegacyID, []LegacyExchange{{ExternalRef: "late", Outcome: "SETTLED", PayloadHash: hash(1), OccurredAt: r.clk.now()}}); !errors.Is(err, ErrState) {
		t.Fatalf("registrations stop at cutover: %v", err)
	}
	// reverting needs a reason and no second person
	if _, err := r.svc.RevertCutover(ctxA(), alice, l.LegacyID, "", 0); !errors.Is(err, ErrBadRequest) {
		t.Fatal(err)
	}
	// retirement is a second request, after the cutover has held
	if _, err := r.svc.RequestLegacyAdvance(ctxA(), alice, l.LegacyID, ""); !errors.Is(err, ErrNotReady) {
		t.Fatalf("retirement waits for the cutover to hold: %v", err)
	}
	r.clk.add(73 * time.Hour)
	q2, err := r.svc.RequestLegacyAdvance(ctxA(), alice, l.LegacyID, "retire")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.svc.Decide(ctxA(), bob, q2.RequestID, true, ""); err != nil {
		t.Fatal(err)
	}
	if got, _ := r.repo.GetLegacy(ctxA(), l.LegacyID); got.State != LegacyRetired {
		t.Fatalf("state %s", got.State)
	}
	if _, err := r.svc.RevertCutover(ctxA(), alice, l.LegacyID, "x", 0); !errors.Is(err, ErrState) {
		t.Fatalf("retired is final: %v", err)
	}
}

func TestRevertCutoverAndStopShadow(t *testing.T) {
	r := newRig(t)
	l := r.legacyShadowing(t)
	r.register(t, l.LegacyID, 25, "SETTLED")
	r.clk.add(25 * time.Hour)
	if _, err := r.svc.RunParity(ctxA(), alice, l.LegacyID, ParityInput{From: r.clk.now().Add(-200 * time.Hour), To: r.clk.now()}); err != nil {
		t.Fatal(err)
	}
	q, _ := r.svc.RequestLegacyAdvance(ctxA(), alice, l.LegacyID, "")
	if _, err := r.svc.Decide(ctxA(), bob, q.RequestID, true, ""); err != nil {
		t.Fatal(err)
	}
	got, err := r.svc.RevertCutover(ctxA(), alice, l.LegacyID, "provider rejects the new mapping", 0)
	if err != nil || got.State != LegacyShadow {
		t.Fatalf("%+v %v", got, err)
	}
	if _, err := r.svc.StopShadow(ctxA(), alice, l.LegacyID, 0); err != nil {
		t.Fatal(err)
	}
	if got, _ := r.repo.GetLegacy(ctxA(), l.LegacyID); got.State != LegacyMapped {
		t.Fatal(got.State)
	}
	if _, err := r.svc.StopShadow(ctxA(), alice, l.LegacyID, 0); !errors.Is(err, ErrState) {
		t.Fatal(err)
	}
}

func TestInboundParityComparesCounts(t *testing.T) {
	r := newRig(t)
	l, _ := r.svc.RegisterLegacy(ctxA(), alice, LegacyInput{Name: "tax-webhook", OwningService: "tax", Direction: "INBOUND"})
	if _, err := r.svc.MapLegacy(ctxA(), alice, l.LegacyID, "conn-1", 0); err != nil {
		t.Fatal(err)
	}
	if _, err := r.svc.StartShadow(ctxA(), alice, l.LegacyID, 0); err != nil {
		t.Fatal(err)
	}
	var in []LegacyExchange
	for i := 0; i < 30; i++ {
		in = append(in, LegacyExchange{ExternalRef: fmt.Sprintf("evt-%d", i), Outcome: "SETTLED", PayloadHash: hash(i), OccurredAt: r.clk.now().Add(-time.Hour)})
	}
	if _, _, err := r.svc.RegisterExchanges(ctxA(), alice, l.LegacyID, in); err != nil {
		t.Fatal(err)
	}
	win := ParityInput{From: r.clk.now().Add(-2 * time.Hour), To: r.clk.now()}
	r.repo.InboundN = 28
	p, _ := r.svc.RunParity(ctxA(), alice, l.LegacyID, win)
	if p.Passed || p.MissingInXIC != 2 {
		t.Fatalf("%+v", p)
	}
	r.repo.InboundN = 31
	p, _ = r.svc.RunParity(ctxA(), alice, l.LegacyID, win)
	if !p.Passed || p.XICOnly != 1 {
		t.Fatalf("%+v", p)
	}
}

func TestEveryEventIsProducedAndIsAValidEnvelope(t *testing.T) {
	r := newRig(t)
	ro := r.planned(t)
	r.advance(t, ro.RolloutID, Attestations)
	q, _ := r.svc.RequestAdvance(ctxA(), alice, ro.RolloutID, nil, "") // refused: no attestation, nothing emitted
	_ = q
	r.clk.add(25 * time.Hour)
	r.repo.OutcomesF = Outcomes{Success: 10}
	// a rejected request
	q1, err := r.svc.RequestAdvance(ctxA(), alice, ro.RolloutID, []string{AttNoP0P1}, "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.svc.Decide(ctxA(), bob, q1.RequestID, false, "later"); err != nil {
		t.Fatal(err)
	}
	l := r.legacyShadowingOn(t, "conn-sb")
	r.register(t, l.LegacyID, 25, "SETTLED")
	r.clk.add(25 * time.Hour)
	if _, err := r.svc.RunParity(ctxA(), alice, l.LegacyID, ParityInput{From: r.clk.now().Add(-200 * time.Hour), To: r.clk.now()}); err != nil {
		t.Fatal(err)
	}
	cq, _ := r.svc.RequestLegacyAdvance(ctxA(), alice, l.LegacyID, "")
	r.svc.Decide(ctxA(), bob, cq.RequestID, true, "")
	r.svc.RevertCutover(ctxA(), alice, l.LegacyID, "x", 0)
	r.svc.StopShadow(ctxA(), alice, l.LegacyID, 0)
	r.svc.StartShadow(ctxA(), alice, l.LegacyID, 0)
	r.clk.add(25 * time.Hour)
	r.svc.RunParity(ctxA(), alice, l.LegacyID, ParityInput{From: r.clk.now().Add(-200 * time.Hour), To: r.clk.now()})
	cq, _ = r.svc.RequestLegacyAdvance(ctxA(), alice, l.LegacyID, "")
	r.svc.Decide(ctxA(), bob, cq.RequestID, true, "")
	r.clk.add(73 * time.Hour)
	rq, _ := r.svc.RequestLegacyAdvance(ctxA(), alice, l.LegacyID, "")
	r.svc.Decide(ctxA(), bob, rq.RequestID, true, "")

	if _, err := r.svc.Rollback(ctxA(), bob, ro.RolloutID, "test", 0); err != nil {
		t.Fatal(err)
	}
	got := map[string]bool{}
	for _, o := range r.repo.Out {
		got[o.Type] = true
		if o.Data["tenant_id"] != tenantA {
			t.Fatalf("%s carries no tenant: %v", o.Type, o.Data)
		}
		for k := range o.Data {
			if strings.Contains(strings.ToLower(k), "secret") || strings.Contains(strings.ToLower(k), "payload") {
				t.Fatalf("%s leaks %s", o.Type, k)
			}
		}
	}
	for _, typ := range EventTypes {
		if !got[typ] {
			t.Errorf("event %s was never produced by a real flow", typ)
		}
	}
	if len(got) != len(EventTypes) {
		t.Errorf("produced %d types, declared %d", len(got), len(EventTypes))
	}
}
