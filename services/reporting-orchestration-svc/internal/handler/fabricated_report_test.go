package handler_test

import (
	"net/http"
	"testing"
)

// TestTriggerRun_DoesNotFabricateCompletion is the regression test for a
// defect distinct from anything tenant-isolation- or authorization-related:
// the service reporting that a report was generated when nothing was
// aggregated.
//
// OrchestratReportRun used to look up a fabricated row count from a
// hardcoded map keyed by report type, unconditionally set Status to
// COMPLETED, and synthesize an OutputLocation path that was never written
// to. ZS-SVC-AC-001 NP-60: state must be derived from execution/
// certification evidence, never assumed. The run is recorded as
// NOT_IMPLEMENTED with zero rows and no output location.
//
// Ported to the rewrite: the definition must now be walked to ACTIVE
// (REP-05) before a run is accepted; the assertions are unchanged.
func TestTriggerRun_DoesNotFabricateCompletion(t *testing.T) {
	c := newClient(t, newRouter(t))
	defID := c.activeDefinition("AUDIT_TRAIL")

	run := expectStatus(t, c.do(http.MethodPost, "/v1/reports/definitions/"+defID+"/runs",
		map[string]string{"triggered_by": "MANUAL", "period_start": "2026-01-01", "period_end": "2026-12-31"}), http.StatusAccepted)

	if run["status"] == "COMPLETED" {
		t.Fatalf("FABRICATION: run reports COMPLETED, but no cross-service aggregation exists in this codebase")
	}
	if run["status"] != "NOT_IMPLEMENTED" {
		t.Fatalf("expected NOT_IMPLEMENTED, got %v", run["status"])
	}
	if run["row_count"].(float64) != 0 {
		t.Fatalf("FABRICATION: row_count is %v, but nothing was aggregated", run["row_count"])
	}
	if run["output_location"] != nil {
		t.Fatalf("FABRICATION: output_location is %v, but nothing was written", run["output_location"])
	}

	// The honest state is what persisted, not just what was returned once.
	stored := expectStatus(t, c.do(http.MethodGet, "/v1/reports/runs/"+run["id"].(string), nil), http.StatusOK)
	if stored["status"] != "NOT_IMPLEMENTED" {
		t.Fatalf("FABRICATION: persisted run status is %v, expected NOT_IMPLEMENTED", stored["status"])
	}
}

// TestTriggerRun_SameFabricationForEveryReportType checks more than one
// report type so the fix is not accidentally type-specific.
func TestTriggerRun_SameFabricationForEveryReportType(t *testing.T) {
	c := newClient(t, newRouter(t))
	for _, reportType := range []string{"FINANCIAL_SUMMARY", "PAYROLL_SUMMARY", "CASH_FLOW"} {
		t.Run(reportType, func(t *testing.T) {
			defID := c.activeDefinition(reportType)
			run := expectStatus(t, c.do(http.MethodPost, "/v1/reports/definitions/"+defID+"/runs",
				map[string]string{"triggered_by": "MANUAL"}), http.StatusAccepted)
			if run["status"] != "NOT_IMPLEMENTED" || run["row_count"].(float64) != 0 {
				t.Fatalf("FABRICATION for report type %s: status=%v row_count=%v", reportType, run["status"], run["row_count"])
			}
		})
	}
}
