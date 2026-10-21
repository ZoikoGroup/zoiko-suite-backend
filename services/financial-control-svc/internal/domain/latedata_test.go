package domain

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func TestLateDataException_IsAnOwnedDatedFindingOnTheFrozenPopulation(t *testing.T) {
	now := time.Date(2026, 10, 1, 9, 0, 0, 0, time.UTC)
	run := ControlRun{RunID: "run-1", TenantID: "t", LegalEntityID: "le"}
	snap := PopulationSnapshot{PopulationID: "pop-b", RunID: "run-1", Side: SideB, SpecRef: "gl/postings", Watermark: "wm-1"}
	e := LateDataException(run, snap, "wm-2", "GL_CONTROLLER", now)

	if e.Category != CatLateData || e.ReasonCode != ReasonLateDataAfterCertification || e.Side != SideB {
		t.Fatalf("wrong finding: %+v", e)
	}
	if len(e.RecordIDs) != 1 || e.RecordIDs[0] != "pop-b" {
		t.Fatalf("record_ids must name the stale frozen population, got %v", e.RecordIDs)
	}
	if e.State != ExOpen || e.OwnerRole != "GL_CONTROLLER" || e.Severity != SeverityHigh || !e.DueAt.Equal(now.Add(DefaultSLA(SeverityHigh))) {
		t.Fatalf("Invariant 7 fields wrong: %+v", e)
	}
	if e.Exposure != "0" || len(e.Currency) != 3 {
		t.Fatalf("exposure/currency: %q %q", e.Exposure, e.Currency)
	}
	if !strings.Contains(e.Detail, `"wm-1"`) || !strings.Contains(e.Detail, `"wm-2"`) || !strings.Contains(e.Detail, "gl/postings") {
		t.Fatalf("detail must name both watermarks and the population: %s", e.Detail)
	}
}

// The gate decides it at the same point as every other reason: a certified latest
// run with an unresolved late-data exception blocks; without one it passes.
func TestEvaluateGate_LateDataAfterCertificationBlocks(t *testing.T) {
	certified := func(code, late string) GateInput {
		return GateInput{ControlCode: code, RunID: "r-" + code, Lifecycle: string(LifecycleCertified), Result: "PASS",
			Certification: string(CertCertified), LateDataExceptionID: late, LateDataDetectedAt: map[bool]string{true: "2026-10-01T09:00:00Z"}[late != ""]}
	}
	g := EvaluateGate("le", "2026-09", []GateInput{certified("A", ""), certified("B", "ex-9")})
	if g.Open || g.Blocking != 1 {
		t.Fatalf("expected one blocking control, got %+v", g)
	}
	a, b := g.Items[0], g.Items[1]
	if a.Status != GatePassed || a.LateDataExceptionID != "" {
		t.Fatalf("A: %+v", a)
	}
	if b.Status != GateBlocking || b.Reason != GateReasonLateDataAfterCertification || b.LateDataExceptionID != "ex-9" ||
		b.LateDataDetectedAt == "" || b.Certificatio != string(CertCertified) {
		t.Fatalf("B: %+v", b)
	}

	// A revoked certification is reported as revoked, whatever else is open on it.
	rev := certified("C", "ex-10")
	rev.Certification = string(CertRevoked)
	if it := EvaluateGate("le", "2026-09", []GateInput{rev}).Items[0]; it.Reason != GateReasonCertificationRevoked {
		t.Fatalf("revoked: %+v", it)
	}
}

func TestScanRule_CannotUseTheReservedLateDataReason(t *testing.T) {
	_, err := ParseRuleLogic(json.RawMessage(`{"kind":"EXCEPTION_SCAN","finding_category":"LATE_DATA",` +
		`"finding_reason":"LATE_DATA_AFTER_CERTIFICATION","finding_assertion":"COMPLETENESS"}`))
	if err == nil {
		t.Fatal("a scan rule must not be able to emit the late-data sweep's reason code")
	}
	if _, err := ParseRuleLogic(json.RawMessage(`{"kind":"EXCEPTION_SCAN","finding_category":"LATE_DATA",` +
		`"finding_reason":"LATE_POSTING","finding_assertion":"COMPLETENESS"}`)); err != nil {
		t.Fatalf("other LATE_DATA scan reasons stay allowed: %v", err)
	}
}
