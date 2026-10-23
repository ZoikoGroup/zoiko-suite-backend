package store_test

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"zoiko.io/connectivity-api-bridge-svc/internal/rollout"
	"zoiko.io/connectivity-api-bridge-svc/internal/store"
	"zoiko.io/connectivity-api-bridge-svc/internal/xicconn"
)

// These run the Wave 8 staged rollout and legacy migration against REAL Postgres as an ordinary role: the SQL, the guard
// triggers that refuse a step forward without an approved request decided by someone else, the atomic pilot budget,
// the append-only registrations, row-level security, and the facts read from the real attempt and exchange tables.

type prodConns struct {
	svc  *xicconn.Service
	cert string
}

// Connection reports the sandbox fixture as a production connection: the rollout rules are about production.
func (p *prodConns) Connection(ctx context.Context, id string) (*xicconn.Connection, error) {
	c, err := p.svc.Connection(ctx, id)
	if err != nil {
		return nil, err
	}
	c.Environment = xicconn.EnvProduction
	return c, nil
}

func (p *prodConns) CertificationStatusOf(context.Context, *xicconn.Connection) (*xicconn.CertStatus, error) {
	return &xicconn.CertStatus{State: p.cert}, nil
}

type roEnv struct {
	*rEnv
	repo  *store.RolloutRepo
	svc   *rollout.Service
	gate  *rollout.Gate
	conns *prodConns
}

var (
	roMaker   = rollout.Actor{PrincipalID: "release-manager", CorrelationID: "corr-ro"}
	roChecker = rollout.Actor{PrincipalID: "release-approver"}
)

func newROEnv(t *testing.T) *roEnv {
	t.Helper()
	r := newREnv(t)
	e := &roEnv{rEnv: r, repo: store.NewRolloutRepo(r.st), conns: &prodConns{svc: r.gwEnv.svc, cert: xicconn.CertValid}}
	e.svc = &rollout.Service{Repo: e.repo, Conns: e.conns, Clock: func() time.Time { return r.now() }}
	e.gate = &rollout.Gate{Repo: e.repo}
	return e
}

func (e *roEnv) ctx() context.Context { return tenantCtx(cTenantA) }

func (e *roEnv) planned(t *testing.T) *rollout.Rollout {
	t.Helper()
	ro, err := e.svc.Create(e.ctx(), roMaker, e.c.ConnectionID, rollout.Terms{PilotOperations: []string{"payment-submit"}, PilotDailyCap: 5, MinSoakHours: 1, MinSuccess: 2, MaxFailurePct: 20})
	if err != nil {
		t.Fatal(err)
	}
	return ro
}

func (e *roEnv) step(t *testing.T, id string, att []string) {
	t.Helper()
	q, err := e.svc.RequestAdvance(e.ctx(), roMaker, id, att, "go")
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	if _, err := e.svc.Decide(e.ctx(), roChecker, q.RequestID, true, "ok"); err != nil {
		t.Fatalf("decide: %v", err)
	}
}

func TestPGRolloutLifecycleAndGate(t *testing.T) {
	e := newROEnv(t)
	ro := e.planned(t)
	if _, err := e.svc.Create(e.ctx(), roMaker, e.c.ConnectionID, rollout.Terms{PilotDailyCap: 1}); !errors.Is(err, rollout.ErrExists) {
		t.Fatalf("one live rollout per connection: %v", err)
	}
	if err := e.gate.Admit(e.ctx(), e.c, "payment-submit", e.now()); err == nil {
		t.Fatal("a planned rollout carries nothing")
	}
	e.step(t, ro.RolloutID, rollout.Attestations)
	got, err := e.repo.GetRollout(e.ctx(), ro.RolloutID)
	if err != nil || got.Stage != rollout.StagePilot || got.AdvanceRequest == "" {
		t.Fatalf("%+v %v", got, err)
	}
	// the pilot allowlist and the atomic daily budget
	if err := e.gate.Admit(e.ctx(), e.c, "other-op", e.now()); err == nil {
		t.Fatal("an operation outside the pilot is refused")
	}
	var wg sync.WaitGroup
	var mu sync.Mutex
	ok, refused := 0, 0
	for i := 0; i < 40; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			err := e.gate.Admit(e.ctx(), e.c, "payment-submit", e.now())
			mu.Lock()
			defer mu.Unlock()
			var ref *xicconn.Refusal
			switch {
			case err == nil:
				ok++
			case errors.As(err, &ref) && ref.Code == xicconn.CodeRateLimited:
				refused++
			default:
				t.Errorf("unexpected: %v", err)
			}
		}()
	}
	wg.Wait()
	if ok != 5 || refused != 35 {
		t.Fatalf("40 parallel admissions against a budget of 5: admitted %d, refused %d", ok, refused)
	}
	if n, _ := e.repo.UsedToday(e.ctx(), ro.RolloutID, e.now()); n != 5 {
		t.Fatalf("used %d", n)
	}
	if err := e.gate.Admit(xicconn.WithoutBudget(e.ctx()), e.c, "payment-submit", e.now()); err != nil {
		t.Fatalf("a re-check of admitted work takes nothing: %v", err)
	}
	// the evidence for GENERAL comes from the real attempt table: nothing yet
	if _, err := e.svc.RequestAdvance(e.ctx(), roMaker, ro.RolloutID, []string{rollout.AttNoP0P1}, ""); !errors.Is(err, rollout.ErrNotReady) {
		t.Fatalf("a pilot that has not soaked or carried anything is not general: %v", err)
	}
	if _, err := e.svc.Rollback(e.ctx(), roMaker, ro.RolloutID, "stop", 0); err != nil {
		t.Fatal(err)
	}
	if err := e.gate.Admit(e.ctx(), e.c, "payment-submit", e.now()); err == nil {
		t.Fatal("rolled back carries nothing")
	}
	latest, err := e.repo.LatestRolloutFor(e.ctx(), e.c.ConnectionID)
	if err != nil || latest.Stage != rollout.StageRolledBack {
		t.Fatalf("%+v %v", latest, err)
	}
	// a new rollout can be planned after a rollback
	if _, err := e.svc.Create(e.ctx(), roMaker, e.c.ConnectionID, rollout.Terms{PilotDailyCap: 2}); err != nil {
		t.Fatal(err)
	}
	evs, err := e.repo.Events(e.ctx(), rollout.KindRollout, ro.RolloutID)
	if err != nil || len(evs) < 4 {
		t.Fatalf("evidence of every step: %d %v", len(evs), err)
	}
}

func TestPGDatabaseRefusesAStepForwardWithoutAnApprovedRequest(t *testing.T) {
	e := newROEnv(t)
	ro := e.planned(t)
	id := ro.RolloutID
	wantDBError(t, "forward without a request", e.execAs(cTenantA, `UPDATE xic_rollouts SET stage='PILOT', version=version+1 WHERE rollout_id=$1::uuid`, id), "approved request")
	wantDBError(t, "skip a stage", e.execAs(cTenantA, `UPDATE xic_rollouts SET stage='GENERAL', version=version+1 WHERE rollout_id=$1::uuid`, id), "cannot go")

	// a request the requester decided himself cannot exist
	q, err := e.svc.RequestAdvance(e.ctx(), roMaker, id, rollout.Attestations, "")
	if err != nil {
		t.Fatal(err)
	}
	wantDBError(t, "self approval", e.execAs(cTenantA, `UPDATE xic_rollout_requests SET status='APPROVED', decided_by=requested_by WHERE request_id=$1::uuid`, q.RequestID), "xic_rq_decider")
	// an approved request that is not USED does not move the rollout
	if err := e.execAs(cTenantA, `UPDATE xic_rollout_requests SET status='APPROVED', decided_by='someone-else', decided_at=now() WHERE request_id=$1::uuid`, q.RequestID); err != nil {
		t.Fatal(err)
	}
	wantDBError(t, "approved but unused", e.execAs(cTenantA, `UPDATE xic_rollouts SET stage='PILOT', advance_request=$2::uuid, version=version+1 WHERE rollout_id=$1::uuid`, id, q.RequestID), "approved request")
	// a request fixed once made
	wantDBError(t, "retarget a request", e.execAs(cTenantA, `UPDATE xic_rollout_requests SET to_state='GENERAL' WHERE request_id=$1::uuid`, q.RequestID), "fixed once made")
	wantDBError(t, "delete a request", e.execAs(cTenantA, `DELETE FROM xic_rollout_requests WHERE request_id=$1::uuid`, q.RequestID), "")
	// a rollout is never deleted, and rollback is always allowed
	wantDBError(t, "delete a rollout", e.execAs(cTenantA, `DELETE FROM xic_rollouts WHERE rollout_id=$1::uuid`, id), "")
	if _, err := e.svc.Rollback(e.ctx(), roMaker, id, "x", 0); err != nil {
		t.Fatal(err)
	}
	wantDBError(t, "revive a rolled-back rollout", e.execAs(cTenantA, `UPDATE xic_rollouts SET stage='PLANNED', version=version+1 WHERE rollout_id=$1::uuid`, id), "final")
	// the pilot terms are fixed once the pilot starts
	ro2, err := e.svc.Create(e.ctx(), roMaker, e.c.ConnectionID, rollout.Terms{PilotDailyCap: 3})
	if err != nil {
		t.Fatal(err)
	}
	e.step(t, ro2.RolloutID, rollout.Attestations)
	wantDBError(t, "raise the budget mid-pilot", e.execAs(cTenantA, `UPDATE xic_rollouts SET pilot_daily_cap=9999, version=version+1 WHERE rollout_id=$1::uuid`, ro2.RolloutID), "fixed once the pilot")
	// the evidence log is append-only
	wantDBError(t, "edit evidence", e.execAs(cTenantA, `UPDATE xic_rollout_events SET kind='X'`), "")
	wantDBError(t, "delete evidence", e.execAs(cTenantA, `DELETE FROM xic_rollout_events`), "")
}

func TestPGRolloutIsTenantIsolated(t *testing.T) {
	e := newROEnv(t)
	ro := e.planned(t)
	l, err := e.svc.RegisterLegacy(e.ctx(), roMaker, rollout.LegacyInput{Name: "bank-x-pay", OwningService: "bank-svc", Direction: "OUTBOUND"})
	if err != nil {
		t.Fatal(err)
	}
	bctx := tenantCtx(cTenantB)
	if _, err := e.repo.GetRollout(bctx, ro.RolloutID); !errors.Is(err, rollout.ErrNotFound) {
		t.Fatalf("another tenant reads the rollout: %v", err)
	}
	if _, err := e.repo.GetLegacy(bctx, l.LegacyID); !errors.Is(err, rollout.ErrNotFound) {
		t.Fatalf("another tenant reads the legacy connector: %v", err)
	}
	if _, err := e.svc.Rollback(bctx, roMaker, ro.RolloutID, "x", 0); !errors.Is(err, rollout.ErrNotFound) {
		t.Fatalf("another tenant rolls it back: %v", err)
	}
	if _, _, err := e.svc.RegisterExchanges(bctx, roMaker, l.LegacyID, []rollout.LegacyExchange{{ExternalRef: "x", Outcome: "SETTLED", PayloadHash: strings.Repeat("a", 64), OccurredAt: e.now()}}); !errors.Is(err, rollout.ErrNotFound) {
		t.Fatalf("another tenant registers: %v", err)
	}
	for _, tbl := range []string{"xic_rollouts", "xic_rollout_requests", "xic_rollout_events", "xic_rollout_usage", "xic_legacy_connectors", "xic_legacy_exchanges", "xic_legacy_parity_runs"} {
		if n := e.countAs(cTenantB, `SELECT count(*) FROM `+tbl); n != 0 {
			t.Fatalf("%s leaks %d rows to another tenant", tbl, n)
		}
	}
	// another tenant's connection cannot be mapped
	if _, err := e.svc.MapLegacy(bctx, roMaker, l.LegacyID, e.c.ConnectionID, 0); !errors.Is(err, rollout.ErrNotFound) {
		t.Fatalf("%v", err)
	}
	wantDBError(t, "forged rollout", e.execAs(cTenantB, `INSERT INTO xic_rollouts (rollout_id, tenant_id, connection_id, connector_id, stage, pilot_daily_cap, min_soak_hours, min_success, max_failure_pct, created_by)
		VALUES (gen_random_uuid(), $1, $2::uuid, 'bank-x', 'GENERAL', 1, 0, 0, 0, 'x')`, cTenantA, e.c.ConnectionID), "")
}

func (e *roEnv) hash(i int) string { return fmt.Sprintf("%064x", i+1) }

func (e *roEnv) legacyShadowing(t *testing.T) *rollout.Legacy {
	t.Helper()
	l, err := e.svc.RegisterLegacy(e.ctx(), roMaker, rollout.LegacyInput{Name: "bank-x-payments", OwningService: "bank-integration-svc", Direction: "OUTBOUND"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.svc.MapLegacy(e.ctx(), roMaker, l.LegacyID, e.c.ConnectionID, 0); err != nil {
		t.Fatal(err)
	}
	if _, err := e.svc.StartShadow(e.ctx(), roMaker, l.LegacyID, 0); err != nil {
		t.Fatal(err)
	}
	got, _ := e.repo.GetLegacy(e.ctx(), l.LegacyID)
	return got
}

func TestPGLegacyRegistrationIsAppendOnlyAtomicAndIdempotent(t *testing.T) {
	e := newROEnv(t)
	l := e.legacyShadowing(t)
	at := e.now().Add(-time.Hour)
	mk := func(i int, outcome string) rollout.LegacyExchange {
		return rollout.LegacyExchange{ExternalRef: fmt.Sprintf("ext-%d", i), DomainRequestRef: "dr-a" + fmt.Sprint(i), Outcome: outcome, PayloadHash: e.hash(i), OccurredAt: at}
	}
	st, dup, err := e.svc.RegisterExchanges(e.ctx(), roMaker, l.LegacyID, []rollout.LegacyExchange{mk(1, "PENDING"), mk(2, "SETTLED")})
	if err != nil || st != 2 || dup != 0 {
		t.Fatalf("%d %d %v", st, dup, err)
	}
	if st, dup, _ = e.svc.RegisterExchanges(e.ctx(), roMaker, l.LegacyID, []rollout.LegacyExchange{mk(1, "PENDING"), mk(2, "SETTLED")}); st != 0 || dup != 2 {
		t.Fatalf("the same registrations again are no-ops: %d %d", st, dup)
	}
	if st, _, err = e.svc.RegisterExchanges(e.ctx(), roMaker, l.LegacyID, []rollout.LegacyExchange{mk(1, "SETTLED")}); err != nil || st != 1 {
		t.Fatalf("an outcome moving on is a new row: %d %v", st, err)
	}
	// different content under a stored reference refuses the whole call
	forged := mk(1, "FAILED")
	forged.PayloadHash = e.hash(99)
	if _, _, err = e.svc.RegisterExchanges(e.ctx(), roMaker, l.LegacyID, []rollout.LegacyExchange{mk(3, "SETTLED"), forged}); !errors.Is(err, rollout.ErrConflict) {
		t.Fatalf("%v", err)
	}
	if n := e.countAs(cTenantA, `SELECT count(*) FROM xic_legacy_exchanges WHERE external_ref='ext-3'`); n != 0 {
		t.Fatalf("a refused call stored %d rows", n)
	}
	// the same reference twice in one call with different content is the same refusal
	if _, _, err = e.svc.RegisterExchanges(e.ctx(), roMaker, l.LegacyID, []rollout.LegacyExchange{mk(4, "PENDING"), func() rollout.LegacyExchange { x := mk(4, "SETTLED"); x.PayloadHash = e.hash(98); return x }()}); !errors.Is(err, rollout.ErrConflict) {
		t.Fatalf("%v", err)
	}
	// the latest registration per reference is what parity sees
	got, err := e.repo.LatestExchanges(e.ctx(), l.LegacyID, e.now().Add(-2*time.Hour), e.now(), 10)
	if err != nil || len(got) != 2 {
		t.Fatalf("%+v %v", got, err)
	}
	for _, x := range got {
		if x.ExternalRef == "ext-1" && x.Outcome != "SETTLED" {
			t.Fatalf("ext-1 is %s", x.Outcome)
		}
	}
	wantDBError(t, "edit a registration", e.execAs(cTenantA, `UPDATE xic_legacy_exchanges SET outcome='FAILED'`), "")
	wantDBError(t, "delete a registration", e.execAs(cTenantA, `DELETE FROM xic_legacy_exchanges`), "")
	// concurrent registrations of the same reference with different content: exactly one content wins
	var wg sync.WaitGroup
	var mu sync.Mutex
	okN, conflictN := 0, 0
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			x := mk(7, "SETTLED")
			x.PayloadHash = e.hash(1000 + i)
			_, _, err := e.svc.RegisterExchanges(e.ctx(), roMaker, l.LegacyID, []rollout.LegacyExchange{x})
			mu.Lock()
			defer mu.Unlock()
			switch {
			case err == nil:
				okN++
			case errors.Is(err, rollout.ErrConflict):
				conflictN++
			default:
				t.Errorf("%v", err)
			}
		}(i)
	}
	wg.Wait()
	if okN != 1 || conflictN != 9 {
		t.Fatalf("one writer wins: ok %d conflict %d", okN, conflictN)
	}
	if n := e.countAs(cTenantA, `SELECT count(DISTINCT payload_hash) FROM xic_legacy_exchanges WHERE external_ref='ext-7'`); n != 1 {
		t.Fatalf("%d contents stored for one reference", n)
	}
}

func TestPGLegacyParityAgainstRealAttemptsAndGuardedCutover(t *testing.T) {
	e := newROEnv(t)
	l := e.legacyShadowing(t)
	at := e.now().Add(-time.Hour)
	var in []rollout.LegacyExchange
	for i := 0; i < 24; i++ {
		id := fmt.Sprintf("00000000-0000-4000-8000-%012d", i+1)
		e.attempt(t, id, "SETTLED", 0, "", false)
		in = append(in, rollout.LegacyExchange{ExternalRef: "ext-" + id, DomainRequestRef: "dr-" + id, Outcome: "SETTLED", PayloadHash: e.hash(i), OccurredAt: at})
	}
	const ghostRef = "dr-00000000-0000-4000-8000-000000000998" // one the platform has not seen yet, and one with a different outcome
	in = append(in, rollout.LegacyExchange{ExternalRef: "ext-ghost", DomainRequestRef: ghostRef, Outcome: "SETTLED", PayloadHash: e.hash(500), OccurredAt: at})
	e.attempt(t, "00000000-0000-4000-8000-000000000999", "FAILED", 0, "", false)
	in = append(in, rollout.LegacyExchange{ExternalRef: "ext-diff", DomainRequestRef: "dr-00000000-0000-4000-8000-000000000999", Outcome: "SETTLED", PayloadHash: e.hash(501), OccurredAt: at})
	if _, _, err := e.svc.RegisterExchanges(e.ctx(), roMaker, l.LegacyID, in); err != nil {
		t.Fatal(err)
	}
	win := rollout.ParityInput{From: e.now().Add(-3 * time.Hour), To: e.now()}
	p, err := e.svc.RunParity(e.ctx(), roMaker, l.LegacyID, win)
	if err != nil {
		t.Fatal(err)
	}
	if p.Passed || p.Compared != 26 || p.Matched != 24 || p.MissingInXIC != 1 || p.OutcomeDiffers != 1 {
		t.Fatalf("%+v", p)
	}
	if !strings.Contains(string(p.Samples), ghostRef) || !strings.Contains(string(p.Samples), "OUTCOME_DIFFERS") {
		t.Fatalf("%s", p.Samples)
	}
	// parity reads; it never changes an attempt
	if n := e.countAs(cTenantA, `SELECT count(*) FROM xic_outbound_attempts WHERE state='SETTLED'`); n != 24 {
		t.Fatalf("%d settled attempts", n)
	}
	wantDBError(t, "edit a parity run", e.execAs(cTenantA, `UPDATE xic_legacy_parity_runs SET passed=true`), "")
	runs, err := e.repo.ListParity(e.ctx(), l.LegacyID, 10)
	if err != nil || len(runs) != 1 || runs[0].Passed {
		t.Fatalf("%+v %v", runs, err)
	}

	// the cutover needs a passed run, a soaked shadow and a second person; the database enforces the last itself
	e.advance(25 * time.Hour)
	if _, err := e.svc.RequestLegacyAdvance(e.ctx(), roMaker, l.LegacyID, ""); !errors.Is(err, rollout.ErrNotReady) {
		t.Fatalf("a failed parity run does not cut over: %v", err)
	}
	wantDBError(t, "cutover without a request", e.execAs(cTenantA, `UPDATE xic_legacy_connectors SET state='CUTOVER', version=version+1 WHERE legacy_id=$1::uuid`, l.LegacyID), "approved request")
	wantDBError(t, "skip a state", e.execAs(cTenantA, `UPDATE xic_legacy_connectors SET state='RETIRED', version=version+1 WHERE legacy_id=$1::uuid`, l.LegacyID), "cannot go")
	// repair the two differences on the platform side's registrations: register the settled truth for the diff and the ghost's attempt
	e.attempt(t, "00000000-0000-4000-8000-000000000998", "SETTLED", 0, "", false)
	fix := []rollout.LegacyExchange{
		{ExternalRef: "ext-diff", DomainRequestRef: "dr-00000000-0000-4000-8000-000000000999", Outcome: "FAILED", PayloadHash: e.hash(501), OccurredAt: at},
	}
	if _, _, err := e.svc.RegisterExchanges(e.ctx(), roMaker, l.LegacyID, fix); err != nil {
		t.Fatal(err)
	}
	p, err = e.svc.RunParity(e.ctx(), roMaker, l.LegacyID, rollout.ParityInput{From: e.now().Add(-30 * time.Hour), To: e.now()})
	if err != nil || !p.Passed {
		t.Fatalf("%+v %v", p, err)
	}
	q, err := e.svc.RequestLegacyAdvance(e.ctx(), roMaker, l.LegacyID, "cut over")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.svc.Decide(e.ctx(), roMaker, q.RequestID, true, ""); !errors.Is(err, rollout.ErrSelfAction) {
		t.Fatalf("%v", err)
	}
	if _, err := e.svc.Decide(e.ctx(), roChecker, q.RequestID, true, ""); err != nil {
		t.Fatal(err)
	}
	got, _ := e.repo.GetLegacy(e.ctx(), l.LegacyID)
	if got.State != rollout.LegacyCutover || got.AdvanceRequest != q.RequestID {
		t.Fatalf("%+v", got)
	}
	if _, _, err := e.svc.RegisterExchanges(e.ctx(), roMaker, l.LegacyID, in[:1]); !errors.Is(err, rollout.ErrState) {
		t.Fatalf("registrations stop at cutover: %v", err)
	}
	wantDBError(t, "retarget after cutover", e.execAs(cTenantA, `UPDATE xic_legacy_connectors SET connection_id=NULL, version=version+1 WHERE legacy_id=$1::uuid`, l.LegacyID), "")
	if _, err := e.svc.RevertCutover(e.ctx(), roMaker, l.LegacyID, "provider rejects the mapping", 0); err != nil {
		t.Fatal(err)
	}
	if got, _ := e.repo.GetLegacy(e.ctx(), l.LegacyID); got.State != rollout.LegacyShadow {
		t.Fatal(got.State)
	}
}

func TestPGFactsReadTheRealTables(t *testing.T) {
	e := newROEnv(t)
	for i, st := range []string{"SETTLED", "SETTLED", "SETTLED", "FAILED", "UNKNOWN"} {
		e.attempt(t, fmt.Sprintf("00000000-0000-4000-8000-%012d", i+1), st, 0, "", false)
	}
	o, err := e.repo.Outcomes(e.ctx(), e.c.ConnectionID, e.now().Add(-24*time.Hour), e.now().Add(time.Hour))
	if err != nil || o.Success != 3 || o.Failure != 1 || o.Unknown != 1 {
		t.Fatalf("%+v %v", o, err)
	}
	if n, err := e.repo.OverdueUnknown(e.ctx(), e.c.ConnectionID, e.now().Add(time.Hour)); err != nil || n != 1 {
		t.Fatalf("%d %v", n, err)
	}
	if n, err := e.repo.OverdueUnknown(e.ctx(), e.c.ConnectionID, e.now().Add(-48*time.Hour)); err != nil || n != 0 {
		t.Fatalf("an UNKNOWN attempt inside its limit is not overdue: %d %v", n, err)
	}
	if n, err := e.repo.OpenHighVariances(e.ctx(), e.c.ConnectionID); err != nil || n != 0 {
		t.Fatalf("%d %v", n, err)
	}
	// another tenant sees none of it
	if o, _ := e.repo.Outcomes(tenantCtx(cTenantB), e.c.ConnectionID, e.now().Add(-24*time.Hour), e.now().Add(time.Hour)); o.Success+o.Failure+o.Unknown != 0 {
		t.Fatalf("another tenant reads the outcomes: %+v", o)
	}
	only, err := e.repo.XICOnly(e.ctx(), rollout.DirOutbound, e.c.ConnectionID, e.now().Add(-24*time.Hour), e.now().Add(time.Hour), nil)
	if err != nil || only != 5 {
		t.Fatalf("with no registered references every platform attempt is unaccounted for: %d %v", only, err)
	}
}

func TestPGPilotEvaluatorRollsBackAcrossTenants(t *testing.T) {
	e := newROEnv(t)
	ro := e.planned(t)
	e.step(t, ro.RolloutID, rollout.Attestations)
	for i := 0; i < 12; i++ {
		st := "SETTLED"
		if i < 6 {
			st = "FAILED"
		}
		e.attempt(t, fmt.Sprintf("00000000-0000-4000-8000-%012d", i+1), st, 0, "", false)
	}
	// the stage began "now" in the service clock; the planted attempts sit just before it, so move the window back
	if err := e.adminExec(`UPDATE xic_rollouts SET stage_since = now() - interval '3 days', version = version + 1`); err != nil {
		t.Fatal(err)
	}
	n, err := e.svc.EvaluatePilots(context.Background(), 10)
	if err != nil || n != 1 {
		t.Fatalf("%d %v", n, err)
	}
	got, _ := e.repo.GetRollout(e.ctx(), ro.RolloutID)
	if got.Stage != rollout.StageRolledBack || !strings.HasPrefix(got.RolledBackBy, "system:") {
		t.Fatalf("%+v", got)
	}
	if err := e.gate.Admit(e.ctx(), e.c, "payment-submit", e.now()); err == nil {
		t.Fatal("the pilot rolled itself back")
	}
}

func TestPGRolloutEventsAreOnTheOutboxWithoutSecrets(t *testing.T) {
	e := newROEnv(t)
	ro := e.planned(t)
	e.step(t, ro.RolloutID, rollout.Attestations)
	l := e.legacyShadowing(t)
	if _, _, err := e.svc.RegisterExchanges(e.ctx(), roMaker, l.LegacyID, []rollout.LegacyExchange{{ExternalRef: "secret-looking-ref-sk_live_x", Outcome: "SETTLED", PayloadHash: e.hash(1), OccurredAt: e.now().Add(-time.Hour)}}); err != nil {
		t.Fatal(err)
	}
	for typ, key := range map[string]string{"integration.rollout.planned": "connection_id", "integration.rollout.advance-requested": "request_id", "integration.rollout.advanced": "request_id", "integration.legacy.discovered": "owning_service", "integration.legacy.mapped": "connection_id", "integration.legacy.shadow-started": "connection_id"} {
		if n := e.countAs(cTenantA, `SELECT count(*) FROM eventing_outbox WHERE event_type LIKE '%.' || $1 AND convert_from(payload, 'UTF8') LIKE '%"`+key+`"%'`, typ); n == 0 {
			t.Fatalf("%s is not on the outbox with its content (positive control)", typ)
		}
	}
	// the registered references and content hashes never travel in an event: registrations are history, not events
	if n := e.countAs(cTenantA, `SELECT count(*) FROM eventing_outbox WHERE event_type LIKE '%.integration.rollout.%' OR event_type LIKE '%.integration.legacy.%'`); n == 0 {
		t.Fatal("no events")
	}
	if n := e.countAs(cTenantA, `SELECT count(*) FROM eventing_outbox WHERE event_type LIKE '%.integration.legacy.%' AND convert_from(payload, 'UTF8') LIKE '%sk_live%'`); n != 0 {
		t.Fatalf("%d events carry a registered reference", n)
	}
}

func (e *roEnv) adminExec(sql string) error {
	_, err := e.admin.Exec(context.Background(), sql)
	return err
}
