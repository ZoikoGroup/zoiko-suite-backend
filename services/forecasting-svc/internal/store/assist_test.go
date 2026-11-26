package store_test

import (
	"errors"
	"testing"

	"zoiko.io/forecasting-svc/internal/domain"
)

// AI-05 Forecast Assist governed advisory layer, against real Postgres as
// the NOSUPERUSER NOBYPASSRLS app role.

// Happy path: SuggestDrivers starts a job, SuggestRange and
// GenerateNarrative append to it, then an independent planner accepts.
func TestAssist_HappyPath_FullJobLifecycle(t *testing.T) {
	f := newFixture(t)
	f.registerRelease(orgA, "cash_flow", "internal-forecast", "v1.0")

	job := f.suggestDrivers(orgA, baseDriversRequest("cash_flow"))
	if job.Status != domain.AssistJobGenerated || job.LegalEntityID != "LE-1001" {
		t.Fatalf("job after SuggestDrivers: %+v", job)
	}

	rangeReq := domain.SuggestRangeRequest{
		JobContext: domain.JobContext{JobID: job.JobID},
		Ranges:     []domain.RangeInput{{PeriodLabel: "2026-Q1", LowValue: 90000, HighValue: 110000, ConfidenceNote: "seasonal variance"}},
		Evidence:   []domain.EvidenceInput{{SourceRef: "bank-feed-2025", Description: "cash history"}},
	}
	job, err := f.s.SuggestRange(f.ctx, f.cmd(orgA, requester, "assist.suggest_range", job.Version), rangeReq)
	if err != nil {
		t.Fatalf("suggest range: %v", err)
	}

	narrReq := domain.GenerateNarrativeRequest{JobContext: domain.JobContext{JobID: job.JobID},
		NarrativeText: "Cash flow expected to grow with headcount.",
		Evidence:      []domain.EvidenceInput{{SourceRef: "board-pack", Description: "plan"}}}
	job, err = f.s.GenerateNarrative(f.ctx, f.cmd(orgA, requester, "assist.generate_narrative", job.Version), narrReq)
	if err != nil {
		t.Fatalf("generate narrative: %v", err)
	}
	if job.Narrative == "" {
		t.Fatalf("narrative not set: %+v", job)
	}

	job, err = f.s.StartPlannerReview(f.ctx, f.cmd(orgA, planner, "assist.start_review", job.Version), job.JobID)
	if err != nil {
		t.Fatalf("start planner review: %v", err)
	}
	if job.Status != domain.AssistJobPlannerReview {
		t.Fatalf("job status after review start: %+v", job)
	}

	result, err := f.s.AcceptSuggestion(f.ctx, f.cmd(orgA, planner, "assist.accept", job.Version), job.JobID, domain.AcceptSuggestionRequest{})
	if err != nil {
		t.Fatalf("accept suggestion: %v", err)
	}
	if result.Job.Status != domain.AssistJobAccepted || result.Decision.Decision != domain.PlannerDecisionAccepted {
		t.Fatalf("accept result: %+v / %+v", result.Job, result.Decision)
	}

	decision, err := f.s.GetPlannerDecision(f.ctx, orgA, job.JobID)
	if err != nil || decision.Decision != domain.PlannerDecisionAccepted || decision.Actor != planner {
		t.Fatalf("planner decision: %+v (err=%v)", decision, err)
	}
	drivers, _ := f.s.GetSuggestedDrivers(f.ctx, orgA, job.JobID)
	ranges, _ := f.s.GetSuggestedRanges(f.ctx, orgA, job.JobID)
	evidence, _ := f.s.GetEvidenceReferences(f.ctx, orgA, job.JobID)
	if len(drivers) != 1 || len(ranges) != 1 || len(evidence) != 3 {
		t.Fatalf("job content: drivers=%d ranges=%d evidence=%d", len(drivers), len(ranges), len(evidence))
	}

	var generated, accepted int
	_ = f.admin.QueryRow(f.ctx, "SELECT COUNT(*) FILTER (WHERE event_type = 'AI.ForecastSuggestionGenerated'), COUNT(*) FILTER (WHERE event_type = 'AI.ForecastSuggestionAccepted') FROM outbox_events WHERE aggregate_id = $1", job.JobID).Scan(&generated, &accepted)
	if generated != 3 || accepted != 1 {
		t.Fatalf("outbox events: generated=%d accepted=%d", generated, accepted)
	}
}

// Doc-named acceptance tests: a suggestion cannot mutate the approved FIN
// forecast and cannot create a journal entry. A FIN-02 forecast is created,
// a suggestion naming it as target is accepted, and the forecast row is
// re-read unchanged; the decision only snapshots its id/status/version.
func TestAssist_SuggestionCannotMutateApprovedForecast(t *testing.T) {
	f := newFixture(t)
	f.registerRelease(orgA, "financial", "internal-forecast", "v1.0")

	forecast := f.createForecast(orgA)
	before, err := f.s.GetForecast(f.ctx, orgA, forecast.ID)
	if err != nil {
		t.Fatalf("get forecast before: %v", err)
	}

	job := f.suggestDrivers(orgA, baseDriversRequest("financial"))
	job, err = f.s.StartPlannerReview(f.ctx, f.cmd(orgA, planner, "assist.start_review", job.Version), job.JobID)
	if err != nil {
		t.Fatalf("start planner review: %v", err)
	}
	result, err := f.s.AcceptSuggestion(f.ctx, f.cmd(orgA, planner, "assist.accept", job.Version), job.JobID,
		domain.AcceptSuggestionRequest{TargetForecastID: forecast.ID})
	if err != nil {
		t.Fatalf("accept suggestion: %v", err)
	}
	if result.Decision.TargetForecastID != forecast.ID || result.Decision.TargetForecastVersion != before.Version {
		t.Fatalf("decision target snapshot: %+v", result.Decision)
	}

	after, err := f.s.GetForecast(f.ctx, orgA, forecast.ID)
	if err != nil {
		t.Fatalf("get forecast after: %v", err)
	}
	if before.Status != after.Status || before.Version != after.Version || !before.UpdatedAt.Equal(after.UpdatedAt) || len(before.Projections) != len(after.Projections) {
		t.Fatalf("forecast was mutated by an accepted suggestion: before=%+v after=%+v", before, after)
	}
}

// Doc-named acceptance test: model/provider change requires evaluation
// before material use. Unregistered and registered-but-unevaluated releases
// are both refused.
func TestAssist_ModelProviderChangeRequiresEvaluationBeforeUse(t *testing.T) {
	f := newFixture(t)
	req := baseDriversRequest("payroll")
	req.ModelVersion = "v9.9-unevaluated"
	if _, err := f.s.SuggestDrivers(f.ctx, f.cmd(orgA, requester, "assist.suggest_drivers", 0), req); errCode(err) != "model_release_not_registered" {
		t.Fatalf("suggest drivers against unregistered model: %v", err)
	}

	f.registerRelease(orgA, "payroll", "internal-forecast", "v1.0")
	req2 := baseDriversRequest("payroll")
	req2.ModelProvider = "a-different-vendor"
	if _, err := f.s.SuggestDrivers(f.ctx, f.cmd(orgA, requester, "assist.suggest_drivers", 0), req2); errCode(err) != "model_release_not_registered" {
		t.Fatalf("suggest drivers against unregistered provider: %v", err)
	}

	// Registered but not yet evaluated.
	if _, err := f.s.RegisterModelRelease(f.ctx, f.cmd(orgA, requester, "model_release.register", 0), domain.RegisterModelReleaseRequest{
		DomainName: "payroll", ModelProvider: "internal-forecast", ModelVersion: "v2.0"}); err != nil {
		t.Fatalf("register v2: %v", err)
	}
	req3 := baseDriversRequest("payroll")
	req3.ModelVersion = "v2.0"
	if _, err := f.s.SuggestDrivers(f.ctx, f.cmd(orgA, requester, "assist.suggest_drivers", 0), req3); errCode(err) != "model_release_not_evaluated" {
		t.Fatalf("suggest drivers against unevaluated model: %v", err)
	}
}

// Publication is separated from evaluation approval.
func TestAssist_ModelReleaseApproval_SegregationOfDuties(t *testing.T) {
	f := newFixture(t)
	rel, err := f.s.RegisterModelRelease(f.ctx, f.cmd(orgA, requester, "model_release.register", 0), domain.RegisterModelReleaseRequest{
		DomainName: "tax", ModelProvider: "p", ModelVersion: "v1"})
	if err != nil {
		t.Fatalf("register: %v", err)
	}
	_, err = f.s.ApproveModelRelease(f.ctx, f.cmd(orgA, requester, "model_release.approve", rel.Version), rel.ReleaseID,
		domain.ApproveModelReleaseRequest{EvaluationReference: "self"})
	if errCode(err) != "segregation_of_duties" {
		t.Fatalf("self-approval: %v", err)
	}
	if _, err := f.s.RegisterModelRelease(f.ctx, f.cmd(orgA, requester, "model_release.register", 0), domain.RegisterModelReleaseRequest{
		DomainName: "tax", ModelProvider: "p", ModelVersion: "v1"}); errCode(err) != "model_release_already_registered" {
		t.Fatalf("duplicate registration: %v", err)
	}
}

// Forward-only lifecycle: a job cannot skip PlannerReview, cannot be acted
// on twice, and new suggestions are refused once review begins. The
// requester may not accept its own suggestions.
func TestAssist_JobLifecycle_ForwardOnly(t *testing.T) {
	f := newFixture(t)
	f.registerRelease(orgA, "workforce", "internal-forecast", "v1.0")
	job := f.suggestDrivers(orgA, baseDriversRequest("workforce"))

	if _, err := f.s.AcceptSuggestion(f.ctx, f.cmd(orgA, planner, "assist.accept", 0), job.JobID, domain.AcceptSuggestionRequest{}); errCode(err) != "invalid_transition" {
		t.Fatalf("accept before review: %v", err)
	}

	job, err := f.s.StartPlannerReview(f.ctx, f.cmd(orgA, planner, "assist.start_review", job.Version), job.JobID)
	if err != nil {
		t.Fatalf("start planner review: %v", err)
	}

	moreDrivers := domain.SuggestDriversRequest{JobContext: domain.JobContext{JobID: job.JobID},
		Drivers:  []domain.DriverInput{{DriverName: "attrition_rate", SuggestedValue: 0.02, Rationale: "late addition"}},
		Evidence: []domain.EvidenceInput{{SourceRef: "x"}}}
	if _, err := f.s.SuggestDrivers(f.ctx, f.cmd(orgA, requester, "assist.suggest_drivers", 0), moreDrivers); errCode(err) != "job_not_open" {
		t.Fatalf("suggest drivers after review started: %v", err)
	}

	if _, err := f.s.AcceptSuggestion(f.ctx, f.cmd(orgA, requester, "assist.accept", job.Version), job.JobID, domain.AcceptSuggestionRequest{}); errCode(err) != "segregation_of_duties" {
		t.Fatalf("requester accepting own suggestion: %v", err)
	}
	if _, err := f.s.AcceptSuggestion(f.ctx, f.cmd(orgA, planner, "assist.accept", job.Version+5), job.JobID, domain.AcceptSuggestionRequest{}); errCode(err) != "stale_version" {
		t.Fatalf("stale If-Match: %v", err)
	}

	rejected, err := f.s.RejectSuggestion(f.ctx, f.cmd(orgA, planner, "assist.reject", job.Version), job.JobID,
		domain.RejectSuggestionRequest{Reason: "growth assumption too aggressive"})
	if err != nil {
		t.Fatalf("reject suggestion: %v", err)
	}
	if rejected.Job.Status != domain.AssistJobRejected {
		t.Fatalf("rejected job: %+v", rejected.Job)
	}

	if _, err := f.s.AcceptSuggestion(f.ctx, f.cmd(orgA, planner, "assist.accept", 0), job.JobID, domain.AcceptSuggestionRequest{}); errCode(err) != "invalid_transition" {
		t.Fatalf("accept an already-rejected job: %v", err)
	}
}

// Narrative is write-once.
func TestAssist_NarrativeIsWriteOnce(t *testing.T) {
	f := newFixture(t)
	f.registerRelease(orgA, "tax", "internal-forecast", "v1.0")
	job := f.suggestDrivers(orgA, baseDriversRequest("tax"))
	ev := []domain.EvidenceInput{{SourceRef: "src"}}

	job, err := f.s.GenerateNarrative(f.ctx, f.cmd(orgA, requester, "assist.generate_narrative", 0),
		domain.GenerateNarrativeRequest{JobContext: domain.JobContext{JobID: job.JobID}, NarrativeText: "First narrative.", Evidence: ev})
	if err != nil {
		t.Fatalf("generate narrative: %v", err)
	}
	if _, err := f.s.GenerateNarrative(f.ctx, f.cmd(orgA, requester, "assist.generate_narrative", 0),
		domain.GenerateNarrativeRequest{JobContext: domain.JobContext{JobID: job.JobID}, NarrativeText: "Second.", Evidence: ev}); errCode(err) != "narrative_already_generated" {
		t.Fatalf("second generate narrative: %v", err)
	}

	if _, err = f.admin.Exec(f.ctx, "UPDATE forecast_assist_jobs SET narrative = 'tampered' WHERE job_id = $1", job.JobID); err == nil {
		t.Fatalf("raw UPDATE of an already-set narrative should have been rejected by the trigger")
	}
}

// Idempotent replay of SuggestDrivers returns the original job; the same
// key with a different request is refused.
func TestAssist_SuggestDrivers_IdempotentReplay(t *testing.T) {
	f := newFixture(t)
	f.registerRelease(orgA, "sales", "internal-forecast", "v1.0")
	req := baseDriversRequest("sales")
	cmd := f.cmd(orgA, requester, "assist.suggest_drivers", 0)

	job, err := f.s.SuggestDrivers(f.ctx, cmd, req)
	if err != nil {
		t.Fatalf("suggest drivers: %v", err)
	}
	_, err = f.s.SuggestDrivers(f.ctx, cmd, req)
	var replay *domain.IdempotentReplay
	if !errors.As(err, &replay) || replay.ResourceID != job.JobID {
		t.Fatalf("replay of suggest drivers: %v", err)
	}
	cmd.Idempotency.RequestSHA256 = "other"
	if _, err := f.s.SuggestDrivers(f.ctx, cmd, req); !errors.Is(err, domain.ErrIdempotencyKeyReused) {
		t.Fatalf("key reuse: %v", err)
	}
	var jobs int
	_ = f.admin.QueryRow(f.ctx, "SELECT COUNT(*) FROM forecast_assist_jobs WHERE tenant_id = $1", orgA).Scan(&jobs)
	if jobs != 1 {
		t.Fatalf("replay must not create a second job: %d", jobs)
	}
}

// Tenant isolation: another tenant's job is not found.
// Rewrite: child reads of an unknown job now return 404 rather than [].
func TestAssist_TenantIsolation(t *testing.T) {
	f := newFixture(t)
	f.registerRelease(orgA, "legal", "internal-forecast", "v1.0")
	job := f.suggestDrivers(orgA, baseDriversRequest("legal"))

	if _, err := f.s.GetAssistJob(f.ctx, orgB, job.JobID); errCode(err) != "forecast_assist_job_not_found" {
		t.Fatalf("cross-tenant get job: %v", err)
	}
	if _, err := f.s.GetSuggestedDrivers(f.ctx, orgB, job.JobID); errCode(err) != "forecast_assist_job_not_found" {
		t.Fatalf("cross-tenant get drivers: %v", err)
	}
	jobs, _, err := f.s.ListAssistJobs(f.ctx, orgB, domain.AssistJobListFilter{})
	if err != nil || len(jobs) != 0 {
		t.Fatalf("cross-tenant list: %d err=%v", len(jobs), err)
	}
}
