package store_test

import (
	"errors"
	"testing"

	"zoiko.io/anomaly-detection-svc/internal/domain"
)

// AI-04 governed advisory layer, against real Postgres as the
// NOSUPERUSER NOBYPASSRLS app role.

func baseRunRequest(domainName string, score int32) domain.RunDetectionRequest {
	return domain.RunDetectionRequest{
		DomainName: domainName, ModelProvider: "internal-anomaly", ModelVersion: "v1.0",
		FeatureSetVersion: "fs-1", ObservedDriftBP: i32(100), BusinessContext: "nightly batch",
		Signals: []domain.GovernedSignalInput{
			{SourceEntityRef: "txn-1", Severity: "HIGH", AnomalyScoreBP: i32(score), Features: map[string]string{"merchant": "Acme"}},
		},
	}
}

const (
	runRoute   = "POST /v1/anomaly-governance/runs"
	ackRoute   = "POST /v1/anomaly-governance/signals/{signalID}:acknowledge"
	escRoute   = "POST /v1/anomaly-governance/signals/{signalID}:escalate"
	closeRoute = "POST /v1/anomaly-governance/signals/{signalID}:close"
	modelRoute = "PUT /v1/anomaly-governance/models/{modelID}"
)

// Happy path: a signal below the review threshold stays Detected; one
// above it lands ReviewPending automatically.
func TestGovernance_HappyPath_ReviewThresholdGating(t *testing.T) {
	f := newFixture(t)
	f.registerModel(orgA, "expense", "internal-anomaly", "v1.0", 8000, 500)

	run := f.runDetection(orgA, baseRunRequest("expense", 3000))
	signals, err := f.s.GetSignalsByRun(f.ctx, orgA, run.RunID)
	if err != nil {
		t.Fatalf("get signals: %v", err)
	}
	if len(signals) != 1 || signals[0].Status != domain.SignalDetected {
		t.Fatalf("low-score signal: %+v", signals)
	}
	if signals[0].ModelProvider != "internal-anomaly" || signals[0].ModelVersion != "v1.0" || signals[0].FeatureSetVersion != "fs-1" {
		t.Fatalf("signal did not retain model provenance: %+v", signals[0])
	}

	run2 := f.runDetection(orgA, baseRunRequest("expense", 9000))
	signals2, err := f.s.GetSignalsByRun(f.ctx, orgA, run2.RunID)
	if err != nil {
		t.Fatalf("get signals: %v", err)
	}
	if len(signals2) != 1 || signals2[0].Status != domain.SignalReviewPending {
		t.Fatalf("high-score signal: %+v", signals2)
	}
}

// Doc-named acceptance test: feature/model version is retained with
// each signal and survives a full Confirmed disposition.
func TestGovernance_FeatureModelVersionRetainedWithSignal(t *testing.T) {
	f := newFixture(t)
	f.registerModel(orgA, "payroll", "internal-anomaly", "v1.0", 5000, 500)
	run := f.runDetection(orgA, baseRunRequest("payroll", 9500))
	signals, _ := f.s.GetSignalsByRun(f.ctx, orgA, run.RunID)

	closed, err := f.closeSignal(orgA, signals[0], domain.SignalConfirmed, "CONFIRMED_EXCEPTION", "confirmed duplicate payroll run")
	if err != nil {
		t.Fatalf("close signal: %v", err)
	}
	if closed.ModelProvider != "internal-anomaly" || closed.ModelVersion != "v1.0" || closed.FeatureSetVersion != "fs-1" {
		t.Fatalf("model provenance lost after disposition: %+v", closed)
	}
}

// Doc-named acceptance test: a dismissed signal remains evidence.
func TestGovernance_DismissedSignalRemainsEvidence(t *testing.T) {
	f := newFixture(t)
	f.registerModel(orgA, "ap", "internal-anomaly", "v1.0", 5000, 500)
	run := f.runDetection(orgA, baseRunRequest("ap", 7000))
	signals, _ := f.s.GetSignalsByRun(f.ctx, orgA, run.RunID)
	sig := signals[0]

	dismissed, err := f.closeSignal(orgA, sig, domain.SignalDismissed, "FALSE_POSITIVE", "known seasonal spike, not anomalous")
	if err != nil {
		t.Fatalf("dismiss signal: %v", err)
	}
	if dismissed.Status != domain.SignalDismissed {
		t.Fatalf("dismissed signal status: %+v", dismissed)
	}

	reloaded, err := f.s.GetGovernedSignal(f.ctx, orgA, sig.SignalID)
	if err != nil {
		t.Fatalf("reload dismissed signal: %v", err)
	}
	if reloaded.AnomalyScoreBP != 7000 || reloaded.ContentHash != sig.ContentHash || reloaded.Features["merchant"] != "Acme" {
		t.Fatalf("dismissed signal evidence changed: %+v", reloaded)
	}

	disp, err := f.s.GetDisposition(f.ctx, orgA, sig.SignalID)
	if err != nil || disp.Outcome != domain.SignalDismissed || disp.ReasonCode != "FALSE_POSITIVE" || disp.Actor != reviewer {
		t.Fatalf("disposition after dismissal: %+v (err=%v)", disp, err)
	}

	// Immutability is structural: a raw UPDATE is rejected by the trigger.
	if _, err = f.admin.Exec(f.ctx, "UPDATE anomaly_signals SET status = 'Detected' WHERE signal_id = $1", sig.SignalID); err == nil {
		t.Fatalf("reopening a dismissed signal via raw UPDATE should have been rejected by the trigger")
	}
}

// Doc-named acceptance test: anomaly score alone cannot freeze
// payment/write off balance.
func TestGovernance_AnomalyScoreAloneCannotFreezePayment(t *testing.T) {
	f := newFixture(t)
	f.registerModel(orgA, "treasury", "internal-anomaly", "v1.0", 1000, 500)
	req := baseRunRequest("treasury", 10000)
	req.Signals[0].Severity = "CRITICAL"
	run := f.runDetection(orgA, req)
	signals, _ := f.s.GetSignalsByRun(f.ctx, orgA, run.RunID)
	sig := signals[0]

	freeze := domain.CloseSignalRequest{Outcome: domain.SignalConfirmed, ReasonCode: "CONFIRMED_EXCEPTION",
		Notes: "freeze it", RequestedAction: "FREEZE_PAYMENT"}
	if err := freeze.Validate(); !errors.Is(err, domain.ErrAdvisoryBoundary) {
		t.Fatalf("FREEZE_PAYMENT must be refused as advisory_boundary_violation, got %v", err)
	}

	confirmed, err := f.closeSignal(orgA, sig, domain.SignalConfirmed, "CONFIRMED_EXCEPTION", "confirmed fraudulent wire attempt")
	if err != nil {
		t.Fatalf("close critical signal: %v", err)
	}
	if confirmed.Status != domain.SignalConfirmed {
		t.Fatalf("confirmed signal: %+v", confirmed)
	}
	disp, err := f.s.GetDisposition(f.ctx, orgA, sig.SignalID)
	if err != nil || disp.Outcome != domain.SignalConfirmed {
		t.Fatalf("disposition evidence: %+v (err=%v)", disp, err)
	}
}

// Doc-named acceptance test (N-37): drift above threshold refuses the run
// before any signal is created; an unregistered model fails closed.
func TestGovernance_ModelDriftBeyondThreshold_BlocksRunDetection(t *testing.T) {
	f := newFixture(t)
	f.registerModel(orgA, "inventory", "internal-anomaly", "v1.0", 5000, 500)

	req := baseRunRequest("inventory", 6000)
	req.ObservedDriftBP = i32(900)
	_, err := f.s.RunDetection(f.ctx, f.cmd(orgA, submitter, runRoute, nil), req)
	if !errors.Is(err, domain.ErrDriftExceedsThreshold) {
		t.Fatalf("run detection with excessive drift: %v", err)
	}
	var runs int
	if err := f.admin.QueryRow(f.ctx, "SELECT COUNT(*) FROM detection_runs WHERE tenant_id = $1", orgA).Scan(&runs); err != nil || runs != 0 {
		t.Fatalf("refused run must persist nothing: runs=%d err=%v", runs, err)
	}

	// Rewrite: an unregistered model is anomaly_model_not_registered (422)
	// rather than the old ErrAnomalyModelNotFound; still fails closed.
	req2 := baseRunRequest("inventory", 6000)
	req2.ModelVersion = "v9.9-unregistered"
	if _, err := f.s.RunDetection(f.ctx, f.cmd(orgA, submitter, runRoute, nil), req2); !errors.Is(err, domain.ErrAnomalyModelNotRegistered) {
		t.Fatalf("run detection against unregistered model: %v", err)
	}
}

// Drift policy DISABLE refuses the run and leaves the model Disabled;
// ESCALATE accepts it but forces every signal to ReviewPending.
func TestGovernance_DriftPolicies(t *testing.T) {
	f := newFixture(t)
	disable := f.registerModel(orgA, "dis", "internal-anomaly", "v1.0", 9000, 100)
	upd := domain.UpdateAnomalyModelRequest{ReviewThresholdBP: i32(9000), MaxDriftBP: i32(100), DriftPolicy: domain.DriftPolicyDisable, Status: domain.ModelActive}
	if _, err := f.s.UpdateAnomalyModel(f.ctx, f.cmd(orgA, submitter, modelRoute, ver(disable.Version)), disable.ModelID, upd); err != nil {
		t.Fatalf("set DISABLE policy: %v", err)
	}
	req := baseRunRequest("dis", 100)
	req.ObservedDriftBP = i32(500)
	if _, err := f.s.RunDetection(f.ctx, f.cmd(orgA, submitter, runRoute, nil), req); !errors.Is(err, domain.ErrDriftExceedsThreshold) {
		t.Fatalf("DISABLE policy run: %v", err)
	}
	model, err := f.s.GetAnomalyModel(f.ctx, orgA, disable.ModelID)
	if err != nil || model.Status != domain.ModelDisabled {
		t.Fatalf("model should be Disabled after drift under DISABLE policy: %+v err=%v", model, err)
	}
	if _, err := f.s.RunDetection(f.ctx, f.cmd(orgA, submitter, runRoute, nil), baseRunRequest("dis", 100)); !errors.Is(err, domain.ErrAnomalyModelDisabled) {
		t.Fatalf("run against disabled model: %v", err)
	}

	esc := f.registerModel(orgA, "esc", "internal-anomaly", "v1.0", 9000, 100)
	upd.DriftPolicy = domain.DriftPolicyEscalate
	if _, err := f.s.UpdateAnomalyModel(f.ctx, f.cmd(orgA, submitter, modelRoute, ver(esc.Version)), esc.ModelID, upd); err != nil {
		t.Fatalf("set ESCALATE policy: %v", err)
	}
	req = baseRunRequest("esc", 100)
	req.ObservedDriftBP = i32(500)
	run := f.runDetection(orgA, req)
	if !run.DriftBreached || run.Signals[0].Status != domain.SignalReviewPending {
		t.Fatalf("ESCALATE run must be drift_breached with ReviewPending signals: %+v", run)
	}

	metrics, err := f.s.GetModelDriftMetrics(f.ctx, orgA, esc.ModelID)
	if err != nil || metrics.RunCount != 1 || metrics.DriftBreachedRuns != 1 || metrics.SignalCount != 1 {
		t.Fatalf("drift metrics: %+v err=%v", metrics, err)
	}
}

// Forward-only lifecycle: Detected -> ReviewPending -> Escalated; no
// skipping, no repeats; If-Match enforced.
func TestGovernance_SignalLifecycle_ForwardOnly(t *testing.T) {
	f := newFixture(t)
	f.registerModel(orgA, "hr", "internal-anomaly", "v1.0", 9000, 500)
	run := f.runDetection(orgA, baseRunRequest("hr", 2000))
	sig := run.Signals[0]
	if sig.Status != domain.SignalDetected {
		t.Fatalf("initial signal status: %+v", sig)
	}

	if _, err := f.s.EscalateForReview(f.ctx, f.cmd(orgA, reviewer, escRoute, ver(sig.Version)), sig.SignalID,
		domain.EscalateForReviewRequest{Reason: "too early"}); !errors.Is(err, domain.ErrSignalNotReviewable) {
		t.Fatalf("escalate before acknowledge: %v", err)
	}

	ack, err := f.s.AcknowledgeSignal(f.ctx, f.cmd(orgA, reviewer, ackRoute, ver(sig.Version)), sig.SignalID, domain.AcknowledgeSignalRequest{})
	if err != nil {
		t.Fatalf("acknowledge signal: %v", err)
	}
	if ack.Resource.Status != domain.SignalReviewPending || ack.Resource.Version != sig.Version+1 {
		t.Fatalf("acknowledged signal: %+v", ack.Resource)
	}

	if _, err := f.s.AcknowledgeSignal(f.ctx, f.cmd(orgA, reviewer, ackRoute, ver(sig.Version)), sig.SignalID, domain.AcknowledgeSignalRequest{}); !errors.Is(err, domain.ErrStaleVersion) {
		t.Fatalf("stale If-Match: %v", err)
	}
	if _, err := f.s.AcknowledgeSignal(f.ctx, f.cmd(orgA, reviewer, ackRoute, nil), sig.SignalID, domain.AcknowledgeSignalRequest{}); !errors.Is(err, domain.ErrPreconditionRequired) {
		t.Fatalf("missing If-Match: %v", err)
	}
	if _, err := f.s.AcknowledgeSignal(f.ctx, f.cmd(orgA, reviewer, ackRoute, ver(ack.Resource.Version)), sig.SignalID, domain.AcknowledgeSignalRequest{}); !errors.Is(err, domain.ErrSignalNotDetected) {
		t.Fatalf("double acknowledge: %v", err)
	}

	escalated, err := f.s.EscalateForReview(f.ctx, f.cmd(orgA, reviewer, escRoute, ver(ack.Resource.Version)), sig.SignalID,
		domain.EscalateForReviewRequest{Reason: "needs compliance review"})
	if err != nil {
		t.Fatalf("escalate signal: %v", err)
	}
	if escalated.Resource.Status != domain.SignalEscalated {
		t.Fatalf("escalated signal: %+v", escalated.Resource)
	}

	if _, err := f.closeSignal(orgA, *escalated.Resource, domain.SignalConfirmed, "CONFIRMED_EXCEPTION", "too late"); !errors.Is(err, domain.ErrSignalNotReviewable) {
		t.Fatalf("close an escalated signal: %v", err)
	}
}

// Segregation of duties: the run submitter cannot close its own signals.
func TestGovernance_CloseSignal_SegregationOfDuties(t *testing.T) {
	f := newFixture(t)
	f.registerModel(orgA, "sod", "internal-anomaly", "v1.0", 1000, 500)
	run := f.runDetection(orgA, baseRunRequest("sod", 9000))
	sig := run.Signals[0]
	_, err := f.s.CloseSignal(f.ctx, f.cmd(orgA, submitter, closeRoute, ver(sig.Version)),
		sig.SignalID, domain.CloseSignalRequest{Outcome: domain.SignalConfirmed, ReasonCode: "CONFIRMED_EXCEPTION", Notes: "self-review"})
	if !errors.Is(err, domain.ErrSegregationOfDuties) {
		t.Fatalf("submitter closing own signal: %v", err)
	}
}

// Idempotent replay of RunDetection returns the original run, never a
// second one; the same key with a different body is 409.
func TestGovernance_RunDetection_IdempotentReplay(t *testing.T) {
	f := newFixture(t)
	f.registerModel(orgA, "legal", "internal-anomaly", "v1.0", 5000, 500)
	req := baseRunRequest("legal", 6000)
	cmd := f.cmd(orgA, submitter, runRoute, nil)

	first, err := f.s.RunDetection(f.ctx, cmd, req)
	if err != nil {
		t.Fatalf("run detection: %v", err)
	}
	replay, err := f.s.RunDetection(f.ctx, cmd, req)
	if err != nil || !replay.Replayed || replay.Resource.RunID != first.Resource.RunID {
		t.Fatalf("replay of run detection: %+v err=%v", replay, err)
	}
	var runs int
	_ = f.admin.QueryRow(f.ctx, "SELECT COUNT(*) FROM detection_runs WHERE tenant_id = $1", orgA).Scan(&runs)
	if runs != 1 {
		t.Fatalf("replay must not create a second run, got %d", runs)
	}

	cmd.Idempotency.RequestSHA256 = "different-body"
	if _, err := f.s.RunDetection(f.ctx, cmd, req); !errors.Is(err, domain.ErrIdempotencyKeyReused) {
		t.Fatalf("key reuse with a different body: %v", err)
	}
}

// Tenant isolation: another tenant's resources are not found, never leaked.
// Rewrite: GetSignalsByRun now reports an unknown run as
// ErrDetectionRunNotFound instead of returning an empty list.
func TestGovernance_TenantIsolation(t *testing.T) {
	f := newFixture(t)
	f.registerModel(orgA, "sales", "internal-anomaly", "v1.0", 5000, 500)
	run := f.runDetection(orgA, baseRunRequest("sales", 6000))

	if _, err := f.s.GetSignalsByRun(f.ctx, orgB, run.RunID); !errors.Is(err, domain.ErrDetectionRunNotFound) {
		t.Fatalf("cross-tenant get signals: %v", err)
	}
	if _, err := f.s.GetGovernedSignal(f.ctx, orgB, run.Signals[0].SignalID); !errors.Is(err, domain.ErrSignalNotFound) {
		t.Fatalf("cross-tenant get signal: %v", err)
	}
	list, _, err := f.s.ListSignals(f.ctx, orgB, domain.SignalFilter{}, domain.PageRequest{Limit: 50})
	if err != nil || len(list) != 0 {
		t.Fatalf("cross-tenant list leaked: %d err=%v", len(list), err)
	}
}
