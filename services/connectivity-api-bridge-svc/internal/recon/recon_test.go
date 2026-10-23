package recon_test

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"zoiko.io/connectivity-api-bridge-svc/internal/middleware"
	"zoiko.io/connectivity-api-bridge-svc/internal/recon"
	"zoiko.io/connectivity-api-bridge-svc/internal/registry"
	"zoiko.io/connectivity-api-bridge-svc/internal/xicconn"
)

const (
	tenantA = "tenant-a"
	tenantB = "tenant-b"
	connA   = "11111111-1111-1111-1111-111111111111"
)

var (
	t0       = time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC)
	maker    = recon.Actor{PrincipalID: "ops-user", CorrelationID: "c1"}
	domain   = recon.Actor{PrincipalID: "domain-svc"}
	reviewer = recon.Actor{PrincipalID: "reviewer"}
)

type conns struct {
	m map[string]*xicconn.Connection
}

func (c conns) Connection(_ context.Context, id string) (*xicconn.Connection, error) {
	if x, ok := c.m[id]; ok {
		return x, nil
	}
	return nil, errors.New("none")
}

type connectors struct{}

func (connectors) GetConnector(context.Context, string) (*registry.Connector, error) {
	return &registry.Connector{ConnectorID: "bank-x", Family: "BANK_FEED"}, nil
}

type env struct {
	repo *recon.MemRepo
	svc  *recon.Service
	now  time.Time
}

func newEnv() *env {
	e := &env{repo: recon.NewMemRepo(), now: t0.Add(48 * time.Hour)}
	e.svc = &recon.Service{Repo: e.repo, Connectors: connectors{}, Clock: func() time.Time { return e.now },
		Conns: conns{m: map[string]*xicconn.Connection{connA: {ConnectionID: connA, TenantID: tenantA, ConnectorID: "bank-x"}}}}
	return e
}

func ctx(t string) context.Context { return middleware.WithTenant(context.Background(), t) }

func (e *env) req(basis []string, ext *recon.External) recon.RunRequest {
	return recon.RunRequest{ConnectionID: connA, PeriodFrom: t0, PeriodTo: t0.Add(24 * time.Hour), Basis: basis, External: ext}
}

func prov(counts map[string]int) *recon.External {
	return &recon.External{Source: "PROVIDER_STATEMENT", Counts: counts}
}

func (e *env) run(t *testing.T, r recon.RunRequest) *recon.Run {
	t.Helper()
	run, existing, err := e.svc.RequestRun(ctx(tenantA), maker, r)
	if err != nil || existing {
		t.Fatalf("%v %v", err, existing)
	}
	return run
}

func (e *env) eval(t *testing.T, r *recon.Run) *recon.Run {
	t.Helper()
	got, err := e.svc.Evaluate(ctx(tenantA), tenantA, r.RunID)
	if err != nil {
		t.Fatal(err)
	}
	return got
}

func (e *env) variances(t *testing.T) []recon.Variance {
	t.Helper()
	v, _ := e.repo.ListVariances(ctx(tenantA), recon.VarianceFilter{})
	return v
}

func outTypes(r *recon.MemRepo) []string {
	var s []string
	for _, o := range r.Out {
		s = append(s, o.Type)
	}
	return s
}

func contains(l []string, v string) bool {
	for _, x := range l {
		if x == v {
			return true
		}
	}
	return false
}

// ---- independence (NP-44) ---------------------------------------------------------------------------------------

func TestRequest_AComparisonWithNoProviderDeclarationIsRefused(t *testing.T) {
	e := newEnv()
	for name, r := range map[string]recon.RunRequest{
		"count with no declaration":    e.req([]string{"COUNT"}, nil),
		"count with an empty one":      e.req([]string{"COUNT"}, prov(nil)),
		"reference with no references": e.req([]string{"REFERENCE"}, prov(map[string]int{"received": 1})),
		"amount with no total":         e.req([]string{"AMOUNT"}, prov(map[string]int{"received": 1})),
		"time with no cutoff":          e.req([]string{"TIME"}, prov(map[string]int{"received": 1})),
		"declared by the platform":     e.req([]string{"COUNT"}, &recon.External{Source: "INTERNAL", Counts: map[string]int{"received": 1}}),
		"declared by the XIC itself":   e.req([]string{"COUNT"}, &recon.External{Source: "XIC", Counts: map[string]int{"received": 1}}),
		"a source nobody recognises":   e.req([]string{"COUNT"}, &recon.External{Source: "", Counts: map[string]int{"received": 1}}),
	} {
		if _, _, err := e.svc.RequestRun(ctx(tenantA), maker, r); !errors.Is(err, recon.ErrNotIndependent) {
			t.Errorf("%s: %v", name, err)
		}
	}
	if l, _ := e.repo.ListRuns(ctx(tenantA), recon.RunFilter{}); len(l) != 0 {
		t.Fatalf("%d runs were stored for refused requests", len(l))
	}
}

func TestRequest_Validation(t *testing.T) {
	e := newEnv()
	cut := t0.Add(-time.Hour)
	bad := map[string]recon.RunRequest{
		"no basis":         e.req(nil, nil),
		"unknown basis":    e.req([]string{"VIBES"}, nil),
		"backwards period": {ConnectionID: connA, PeriodFrom: t0.Add(time.Hour), PeriodTo: t0, Basis: []string{"STATE"}},
		"future period":    {ConnectionID: connA, PeriodFrom: t0, PeriodTo: e.now.Add(48 * time.Hour), Basis: []string{"STATE"}},
		"too wide":         {ConnectionID: connA, PeriodFrom: t0.Add(-60 * 24 * time.Hour), PeriodTo: t0, Basis: []string{"STATE"}},
		"unknown count":    e.req([]string{"COUNT"}, prov(map[string]int{"banana": 1})),
		"negative count":   e.req([]string{"COUNT"}, prov(map[string]int{"received": -1})),
		"bad amount":       e.req([]string{"AMOUNT"}, &recon.External{Source: "PROVIDER_STATEMENT", TotalAmount: "12,5", Currency: "GBP"}),
		"bad currency":     e.req([]string{"AMOUNT"}, &recon.External{Source: "PROVIDER_STATEMENT", TotalAmount: "12.5", Currency: "pounds"}),
		"cutoff outside":   e.req([]string{"TIME"}, &recon.External{Source: "PROVIDER_STATEMENT", Cutoff: &cut}),
	}
	for name, r := range bad {
		if _, _, err := e.svc.RequestRun(ctx(tenantA), maker, r); !errors.Is(err, recon.ErrBadRequest) {
			t.Errorf("%s: %v", name, err)
		}
	}
	if _, _, err := e.svc.RequestRun(ctx(tenantA), recon.Actor{}, e.req([]string{"STATE"}, nil)); !errors.Is(err, recon.ErrBadRequest) {
		t.Errorf("no actor: %v", err)
	}
}

func TestRequest_AnotherTenantsConnectionIsRefusedAndRaisedAsASignal(t *testing.T) {
	e := newEnv()
	_, _, err := e.svc.RequestRun(ctx(tenantB), maker, e.req([]string{"STATE"}, nil)) // NP-52
	if !errors.Is(err, recon.ErrNotFound) {
		t.Fatalf("%v", err)
	}
	if len(e.repo.Signals) != 1 || e.repo.Signals[0].Type != "integration.security.tenant-scope-mismatch" {
		t.Fatalf("%+v", e.repo.Signals)
	}
	if l, _ := e.repo.ListRuns(ctx(tenantB), recon.RunFilter{}); len(l) != 0 {
		t.Fatal("a run was stored")
	}
	// a connection that does not exist is the same answer, with no signal
	r := e.req([]string{"STATE"}, nil)
	r.ConnectionID = "22222222-2222-2222-2222-222222222222"
	if _, _, err := e.svc.RequestRun(ctx(tenantA), maker, r); !errors.Is(err, recon.ErrNotFound) || len(e.repo.Signals) != 1 {
		t.Fatalf("%v %d", err, len(e.repo.Signals))
	}
}

func TestRequest_TheSameRequestIsTheSameRun(t *testing.T) {
	e := newEnv()
	a, existing, err := e.svc.RequestRun(ctx(tenantA), maker, e.req([]string{"COUNT"}, prov(map[string]int{"received": 5})))
	if err != nil || existing {
		t.Fatal(err)
	}
	b, existing, err := e.svc.RequestRun(ctx(tenantA), maker, e.req([]string{"COUNT"}, prov(map[string]int{"received": 5})))
	if err != nil || !existing || b.RunID != a.RunID {
		t.Fatalf("%v %v", err, existing)
	}
	c, existing, _ := e.svc.RequestRun(ctx(tenantA), maker, e.req([]string{"COUNT"}, prov(map[string]int{"received": 6})))
	if existing || c.RunID == a.RunID {
		t.Fatal("a different declaration is a different run")
	}
	if a.Family != "BANK_FEED" || a.State != recon.RunOpen || a.ExternalSuppliedBy != "ops-user" || a.ExternalSource != "PROVIDER_STATEMENT" {
		t.Fatalf("%+v", a)
	}
}

// ---- comparison -----------------------------------------------------------------------------------------------

func TestEvaluate_CountsThatAgreeMatchAndThoseThatDifferOpenAVariance(t *testing.T) {
	e := newEnv()
	e.repo.Inbound = map[string]int{"received": 10, "accepted": 9, "rejected": 1}
	e.repo.Outbound = map[string]int{"submitted": 4, "settled": 4}
	ok := e.eval(t, e.run(t, e.req([]string{"COUNT"}, prov(map[string]int{"received": 10, "accepted": 9, "settled": 4}))))
	if ok.State != recon.RunMatched || ok.VarianceCount != 0 || ok.EvaluatedAt == nil {
		t.Fatalf("%+v", ok)
	}
	// NP-45: the provider's total differs from ours
	bad := e.eval(t, e.run(t, e.req([]string{"COUNT"}, prov(map[string]int{"received": 12, "accepted": 9}))))
	if bad.State != recon.RunVariance || bad.VarianceCount != 1 || bad.OpenVariances != 1 {
		t.Fatalf("%+v", bad)
	}
	v := e.variances(t)[0]
	var refs map[string]any
	_ = json.Unmarshal(v.Refs, &refs)
	if v.Type != recon.VarCount || v.Severity != recon.SevLow || refs["external"].(float64) != 12 || refs["internal"].(float64) != 10 || refs["name"] != "received" {
		t.Fatalf("%+v %v", v, refs)
	}
	// the count difference is large: HIGH, and the run is escalated
	big := e.eval(t, e.run(t, e.req([]string{"COUNT"}, prov(map[string]int{"settled": 40}))))
	if big.State != recon.RunEscalated || big.HighVariances != 1 {
		t.Fatalf("%+v", big)
	}
}

func TestEvaluate_ReferencesAreComparedBothWays(t *testing.T) {
	e := newEnv()
	e.repo.Refs = []string{"R1", "R2", "R3"}
	ok := e.eval(t, e.run(t, e.req([]string{"REFERENCE"}, &recon.External{Source: "PROVIDER_API", References: []string{"R3", "R2", "R1"}})))
	if ok.State != recon.RunMatched {
		t.Fatalf("%+v", ok)
	}
	bad := e.eval(t, e.run(t, e.req([]string{"REFERENCE"}, &recon.External{Source: "PROVIDER_API", References: []string{"R1", "R2", "R9"}})))
	if bad.State != recon.RunVariance || bad.VarianceCount != 2 {
		t.Fatalf("%+v", bad)
	}
	types := map[string]bool{}
	for _, v := range e.variances(t) {
		types[v.Type] = true
		if strings.Contains(string(v.Refs), "R1") && v.Type == recon.VarRefMissingInt {
			t.Fatal("a matching reference was reported")
		}
	}
	if !types[recon.VarRefMissingInt] || !types[recon.VarRefMissingExt] {
		t.Fatalf("%v", types)
	}
}

func TestEvaluate_AmountsNeedTheDomainsOwnTotalsFromAnotherPerson(t *testing.T) {
	e := newEnv()
	r := e.run(t, e.req([]string{"AMOUNT"}, &recon.External{Source: "PROVIDER_STATEMENT", TotalAmount: "1000.00", Currency: "GBP"}))
	if !r.AwaitingDomain {
		t.Fatal("an amount run waits for the domain")
	}
	// nothing is evaluated while the domain's side is missing
	if got := e.eval(t, r); got.State != recon.RunOpen || got.EvaluatedAt != nil {
		t.Fatalf("%+v", got)
	}
	if n, _ := e.svc.DriveDue(context.Background(), 10); n != 0 {
		t.Fatalf("%d runs driven", n)
	}
	// the person who supplied the provider's side cannot supply the domain's
	if _, err := e.svc.SupplyDomainTotals(ctx(tenantA), maker, r.RunID, recon.DomainTotals{TotalAmount: "1000.00", Currency: "GBP"}); !errors.Is(err, recon.ErrSelfAction) {
		t.Fatalf("%v", err)
	}
	if _, err := e.svc.SupplyDomainTotals(ctx(tenantA), domain, r.RunID, recon.DomainTotals{TotalAmount: "abc", Currency: "GBP"}); !errors.Is(err, recon.ErrBadRequest) {
		t.Fatalf("%v", err)
	}
	if _, err := e.svc.SupplyDomainTotals(ctx(tenantB), domain, r.RunID, recon.DomainTotals{TotalAmount: "1000.00", Currency: "GBP"}); !errors.Is(err, recon.ErrNotFound) {
		t.Fatalf("%v", err)
	}
	got, err := e.svc.SupplyDomainTotals(ctx(tenantA), domain, r.RunID, recon.DomainTotals{TotalAmount: "990.00", Currency: "GBP", Reference: "ledger-9"})
	if err != nil || got.AwaitingDomain || got.DomainTotalsBy != "domain-svc" {
		t.Fatalf("%v %+v", err, got)
	}
	if _, err := e.svc.SupplyDomainTotals(ctx(tenantA), reviewer, r.RunID, recon.DomainTotals{TotalAmount: "1.00", Currency: "GBP"}); !errors.Is(err, recon.ErrState) {
		t.Fatalf("totals are supplied once: %v", err)
	}
	if n, _ := e.svc.DriveDue(context.Background(), 10); n != 1 {
		t.Fatalf("%d", n)
	}
	done, _ := e.repo.GetRun(ctx(tenantA), r.RunID)
	if done.State != recon.RunEscalated { // 10.00 of 1000.00 is 100 bp: HIGH under the BANK_FEED policy
		t.Fatalf("%+v", done)
	}
	var refs map[string]string
	_ = json.Unmarshal(e.variances(t)[0].Refs, &refs)
	if refs["external_total"] != "1000" || refs["domain_total"] != "990" || refs["difference"] != "10" {
		t.Fatalf("%v", refs)
	}
	// a currency that differs is a variance whatever the numbers say
	r2 := e.run(t, e.req([]string{"AMOUNT"}, &recon.External{Source: "PROVIDER_STATEMENT", TotalAmount: "5.00", Currency: "EUR"}))
	_, _ = e.svc.SupplyDomainTotals(ctx(tenantA), domain, r2.RunID, recon.DomainTotals{TotalAmount: "5.00", Currency: "GBP"})
	if got := e.eval(t, r2); got.State != recon.RunEscalated {
		t.Fatalf("%+v", got)
	}
	// totals that agree match
	r3 := e.run(t, e.req([]string{"AMOUNT"}, &recon.External{Source: "PROVIDER_PORTAL_EXPORT", TotalAmount: "77.5", Currency: "GBP"}))
	_, _ = e.svc.SupplyDomainTotals(ctx(tenantA), domain, r3.RunID, recon.DomainTotals{TotalAmount: "77.50", Currency: "GBP"})
	if got := e.eval(t, r3); got.State != recon.RunMatched {
		t.Fatalf("%+v", got)
	}
}

func TestEvaluate_AnUnknownAttemptPastItsLimitEscalatesAndNothingForcesItFailed(t *testing.T) {
	e := newEnv()
	old := e.now.Add(-72 * time.Hour)
	e.repo.AttemptsF = []recon.AttemptFact{
		{AttemptID: "a-unknown", State: "UNKNOWN", CreatedAt: old, UpdatedAt: old},
		{AttemptID: "a-fresh", State: "UNKNOWN", CreatedAt: e.now.Add(-time.Hour), UpdatedAt: e.now.Add(-time.Hour)},
		{AttemptID: "a-pending", State: "PENDING", CreatedAt: old, UpdatedAt: old},
		{AttemptID: "a-contra", State: "SETTLED", NeedsReconcile: true, LatestReceiptState: "FAILED", CreatedAt: old, UpdatedAt: old},
		{AttemptID: "a-mismatch", State: "SETTLED", LatestReceiptState: "FAILED", CreatedAt: old, UpdatedAt: old},
		{AttemptID: "a-fine", State: "SETTLED", LatestReceiptState: "SETTLED", CreatedAt: old, UpdatedAt: old},
	}
	got := e.eval(t, e.run(t, e.req([]string{"STATE"}, nil)))
	if got.State != recon.RunEscalated || got.VarianceCount != 4 {
		t.Fatalf("%+v", got)
	}
	types := map[string]string{}
	for _, v := range e.variances(t) {
		var refs map[string]any
		_ = json.Unmarshal(v.Refs, &refs)
		types[refs["attempt_id"].(string)] = v.Type
	}
	want := map[string]string{"a-unknown": recon.VarUnknown, "a-pending": recon.VarStalePending, "a-contra": recon.VarLateContradicts, "a-mismatch": recon.VarState}
	for k, v := range want {
		if types[k] != v {
			t.Errorf("%s: %s want %s", k, types[k], v)
		}
	}
	if types["a-fresh"] != "" || types["a-fine"] != "" {
		t.Fatalf("%v", types)
	}
	if !contains(outTypes(e.repo), "integration.reconciliation.escalated") {
		t.Fatal(outTypes(e.repo))
	}
}

func TestEvaluate_BatchEvidenceAndLateArrivals(t *testing.T) {
	e := newEnv()
	d := 10
	e.repo.BatchesF = []recon.BatchFact{
		{BatchID: "b1", ExternalBatchID: "X1", State: "RECONCILED", DeclaredRows: &d, AdmittedRows: 10, ProcessingDone: true, ReceivedAt: e.now.Add(-48 * time.Hour)},
		{BatchID: "b2", ExternalBatchID: "X2", State: "PARTIAL", DeclaredRows: &d, AdmittedRows: 8, RejectedRows: 1, VarianceRows: 1, ProcessingDone: true, ReceivedAt: e.now.Add(-48 * time.Hour)},
		{BatchID: "b3", ExternalBatchID: "X3", State: "PROCESSING", PendingRows: 3, ReceivedAt: e.now.Add(-48 * time.Hour)},
		{BatchID: "b4", ExternalBatchID: "X4", State: "PROCESSING", PendingRows: 3, ReceivedAt: e.now.Add(-time.Hour)},
	}
	e.repo.Late = 3
	cut := t0.Add(12 * time.Hour)
	got := e.eval(t, e.run(t, e.req([]string{"EVIDENCE", "TIME"}, &recon.External{Source: "PROVIDER_MANIFESTS", Cutoff: &cut})))
	types := map[string]int{}
	for _, v := range e.variances(t) {
		types[v.Type]++
	}
	// b2 has a manifest variance and is PARTIAL beyond the lag; b3 is processing beyond it; b4 is recent; b1 is fine
	if got.State != recon.RunVariance || types[recon.VarManifest] != 1 || types[recon.VarBatchIncomplete] != 2 || types[recon.VarLateArrival] != 1 {
		t.Fatalf("%+v %v", got, types)
	}
}

// ---- variances ---------------------------------------------------------------------------------------------------

func (e *env) oneVariance(t *testing.T) (*recon.Run, recon.Variance) {
	t.Helper()
	e.repo.Inbound = map[string]int{"received": 10}
	r := e.eval(t, e.run(t, e.req([]string{"COUNT"}, prov(map[string]int{"received": 11}))))
	return r, e.variances(t)[0]
}

func TestVariance_RepeatFindingsCountInsteadOfDuplicating(t *testing.T) {
	e := newEnv()
	r1, v := e.oneVariance(t)
	// the same difference found by a second run in a later period
	req := e.req([]string{"COUNT"}, prov(map[string]int{"received": 11}))
	req.PeriodTo = req.PeriodTo.Add(time.Hour)
	r2 := e.eval(t, e.run(t, req))
	vs := e.variances(t)
	if len(vs) != 1 || vs[0].VarianceID != v.VarianceID || vs[0].RepeatCount != 2 || vs[0].LastRunID != r2.RunID || vs[0].RunID != r1.RunID {
		t.Fatalf("%+v", vs)
	}
	// a settled variance does not absorb the next finding: that is a new one
	if _, err := e.svc.Resolve(ctx(tenantA), reviewer, v.VarianceID, "CORRECTED_AT_SOURCE", "provider re-sent", 0); err != nil {
		t.Fatal(err)
	}
	req.PeriodTo = req.PeriodTo.Add(time.Hour)
	e.eval(t, e.run(t, req))
	if vs := e.variances(t); len(vs) != 2 {
		t.Fatalf("%d", len(vs))
	}
}

func TestVariance_CaseResolutionAcceptanceAndAutoClose(t *testing.T) {
	e := newEnv()
	r, v := e.oneVariance(t)
	if _, err := e.svc.LinkCase(ctx(tenantA), reviewer, v.VarianceID, " ", 0); !errors.Is(err, recon.ErrBadRequest) {
		t.Fatalf("%v", err)
	}
	got, err := e.svc.LinkCase(ctx(tenantA), reviewer, v.VarianceID, "CASE-77", v.Version)
	if err != nil || got.CaseRef != "CASE-77" {
		t.Fatalf("%v %+v", err, got)
	}
	if _, err := e.svc.LinkCase(ctx(tenantA), reviewer, v.VarianceID, "CASE-78", v.Version); !errors.Is(err, recon.ErrVersionConflict) {
		t.Fatalf("a stale version: %v", err)
	}
	// the run cannot close with an open variance
	if _, err := e.svc.CloseRun(ctx(tenantA), reviewer, r.RunID, ""); !errors.Is(err, recon.ErrState) {
		t.Fatalf("%v", err)
	}
	if _, err := e.svc.Resolve(ctx(tenantA), reviewer, v.VarianceID, "MADE_UP", "x", 0); !errors.Is(err, recon.ErrBadRequest) {
		t.Fatalf("%v", err)
	}
	if _, err := e.svc.Resolve(ctx(tenantA), reviewer, v.VarianceID, "DOMAIN_CORRECTED", "", 0); !errors.Is(err, recon.ErrBadRequest) {
		t.Fatalf("%v", err)
	}
	// another tenant cannot see or settle it
	if _, err := e.svc.Resolve(ctx(tenantB), reviewer, v.VarianceID, "DOMAIN_CORRECTED", "note", 0); !errors.Is(err, recon.ErrNotFound) {
		t.Fatalf("%v", err)
	}
	res, err := e.svc.Resolve(ctx(tenantA), reviewer, v.VarianceID, "DOMAIN_CORRECTED", "ledger corrected", 0)
	if err != nil || res.State != recon.VarResolved || res.ResolvedBy != "reviewer" || res.ResolvedAt == nil {
		t.Fatalf("%v %+v", err, res)
	}
	if _, err := e.svc.Resolve(ctx(tenantA), reviewer, v.VarianceID, "DOMAIN_CORRECTED", "again", 0); !errors.Is(err, recon.ErrState) {
		t.Fatalf("a settled variance is final: %v", err)
	}
	closed, _ := e.repo.GetRun(ctx(tenantA), r.RunID)
	if closed.State != recon.RunClosed || closed.OpenVariances != 0 {
		t.Fatalf("%+v", closed)
	}
	for _, want := range recon.EventTypes {
		if !contains(outTypes(e.repo), want) && want != "integration.reconciliation.escalated" {
			t.Errorf("no %s event", want)
		}
	}
	// the variance event carries the contract's fields and no content
	for _, o := range e.repo.Out {
		if o.Type == "integration.reconciliation.variance" {
			for _, k := range []string{"run_id", "variance_id", "connection_id", "variance_type", "refs"} {
				if _, ok := o.Data[k]; !ok {
					t.Errorf("variance event lacks %s", k)
				}
			}
		}
	}
}

func TestVariance_AcceptingADifferenceNeedsACaseAndAHighOneAnotherPerson(t *testing.T) {
	e := newEnv()
	e.repo.Inbound = map[string]int{"received": 10}
	r := e.eval(t, e.run(t, e.req([]string{"COUNT"}, prov(map[string]int{"received": 50}))))
	if r.State != recon.RunEscalated {
		t.Fatalf("%+v", r)
	}
	v := e.variances(t)[0]
	if v.Severity != recon.SevHigh || v.State != recon.VarEscalated || v.EscalatedAt == nil {
		t.Fatalf("%+v", v)
	}
	if _, err := e.svc.Accept(ctx(tenantA), reviewer, v.VarianceID, "", "known provider rounding", 0); !errors.Is(err, recon.ErrBadRequest) {
		t.Fatalf("no case: %v", err)
	}
	if _, err := e.svc.Accept(ctx(tenantA), maker, v.VarianceID, "CASE-1", "waved through", 0); !errors.Is(err, recon.ErrSelfAction) {
		t.Fatalf("the person who brought the declaration cannot wave it off: %v", err)
	}
	if _, err := e.svc.Accept(ctx(tenantA), reviewer, v.VarianceID, "CASE-1", "", 0); !errors.Is(err, recon.ErrBadRequest) {
		t.Fatalf("no reason: %v", err)
	}
	acc, err := e.svc.Accept(ctx(tenantA), reviewer, v.VarianceID, "CASE-1", "provider confirmed a test batch", 0)
	if err != nil || acc.State != recon.VarAccepted || acc.CaseRef != "CASE-1" {
		t.Fatalf("%v %+v", err, acc)
	}
	if got, _ := e.repo.GetRun(ctx(tenantA), r.RunID); got.State != recon.RunClosed {
		t.Fatalf("%+v", got)
	}
}

func TestRun_AMatchedRunClosesExplicitlyAndAnOpenOneNot(t *testing.T) {
	e := newEnv()
	e.repo.Inbound = map[string]int{"received": 3}
	open := e.run(t, e.req([]string{"COUNT"}, prov(map[string]int{"received": 3})))
	if _, err := e.svc.CloseRun(ctx(tenantA), reviewer, open.RunID, "n"); !errors.Is(err, recon.ErrState) {
		t.Fatalf("an unevaluated run is not closed: %v", err)
	}
	m := e.eval(t, open)
	if m.State != recon.RunMatched {
		t.Fatalf("%+v", m)
	}
	c, err := e.svc.CloseRun(ctx(tenantA), reviewer, m.RunID, "reviewed")
	if err != nil || c.State != recon.RunClosed || c.ClosedBy != "reviewer" {
		t.Fatalf("%v %+v", err, c)
	}
	if _, err := e.svc.CloseRun(ctx(tenantA), reviewer, m.RunID, ""); !errors.Is(err, recon.ErrState) {
		t.Fatalf("%v", err)
	}
}

func TestEvaluate_ARunIsEvaluatedOnceAndAFailedReadLeavesItOpen(t *testing.T) {
	e := newEnv()
	e.repo.Inbound = map[string]int{"received": 3}
	r := e.run(t, e.req([]string{"COUNT"}, prov(map[string]int{"received": 4})))
	e.repo.FactsErr = errors.New("db down")
	if _, err := e.svc.Evaluate(ctx(tenantA), tenantA, r.RunID); !errors.Is(err, recon.ErrDependency) {
		t.Fatalf("%v", err)
	}
	if got, _ := e.repo.GetRun(ctx(tenantA), r.RunID); got.State != recon.RunOpen || got.LeaseUntil != nil {
		t.Fatalf("%+v", got)
	}
	e.repo.FactsErr = nil
	e.now = e.now.Add(time.Minute)
	first := e.eval(t, r)
	again := e.eval(t, r)
	if first.Version != again.Version || len(e.variances(t)) != 1 {
		t.Fatalf("%d %d", first.Version, again.Version)
	}
}

// ---- scheduling (OD-12) ------------------------------------------------------------------------------------------

type live struct{ refs []xicconn.DueRef }

func (l live) LiveRefs(context.Context, int) ([]xicconn.DueRef, error) { return l.refs, nil }

func TestSchedule_AConnectionIsReconciledAtItsFamilysFrequencyOnceForEachPeriod(t *testing.T) {
	e := newEnv()
	e.svc.Live = live{refs: []xicconn.DueRef{{TenantID: tenantA, ConnectionID: connA}}}
	n, err := e.svc.Schedule(context.Background())
	if err != nil || n != 1 {
		t.Fatalf("%d %v", n, err)
	}
	// the same moment again creates nothing
	if n, _ := e.svc.Schedule(context.Background()); n != 0 {
		t.Fatalf("%d", n)
	}
	runs, _ := e.repo.ListRuns(ctx(tenantA), recon.RunFilter{})
	if len(runs) != 1 || runs[0].Origin != recon.OriginScheduled || runs[0].RequestedBy != "system:xic-reconciler" ||
		runs[0].PeriodTo.Sub(runs[0].PeriodFrom) != 24*time.Hour || runs[0].ExternalSource != "" {
		t.Fatalf("%+v", runs)
	}
	// not due again until a whole period has passed
	e.now = e.now.Add(10 * time.Hour)
	if n, _ := e.svc.Schedule(context.Background()); n != 0 {
		t.Fatalf("%d", n)
	}
	e.now = e.now.Add(20 * time.Hour)
	if n, _ := e.svc.Schedule(context.Background()); n != 1 {
		t.Fatalf("%d", n)
	}
	runs, _ = e.repo.ListRuns(ctx(tenantA), recon.RunFilter{})
	if len(runs) != 2 {
		t.Fatalf("%d", len(runs))
	}
	// the scheduled runs are driven by the worker
	if n, _ := e.svc.DriveDue(context.Background(), 10); n != 2 {
		t.Fatalf("%d", n)
	}
	all, _ := e.repo.ListRuns(ctx(tenantA), recon.RunFilter{})
	for _, r := range all {
		if r.State != recon.RunMatched {
			t.Fatalf("%+v", r)
		}
	}
}

func TestPolicy_FamiliesHaveTheirOwnDefaultsAndOverrides(t *testing.T) {
	e := newEnv()
	if p := e.svc.PolicyFor("PAYMENT_RAIL"); p.FrequencyHours != 6 || p.UnknownMaxAge != 4*time.Hour || p.CountTolerance != 0 {
		t.Fatalf("%+v", p)
	}
	if p := e.svc.PolicyFor("HRIS"); p.CountTolerance != 1 || p.FrequencyHours != 24 {
		t.Fatalf("%+v", p)
	}
	if p := e.svc.PolicyFor("SOMETHING_NEW"); p.FrequencyHours != 24 || p.CountTolerance != 0 {
		t.Fatalf("an unknown family gets the strict default: %+v", p)
	}
	e.svc.Policies = map[string]recon.Policy{"BANK_FEED": {Domain: "BANK_FEED", FrequencyHours: 2}}
	if p := e.svc.PolicyFor("BANK_FEED"); p.FrequencyHours != 2 {
		t.Fatalf("%+v", p)
	}
	// a tolerance means a small difference is a match
	e2 := newEnv()
	e2.svc.Policies = map[string]recon.Policy{"BANK_FEED": {Domain: "BANK_FEED", FrequencyHours: 24, CountTolerance: 2, MediumCountDelta: 5, HighCountDelta: 10}}
	e2.repo.Inbound = map[string]int{"received": 10}
	if got := e2.eval(t, e2.run(t, e2.req([]string{"COUNT"}, prov(map[string]int{"received": 12})))); got.State != recon.RunMatched {
		t.Fatalf("%+v", got)
	}
}
