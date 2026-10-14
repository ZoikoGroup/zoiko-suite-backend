package inputpull_test

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"zoiko.io/payroll-run-svc/internal/benefits"
	"zoiko.io/payroll-run-svc/internal/compensation"
	"zoiko.io/payroll-run-svc/internal/contract"
	"zoiko.io/payroll-run-svc/internal/domain"
	"zoiko.io/payroll-run-svc/internal/employee"
	"zoiko.io/payroll-run-svc/internal/inputpull"
	"zoiko.io/payroll-run-svc/internal/workerctx"
)

type fakes struct {
	employees []employee.Employee
	empErr    error

	contracts   map[string]*contract.ActiveContract // missing => not found
	contractErr error

	breakdown    map[string]*compensation.Breakdown // missing => ErrNoStructure
	breakdownErr error
	bonuses      map[string][]compensation.Bonus
	bonusErr     error

	plans        []benefits.Plan
	planErr      error
	elections    map[string][]benefits.Election
	electionsErr error

	ctx    map[string]domain.WorkerContext
	ctxErr error

	calls       atomic.Int32
	lastBaseAmt atomic.Value
}

func (f *fakes) ListActiveEmployeesByEntity(context.Context, string, string, string) ([]employee.Employee, error) {
	return f.employees, f.empErr
}

func (f *fakes) GetActiveContract(_ context.Context, _, _, id string) (*contract.ActiveContract, error) {
	f.calls.Add(1)
	if f.contractErr != nil {
		return nil, f.contractErr
	}
	c, ok := f.contracts[id]
	if !ok {
		return nil, domain.ErrPayrollRunNotFound
	}
	return c, nil
}

func (f *fakes) GetBreakdown(_ context.Context, _, _, id string, base float64) (*compensation.Breakdown, error) {
	f.lastBaseAmt.Store(base)
	if f.breakdownErr != nil {
		return nil, f.breakdownErr
	}
	b, ok := f.breakdown[id]
	if !ok {
		return nil, compensation.ErrNoStructure
	}
	return b, nil
}

func (f *fakes) ListApprovedBonuses(_ context.Context, _, _, id string) ([]compensation.Bonus, error) {
	return f.bonuses[id], f.bonusErr
}

func (f *fakes) ListPlans(context.Context, string, string, string) ([]benefits.Plan, error) {
	return f.plans, f.planErr
}

func (f *fakes) ListActiveElections(_ context.Context, _, _, id string) ([]benefits.Election, error) {
	return f.elections[id], f.electionsErr
}

func (f *fakes) Validate(_ context.Context, _ workerctx.Caller, id, _, _ string) (domain.WorkerContext, error) {
	if f.ctxErr != nil {
		return domain.WorkerContext{}, f.ctxErr
	}
	if c, ok := f.ctx[id]; ok {
		return c, nil
	}
	return domain.WorkerContext{Valid: true, PayCurrencies: []string{"GBP"}}, nil
}

func newPuller(f *fakes) *inputpull.Puller {
	return inputpull.New(f, f, f, f, f, zap.NewNop())
}

var req = inputpull.Request{
	TenantID: "t-1", PrincipalID: "p-1", CorrelationID: "c-1", LegalEntityID: "le-1",
	PeriodStart: "2026-09-01", PeriodEnd: "2026-09-30",
}

func baseFakes() *fakes {
	by := "mgr-1"
	return &fakes{
		employees: []employee.Employee{{EmployeeID: "w-2"}, {EmployeeID: "w-1"}},
		contracts: map[string]*contract.ActiveContract{
			"w-1": {ContractID: "c-1", BaseSalaryAmount: 3000, Currency: "GBP", PayFrequency: "MONTHLY"},
			"w-2": {ContractID: "c-2", BaseSalaryAmount: 2000, Currency: "GBP", PayFrequency: "MONTHLY"},
		},
		breakdown: map[string]*compensation.Breakdown{
			"w-1": {StructureID: "s-1", Currency: "GBP", Lines: []compensation.BreakdownLine{
				{ComponentCode: "HRA", ComponentName: "House", ComponentType: "EARNING", IsTaxable: true, Amount: 100},
			}},
		},
		bonuses: map[string][]compensation.Bonus{"w-1": {
			{GrantID: "g-in", BonusType: "ANNUAL", Amount: 500, Currency: "GBP", GrantDate: "2026-09-15", ApprovedBy: &by},
			{GrantID: "g-late", BonusType: "ANNUAL", Amount: 900, Currency: "GBP", GrantDate: "2026-10-02", ApprovedBy: &by},
			{GrantID: "g-retro", BonusType: "SIGNING", Amount: 300, Currency: "GBP", GrantDate: "2026-07-01", ApprovedBy: &by},
			{GrantID: "g-taken", BonusType: "ANNUAL", Amount: 100, Currency: "GBP", GrantDate: "2026-08-01", ApprovedBy: &by},
		}},
		plans: []benefits.Plan{{PlanID: "pl-1", PlanType: "HEALTH_INSURANCE", DeductionTaxTreatment: "PRE_TAX", Currency: "GBP"}},
		elections: map[string][]benefits.Election{"w-1": {
			{ElectionID: "e-ok", PlanID: "pl-1", EmployeeContributionAmount: 50, EmployerContributionAmount: 120, EffectiveFrom: "2026-01-01"},
			{ElectionID: "e-future", PlanID: "pl-1", EffectiveFrom: "2026-10-01"},
			{ElectionID: "e-ended", PlanID: "pl-1", EffectiveFrom: "2026-01-01", EffectiveTo: strPtr("2026-08-31")},
			{ElectionID: "e-unknown", PlanID: "pl-gone", EmployeeContributionAmount: 10, EffectiveFrom: "2026-01-01"},
		}},
	}
}

func strPtr(s string) *string { return &s }

func TestPull_AssemblesSortedSourcesWithFilters(t *testing.T) {
	f := baseFakes()
	r := req
	r.ExcludeBonuses = map[string]bool{"BONUS:g-taken": true}
	got, err := newPuller(f).Pull(context.Background(), r)
	require.NoError(t, err)
	require.Len(t, got, 2)
	assert.Equal(t, []string{"w-1", "w-2"}, []string{got[0].WorkerID, got[1].WorkerID}, "sorted by worker id")

	w1 := got[0]
	require.NotNil(t, w1.Base)
	assert.Equal(t, "c-1", w1.Base.ContractID)
	require.Len(t, w1.Components, 1)
	assert.Equal(t, "GBP", w1.Components[0].Currency, "the breakdown's currency")
	assert.Equal(t, "s-1", w1.Components[0].StructureID)

	var bonusIDs []string
	for _, b := range w1.Bonuses {
		bonusIDs = append(bonusIDs, b.GrantID)
	}
	assert.ElementsMatch(t, []string{"g-in", "g-retro"}, bonusIDs,
		"a future-dated bonus and one already held by another period are excluded; a retro one is included")

	var electionIDs []string
	for _, e := range w1.Elections {
		electionIDs = append(electionIDs, e.ElectionID)
	}
	assert.ElementsMatch(t, []string{"e-ok", "e-unknown"}, electionIDs, "only elections in force during the period")
	for _, e := range w1.Elections {
		if e.ElectionID == "e-unknown" {
			assert.Equal(t, "UNKNOWN_PLAN", e.PlanType, "an election on a missing plan surfaces instead of vanishing")
		}
		if e.ElectionID == "e-ok" {
			assert.Equal(t, "HEALTH_INSURANCE", e.PlanType)
			assert.Equal(t, "PRE_TAX", e.TaxTreatment)
			assert.Equal(t, 120.0, e.EmployerAmount)
		}
	}

	// w-2 has a contract but no compensation structure: normal, not an error.
	assert.NotNil(t, got[1].Base)
	assert.Empty(t, got[1].Components)
}

func TestPull_NoActiveContractIsAStateNotAnOutage(t *testing.T) {
	f := baseFakes()
	delete(f.contracts, "w-2")
	got, err := newPuller(f).Pull(context.Background(), req)
	require.NoError(t, err)
	assert.Nil(t, got[1].Base)
	assert.Empty(t, got[1].Components, "no base, so no breakdown is requested")
}

func TestPull_WorkerContextIsCarried(t *testing.T) {
	f := baseFakes()
	f.ctx = map[string]domain.WorkerContext{"w-2": {Valid: false, Reason: "NO_ACTIVE_PROFILE"}}
	got, err := newPuller(f).Pull(context.Background(), req)
	require.NoError(t, err)
	assert.True(t, got[0].Context.Valid)
	assert.Equal(t, []string{"GBP"}, got[0].Context.PayCurrencies)
	assert.False(t, got[1].Context.Valid)
	assert.Equal(t, "NO_ACTIVE_PROFILE", got[1].Context.Reason)
}

func TestPull_FailsClosedWhenAnySourceCannotAnswer(t *testing.T) {
	boom := errors.New("connection refused to http://secret-host:1234")
	cases := map[string]func(*fakes){
		"employee list":  func(f *fakes) { f.empErr = boom },
		"plans":          func(f *fakes) { f.planErr = boom },
		"worker context": func(f *fakes) { f.ctxErr = domain.ErrSourceUnavailable },
		"contract":       func(f *fakes) { f.contractErr = boom },
		"breakdown":      func(f *fakes) { f.breakdownErr = boom },
		"bonuses":        func(f *fakes) { f.bonusErr = boom },
		"elections":      func(f *fakes) { f.electionsErr = boom },
	}
	for name, setup := range cases {
		f := baseFakes()
		setup(f)
		got, err := newPuller(f).Pull(context.Background(), req)
		require.Error(t, err, name)
		assert.ErrorIs(t, err, domain.ErrSourceUnavailable, name)
		assert.Nil(t, got, "%s: no partial population is ever returned", name)
		assert.NotContains(t, err.Error(), "secret-host", "%s: the detail is logged, not returned", name)
	}
}

func TestPull_RefusesAnEntityTooLargeForASynchronousBatch(t *testing.T) {
	f := baseFakes()
	p := newPuller(f)
	p.MaxWorkers = 1
	_, err := p.Pull(context.Background(), req)
	assert.ErrorIs(t, err, domain.ErrTooManyWorkers)
	assert.Zero(t, f.calls.Load(), "nothing is pulled once the population is known to be too large")
}

func TestPull_ManyWorkersConcurrentlyKeepsEveryWorker(t *testing.T) {
	f := baseFakes()
	f.employees = nil
	f.contracts = map[string]*contract.ActiveContract{}
	for i := 0; i < 60; i++ {
		id := string(rune('a'+i/26)) + string(rune('a'+i%26))
		f.employees = append(f.employees, employee.Employee{EmployeeID: id})
		f.contracts[id] = &contract.ActiveContract{ContractID: "c-" + id, BaseSalaryAmount: float64(1000 + i), Currency: "GBP"}
	}
	f.bonuses, f.elections, f.breakdown = nil, nil, nil
	got, err := newPuller(f).Pull(context.Background(), req)
	require.NoError(t, err)
	require.Len(t, got, 60)
	for i := 1; i < len(got); i++ {
		assert.Less(t, got[i-1].WorkerID, got[i].WorkerID)
	}
	for _, w := range got {
		require.NotNil(t, w.Base, w.WorkerID)
		assert.Equal(t, "c-"+w.WorkerID, w.Base.ContractID, "each worker keeps its own data under concurrency")
	}
}
