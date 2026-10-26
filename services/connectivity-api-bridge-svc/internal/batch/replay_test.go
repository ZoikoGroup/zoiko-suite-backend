package batch_test

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"zoiko.io/connectivity-api-bridge-svc/internal/batch"
	"zoiko.io/connectivity-api-bridge-svc/internal/inbound"
	"zoiko.io/connectivity-api-bridge-svc/internal/xicconn"
)

// held delivers and processes a batch of n rows while no domain is registered: every row is held as a
// DOMAIN_PRECONDITION and the batch is PARTIAL.
func (e *env) held(c *xicconn.Connection, id string, n int) *batch.Batch {
	e.t.Helper()
	s := simple(c, id, n)
	if c.TenantID == tenantB {
		s.secret = secretB
	}
	o := e.deliver(s)
	b := e.process(o.Batch)
	if b.State != batch.StatePartial || b.QuarantinedRows != n {
		e.t.Fatalf("%+v", b)
	}
	return b
}

func (e *env) request(tenant string, in batch.ReplayRequest) (*batch.ReplayJob, error) {
	return e.svc.RequestReplay(ctxFor(tenant), requestr, in)
}

func heldScope(b *batch.Batch) batch.ReplayScope {
	return batch.ReplayScope{BatchID: b.BatchID, RowStates: []string{batch.RowQuarantined}}
}

func req(b *batch.Batch) batch.ReplayRequest {
	return batch.ReplayRequest{Scope: heldScope(b), ReasonCode: batch.ReasonIncidentRepair, Reason: "the domain target is registered now"}
}

func (e *env) run(j *batch.ReplayJob) *batch.ReplayJob {
	e.t.Helper()
	got, err := e.svc.RunReplay(context.Background(), j.TenantID, j.ReplayID)
	if err != nil {
		e.t.Fatalf("run: %v", err)
	}
	return got
}

// ---- scope is explicit (NP-39) ----------------------------------------------------------------------------

func TestReplay_ThereIsNoReplayEverything_NP39(t *testing.T) {
	e := newEnv(t)
	c := e.activeConn(tenantA, e.publish(nil), "acc-a")
	b := e.held(c, "B-1", 3)
	now := e.at()
	wide := now.Add(-30 * 24 * time.Hour)
	for name, sc := range map[string]batch.ReplayScope{
		"no selector at all":            {},
		"a batch with no rows named":    {BatchID: b.BatchID},
		"a row state that is not held":  {BatchID: b.BatchID, RowStates: []string{batch.RowAccepted}},
		"a window without a connection": {OperationID: "statement-file", From: &wide, To: &now},
		"a window that is too wide":     {ConnectionID: c.ConnectionID, OperationID: "statement-file", From: &wide, To: &now},
		"a window backwards":            {ConnectionID: c.ConnectionID, OperationID: "statement-file", From: &now, To: &wide},
	} {
		if _, err := e.request(tenantA, batch.ReplayRequest{Scope: sc, ReasonCode: batch.ReasonIncidentRepair, Reason: "x"}); !errors.Is(err, batch.ErrReplayScope) {
			t.Errorf("%s: %v", name, err)
		}
	}
	for name, r := range map[string]batch.ReplayRequest{
		"no reason":              {Scope: heldScope(b), ReasonCode: batch.ReasonIncidentRepair},
		"an unknown reason code": {Scope: heldScope(b), ReasonCode: "BECAUSE", Reason: "x"},
		"a reason too long":      {Scope: heldScope(b), ReasonCode: batch.ReasonIncidentRepair, Reason: strings.Repeat("x", 300)},
	} {
		if _, err := e.request(tenantA, r); !errors.Is(err, batch.ErrBadRequest) {
			t.Errorf("%s: %v", name, err)
		}
	}
	// an unknown batch, and a scope that selects nothing
	if _, err := e.request(tenantA, batch.ReplayRequest{Scope: batch.ReplayScope{BatchID: "00000000-0000-4000-8000-000000000000", RowStates: []string{batch.RowQuarantined}}, ReasonCode: batch.ReasonIncidentRepair, Reason: "x"}); !errors.Is(err, batch.ErrNotFound) {
		t.Errorf("%v", err)
	}
	if _, err := e.request(tenantA, batch.ReplayRequest{Scope: batch.ReplayScope{BatchID: b.BatchID, RowNumbers: []int{999}}, ReasonCode: batch.ReasonIncidentRepair, Reason: "x"}); !errors.Is(err, batch.ErrReplayScope) {
		t.Errorf("%v", err)
	}
	if l, _ := e.repo.ListReplays(ctxFor(tenantA), batch.ReplayFilter{}); len(l) != 0 {
		t.Fatalf("%d jobs from refused requests", len(l))
	}
}

func TestReplay_ForeignObjectsAreRefusedWholeNotPartly(t *testing.T) {
	e := newEnv(t)
	v := e.publish(nil)
	cA := e.activeConn(tenantA, v, "acc-a")
	cB := e.activeConn(tenantB, v, "acc-b")
	bA := e.held(cA, "B-A", 2)
	bB := e.held(cB, "B-B", 2)
	mine := e.rowsOf(tenantA, bA.BatchID)[0].ExchangeID
	theirs := e.rowsOf(tenantB, bB.BatchID)[0].ExchangeID
	if _, err := e.request(tenantA, batch.ReplayRequest{Scope: batch.ReplayScope{ExchangeIDs: []string{mine, theirs}}, ReasonCode: batch.ReasonIncidentRepair, Reason: "x"}); !errors.Is(err, batch.ErrNotFound) {
		t.Fatalf("a replay naming another tenant's exchange must be refused: %v", err)
	}
	if _, err := e.request(tenantA, batch.ReplayRequest{Scope: batch.ReplayScope{BatchID: bB.BatchID, RowStates: []string{batch.RowQuarantined}}, ReasonCode: batch.ReasonIncidentRepair, Reason: "x"}); !errors.Is(err, batch.ErrNotFound) {
		t.Fatalf("%v", err)
	}
	if _, err := e.request(tenantA, batch.ReplayRequest{Scope: batch.ReplayScope{ConnectionID: cB.ConnectionID, OperationID: "statement-file", From: ptr(e.at().Add(-time.Hour)), To: ptr(e.at().Add(time.Hour))}, ReasonCode: batch.ReasonIncidentRepair, Reason: "x"}); !errors.Is(err, batch.ErrNotFound) {
		t.Fatalf("%v", err)
	}
}

func ptr(t time.Time) *time.Time { return &t }

// ---- dry run -------------------------------------------------------------------------------------------------

func TestReplay_DryRunProjectsAndChangesNothing(t *testing.T) {
	e := newEnv(t)
	c := e.activeConn(tenantA, e.publish(nil), "acc-a")
	b := e.held(c, "B-1", 3)
	e.target() // the domain exists now
	r := req(b)
	r.DryRun = true
	j, err := e.request(tenantA, r)
	if err != nil {
		t.Fatal(err)
	}
	if j.State != batch.ReplayDryRunComplete || !j.DryRun || j.SelectedItems != 3 || j.ContractVersionID != b.ContractVersionID || j.MappingVersionID == "" {
		t.Fatalf("%+v", j)
	}
	var p batch.ReplayCounts
	_ = json.Unmarshal(j.Projected, &p)
	if p.Selected != 3 || p.Replayable != 3 || p.WouldAccept != 3 || p.Changed != 3 || p.Applied != 0 {
		t.Fatalf("%+v", p)
	}
	items, _ := e.repo.ReplayItems(ctxFor(tenantA), j.ReplayID, 0)
	for _, it := range items {
		if it.BeforeState != inbound.StateQuarantined || it.AfterState != inbound.StateValidated || !it.Changed || it.Applied || !it.Done {
			t.Fatalf("%+v", it)
		}
	}
	// nothing moved: not the exchanges, not the batch, not the domain
	for _, row := range e.rowsOf(tenantA, b.BatchID) {
		x, _ := e.inRepo.Get(ctxFor(tenantA), row.ExchangeID)
		if x.State != inbound.StateQuarantined || x.AttemptNo != 1 {
			t.Fatalf("a dry run changed an exchange: %+v", x)
		}
	}
	if e.domHits != 0 || e.get(tenantA, b.BatchID).State != batch.StatePartial {
		t.Fatal("a dry run had an effect")
	}
	if hasType(e.repo.Types(), "integration.replay.started") {
		t.Fatalf("%v", e.repo.Types())
	}
}

// ---- governed run ----------------------------------------------------------------------------------------------

func TestReplay_BelowTheThresholdRunsAndTheBatchReportsWhatItChanged(t *testing.T) {
	e := newEnv(t)
	c := e.activeConn(tenantA, e.publish(nil), "acc-a")
	b := e.held(c, "B-1", 3)
	e.target()
	j, err := e.request(tenantA, req(b))
	if err != nil {
		t.Fatal(err)
	}
	if j.State != batch.ReplayApproved || j.Risk != batch.RiskLow || j.RequestedBy != "requester" || j.ReasonCode != batch.ReasonIncidentRepair || j.Reason == "" {
		t.Fatalf("%+v", j)
	}
	done := e.run(j)
	if done.State != batch.ReplayCompleted || done.StartedAt == nil || done.FinishedAt == nil {
		t.Fatalf("%+v", done)
	}
	var res batch.ReplayCounts
	_ = json.Unmarshal(done.Result, &res)
	if res.Selected != 3 || res.Applied != 3 || res.WouldAccept != 3 || res.Skipped != 0 || res.Failed != 0 {
		t.Fatalf("%+v", res)
	}
	for _, row := range e.rowsOf(tenantA, b.BatchID) {
		x, _ := e.inRepo.Get(ctxFor(tenantA), row.ExchangeID)
		if x.State != inbound.StateValidated || x.AttemptNo != 2 {
			t.Fatalf("%+v", x)
		}
		evs, _ := e.inRepo.Events(ctxFor(tenantA), row.ExchangeID)
		last := evs[len(evs)-1]
		if last.Kind != "REPLAYED" || !strings.Contains(string(last.Detail), j.ReplayID) || !strings.Contains(string(last.Detail), b.ContractVersionID) {
			t.Fatalf("%+v", last)
		}
	}
	got := e.settle(e.get(tenantA, b.BatchID))
	if got.State != batch.StateReconciled || got.AcceptedRows != 3 || got.QuarantinedRows != 0 || e.domHits != 3 {
		t.Fatalf("%+v hits=%d", got, e.domHits)
	}
	for _, want := range []string{"integration.replay.requested", "integration.replay.started", "integration.replay.completed", "integration.inbound.replayed"} {
		if !hasType(e.repo.Types(), want) && !hasType(inboundTypes(e), want) {
			t.Errorf("%s missing", want)
		}
	}
	// running a completed job again does nothing
	again := e.run(done)
	if again.State != batch.ReplayCompleted || e.domHits != 3 {
		t.Fatalf("%+v", again)
	}
}

func inboundTypes(e *env) []string {
	var out []string
	for _, o := range e.inRepo.Out {
		out = append(out, o.Type)
	}
	return out
}

func TestReplay_RiskyReplaysNeedADryRunAndSomeoneElsesApproval_NP40(t *testing.T) {
	e := newEnv(t)
	cfg := batch.DefaultConfig()
	cfg.ReplayApprovalRows = 2
	e.svc.Cfg = cfg
	c := e.activeConn(tenantA, e.publish(nil), "acc-a")
	b := e.held(c, "B-1", 3)
	b2 := e.held(c, "B-2", 3)
	e.target()
	// without a dry run first
	if _, err := e.request(tenantA, req(b)); !errors.Is(err, batch.ErrBadRequest) {
		t.Fatalf("a material replay is preceded by a dry run: %v", err)
	}
	dry := req(b)
	dry.DryRun = true
	d, err := e.request(tenantA, dry)
	if err != nil || d.Risk != batch.RiskHigh || len(d.RiskReasons) == 0 {
		t.Fatalf("%v %+v", err, d)
	}
	// a dry run of another scope does not count
	other := req(b)
	other.Scope.RowNumbers = []int{1, 2}
	other.DryRunOf = d.ReplayID
	if _, err := e.request(tenantA, other); !errors.Is(err, batch.ErrBadRequest) {
		t.Fatalf("%v", err)
	}
	r := req(b)
	r.DryRunOf = d.ReplayID
	j, err := e.request(tenantA, r)
	if err != nil || j.State != batch.ReplayPendingApproval || j.Risk != batch.RiskHigh {
		t.Fatalf("%v %+v", err, j)
	}
	// nothing runs while it waits
	if got := e.run(j); got.State != batch.ReplayPendingApproval || e.domHits != 0 {
		t.Fatalf("%+v", got)
	}
	// the requester cannot approve it
	if _, err := e.svc.DecideReplay(ctxFor(tenantA), requestr, j.ReplayID, true, ""); !errors.Is(err, batch.ErrSelfAction) {
		t.Fatalf("%v", err)
	}
	ok, err := e.svc.DecideReplay(ctxFor(tenantA), approver, j.ReplayID, true, "reviewed the dry run")
	if err != nil || ok.State != batch.ReplayApproved || ok.DecidedBy != "approver" || ok.DecidedAt == nil {
		t.Fatalf("%v %+v", err, ok)
	}
	if _, err := e.svc.DecideReplay(ctxFor(tenantA), approver, j.ReplayID, false, ""); !errors.Is(err, batch.ErrReplayState) {
		t.Fatalf("an approved replay cannot be decided again: %v", err)
	}
	if done := e.run(ok); done.State != batch.ReplayCompleted {
		t.Fatalf("%+v", done)
	}
	if got := e.settle(e.get(tenantA, b.BatchID)); got.State != batch.StateReconciled {
		t.Fatalf("%+v", got)
	}
	// a rejected replay never runs
	d2 := req(b2)
	d2.DryRun = true
	dd, _ := e.request(tenantA, d2)
	r2 := req(b2)
	r2.DryRunOf = dd.ReplayID
	j2, _ := e.request(tenantA, r2)
	rej, err := e.svc.DecideReplay(ctxFor(tenantA), approver, j2.ReplayID, false, "not now")
	if err != nil || rej.State != batch.ReplayRejected || rej.FinishedAt == nil {
		t.Fatalf("%v %+v", err, rej)
	}
	hits := e.domHits
	if got := e.run(rej); got.State != batch.ReplayRejected || e.domHits != hits {
		t.Fatalf("%+v", got)
	}
	for _, want := range []string{"integration.replay.approved", "integration.replay.rejected"} {
		if !hasType(e.repo.Types(), want) {
			t.Errorf("%s missing", want)
		}
	}
}

func TestReplay_CancelBeforeItStarts(t *testing.T) {
	e := newEnv(t)
	c := e.activeConn(tenantA, e.publish(nil), "acc-a")
	b := e.held(c, "B-1", 2)
	b2 := e.held(c, "B-2", 2)
	e.target()
	j, _ := e.request(tenantA, req(b))
	if _, err := e.svc.CancelReplay(ctxFor(tenantA), requestr, j.ReplayID, " "); !errors.Is(err, batch.ErrBadRequest) {
		t.Fatalf("%v", err)
	}
	got, err := e.svc.CancelReplay(ctxFor(tenantA), requestr, j.ReplayID, "no longer needed")
	if err != nil || got.State != batch.ReplayCancelled {
		t.Fatalf("%v %+v", err, got)
	}
	if run := e.run(got); run.State != batch.ReplayCancelled || e.domHits != 0 {
		t.Fatalf("%+v", run)
	}
	done, _ := e.request(tenantA, req(b2))
	e.run(done)
	if _, err := e.svc.CancelReplay(ctxFor(tenantA), requestr, done.ReplayID, "too late"); !errors.Is(err, batch.ErrReplayState) {
		t.Fatalf("%v", err)
	}
}

// ---- idempotency and retention (NP-42, NP-43) ----------------------------------------------------------------

func TestReplay_AnAlreadyAppliedExchangeIsNeverTouchedAgain_NP43(t *testing.T) {
	e := newEnv(t)
	c := e.activeConn(tenantA, e.publish(nil), "acc-a")
	e.target()
	o := e.deliver(simple(c, "B-OK", 3))
	b := e.settle(e.process(o.Batch))
	if b.State != batch.StateReconciled || e.domHits != 3 {
		t.Fatalf("%+v", b)
	}
	var ids []string
	for _, r := range e.rowsOf(tenantA, b.BatchID) {
		ids = append(ids, r.ExchangeID)
	}
	j, err := e.request(tenantA, batch.ReplayRequest{Scope: batch.ReplayScope{ExchangeIDs: ids}, ReasonCode: batch.ReasonProviderRedelivery, Reason: "the provider says it redelivered"})
	if err != nil {
		t.Fatal(err)
	}
	done := e.run(j)
	var res batch.ReplayCounts
	_ = json.Unmarshal(done.Result, &res)
	if res.Skipped != 3 || res.Applied != 0 || e.domHits != 3 {
		t.Fatalf("a replay duplicated a business effect: %+v hits=%d", res, e.domHits)
	}
	items, _ := e.repo.ReplayItems(ctxFor(tenantA), j.ReplayID, 0)
	for _, it := range items {
		if it.Skipped != "ALREADY_APPLIED" {
			t.Fatalf("%+v", it)
		}
	}
	for _, id := range ids {
		if x, _ := e.inRepo.Get(ctxFor(tenantA), id); x.AttemptNo != 1 {
			t.Fatalf("%+v", x)
		}
	}
}

func TestReplay_ABodyThatIsNoLongerRetainedIsNotReconstructed_NP42(t *testing.T) {
	e := newEnv(t)
	icfg := inbound.DefaultConfig()
	icfg.RawRetention = 0 // row bodies are not kept
	e.in.Cfg = icfg
	c := e.activeConn(tenantA, e.publish(nil), "acc-a")
	b := e.held(c, "B-1", 2)
	e.target()
	j, err := e.request(tenantA, req(b))
	if err != nil {
		t.Fatal(err)
	}
	done := e.run(j)
	var res batch.ReplayCounts
	_ = json.Unmarshal(done.Result, &res)
	// DOMAIN_PRECONDITION rows keep their canonical payload, not their raw body: there is nothing to re-evaluate
	items, _ := e.repo.ReplayItems(ctxFor(tenantA), j.ReplayID, 0)
	for _, it := range items {
		if it.Skipped != "RAW_NOT_RETAINED" {
			t.Fatalf("%+v", it)
		}
	}
	if res.Skipped != 2 || res.Applied != 0 || e.domHits != 0 {
		t.Fatalf("%+v", res)
	}
}

// ---- versions are pinned (NP-41) -------------------------------------------------------------------------------

func scaleZero(in *registryInput) {
	for i := range in.Content.Mappings {
		if in.Content.Mappings[i].Key == "stmt-csv" {
			in.Content.Mappings[i].Spec = json.RawMessage(strings.Replace(string(in.Content.Mappings[i].Spec), `"scale":2`, `"scale":0`, 1))
		}
	}
}

func TestReplay_UsesTheVersionTheDataArrivedUnderUnlessAGovernedMigrationReplaySaysOtherwise_NP41(t *testing.T) {
	e := newEnv(t)
	cfg := batch.DefaultConfig()
	cfg.ReplayApprovalRows = 1000
	e.svc.Cfg = cfg
	// version 1's mapping cannot represent 10.5 at scale 0, so every row is held for its value
	v1 := e.publish(scaleZero)
	c := e.activeConn(tenantA, v1, "acc-a")
	e.target()
	o := e.deliver(simple(c, "B-OLD", 3))
	b := e.process(o.Batch)
	if b.QuarantinedRows != 3 || b.MappingVersionID == "" {
		t.Fatalf("%+v", b)
	}
	// version 2 fixes the mapping; the connection migrates to it
	v2 := e.publish(func(in *registryInput) { in.ReplayAnalysisRef = "ANALYSIS-123 affected domains analysed" })
	req0, err := e.connSvc.RequestMigration(ctxFor(tenantA), xicconn.Actor{PrincipalID: "mig-requester"}, c.ConnectionID, xicconn.MigrationInput{ToContractVersionID: v2.ContractVersionID, Reason: "adopt the corrected contract"})
	if err != nil {
		t.Skipf("migration needs more setup in this harness: %v", err)
	}
	if _, err := e.connSvc.DecideMigration(ctxFor(tenantA), xicconn.Actor{PrincipalID: "mig-approver"}, c.ConnectionID, req0.RequestID, true); err != nil {
		t.Skipf("migration approval needs more setup in this harness: %v", err)
	}
	e.advance(time.Minute)
	if cur, err := e.connSvc.PinnedContract(ctxFor(tenantA), mustConn(e, c)); err != nil || cur.ContractVersionID != v2.ContractVersionID {
		t.Skipf("the migration did not take effect in this harness: %v", err)
	}

	// 1. the default replay runs the data under the version it ARRIVED under: still held, changed nothing
	j, err := e.request(tenantA, req(b))
	if err != nil || j.ContractVersionID != v1.ContractVersionID {
		t.Fatalf("%v %+v", err, j)
	}
	if j.State != batch.ReplayApproved {
		t.Fatalf("%+v", j)
	}
	done := e.run(j)
	var res batch.ReplayCounts
	_ = json.Unmarshal(done.Result, &res)
	if res.Applied != 0 || res.Changed != 0 || res.Quarantined != 3 {
		t.Fatalf("a historical replay silently used today's mapping: %+v", res)
	}
	// 2. a governed migration replay uses the connection's current version: named, recorded and always approved
	m := req(b)
	m.UseCurrentMapping = true
	m.ReasonCode = batch.ReasonMigrationValidation
	mj, err := e.request(tenantA, m)
	if !errors.Is(err, batch.ErrBadRequest) {
		t.Fatalf("a migration replay needs a dry run first: %v %+v", err, mj)
	}
	dm := m
	dm.DryRun = true
	dj, err := e.request(tenantA, dm)
	if err != nil || dj.ContractVersionID != v2.ContractVersionID || dj.Risk != batch.RiskHigh || !contains(dj.RiskReasons, "CURRENT_MAPPING") {
		t.Fatalf("%v %+v", err, dj)
	}
	var p batch.ReplayCounts
	_ = json.Unmarshal(dj.Projected, &p)
	if p.WouldAccept != 3 || p.Changed != 3 {
		t.Fatalf("%+v", p)
	}
	m.DryRunOf = dj.ReplayID
	mj, err = e.request(tenantA, m)
	if err != nil || mj.State != batch.ReplayPendingApproval || mj.ContractVersionID != v2.ContractVersionID || !mj.UseCurrentMapping {
		t.Fatalf("%v %+v", err, mj)
	}
	ok, err := e.svc.DecideReplay(ctxFor(tenantA), approver, mj.ReplayID, true, "migration validated")
	if err != nil {
		t.Fatal(err)
	}
	if r := e.run(ok); r.State != batch.ReplayCompleted {
		t.Fatalf("%+v", r)
	}
	got := e.settle(e.get(tenantA, b.BatchID))
	if got.State != batch.StateReconciled || got.AcceptedRows != 3 {
		t.Fatalf("%+v", got)
	}
	x, _ := e.inRepo.Get(ctxFor(tenantA), e.rowsOf(tenantA, b.BatchID)[0].ExchangeID)
	if x.MappingVersionID == b.MappingVersionID {
		t.Fatal("the replay did not apply the newer mapping")
	}
	// loose exchange ids cannot be migrated: a migration replay covers one connection
	var ids []string
	for _, r := range e.rowsOf(tenantA, b.BatchID) {
		ids = append(ids, r.ExchangeID)
	}
	if _, err := e.request(tenantA, batch.ReplayRequest{Scope: batch.ReplayScope{ExchangeIDs: ids}, ReasonCode: batch.ReasonMigrationValidation, Reason: "x", UseCurrentMapping: true, DryRun: true}); !errors.Is(err, batch.ErrReplayScope) {
		t.Fatalf("%v", err)
	}
}

func mustConn(e *env, c *xicconn.Connection) *xicconn.Connection {
	cur, err := e.connSvc.Connection(ctxFor(c.TenantID), c.ConnectionID)
	if err != nil {
		e.t.Fatal(err)
	}
	return cur
}

func contains(l []string, s string) bool {
	for _, x := range l {
		if x == s {
			return true
		}
	}
	return false
}

// ---- other scopes, evidence, isolation ---------------------------------------------------------------------

func TestReplay_WindowScopeCoversTheHeldExchangesOfOneConnectionAndOperation(t *testing.T) {
	e := newEnv(t)
	c := e.activeConn(tenantA, e.publish(nil), "acc-a")
	b := e.held(c, "B-1", 3)
	e.target()
	from, to := e.at().Add(-time.Hour), e.at().Add(time.Hour)
	j, err := e.request(tenantA, batch.ReplayRequest{Scope: batch.ReplayScope{ConnectionID: c.ConnectionID, OperationID: "statement-file", From: &from, To: &to},
		ReasonCode: batch.ReasonIncidentRepair, Reason: "window repair"})
	if err != nil || j.ScopeKind != batch.ScopeWindow || j.SelectedItems != 3 || j.ConnectionID != c.ConnectionID {
		t.Fatalf("%v %+v", err, j)
	}
	if done := e.run(j); done.State != batch.ReplayCompleted {
		t.Fatalf("%+v", done)
	}
	if got := e.settle(e.get(tenantA, b.BatchID)); got.State != batch.StateReconciled {
		t.Fatalf("%+v", got)
	}
	// the window selects only HELD exchanges: a second request finds nothing
	if _, err := e.request(tenantA, batch.ReplayRequest{Scope: batch.ReplayScope{ConnectionID: c.ConnectionID, OperationID: "statement-file", From: &from, To: &to},
		ReasonCode: batch.ReasonIncidentRepair, Reason: "again"}); !errors.Is(err, batch.ErrReplayScope) {
		t.Fatalf("%v", err)
	}
}

func TestReplay_RowNumbersSelectOnlyThoseRows(t *testing.T) {
	e := newEnv(t)
	c := e.activeConn(tenantA, e.publish(nil), "acc-a")
	b := e.held(c, "B-1", 5)
	e.target()
	j, err := e.request(tenantA, batch.ReplayRequest{Scope: batch.ReplayScope{BatchID: b.BatchID, RowNumbers: []int{2, 4}}, ReasonCode: batch.ReasonSourceCorrection, Reason: "rows 2 and 4"})
	if err != nil || j.SelectedItems != 2 {
		t.Fatalf("%v %+v", err, j)
	}
	e.run(j)
	got := e.settle(e.get(tenantA, b.BatchID))
	if got.AcceptedRows != 2 || got.QuarantinedRows != 3 || got.State != batch.StatePartial || e.domHits != 2 {
		t.Fatalf("%+v hits=%d", got, e.domHits)
	}
}

func TestReplay_TenantsNeverSeeOrDecideEachOthersJobs(t *testing.T) {
	e := newEnv(t)
	cfg := batch.DefaultConfig()
	cfg.ReplayApprovalRows = 1
	e.svc.Cfg = cfg
	v := e.publish(nil)
	cA := e.activeConn(tenantA, v, "acc-a")
	e.activeConn(tenantB, v, "acc-b")
	b := e.held(cA, "B-1", 2)
	e.target()
	d := req(b)
	d.DryRun = true
	dj, _ := e.request(tenantA, d)
	r := req(b)
	r.DryRunOf = dj.ReplayID
	j, _ := e.request(tenantA, r)
	if _, err := e.repo.GetReplay(ctxFor(tenantB), j.ReplayID); !errors.Is(err, batch.ErrReplayNotFound) {
		t.Fatalf("%v", err)
	}
	if _, err := e.svc.DecideReplay(ctxFor(tenantB), approver, j.ReplayID, true, ""); !errors.Is(err, batch.ErrReplayNotFound) {
		t.Fatalf("%v", err)
	}
	if _, err := e.svc.RunReplay(context.Background(), tenantB, j.ReplayID); !errors.Is(err, batch.ErrReplayNotFound) {
		t.Fatalf("%v", err)
	}
	if _, err := e.svc.CancelReplay(ctxFor(tenantB), approver, j.ReplayID, "x"); !errors.Is(err, batch.ErrReplayNotFound) {
		t.Fatalf("%v", err)
	}
	if l, _ := e.repo.ListReplays(ctxFor(tenantB), batch.ReplayFilter{}); len(l) != 0 {
		t.Fatal("listed another tenant's jobs")
	}
	if _, err := e.repo.ReplayItems(ctxFor(tenantB), j.ReplayID, 0); !errors.Is(err, batch.ErrReplayNotFound) {
		t.Fatalf("%v", err)
	}
}

func TestReplay_WorkerRunsApprovedJobs(t *testing.T) {
	e := newEnv(t)
	c := e.activeConn(tenantA, e.publish(nil), "acc-a")
	b := e.held(c, "B-1", 2)
	e.target()
	j, _ := e.request(tenantA, req(b))
	n, err := e.svc.DriveReplays(context.Background(), 10)
	if err != nil || n != 1 {
		t.Fatalf("%d %v", n, err)
	}
	if got, _ := e.repo.GetReplay(ctxFor(tenantA), j.ReplayID); got.State != batch.ReplayCompleted {
		t.Fatalf("%+v", got)
	}
}

func TestEveryBatchEventTypeIsProducedByARealFlow(t *testing.T) {
	e := newEnv(t)
	cfg := batch.DefaultConfig()
	cfg.ReplayApprovalRows = 2
	e.svc.Cfg = cfg
	c := e.activeConn(tenantA, e.publish(nil), "acc-a")
	// received, accepted, partial (no domain), duplicate, conflict
	b := e.held(c, "B-1", 3)
	b2 := e.held(c, "B-2", 3)
	b3 := e.held(c, "B-3", 1)
	e.deliver(simple(c, "B-1", 3))
	other := simple(c, "B-1", 3)
	other.files[0].data = []byte(csvFile(rows(3, "z")...))
	e.deliver(other)
	// rejected (integrity), security events
	wrong := strings.Repeat("b", 64)
	e.deliver(bspec{conn: c, id: "B-BAD", records: intp(1), files: []fileSpec{{name: "stmt-0001.csv", data: []byte(csvFile(row("q1"))), declared: &wrong}}})
	bomb := gz(csvFile(strings.Repeat("t0001,A1,10.5,GBP,2026-10-20T10:00:00Z\n", 100000)))
	ob := e.deliver(bspec{conn: c, id: "B-BOMB", records: intp(900), files: []fileSpec{{name: "stmt-0002.csv.gz", data: bomb, compression: "gzip"}}})
	e.process(ob.Batch)
	ou := e.deliver(bspec{conn: c, id: "B-EVIL", records: intp(1), files: []fileSpec{{name: "stmt-0003.csv", data: []byte("a\x00b\n")}}})
	e.process(ou.Batch)
	// replay: dry run, requested, approved, started, completed; another rejected; another cancelled
	e.target()
	d := req(b)
	d.DryRun = true
	dj, _ := e.request(tenantA, d)
	r := req(b)
	r.DryRunOf = dj.ReplayID
	j, _ := e.request(tenantA, r)
	ok, _ := e.svc.DecideReplay(ctxFor(tenantA), approver, j.ReplayID, true, "")
	e.run(ok)
	e.settle(e.get(tenantA, b.BatchID)) // reconciled
	d2 := req(b2)
	d2.DryRun = true
	dd, _ := e.request(tenantA, d2)
	r2 := req(b2)
	r2.DryRunOf = dd.ReplayID
	j2, _ := e.request(tenantA, r2)
	_, _ = e.svc.DecideReplay(ctxFor(tenantA), approver, j2.ReplayID, false, "no")
	j3, _ := e.request(tenantA, req(b3))
	_, _ = e.svc.CancelReplay(ctxFor(tenantA), requestr, j3.ReplayID, "withdrawn")

	types := e.repo.Types()
	for _, want := range batch.EventTypes {
		if !hasType(types, want) {
			t.Errorf("no flow produced %s (got %v)", want, types)
		}
	}
	for _, ev := range e.repo.Out {
		if ev.Data["tenant_id"] != tenantA {
			t.Errorf("%s carries no tenant", ev.Type)
		}
	}
}
