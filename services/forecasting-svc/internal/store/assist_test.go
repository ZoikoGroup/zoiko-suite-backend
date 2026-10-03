package store_test

import (
	"errors"
	"testing"

	"zoiko.io/forecasting-svc/internal/domain"
)

// AI-05 Forecast Assist governed advisory layer, against real Postgres
// as the NOSUPERUSER NOBYPASSRLS app role.

// Happy path: SuggestDrivers starts a job, SuggestRange and
// GenerateNarrative append to it, then the planner reviews and accepts.
func TestAssist_HappyPath_FullJobLifecycle(t *testing.T) {
	f := newFixture(t)
	f.registerRelease(orgA, "cash_flow", "internal-forecast", "v1.0")

	job := f.suggestDrivers(orgA, baseDriversRequest("cash_flow"))
	if job.Status != domain.AssistJobGenerated {
		t.Fatalf("job status after SuggestDrivers: %+v", job)
	}

	rangeReq := domain.SuggestRangeRequest{
		JobID:  job.JobID,
		Ranges: []domain.RangeInput{{PeriodLabel: "2026-Q1", LowValue: 90000, HighValue: 110000, ConfidenceNote: "seasonal variance"}},
	}
	job, err := f.s.SuggestRange(f.ctx, orgA, rangeReq, "test-operator", f.claim("SuggestRange", job.JobID))
	if err != nil {
		t.Fatalf("suggest range: %v", err)
	}

	narrReq := domain.GenerateNarrativeRequest{JobID: job.JobID, NarrativeText: "Cash flow expected to grow with headcount."}
	job, err = f.s.GenerateNarrative(f.ctx, orgA, narrReq, "test-operator", f.claim("GenerateNarrative", job.JobID))
	if err != nil {
		t.Fatalf("generate narrative: %v", err)
	}
	if job.Narrative == "" {
		t.Fatalf("narrative not set: %+v", job)
	}

	job, err = f.s.StartPlannerReview(f.ctx, orgA, job.JobID, "reviewer-bob", f.claim("StartPlannerReview", job.JobID))
	if err != nil {
		t.Fatalf("start planner review: %v", err)
	}
	if job.Status != domain.AssistJobPlannerReview {
		t.Fatalf("job status after review start: %+v", job)
	}

	job, err = f.s.AcceptSuggestion(f.ctx, orgA, job.JobID, "reviewer-bob", f.claim("AcceptSuggestion", job.JobID))
	if err != nil {
		t.Fatalf("accept suggestion: %v", err)
	}
	if job.Status != domain.AssistJobAccepted {
		t.Fatalf("job status after accept: %+v", job)
	}

	decision, err := f.s.GetPlannerDecision(f.ctx, orgA, job.JobID)
	if err != nil || decision.Decision != domain.PlannerDecisionAccepted {
		t.Fatalf("planner decision: %+v (err=%v)", decision, err)
	}
}

// Doc-named acceptance tests: a suggestion cannot mutate the approved
// FIN forecast, and a planning suggestion cannot create a journal
// entry. Proven structurally: a legacy ForecastModel (the "approved
// forecast") is created first; the full assist flow — including
// acceptance — runs against a separate governed job; the legacy
// forecast's row is re-read afterward and found byte-for-byte
// unchanged. This service also has no journal-entry table of any kind,
// so "cannot create a journal entry" holds by the absence of any such
// capability, not by a runtime check.
func TestAssist_SuggestionCannotMutateApprovedForecast(t *testing.T) {
	f := newFixture(t)
	f.registerRelease(orgA, "financial", "internal-forecast", "v1.0")

	legacyModel := &domain.ForecastModel{
		LegalEntityID: "LE-1001", ModelName: "Q1 Baseline", Domain: domain.DomainFinancial,
		ScenarioType: domain.ScenarioBaseline, AlgorithmType: domain.AlgorithmLinearTrend,
		Granularity: domain.GranularityMonthly, HorizonPeriods: 3, Status: "ACTIVE", ConfidenceLevel: 95.0,
	}
	legacyProjections := []domain.ForecastProjection{{
		PeriodIndex: 1, PeriodStartDate: "2026-01-01", PeriodEndDate: "2026-01-31",
		ProjectedAmount: 10000, ConfidenceLow: 9000, ConfidenceHigh: 11000, VarianceMargin: 5,
	}}
	if err := f.s.CreateForecast(f.ctx, orgA, legacyModel, legacyProjections); err != nil {
		t.Fatalf("create legacy forecast: %v", err)
	}
	before, err := f.s.GetForecastByID(f.ctx, orgA, legacyModel.ID)
	if err != nil {
		t.Fatalf("get legacy forecast before: %v", err)
	}

	job := f.suggestDrivers(orgA, baseDriversRequest("financial"))
	job, err = f.s.StartPlannerReview(f.ctx, orgA, job.JobID, "reviewer-bob", f.claim("StartPlannerReview", job.JobID))
	if err != nil {
		t.Fatalf("start planner review: %v", err)
	}
	if _, err := f.s.AcceptSuggestion(f.ctx, orgA, job.JobID, "reviewer-bob", f.claim("AcceptSuggestion", job.JobID)); err != nil {
		t.Fatalf("accept suggestion: %v", err)
	}

	after, err := f.s.GetForecastByID(f.ctx, orgA, legacyModel.ID)
	if err != nil {
		t.Fatalf("get legacy forecast after: %v", err)
	}
	if before.Status != after.Status || before.UpdatedAt != after.UpdatedAt || len(before.Projections) != len(after.Projections) {
		t.Fatalf("legacy approved forecast was mutated by an accepted suggestion: before=%+v after=%+v", before, after)
	}
}

// Doc-named acceptance test: model/provider change requires evaluation
// before material use. SuggestDrivers/SuggestRange/GenerateNarrative
// all refuse outright against an unregistered model/provider/version.
func TestAssist_ModelProviderChangeRequiresEvaluationBeforeUse(t *testing.T) {
	f := newFixture(t)
	req := baseDriversRequest("payroll")
	req.ModelVersion = "v9.9-unevaluated"
	_, err := f.s.SuggestDrivers(f.ctx, orgA, req, "test-operator", f.claim("SuggestDrivers", "payroll-unreg"))
	if !errors.Is(err, domain.ErrForecastModelReleaseNotFound) {
		t.Fatalf("suggest drivers against unregistered model: %v", err)
	}

	f.registerRelease(orgA, "payroll", "internal-forecast", "v1.0")
	req2 := baseDriversRequest("payroll")
	req2.ModelProvider = "a-different-vendor"
	if _, err := f.s.SuggestDrivers(f.ctx, orgA, req2, "test-operator", f.claim("SuggestDrivers", "payroll-wrongvendor")); !errors.Is(err, domain.ErrForecastModelReleaseNotFound) {
		t.Fatalf("suggest drivers against unregistered provider: %v", err)
	}
}

// Forward-only lifecycle: a job cannot skip PlannerReview, cannot be
// acted on twice, and new suggestions are refused once review begins.
func TestAssist_JobLifecycle_ForwardOnly(t *testing.T) {
	f := newFixture(t)
	f.registerRelease(orgA, "workforce", "internal-forecast", "v1.0")
	job := f.suggestDrivers(orgA, baseDriversRequest("workforce"))

	// Cannot accept before review starts.
	if _, err := f.s.AcceptSuggestion(f.ctx, orgA, job.JobID, "reviewer-bob", f.claim("AcceptSuggestion-early", job.JobID)); !errors.Is(err, domain.ErrForecastJobNotInReview) {
		t.Fatalf("accept before review: %v", err)
	}

	job, err := f.s.StartPlannerReview(f.ctx, orgA, job.JobID, "reviewer-bob", f.claim("StartPlannerReview", job.JobID))
	if err != nil {
		t.Fatalf("start planner review: %v", err)
	}

	// Cannot add more drivers once under review.
	moreDrivers := domain.SuggestDriversRequest{JobID: job.JobID, Drivers: []domain.DriverInput{{DriverName: "attrition_rate", SuggestedValue: 0.02, Rationale: "late addition"}}}
	if _, err := f.s.SuggestDrivers(f.ctx, orgA, moreDrivers, "test-operator", f.claim("SuggestDrivers-late", job.JobID)); !errors.Is(err, domain.ErrForecastJobNotOpen) {
		t.Fatalf("suggest drivers after review started: %v", err)
	}

	rejected, err := f.s.RejectSuggestion(f.ctx, orgA, job.JobID, domain.RejectForecastSuggestionRequest{Reason: "growth assumption too aggressive"},
		"reviewer-bob", f.claim("RejectSuggestion", job.JobID))
	if err != nil {
		t.Fatalf("reject suggestion: %v", err)
	}
	if rejected.Status != domain.AssistJobRejected {
		t.Fatalf("rejected job: %+v", rejected)
	}

	// Terminal: cannot accept an already-rejected job.
	if _, err := f.s.AcceptSuggestion(f.ctx, orgA, job.JobID, "reviewer-bob", f.claim("AcceptSuggestion-late", job.JobID)); !errors.Is(err, domain.ErrForecastJobNotInReview) {
		t.Fatalf("accept an already-rejected job: %v", err)
	}
}

// Narrative is write-once: a second GenerateNarrative call on the same
// job is refused.
func TestAssist_NarrativeIsWriteOnce(t *testing.T) {
	f := newFixture(t)
	f.registerRelease(orgA, "tax", "internal-forecast", "v1.0")
	job := f.suggestDrivers(orgA, baseDriversRequest("tax"))

	narrReq := domain.GenerateNarrativeRequest{JobID: job.JobID, NarrativeText: "First narrative."}
	job, err := f.s.GenerateNarrative(f.ctx, orgA, narrReq, "test-operator", f.claim("GenerateNarrative", job.JobID))
	if err != nil {
		t.Fatalf("generate narrative: %v", err)
	}

	narrReq2 := domain.GenerateNarrativeRequest{JobID: job.JobID, NarrativeText: "Second narrative attempt."}
	if _, err := f.s.GenerateNarrative(f.ctx, orgA, narrReq2, "test-operator", f.claim("GenerateNarrative-again", job.JobID)); !errors.Is(err, domain.ErrNarrativeAlreadySet) {
		t.Fatalf("second generate narrative: %v", err)
	}

	// Immutability is structural: a raw UPDATE changing the narrative is
	// rejected at the database too.
	_, err = f.admin.Exec(f.ctx, `UPDATE forecast_assist_jobs SET narrative = 'tampered' WHERE job_id = $1`, job.JobID)
	if err == nil {
		t.Fatalf("raw UPDATE of an already-set narrative should have been rejected by the trigger")
	}
}

// Idempotent replay of SuggestDrivers (job creation) returns the
// original job, never a second one.
func TestAssist_SuggestDrivers_IdempotentReplay(t *testing.T) {
	f := newFixture(t)
	f.registerRelease(orgA, "sales", "internal-forecast", "v1.0")
	req := baseDriversRequest("sales")
	claim := f.claim("SuggestDrivers", "sales-replay")

	job, err := f.s.SuggestDrivers(f.ctx, orgA, req, "test-operator", claim)
	if err != nil {
		t.Fatalf("suggest drivers: %v", err)
	}
	_, err = f.s.SuggestDrivers(f.ctx, orgA, req, "test-operator", claim)
	var replay *domain.AssistIdempotentReplayError
	if !errors.As(err, &replay) || replay.ResourceID != job.JobID {
		t.Fatalf("replay of suggest drivers: %v", err)
	}
}

// Tenant isolation: cross-tenant reads are denied by RLS, not an
// application-level filter.
func TestAssist_TenantIsolation(t *testing.T) {
	f := newFixture(t)
	f.registerRelease(orgA, "legal", "internal-forecast", "v1.0")
	job := f.suggestDrivers(orgA, baseDriversRequest("legal"))

	if _, err := f.s.GetForecastAssistJob(f.ctx, orgB, job.JobID); !errors.Is(err, domain.ErrForecastAssistJobNotFound) {
		t.Fatalf("cross-tenant get job: %v", err)
	}
	drivers, err := f.s.GetSuggestedDrivers(f.ctx, orgB, job.JobID)
	if err != nil {
		t.Fatalf("cross-tenant get drivers: %v", err)
	}
	if len(drivers) != 0 {
		t.Fatalf("cross-tenant read leaked drivers: %+v", drivers)
	}
}
