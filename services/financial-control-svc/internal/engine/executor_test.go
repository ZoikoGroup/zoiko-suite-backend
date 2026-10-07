package engine

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"zoiko.io/financial-control-svc/internal/domain"
	"zoiko.io/financial-control-svc/internal/source"
	"zoiko.io/financial-control-svc/internal/store"
)

type fakeStore struct {
	ec        *store.ExecutionContext
	frozen    []store.PopulationBundle
	recorded  *store.ExecutionRecord
	failed    string
	freezeErr error
	recordErr error
}

func (f *fakeStore) GetExecutionContext(context.Context, string, string) (*store.ExecutionContext, error) {
	return f.ec, nil
}
func (f *fakeStore) FreezePopulations(_ context.Context, _, _, _, _ string, b []store.PopulationBundle) (*domain.ControlRun, error) {
	if f.freezeErr != nil {
		return nil, f.freezeErr
	}
	f.frozen = b
	return &domain.ControlRun{LifecycleState: domain.LifecyclePopulationFroze}, nil
}
func (f *fakeStore) RecordExecution(_ context.Context, _, _, _, _ string, rec store.ExecutionRecord) (*domain.ControlRun, error) {
	if f.recordErr != nil {
		return nil, f.recordErr
	}
	f.recorded = &rec
	lc := domain.LifecycleReadyToCertify
	if rec.Result == domain.ResultFail {
		lc = domain.LifecycleExceptionReview
	}
	return &domain.ControlRun{LifecycleState: lc, ResultState: rec.Result}, nil
}
func (f *fakeStore) FailRun(_ context.Context, _, _, _, _, reason string) (*domain.ControlRun, error) {
	f.failed = reason
	return &domain.ControlRun{LifecycleState: domain.LifecycleFailed, ResultState: domain.ResultIndeterminate}, nil
}

type fakeFetcher struct {
	byRef map[string]*source.Fetched
	errs  map[string]error
}

func (f *fakeFetcher) Fetch(_ context.Context, s source.Spec, _ source.Scope) (*source.Fetched, error) {
	if err := f.errs[s.Ref()]; err != nil {
		return nil, err
	}
	return f.byRef[s.Ref()], nil
}

func rec(id, ref, amt string) domain.PopulationRecord {
	return domain.PopulationRecord{RecordID: id, Reference: ref, Amount: amt, Currency: "USD", Date: "2026-09-01"}
}

func newEC() *store.ExecutionContext {
	return &store.ExecutionContext{
		Run: domain.ControlRun{RunID: "r1", TenantID: "t1", LegalEntityID: "e1", RuleVersion: 1, RuleDigest: "sha256:" + string(make([]byte, 0)),
			TriggerType: "PERIOD_END", PeriodID: "2026-09"},
		Definition: domain.ControlDefinition{ControlDefinitionID: "d1", ControlCode: "FIN-CTRL-001", Name: "AR to GL",
			OwnerRole: "AR_CONTROLLER", Assertions: []string{"COMPLETENESS"}, RiskTier: "KEY", Frequency: "PERIOD_END",
			SourceSpec: json.RawMessage(`{"system":"ar","population":"invoices"}`),
			TargetSpec: json.RawMessage(`{"system":"gl","population":"ar-postings"}`)},
	}
}

func newFetcher(a, b []domain.PopulationRecord) *fakeFetcher {
	return &fakeFetcher{byRef: map[string]*source.Fetched{
		"ar/invoices":    {Records: a, Watermark: "ar-wm-9"},
		"gl/ar-postings": {Records: b, Watermark: "gl-wm-4"},
	}, errs: map[string]error{}}
}

func run(t *testing.T, st *fakeStore, f *fakeFetcher) *domain.ControlRun {
	t.Helper()
	ex := New(st, f, zap.NewNop())
	ex.now = func() time.Time { return time.Date(2026, 9, 30, 10, 0, 0, 0, time.UTC) }
	r, err := ex.Execute(context.Background(), Input{TenantID: "t1", RunID: "r1", Actor: "alice", CorrelationID: "c1"})
	require.NoError(t, err)
	return r
}

func TestExecute_CleanRunPassesWithSealedEvidence(t *testing.T) {
	st := &fakeStore{ec: newEC()}
	r := run(t, st, newFetcher([]domain.PopulationRecord{rec("a1", "I-1", "10"), rec("a2", "I-2", "20")},
		[]domain.PopulationRecord{rec("b1", "I-1", "10.00"), rec("b2", "I-2", "20")}))

	assert.Equal(t, domain.ResultPass, r.ResultState)
	assert.Equal(t, domain.LifecycleReadyToCertify, r.LifecycleState)
	require.NotNil(t, st.recorded)
	assert.Empty(t, st.recorded.Exceptions)
	assert.Len(t, st.recorded.Matches, 2)

	require.Len(t, st.frozen, 2)
	assert.Equal(t, "ar-wm-9", st.frozen[0].Snapshot.Watermark)
	assert.True(t, domain.ValidDigest(st.frozen[0].Snapshot.PopulationHash))
	assert.Equal(t, 2, st.frozen[0].Snapshot.RowCount)
	assert.Equal(t, "30", st.frozen[0].Snapshot.Totals[0].Total)

	assert.True(t, domain.VerifyEvidence(st.recorded.EvidenceContent, st.recorded.EvidenceDigest), "evidence is sealed and verifiable")
	var c domain.EvidenceContent
	require.NoError(t, json.Unmarshal(st.recorded.EvidenceContent, &c))
	assert.Equal(t, domain.MatchAlgorithmVersion, c.Execution.Algorithm)
	assert.Equal(t, "FIN-CTRL-001", c.Definition.ControlCode)
	assert.Len(t, c.Populations, 2, "both sides' watermarks and hashes are in the evidence")
	assert.True(t, domain.ValidDigest(c.Execution.MatchSetDigest))
}

func TestExecute_MissingRecordFailsWithOwnedDatedException(t *testing.T) {
	st := &fakeStore{ec: newEC()}
	r := run(t, st, newFetcher([]domain.PopulationRecord{rec("a1", "I-1", "10"), rec("a2", "I-2", "20")},
		[]domain.PopulationRecord{rec("b1", "I-1", "10")}))

	assert.Equal(t, domain.ResultFail, r.ResultState)
	assert.Equal(t, domain.LifecycleExceptionReview, r.LifecycleState)
	require.Len(t, st.recorded.Exceptions, 1)
	x := st.recorded.Exceptions[0]
	assert.Equal(t, domain.CatMissing, x.Category)
	assert.Equal(t, "AR_CONTROLLER", x.OwnerRole, "Invariant 7: never ownerless")
	assert.Equal(t, domain.ExOpen, x.State)
	assert.Equal(t, time.Date(2026, 10, 5, 10, 0, 0, 0, time.UTC), x.DueAt, "MEDIUM default SLA of 5 days")
	assert.NotEmpty(t, x.ExceptionID)
}

func TestExecute_SeverityComesFromPinnedMateriality(t *testing.T) {
	ec := newEC()
	ec.Materiality = &domain.MaterialityPolicy{MaterialityID: "m1", AmountThreshold: "15", Currency: "USD"}
	st := &fakeStore{ec: ec}
	run(t, st, newFetcher([]domain.PopulationRecord{rec("a2", "I-2", "20")}, nil))
	require.Len(t, st.recorded.Exceptions, 1)
	assert.Equal(t, domain.SeverityHigh, st.recorded.Exceptions[0].Severity, "20 >= 15")
	assert.Equal(t, time.Date(2026, 10, 2, 10, 0, 0, 0, time.UTC), st.recorded.Exceptions[0].DueAt, "HIGH default SLA of 2 days")
}

// Scenario 04: the tolerance applied is the one the run pinned, evidence records it.
func TestExecute_UsesPinnedToleranceAndRecordsIt(t *testing.T) {
	ec := newEC()
	ec.Tolerance = &domain.TolerancePolicy{ToleranceID: "tol-1", AbsoluteTolerance: "0.05", DateToleranceDays: 0}
	st := &fakeStore{ec: ec}
	r := run(t, st, newFetcher([]domain.PopulationRecord{rec("a1", "I-1", "10.00")}, []domain.PopulationRecord{rec("b1", "I-1", "9.96")}))
	assert.Equal(t, domain.ResultPass, r.ResultState, "0.04 <= 0.05")
	assert.Equal(t, domain.OutcomeWithinTolerance, st.recorded.Matches[0].Outcome)
	var c domain.EvidenceContent
	require.NoError(t, json.Unmarshal(st.recorded.EvidenceContent, &c))
	assert.Equal(t, "tol-1", c.Execution.ToleranceID)
	assert.Equal(t, "0.05", c.Execution.AbsoluteTolerance)
}

func TestExecute_NoPinnedToleranceMeansExactMatch(t *testing.T) {
	st := &fakeStore{ec: newEC()}
	r := run(t, st, newFetcher([]domain.PopulationRecord{rec("a1", "I-1", "10.00")}, []domain.PopulationRecord{rec("b1", "I-1", "9.99")}))
	assert.Equal(t, domain.ResultFail, r.ResultState, "absence of a policy can only be stricter")
}

// Scenario 09: an outage yields Indeterminate/Failed, never Pass.
func TestExecute_SourceOutageIsFailedIndeterminate_NeverPass(t *testing.T) {
	for name, err := range map[string]error{
		"unavailable":  source.ErrSourceUnavailable,
		"unknown":      source.ErrSourceUnknown,
		"inconsistent": source.ErrSourceInconsistent,
		"too large":    source.ErrPopulationTooLarge,
	} {
		for _, side := range []string{"ar/invoices", "gl/ar-postings"} {
			st := &fakeStore{ec: newEC()}
			f := newFetcher(nil, nil)
			f.errs[side] = err
			r := run(t, st, f)
			assert.Equal(t, domain.LifecycleFailed, r.LifecycleState, "%s %s", name, side)
			assert.Equal(t, domain.ResultIndeterminate, r.ResultState, "%s %s", name, side)
			assert.Nil(t, st.recorded, "a failed control records no result")
			assert.Empty(t, st.frozen, "nothing is frozen from a partial fetch")
			assert.NotEmpty(t, st.failed)
			assert.True(t, IsTechnicalFailure(err))
		}
	}
}

func TestExecute_UnexecutableSpecFails(t *testing.T) {
	ec := newEC()
	ec.Definition.SourceSpec = json.RawMessage(`{"system":"ar"}`)
	st := &fakeStore{ec: ec}
	r := run(t, st, newFetcher(nil, nil))
	assert.Equal(t, domain.ResultIndeterminate, r.ResultState)
	assert.Contains(t, st.failed, "source_spec")
}

func TestExecute_FreezeOrRecordFailureFailsTheRunNotPasses(t *testing.T) {
	st := &fakeStore{ec: newEC(), freezeErr: errors.New("db down")}
	r := run(t, st, newFetcher([]domain.PopulationRecord{rec("a1", "I-1", "1")}, []domain.PopulationRecord{rec("b1", "I-1", "1")}))
	assert.Equal(t, domain.ResultIndeterminate, r.ResultState)
	assert.Contains(t, st.failed, "could not be frozen")

	st = &fakeStore{ec: newEC(), recordErr: errors.New("commit failed")}
	r = run(t, st, newFetcher([]domain.PopulationRecord{rec("a1", "I-1", "1")}, []domain.PopulationRecord{rec("b1", "I-1", "1")}))
	assert.Equal(t, domain.ResultIndeterminate, r.ResultState)
	assert.Contains(t, st.failed, "could not be recorded")
}

func TestExecute_InvalidPinnedToleranceFails(t *testing.T) {
	ec := newEC()
	ec.Tolerance = &domain.TolerancePolicy{ToleranceID: "t", AbsoluteTolerance: "oops"}
	st := &fakeStore{ec: ec}
	r := run(t, st, newFetcher(nil, nil))
	assert.Equal(t, domain.ResultIndeterminate, r.ResultState)
}

// Scenario 03 end to end: headline totals tie, composition does not.
func TestExecute_OffsettingErrorsFailDespiteAgreeingTotals(t *testing.T) {
	st := &fakeStore{ec: newEC()}
	r := run(t, st, newFetcher(
		[]domain.PopulationRecord{rec("a1", "I-1", "100"), rec("a2", "I-2", "200")},
		[]domain.PopulationRecord{rec("b1", "I-1", "150"), rec("b2", "I-2", "150")}))
	assert.Equal(t, domain.ResultFail, r.ResultState)
	var c domain.EvidenceContent
	require.NoError(t, json.Unmarshal(st.recorded.EvidenceContent, &c))
	assert.True(t, c.Execution.TotalsAgree)
	assert.Equal(t, 2, c.Execution.ExceptionCount)
}
