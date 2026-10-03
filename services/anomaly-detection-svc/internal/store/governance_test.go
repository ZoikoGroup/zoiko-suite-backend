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
		ObservedDriftBP: 100, BusinessContext: "nightly batch",
		Signals: []domain.GovernedSignalInput{
			{SourceEntityRef: "txn-1", Severity: "HIGH", AnomalyScoreBP: score, Features: map[string]string{"merchant": "Acme"}},
		},
	}
}

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
	if signals[0].ModelProvider != "internal-anomaly" || signals[0].ModelVersion != "v1.0" {
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
// each signal — already asserted above for the happy path; here we
// additionally assert it survives a full Confirmed disposition.
func TestGovernance_FeatureModelVersionRetainedWithSignal(t *testing.T) {
	f := newFixture(t)
	f.registerModel(orgA, "payroll", "internal-anomaly", "v1.0", 5000, 500)
	run := f.runDetection(orgA, baseRunRequest("payroll", 9500))
	signals, _ := f.s.GetSignalsByRun(f.ctx, orgA, run.RunID)
	sig := signals[0]

	closed, err := f.s.CloseSignal(f.ctx, orgA, sig.SignalID, domain.CloseSignalRequest{
		Outcome: domain.SignalConfirmed, Notes: "confirmed duplicate payroll run",
	}, "reviewer-bob", f.claim("CloseSignal", sig.SignalID))
	if err != nil {
		t.Fatalf("close signal: %v", err)
	}
	if closed.ModelProvider != "internal-anomaly" || closed.ModelVersion != "v1.0" {
		t.Fatalf("model provenance lost after disposition: %+v", closed)
	}
}

// Doc-named acceptance test: a dismissed signal remains evidence for
// evaluation and drift review — its full record (score, features,
// model version) stays intact and readable after dismissal, and the
// disposition itself is permanent.
func TestGovernance_DismissedSignalRemainsEvidence(t *testing.T) {
	f := newFixture(t)
	f.registerModel(orgA, "ap", "internal-anomaly", "v1.0", 5000, 500)
	run := f.runDetection(orgA, baseRunRequest("ap", 7000))
	signals, _ := f.s.GetSignalsByRun(f.ctx, orgA, run.RunID)
	sig := signals[0]

	dismissed, err := f.s.CloseSignal(f.ctx, orgA, sig.SignalID, domain.CloseSignalRequest{
		Outcome: domain.SignalDismissed, Notes: "known seasonal spike, not anomalous",
	}, "reviewer-bob", f.claim("CloseSignal", sig.SignalID))
	if err != nil {
		t.Fatalf("dismiss signal: %v", err)
	}
	if dismissed.Status != domain.SignalDismissed {
		t.Fatalf("dismissed signal status: %+v", dismissed)
	}

	// The full evidentiary record survives, unchanged, after dismissal.
	reloaded, err := f.s.GetGovernedSignal(f.ctx, orgA, sig.SignalID)
	if err != nil {
		t.Fatalf("reload dismissed signal: %v", err)
	}
	if reloaded.AnomalyScoreBP != 7000 || reloaded.ContentHash != sig.ContentHash || reloaded.Features["merchant"] != "Acme" {
		t.Fatalf("dismissed signal evidence changed: %+v", reloaded)
	}

	disp, err := f.s.GetDisposition(f.ctx, orgA, sig.SignalID)
	if err != nil || disp.Outcome != domain.SignalDismissed {
		t.Fatalf("disposition after dismissal: %+v (err=%v)", disp, err)
	}

	// Immutability is structural, not just convention: a raw UPDATE
	// through the app role is rejected at the database.
	_, err = f.admin.Exec(f.ctx, `UPDATE anomaly_signals SET status = 'Detected' WHERE signal_id = $1`, sig.SignalID)
	if err == nil {
		t.Fatalf("reopening a dismissed signal via raw UPDATE should have been rejected by the trigger")
	}
}

// Doc-named acceptance test: anomaly score alone cannot freeze
// payment/write off balance. CloseSignal, even for a CRITICAL severity
// Confirmed disposition, writes only this layer's own evidence row —
// there is no field or side effect capable of freezing anything else.
func TestGovernance_AnomalyScoreAloneCannotFreezePayment(t *testing.T) {
	f := newFixture(t)
	f.registerModel(orgA, "treasury", "internal-anomaly", "v1.0", 1000, 500)
	req := baseRunRequest("treasury", 10000)
	req.Signals[0].Severity = "CRITICAL"
	run := f.runDetection(orgA, req)
	signals, _ := f.s.GetSignalsByRun(f.ctx, orgA, run.RunID)
	sig := signals[0]

	confirmed, err := f.s.CloseSignal(f.ctx, orgA, sig.SignalID, domain.CloseSignalRequest{
		Outcome: domain.SignalConfirmed, Notes: "confirmed fraudulent wire attempt",
	}, "reviewer-bob", f.claim("CloseSignal", sig.SignalID))
	if err != nil {
		t.Fatalf("close critical signal: %v", err)
	}
	if confirmed.Status != domain.SignalConfirmed {
		t.Fatalf("confirmed signal: %+v", confirmed)
	}
	// CloseSignalRequest structurally has no field for a payment/freeze
	// instruction — the only artifact this call can produce is the
	// disposition evidence row, confirmed below.
	disp, err := f.s.GetDisposition(f.ctx, orgA, sig.SignalID)
	if err != nil || disp.Outcome != domain.SignalConfirmed {
		t.Fatalf("disposition evidence: %+v (err=%v)", disp, err)
	}
}

// Doc-named acceptance test (negative scenario N-37): anomaly model
// drift exceeds threshold -> the run is refused outright, before any
// signal is created.
func TestGovernance_ModelDriftBeyondThreshold_BlocksRunDetection(t *testing.T) {
	f := newFixture(t)
	f.registerModel(orgA, "inventory", "internal-anomaly", "v1.0", 5000, 500)

	req := baseRunRequest("inventory", 6000)
	req.ObservedDriftBP = 900 // exceeds the registered 500bp max
	_, err := f.s.RunDetection(f.ctx, orgA, req, "test-operator", f.claim("RunDetection", "inventory-drift"))
	if !errors.Is(err, domain.ErrDriftExceedsThreshold) {
		t.Fatalf("run detection with excessive drift: %v", err)
	}

	// Against an unregistered model version, fails closed too.
	req2 := baseRunRequest("inventory", 6000)
	req2.ModelVersion = "v9.9-unregistered"
	if _, err := f.s.RunDetection(f.ctx, orgA, req2, "test-operator", f.claim("RunDetection", "inventory-unreg")); !errors.Is(err, domain.ErrAnomalyModelNotFound) {
		t.Fatalf("run detection against unregistered model: %v", err)
	}
}

// Forward-only lifecycle: AcknowledgeSignal then EscalateForReview
// moves Detected -> ReviewPending -> Escalated; a signal cannot skip a
// stage or be acted on twice.
func TestGovernance_SignalLifecycle_ForwardOnly(t *testing.T) {
	f := newFixture(t)
	f.registerModel(orgA, "hr", "internal-anomaly", "v1.0", 9000, 500)
	run := f.runDetection(orgA, baseRunRequest("hr", 2000))
	signals, _ := f.s.GetSignalsByRun(f.ctx, orgA, run.RunID)
	sig := signals[0]
	if sig.Status != domain.SignalDetected {
		t.Fatalf("initial signal status: %+v", sig)
	}

	// Cannot escalate or close before acknowledgement.
	if _, err := f.s.EscalateForReview(f.ctx, orgA, sig.SignalID, domain.EscalateForReviewRequest{Reason: "too early"},
		"reviewer-bob", f.claim("EscalateForReview-early", sig.SignalID)); !errors.Is(err, domain.ErrSignalNotReviewable) {
		t.Fatalf("escalate before acknowledge: %v", err)
	}

	ack, err := f.s.AcknowledgeSignal(f.ctx, orgA, sig.SignalID, "reviewer-bob", f.claim("AcknowledgeSignal", sig.SignalID))
	if err != nil {
		t.Fatalf("acknowledge signal: %v", err)
	}
	if ack.Status != domain.SignalReviewPending {
		t.Fatalf("acknowledged signal: %+v", ack)
	}

	// Cannot acknowledge twice.
	if _, err := f.s.AcknowledgeSignal(f.ctx, orgA, sig.SignalID, "reviewer-bob",
		f.claim("AcknowledgeSignal-again", sig.SignalID)); !errors.Is(err, domain.ErrSignalNotDetected) {
		t.Fatalf("double acknowledge: %v", err)
	}

	escalated, err := f.s.EscalateForReview(f.ctx, orgA, sig.SignalID, domain.EscalateForReviewRequest{Reason: "needs compliance review"},
		"reviewer-bob", f.claim("EscalateForReview", sig.SignalID))
	if err != nil {
		t.Fatalf("escalate signal: %v", err)
	}
	if escalated.Status != domain.SignalEscalated {
		t.Fatalf("escalated signal: %+v", escalated)
	}

	// Terminal: cannot close an already-escalated signal.
	if _, err := f.s.CloseSignal(f.ctx, orgA, sig.SignalID, domain.CloseSignalRequest{Outcome: domain.SignalConfirmed, Notes: "too late"},
		"reviewer-bob", f.claim("CloseSignal-late", sig.SignalID)); !errors.Is(err, domain.ErrSignalNotReviewable) {
		t.Fatalf("close an escalated signal: %v", err)
	}
}

// Idempotent replay of RunDetection returns the original run, never a
// second one.
func TestGovernance_RunDetection_IdempotentReplay(t *testing.T) {
	f := newFixture(t)
	f.registerModel(orgA, "legal", "internal-anomaly", "v1.0", 5000, 500)
	req := baseRunRequest("legal", 6000)
	claim := f.claim("RunDetection", "legal-replay")

	run, err := f.s.RunDetection(f.ctx, orgA, req, "test-operator", claim)
	if err != nil {
		t.Fatalf("run detection: %v", err)
	}
	_, err = f.s.RunDetection(f.ctx, orgA, req, "test-operator", claim)
	var replay *domain.IdempotentReplayError
	if !errors.As(err, &replay) || replay.ResourceID != run.RunID {
		t.Fatalf("replay of run detection: %v", err)
	}
}

// Tenant isolation: cross-tenant reads are denied by RLS, not an
// application-level filter.
func TestGovernance_TenantIsolation(t *testing.T) {
	f := newFixture(t)
	f.registerModel(orgA, "sales", "internal-anomaly", "v1.0", 5000, 500)
	run := f.runDetection(orgA, baseRunRequest("sales", 6000))

	signals, err := f.s.GetSignalsByRun(f.ctx, orgB, run.RunID)
	if err != nil {
		t.Fatalf("cross-tenant get signals: %v", err)
	}
	if len(signals) != 0 {
		t.Fatalf("cross-tenant read leaked signals: %+v", signals)
	}
}
