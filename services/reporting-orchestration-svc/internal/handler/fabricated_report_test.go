package handler_test

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"zoiko.io/reporting-orchestration-svc/internal/domain"
)

// TestTriggerRun_DoesNotFabricateCompletion is the regression test for a
// defect distinct from anything tenant-isolation- or authorization-related:
// the service reporting that a report was generated when nothing was
// aggregated.
//
// OrchestratReportRun used to look up a fabricated row count from a
// hardcoded map keyed by report type, unconditionally set Status to
// COMPLETED, and synthesize an OutputLocation path that was never written
// to. There is no cross-service data-aggregation fan-out anywhere in this
// codebase — every triggered run produced an identically fake "successful"
// report regardless of report type or whether any source data existed.
//
// ZS-SVC-AC-001 (Governed Reporting/Analytics/Semantic Metrics/Export
// Control) names this exact anti-pattern in its own negative-path test
// NP-60: state must be derived from execution/certification evidence, never
// assumed. This test asserts the fix: the run is recorded as
// NOT_IMPLEMENTED with zero rows and no output location — the state that is
// actually true — rather than a fabricated COMPLETED.
func TestTriggerRun_DoesNotFabricateCompletion(t *testing.T) {
	router := newRouter(t)

	defBody, _ := json.Marshal(domain.CreateDefinitionRequest{
		LegalEntityID: "LE-9001", ReportName: "Fabrication regression report",
		ReportType: domain.ReportTypeAuditTrail, OutputFormat: domain.FormatJSON,
		DataSources: []string{"ledger-svc"},
	})
	defReq := httptest.NewRequest(http.MethodPost, "/v1/reports/definitions", bytes.NewReader(defBody))
	defReq.Header.Set("Content-Type", "application/json")
	defReq.Header.Set("X-Tenant-ID", "tenant-report-fab")
	defReq.Header.Set("X-Principal-Id", "principal-01")
	defRec := httptest.NewRecorder()
	router.ServeHTTP(defRec, defReq)
	if defRec.Code != http.StatusCreated {
		t.Fatalf("create definition: expected 201, got %d: %s", defRec.Code, defRec.Body.String())
	}
	var def domain.ReportDefinition
	if err := json.Unmarshal(defRec.Body.Bytes(), &def); err != nil {
		t.Fatalf("decode definition: %v", err)
	}

	runBody, _ := json.Marshal(domain.TriggerRunRequest{
		TriggeredBy: domain.TriggerManual, PeriodStart: "2026-01-01", PeriodEnd: "2026-12-31",
	})
	runReq := httptest.NewRequest(http.MethodPost, "/v1/reports/definitions/"+def.ID+"/runs", bytes.NewReader(runBody))
	runReq.Header.Set("Content-Type", "application/json")
	runReq.Header.Set("X-Tenant-ID", "tenant-report-fab")
	runReq.Header.Set("X-Principal-Id", "principal-01")
	runRec := httptest.NewRecorder()
	router.ServeHTTP(runRec, runReq)

	if runRec.Code != http.StatusAccepted {
		t.Fatalf("expected 202 (recorded, not completed), got %d: %s", runRec.Code, runRec.Body.String())
	}

	var run domain.ReportRun
	if err := json.Unmarshal(runRec.Body.Bytes(), &run); err != nil {
		t.Fatalf("decode run: %v", err)
	}

	if run.Status == domain.RunStatusCompleted {
		t.Fatalf("FABRICATION: run reports COMPLETED, but no cross-service aggregation exists in this codebase")
	}
	if run.Status != domain.RunStatusNotImplemented {
		t.Fatalf("expected NOT_IMPLEMENTED, got %q", run.Status)
	}
	if run.RowCount != 0 {
		t.Fatalf("FABRICATION: row_count is %d, but nothing was aggregated", run.RowCount)
	}
	if run.OutputLocation != "" {
		t.Fatalf("FABRICATION: output_location is %q, but nothing was written to any file", run.OutputLocation)
	}

	// Confirm via a fresh GET too — not just the trigger response — that
	// the honest state is what actually persisted, not just what was
	// returned once.
	getReq := httptest.NewRequest(http.MethodGet, "/v1/reports/runs/"+run.ID, nil)
	getReq.Header.Set("X-Tenant-ID", "tenant-report-fab")
	getRec := httptest.NewRecorder()
	router.ServeHTTP(getRec, getReq)
	if getRec.Code != http.StatusOK {
		t.Fatalf("get run: expected 200, got %d: %s", getRec.Code, getRec.Body.String())
	}
	var stored domain.ReportRun
	if err := json.Unmarshal(getRec.Body.Bytes(), &stored); err != nil {
		t.Fatalf("decode stored run: %v", err)
	}
	if stored.Status != domain.RunStatusNotImplemented {
		t.Fatalf("FABRICATION: persisted run status is %q, expected NOT_IMPLEMENTED", stored.Status)
	}
}

// TestTriggerRun_SameFabricationForEveryReportType pins that the old
// behavior was not report-type-specific bad luck: every one of the six
// known report types received a fabricated row count from the same
// hardcoded map, so this checks more than one to make sure the fix is not
// accidentally type-specific either.
func TestTriggerRun_SameFabricationForEveryReportType(t *testing.T) {
	router := newRouter(t)

	for _, rt := range []domain.ReportType{
		domain.ReportTypeFinancialSummary,
		domain.ReportTypePayrollSummary,
		domain.ReportTypeCashFlow,
	} {
		t.Run(string(rt), func(t *testing.T) {
			defBody, _ := json.Marshal(domain.CreateDefinitionRequest{
				LegalEntityID: "LE-9002", ReportName: "type check " + string(rt),
				ReportType: rt, OutputFormat: domain.FormatJSON, DataSources: []string{"ledger-svc"},
			})
			defReq := httptest.NewRequest(http.MethodPost, "/v1/reports/definitions", bytes.NewReader(defBody))
			defReq.Header.Set("Content-Type", "application/json")
			defReq.Header.Set("X-Tenant-ID", "tenant-report-fab2")
			defReq.Header.Set("X-Principal-Id", "principal-01")
			defRec := httptest.NewRecorder()
			router.ServeHTTP(defRec, defReq)
			var def domain.ReportDefinition
			_ = json.Unmarshal(defRec.Body.Bytes(), &def)

			runBody, _ := json.Marshal(domain.TriggerRunRequest{TriggeredBy: domain.TriggerManual})
			runReq := httptest.NewRequest(http.MethodPost, "/v1/reports/definitions/"+def.ID+"/runs", bytes.NewReader(runBody))
			runReq.Header.Set("Content-Type", "application/json")
			runReq.Header.Set("X-Tenant-ID", "tenant-report-fab2")
			runReq.Header.Set("X-Principal-Id", "principal-01")
			runRec := httptest.NewRecorder()
			router.ServeHTTP(runRec, runReq)

			var run domain.ReportRun
			_ = json.Unmarshal(runRec.Body.Bytes(), &run)
			if run.Status != domain.RunStatusNotImplemented || run.RowCount != 0 {
				t.Fatalf("FABRICATION for report type %s: status=%s row_count=%d", rt, run.Status, run.RowCount)
			}
		})
	}
}
