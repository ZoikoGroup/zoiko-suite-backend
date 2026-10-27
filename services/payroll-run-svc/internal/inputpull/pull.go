// Package inputpull gathers the source facts a WFP-03 payroll input batch is
// built from: the worker population (employee-master-svc), each worker's WFP-01
// context, the base salary (employment-contracts-svc), the compensation
// breakdown and approved bonuses (compensation-svc) and benefit elections
// (benefits-svc).
//
// It only READS, and it fails closed: if any source cannot answer, the whole
// pull fails. "No answer" is never read as "no bonuses" or "no elections".
package inputpull

import (
	"context"
	"errors"
	"sort"
	"sync"

	"go.uber.org/zap"

	"zoiko.io/payroll-run-svc/internal/benefits"
	"zoiko.io/payroll-run-svc/internal/compensation"
	"zoiko.io/payroll-run-svc/internal/contract"
	"zoiko.io/payroll-run-svc/internal/domain"
	"zoiko.io/payroll-run-svc/internal/employee"
	"zoiko.io/payroll-run-svc/internal/workerctx"
)

type Employees interface {
	ListActiveEmployeesByEntity(ctx context.Context, tenantID, principalID, legalEntityID string) ([]employee.Employee, error)
}

type Contracts interface {
	GetActiveContract(ctx context.Context, tenantID, principalID, employeeID string) (*contract.ActiveContract, error)
}

type Compensation interface {
	GetBreakdown(ctx context.Context, tenantID, principalID, employeeID string, baseAmount float64) (*compensation.Breakdown, error)
	ListApprovedBonuses(ctx context.Context, tenantID, principalID, employeeID string) ([]compensation.Bonus, error)
}

type Benefits interface {
	ListPlans(ctx context.Context, tenantID, principalID, legalEntityID string) ([]benefits.Plan, error)
	ListActiveElections(ctx context.Context, tenantID, principalID, employeeID string) ([]benefits.Election, error)
}

type Workers interface {
	Validate(ctx context.Context, caller workerctx.Caller, workerID, legalEntityID, asOf string) (domain.WorkerContext, error)
}

// Request scopes one pull. ExcludeBonuses are bonus source lines already held by
// another period's live batch (a bonus is imported into exactly one period).
type Request struct {
	TenantID, PrincipalID, CorrelationID string
	LegalEntityID                        string
	PeriodStart, PeriodEnd               string
	ExcludeBonuses                       map[string]bool
}

type Puller struct {
	employees    Employees
	contracts    Contracts
	compensation Compensation
	benefits     Benefits
	workers      Workers
	log          *zap.Logger
	// MaxWorkers bounds one synchronous pull: a batch is built inside one HTTP
	// request. Larger populations need an asynchronous job (not built yet).
	MaxWorkers  int
	Concurrency int
}

func New(e Employees, c Contracts, comp Compensation, b Benefits, w Workers, log *zap.Logger) *Puller {
	return &Puller{employees: e, contracts: c, compensation: comp, benefits: b, workers: w, log: log, MaxWorkers: 500, Concurrency: 8}
}

func (p *Puller) fail(what string, err error) error {
	// The detail (a URL, a status) is logged, not returned.
	p.log.Error("payroll input source unavailable", zap.String("source", what), zap.Error(err))
	return domain.ErrSourceUnavailable
}

// Pull returns one WorkerSource per active worker of the entity, sorted by worker id.
func (p *Puller) Pull(ctx context.Context, req Request) ([]domain.WorkerSource, error) {
	emps, err := p.employees.ListActiveEmployeesByEntity(ctx, req.TenantID, req.PrincipalID, req.LegalEntityID)
	if err != nil {
		return nil, p.fail("employee-master-svc", err)
	}
	if len(emps) > p.MaxWorkers {
		return nil, domain.ErrTooManyWorkers
	}
	plansList, err := p.benefits.ListPlans(ctx, req.TenantID, req.PrincipalID, req.LegalEntityID)
	if err != nil {
		return nil, p.fail("benefits-svc", err)
	}
	plans := map[string]benefits.Plan{}
	for _, pl := range plansList {
		plans[pl.PlanID] = pl
	}

	out := make([]domain.WorkerSource, len(emps))
	var (
		wg       sync.WaitGroup
		mu       sync.Mutex
		firstErr error
		sem      = make(chan struct{}, max(1, p.Concurrency))
	)
	cctx, cancel := context.WithCancel(ctx)
	defer cancel()
	for i, e := range emps {
		wg.Add(1)
		sem <- struct{}{}
		go func(i int, workerID string) {
			defer wg.Done()
			defer func() { <-sem }()
			if cctx.Err() != nil {
				return
			}
			ws, err := p.pullWorker(cctx, req, workerID, plans)
			if err != nil {
				mu.Lock()
				if firstErr == nil {
					firstErr = err
					cancel()
				}
				mu.Unlock()
				return
			}
			out[i] = ws
		}(i, e.EmployeeID)
	}
	wg.Wait()
	if firstErr != nil {
		return nil, firstErr
	}
	sort.Slice(out, func(i, j int) bool { return out[i].WorkerID < out[j].WorkerID })
	return out, nil
}

func (p *Puller) pullWorker(ctx context.Context, req Request, workerID string, plans map[string]benefits.Plan) (domain.WorkerSource, error) {
	ws := domain.WorkerSource{WorkerID: workerID}

	wc, err := p.workers.Validate(ctx, workerctx.Caller{
		TenantID: req.TenantID, PrincipalID: req.PrincipalID, LegalEntityID: req.LegalEntityID, CorrelationID: req.CorrelationID,
	}, workerID, req.LegalEntityID, req.PeriodEnd)
	if err != nil {
		return ws, p.fail("employee-master-svc (worker context)", err)
	}
	ws.Context = wc

	ctr, err := p.contracts.GetActiveContract(ctx, req.TenantID, req.PrincipalID, workerID)
	switch {
	case errors.Is(err, domain.ErrPayrollRunNotFound):
		// No active contract is a state to report, not an outage.
	case err != nil:
		return ws, p.fail("employment-contracts-svc", err)
	default:
		ws.Base = &domain.SourceBase{ContractID: ctr.ContractID, Amount: ctr.BaseSalaryAmount, Currency: ctr.Currency, PayFrequency: ctr.PayFrequency}
		bd, err := p.compensation.GetBreakdown(ctx, req.TenantID, req.PrincipalID, workerID, ctr.BaseSalaryAmount)
		switch {
		case errors.Is(err, compensation.ErrNoStructure):
			// A flat base salary with no components is normal.
		case err != nil:
			return ws, p.fail("compensation-svc (breakdown)", err)
		default:
			for _, l := range bd.Lines {
				ws.Components = append(ws.Components, domain.SourceComponent{
					StructureID: bd.StructureID, Code: l.ComponentCode, Name: l.ComponentName, Type: l.ComponentType,
					IsTaxable: l.IsTaxable, Amount: l.Amount, Currency: bd.Currency,
				})
			}
		}
	}

	bonuses, err := p.compensation.ListApprovedBonuses(ctx, req.TenantID, req.PrincipalID, workerID)
	if err != nil {
		return ws, p.fail("compensation-svc (bonuses)", err)
	}
	for _, b := range bonuses {
		if b.GrantDate > req.PeriodEnd || req.ExcludeBonuses["BONUS:"+b.GrantID] {
			continue
		}
		ws.Bonuses = append(ws.Bonuses, domain.SourceBonus{
			GrantID: b.GrantID, BonusType: b.BonusType, Amount: b.Amount, Currency: b.Currency, GrantDate: b.GrantDate, ApprovedBy: b.ApprovedBy,
		})
	}

	elections, err := p.benefits.ListActiveElections(ctx, req.TenantID, req.PrincipalID, workerID)
	if err != nil {
		return ws, p.fail("benefits-svc (elections)", err)
	}
	for _, e := range elections {
		if e.EffectiveFrom > req.PeriodEnd || (e.EffectiveTo != nil && *e.EffectiveTo < req.PeriodStart) {
			continue
		}
		plan, known := plans[e.PlanID]
		se := domain.SourceElection{
			ElectionID: e.ElectionID, PlanID: e.PlanID, PlanType: plan.PlanType, TaxTreatment: plan.DeductionTaxTreatment,
			Currency: plan.Currency, EmployeeAmount: e.EmployeeContributionAmount, EmployerAmount: e.EmployerContributionAmount,
		}
		if !known {
			se.PlanType = "UNKNOWN_PLAN" // surfaces as UNMAPPED_CODE rather than vanishing
		}
		ws.Elections = append(ws.Elections, se)
	}
	return ws, nil
}
