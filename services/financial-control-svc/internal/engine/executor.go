// Package engine orchestrates one control execution (ZS-CONTROL-001 §6 pipeline):
// trigger -> freeze populations -> deterministic comparison -> exceptions ->
// sealed evidence.
//
// Its central promise is the negative one: a control that cannot determine a
// valid result ends FAILED with an Indeterminate result — never Pass
// (scenario 09, anti-pattern "treating a technical control failure as financial
// Pass"). Every error path below funnels into failRun.
package engine

import (
	"context"
	"errors"
	"fmt"
	"math/big"
	"time"

	"go.uber.org/zap"

	"zoiko.io/financial-control-svc/internal/domain"
	"zoiko.io/financial-control-svc/internal/source"
	"zoiko.io/financial-control-svc/internal/store"
)

// Store is the persistence surface the engine needs.
type Store interface {
	GetExecutionContext(ctx context.Context, tenantID, runID string) (*store.ExecutionContext, error)
	FreezePopulations(ctx context.Context, tenantID, runID, actor, correlationID string, bundles []store.PopulationBundle) (*domain.ControlRun, error)
	RecordExecution(ctx context.Context, tenantID, runID, actor, correlationID string, rec store.ExecutionRecord) (*domain.ControlRun, error)
	FailRun(ctx context.Context, tenantID, runID, actor, correlationID, reason string) (*domain.ControlRun, error)
}

type Executor struct {
	store   Store
	fetcher source.Fetcher
	log     *zap.Logger
	now     func() time.Time
}

func New(s Store, f source.Fetcher, log *zap.Logger) *Executor {
	return &Executor{store: s, fetcher: f, log: log, now: func() time.Time { return time.Now().UTC() }}
}

// Input identifies the execution. Tenant and actor are the caller's VERIFIED
// values; the run is already PREPARING (BeginExecution).
type Input struct {
	TenantID      string
	RunID         string
	Actor         string
	CorrelationID string
}

// Execute runs the pipeline to a terminal-for-this-step state and returns the
// run as it now stands. A technical failure is NOT returned as an error: it is
// a durable outcome (FAILED / INDETERMINATE) of the run itself. An error is
// returned only when even that outcome could not be recorded.
func (e *Executor) Execute(ctx context.Context, in Input) (*domain.ControlRun, error) {
	ec, err := e.store.GetExecutionContext(ctx, in.TenantID, in.RunID)
	if err != nil {
		return nil, err
	}
	logic := ec.RuleLogic
	if logic.Kind == "" {
		logic.Kind = domain.KindMatch
	}

	srcSpec, err := source.ParseSpec(ec.Definition.SourceSpec)
	if err != nil {
		return e.failRun(in, "source_spec is not executable: "+err.Error())
	}
	srcSpec, err = source.ResolveScopeParams(srcSpec, ec.Run.Scope)
	if err != nil {
		return e.failRun(in, "source_spec scope binding: "+err.Error())
	}
	scope := source.Scope{TenantID: in.TenantID, PrincipalID: in.Actor, LegalEntityID: ec.Run.LegalEntityID,
		PeriodID: ec.Run.PeriodID, CorrelationID: in.CorrelationID}

	// 1. Build the population(s). Anything short of a complete, reproducible
	//    population is an outage of the control, not a finding about the books.
	fa, err := e.fetcher.Fetch(ctx, srcSpec, scope)
	if err != nil {
		return e.failRun(in, "side A population could not be built: "+err.Error())
	}
	// Exclusions come only from the PINNED rule and are recorded (count, value, digest,
	// authority) on the frozen snapshot. The source's declared totals were already
	// verified against the full transfer inside Fetch, before anything is excluded.
	keptA, exclA, err := domain.ApplyExclusions(fa.Records, logic.Exclusions, domain.SideA)
	if err != nil {
		return e.failRun(in, "side A exclusions: "+err.Error())
	}
	fa = &source.Fetched{Records: keptA, Watermark: fa.Watermark}
	snapA, err := snapshot(domain.SideA, srcSpec, fa, exclA)
	if err != nil {
		return e.failRun(in, "side A snapshot: "+err.Error())
	}
	bundles := []store.PopulationBundle{{Snapshot: snapA, Records: fa.Records}}
	snaps := []domain.PopulationSnapshot{snapA}

	var fb *source.Fetched
	if logic.TwoSided() {
		tgtSpec, err := source.ParseSpec(ec.Definition.TargetSpec)
		if err != nil {
			return e.failRun(in, "target_spec is not executable: "+err.Error())
		}
		tgtSpec, err = source.ResolveScopeParams(tgtSpec, ec.Run.Scope)
		if err != nil {
			return e.failRun(in, "target_spec scope binding: "+err.Error())
		}
		fb, err = e.fetcher.Fetch(ctx, tgtSpec, scope)
		if err != nil {
			return e.failRun(in, "side B population could not be built: "+err.Error())
		}
		keptB, exclB, err := domain.ApplyExclusions(fb.Records, logic.Exclusions, domain.SideB)
		if err != nil {
			return e.failRun(in, "side B exclusions: "+err.Error())
		}
		fb = &source.Fetched{Records: keptB, Watermark: fb.Watermark}
		snapB, err := snapshot(domain.SideB, tgtSpec, fb, exclB)
		if err != nil {
			return e.failRun(in, "side B snapshot: "+err.Error())
		}
		bundles = append(bundles, store.PopulationBundle{Snapshot: snapB, Records: fb.Records})
		snaps = append(snaps, snapB)
	}

	// 2. Freeze (atomic with the state move). After this the population is append-only.
	if _, err := e.store.FreezePopulations(ctx, in.TenantID, in.RunID, in.Actor, in.CorrelationID, bundles); err != nil {
		return e.failRun(in, "population could not be frozen: "+err.Error())
	}

	// 3. Deterministic check under the PINNED tolerance and the PINNED rule logic only.
	params, tolView, err := pinnedParams(ec)
	if err != nil {
		return e.failRun(in, err.Error())
	}
	out, err := e.compare(logic, ec, params, fa, fb)
	if err != nil {
		return e.failRun(in, "comparison failed: "+err.Error())
	}

	// 4. Exceptions (owner, severity, due date) and the sealed evidence package.
	now := e.now()
	var mat *domain.MaterialityView
	if ec.Materiality != nil {
		mat = &domain.MaterialityView{AmountThreshold: ec.Materiality.AmountThreshold, Currency: ec.Materiality.Currency}
	}
	exceptions := domain.BuildExceptions(ec.Run, out.Exceptions, mat, ec.Definition.OwnerRole, now)

	tolView.Kind = logic.Kind
	content, digest, err := buildEvidence(ec, in, snaps, out, exceptions, tolView)
	if err != nil {
		return e.failRun(in, "evidence could not be sealed: "+err.Error())
	}

	run, err := e.store.RecordExecution(ctx, in.TenantID, in.RunID, in.Actor, in.CorrelationID, store.ExecutionRecord{
		Matches: out.Matches, Exceptions: exceptions, EvidenceContent: content, EvidenceDigest: digest, Result: out.Result(),
	})
	if err != nil {
		return e.failRun(in, "execution result could not be recorded: "+err.Error())
	}
	return run, nil
}

// compare runs the check selected by the pinned rule logic.
func (e *Executor) compare(l domain.RuleLogic, ec *store.ExecutionContext, p domain.MatchParams, fa, fb *source.Fetched) (domain.MatchOutput, error) {
	switch l.Kind {
	case domain.KindMatch:
		p.AllowGroups, p.SkipContentDuplicates, p.OutstandingDays = l.AllowGroups, l.SkipContentDuplicates, l.OutstandingDays
		if _, end, ok := domain.PeriodBounds(ec.Run.PeriodID); ok {
			p.PeriodEnd = end
		}
		return domain.MatchPopulations(fa.Records, fb.Records, p)
	case domain.KindDuplicateScan:
		return domain.ScanDuplicates(fa.Records, l)
	case domain.KindSequenceGap:
		return domain.ScanSequenceGaps(fa.Records, l)
	case domain.KindDateCoverage:
		return domain.CheckDateCoverage(fa.Records, l, ec.Run.PeriodID, e.now())
	case domain.KindArithmetic:
		return domain.CheckArithmetic(fa.Records, l, p.AbsoluteTolerance)
	case domain.KindExceptionScan:
		return domain.ScanExceptions(fa.Records, l)
	}
	return domain.MatchOutput{}, fmt.Errorf("unknown check kind %q", l.Kind)
}

// failRun makes the technical failure durable. It uses a context that survives
// the caller going away, so a client disconnect cannot leave a run stranded
// mid-flight.
func (e *Executor) failRun(in Input, reason string) (*domain.ControlRun, error) {
	e.log.Warn("control execution failed — recording FAILED/INDETERMINATE",
		zap.String("run_id", in.RunID), zap.String("reason", reason))
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	run, err := e.store.FailRun(ctx, in.TenantID, in.RunID, in.Actor, in.CorrelationID, truncate(reason, 500))
	if err != nil {
		return nil, fmt.Errorf("control failed (%s) and the failure could not be recorded: %w", reason, err)
	}
	return run, nil
}

func snapshot(side domain.Side, spec source.Spec, f *source.Fetched, excl []domain.Exclusion) (domain.PopulationSnapshot, error) {
	totals, err := domain.ComputeTotals(f.Records)
	if err != nil {
		return domain.PopulationSnapshot{}, err
	}
	hash, err := domain.HashPopulation(f.Records)
	if err != nil {
		return domain.PopulationSnapshot{}, err
	}
	return domain.PopulationSnapshot{Side: side, SourceSystem: spec.System, SpecRef: spec.Ref(), RowCount: len(f.Records),
		Totals: totals, PopulationHash: hash, Watermark: f.Watermark, Exclusions: excl}, nil
}

// pinnedParams derives comparison parameters from the tolerance the run pinned
// at creation. No pinned policy means EXACT matching (zero tolerance): the
// absence of a policy can only make the control stricter, never looser.
func pinnedParams(ec *store.ExecutionContext) (domain.MatchParams, domain.ExecutionEvidence, error) {
	ev := domain.ExecutionEvidence{Algorithm: domain.MatchAlgorithmVersion, AbsoluteTolerance: "0"}
	p := domain.MatchParams{AbsoluteTolerance: new(big.Rat)}
	if ec.Tolerance != nil {
		r, ok := new(big.Rat).SetString(ec.Tolerance.AbsoluteTolerance)
		if !ok || r.Sign() < 0 {
			return p, ev, fmt.Errorf("pinned tolerance %s has an invalid absolute value", ec.Tolerance.ToleranceID)
		}
		p.AbsoluteTolerance = r
		p.DateToleranceDays = ec.Tolerance.DateToleranceDays
		ev.ToleranceID = ec.Tolerance.ToleranceID
		ev.AbsoluteTolerance = domain.CanonicalAmount(r)
		ev.DateToleranceDays = ec.Tolerance.DateToleranceDays
	}
	if ec.Materiality != nil {
		ev.MaterialityID = ec.Materiality.MaterialityID
	}
	return p, ev, nil
}

func buildEvidence(ec *store.ExecutionContext, in Input, snaps []domain.PopulationSnapshot, out domain.MatchOutput,
	exceptions []domain.ControlException, ev domain.ExecutionEvidence) ([]byte, string, error) {

	msd, err := domain.MatchSetDigest(out.Matches)
	if err != nil {
		return nil, "", err
	}
	ev.MatchedCount = out.MatchedCount
	for _, m := range out.Matches {
		if m.Kind == domain.MatchGroup {
			ev.GroupMatchCount++
		}
	}
	ev.ExceptionCount = len(exceptions)
	ev.TotalsAgree = out.TotalsAgree
	ev.Result = out.Result()
	ev.MatchSetDigest = msd

	c := domain.EvidenceContent{
		Definition: domain.DefinitionEvidence{
			ControlDefinitionID: ec.Definition.ControlDefinitionID, ControlCode: ec.Definition.ControlCode,
			Name: ec.Definition.Name, OwnerRole: ec.Definition.OwnerRole, Assertions: ec.Definition.Assertions,
			RiskTier: ec.Definition.RiskTier, Frequency: ec.Definition.Frequency,
			RuleVersion: ec.Run.RuleVersion, RuleDigest: ec.Run.RuleDigest,
		},
		Run: domain.RunEvidence{RunID: ec.Run.RunID, TenantID: ec.Run.TenantID, LegalEntityID: ec.Run.LegalEntityID,
			PeriodID: ec.Run.PeriodID, TriggerType: ec.Run.TriggerType, ExecutedBy: in.Actor, Service: "financial-control-svc"},
		Execution:  ev,
		Exceptions: make([]domain.ExceptionEvidence, 0, len(exceptions)),
	}
	for _, s := range snaps {
		c.Populations = append(c.Populations, domain.PopulationEvidence{Side: s.Side, SourceSystem: s.SourceSystem,
			SpecRef: s.SpecRef, RowCount: s.RowCount, ControlTotals: s.Totals, PopulationHash: s.PopulationHash,
			Watermark: s.Watermark, Exclusions: s.Exclusions})
	}
	for _, x := range exceptions {
		c.Exceptions = append(c.Exceptions, domain.ExceptionEvidence{ExceptionID: x.ExceptionID, Category: x.Category,
			ReasonCode: x.ReasonCode, Severity: x.Severity, Exposure: x.Exposure, Currency: x.Currency, RecordIDs: x.RecordIDs})
	}
	raw, digest, err := domain.SealEvidence(c)
	return raw, digest, err
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}

// IsTechnicalFailure reports whether a fetch error means the control could not
// run (as opposed to a finding about the books).
func IsTechnicalFailure(err error) bool {
	return errors.Is(err, source.ErrSourceUnavailable) || errors.Is(err, source.ErrSourceUnknown) ||
		errors.Is(err, source.ErrSourceInconsistent) || errors.Is(err, source.ErrPopulationTooLarge)
}

// WithClock replaces the engine's clock. It exists so time-dependent controls
// (calendar coverage, exception due dates) are testable deterministically.
func (e *Executor) WithClock(now func() time.Time) *Executor {
	e.now = now
	return e
}
