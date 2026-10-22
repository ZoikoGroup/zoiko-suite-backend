package engine

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/google/uuid"
	"go.uber.org/zap"

	"zoiko.io/financial-control-svc/internal/domain"
	"zoiko.io/financial-control-svc/internal/source"
	"zoiko.io/financial-control-svc/internal/store"
)

// LateDataStore is the persistence surface the late-data sweep needs.
type LateDataStore interface {
	LateDataCandidates(ctx context.Context, checkedBefore time.Time, batch int) ([]store.RunRef, error)
	GetExecutionContext(ctx context.Context, tenantID, runID string) (*store.ExecutionContext, error)
	ListPopulationSnapshots(ctx context.Context, tenantID, runID string) ([]domain.PopulationSnapshot, error)
	RunExecutor(ctx context.Context, tenantID, runID string) (string, error)
	RecordWatermarkCheck(ctx context.Context, tenantID string, c store.WatermarkCheck) (store.CheckOutcome, error)
}

// LateDataActor is the actor recorded on a late-data exception's opening.
const LateDataActor = "system:late-data-sweep"

// lateDataSourceTimeout bounds one watermark read, so one slow source cannot
// hold up the rest of the sweep.
const lateDataSourceTimeout = 10 * time.Second

// LateDataSweeper detects late data after certification (ZS-CONTROL-001
// Invariant 14, scenarios 05 and 21; SPEC_DEVIATIONS A7). For the latest
// CERTIFIED run of each control, entity and period it re-reads each side's
// source watermark (page 1, limit=1, through the same client call execution
// builds populations with) and compares it with the watermark the run froze.
// When it has moved, the store opens a LATE_DATA exception on the run, and the
// close gate reports the control as blocking until a new run replaces it or the
// exception is resolved under authority.
//
// It is a detector, not a gate: a source it cannot read is recorded as
// UNVERIFIED and changes nothing else. It never touches the certified run, its
// population or its certification.
type LateDataSweeper struct {
	store        LateDataStore
	source       source.WatermarkReader
	log          *zap.Logger
	now          func() time.Time
	recheckAfter time.Duration
}

// NewLateDataSweeper builds a sweeper that re-checks a run at most once per
// recheckAfter.
func NewLateDataSweeper(s LateDataStore, r source.WatermarkReader, log *zap.Logger, recheckAfter time.Duration) *LateDataSweeper {
	return &LateDataSweeper{store: s, source: r, log: log, now: func() time.Time { return time.Now().UTC() }, recheckAfter: recheckAfter}
}

// LateDataResult counts what one sweep did, per population side checked.
type LateDataResult struct {
	Runs       int // certified runs examined
	Current    int // sides whose watermark is the frozen one
	Moved      int // sides whose watermark has moved
	Opened     int // late-data exceptions opened (a subset of Moved)
	Unverified int // sides whose source could not be read
	Errors     int // runs or sides the sweep itself could not process
}

// Sweep checks up to batch runs, least recently checked first. Safe to run
// repeatedly and from several replicas: each check is recorded under the run's
// row lock, so a late-data exception is opened once.
func (w *LateDataSweeper) Sweep(ctx context.Context, batch int) (LateDataResult, error) {
	var res LateDataResult
	now := w.now()
	cands, err := w.store.LateDataCandidates(ctx, now.Add(-w.recheckAfter), batch)
	if err != nil {
		return res, err
	}
	for _, c := range cands {
		if ctx.Err() != nil {
			return res, ctx.Err()
		}
		res.Runs++
		if err := w.checkRun(ctx, c, now, &res); err != nil {
			res.Errors++
			w.log.Warn("late-data sweep: run not checked", zap.String("run_id", c.RunID), zap.Error(err))
		}
	}
	return res, nil
}

func (w *LateDataSweeper) checkRun(ctx context.Context, c store.RunRef, now time.Time, res *LateDataResult) error {
	ec, err := w.store.GetExecutionContext(ctx, c.TenantID, c.RunID)
	if err != nil {
		return err
	}
	snaps, err := w.store.ListPopulationSnapshots(ctx, c.TenantID, c.RunID)
	if err != nil {
		return err
	}
	if len(snaps) == 0 {
		return fmt.Errorf("certified run has no frozen population")
	}
	executor, err := w.store.RunExecutor(ctx, c.TenantID, c.RunID)
	if err != nil {
		return fmt.Errorf("execution principal: %w", err)
	}
	for _, snap := range snaps {
		correlationID := uuid.NewString()
		observed, checkErr := w.readWatermark(ctx, ec, snap, executor, correlationID)
		out, err := w.store.RecordWatermarkCheck(ctx, c.TenantID, store.WatermarkCheck{
			RunID: c.RunID, Snapshot: snap, Observed: observed, Error: truncate(checkErr, 500),
			OwnerRole: ec.Definition.OwnerRole, At: now, Actor: LateDataActor, CorrelationID: correlationID,
		})
		if err != nil {
			res.Errors++
			w.log.Warn("late-data sweep: check not recorded", zap.String("run_id", c.RunID),
				zap.String("side", string(snap.Side)), zap.Error(err))
			continue
		}
		switch out.Outcome {
		case store.WatermarkCurrent:
			res.Current++
		case store.WatermarkMoved:
			res.Moved++
		case store.WatermarkUnverified:
			res.Unverified++
			w.log.Warn("late-data sweep: source watermark could not be read", zap.String("run_id", c.RunID),
				zap.String("side", string(snap.Side)), zap.String("spec_ref", snap.SpecRef), zap.String("error", checkErr))
		}
		if out.Opened != nil {
			res.Opened++
			w.log.Info("late data after certification: exception opened", zap.String("run_id", c.RunID),
				zap.String("exception_id", out.Opened.ExceptionID), zap.String("spec_ref", snap.SpecRef))
		}
	}
	return nil
}

// readWatermark resolves the side's spec exactly as Execute does (definition
// spec, bound to the run's scope) and reads the source's current watermark on
// behalf of the run's executor. It returns the reason instead when it cannot.
func (w *LateDataSweeper) readWatermark(ctx context.Context, ec *store.ExecutionContext, snap domain.PopulationSnapshot,
	executor, correlationID string) (string, string) {
	raw := ec.Definition.SourceSpec
	if snap.Side == domain.SideB {
		raw = ec.Definition.TargetSpec
	}
	spec, err := resolveSpec(raw, ec.Run.Scope)
	if err != nil {
		return "", "spec: " + err.Error()
	}
	if spec.Ref() != snap.SpecRef {
		// Definitions are immutable, so this means the frozen population cannot be
		// re-addressed; comparing another population's watermark would be meaningless.
		return "", fmt.Sprintf("spec resolves to %s, not the frozen %s", spec.Ref(), snap.SpecRef)
	}
	cctx, cancel := context.WithTimeout(ctx, lateDataSourceTimeout)
	defer cancel()
	wm, err := w.source.Watermark(cctx, spec, source.Scope{TenantID: ec.Run.TenantID, PrincipalID: executor,
		LegalEntityID: ec.Run.LegalEntityID, PeriodID: ec.Run.PeriodID, CorrelationID: correlationID})
	if err != nil {
		return "", err.Error()
	}
	return wm, ""
}

func resolveSpec(raw, runScope json.RawMessage) (source.Spec, error) {
	spec, err := source.ParseSpec(raw)
	if err != nil {
		return spec, err
	}
	return source.ResolveScopeParams(spec, runScope)
}

// WithClock replaces the sweeper's clock, for tests.
func (w *LateDataSweeper) WithClock(now func() time.Time) *LateDataSweeper {
	w.now = now
	return w
}
